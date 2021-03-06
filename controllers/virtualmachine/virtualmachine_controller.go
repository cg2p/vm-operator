// Copyright (c) 2019-2021 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package virtualmachine

import (
	goctx "context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"github.com/pkg/errors"

	vmopv1alpha1 "github.com/vmware-tanzu/vm-operator-api/api/v1alpha1"

	"github.com/vmware-tanzu/vm-operator/pkg/conditions"
	"github.com/vmware-tanzu/vm-operator/pkg/context"
	"github.com/vmware-tanzu/vm-operator/pkg/lib"
	"github.com/vmware-tanzu/vm-operator/pkg/patch"
	"github.com/vmware-tanzu/vm-operator/pkg/prober"
	"github.com/vmware-tanzu/vm-operator/pkg/record"
	"github.com/vmware-tanzu/vm-operator/pkg/topology"
	"github.com/vmware-tanzu/vm-operator/pkg/vmprovider"
	"github.com/vmware-tanzu/vm-operator/pkg/vmprovider/providers/vsphere/constants"
	"github.com/vmware-tanzu/vm-operator/pkg/vmprovider/providers/vsphere/instancestorage"
)

const finalizerName = "virtualmachine.vmoperator.vmware.com"

// AddToManager adds this package's controller to the provided manager.
func AddToManager(ctx *context.ControllerManagerContext, mgr manager.Manager) error {
	var (
		controlledType     = &vmopv1alpha1.VirtualMachine{}
		controlledTypeName = reflect.TypeOf(controlledType).Elem().Name()

		controllerNameShort = fmt.Sprintf("%s-controller", strings.ToLower(controlledTypeName))
		controllerNameLong  = fmt.Sprintf("%s/%s/%s", ctx.Namespace, ctx.Name, controllerNameShort)
	)

	proberManager, err := prober.AddToManager(mgr, ctx.VMProvider)
	if err != nil {
		return err
	}

	r := NewReconciler(
		mgr.GetClient(),
		ctx.MaxConcurrentReconciles,
		ctrl.Log.WithName("controllers").WithName(controlledTypeName),
		record.New(mgr.GetEventRecorderFor(controllerNameLong)),
		ctx.VMProvider,
		proberManager,
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(controlledType).
		WithOptions(controller.Options{MaxConcurrentReconciles: ctx.MaxConcurrentReconciles}).
		Watches(&source.Kind{Type: &vmopv1alpha1.VirtualMachineClassBinding{}},
			handler.EnqueueRequestsFromMapFunc(classBindingToVMMapperFn(ctx, r.Client))).
		Watches(&source.Kind{Type: &vmopv1alpha1.ContentSourceBinding{}},
			handler.EnqueueRequestsFromMapFunc(csBindingToVMMapperFn(ctx, r.Client))).
		Complete(r)
}

// csBindingToVMMapperFn returns a mapper function that can be used to queue reconcile request
// for the VirtualMachines in response to an event on the ContentSourceBinding resource.
func csBindingToVMMapperFn(ctx *context.ControllerManagerContext, c client.Reader) func(o client.Object) []reconcile.Request {
	return func(o client.Object) []reconcile.Request {
		binding := o.(*vmopv1alpha1.ContentSourceBinding)
		logger := ctx.Logger.WithValues("name", binding.Name, "namespace", binding.Namespace)

		logger.V(4).Info("Reconciling all VMs using images from a ContentSource because of a ContentSourceBinding watch")

		contentSource := &vmopv1alpha1.ContentSource{}
		if err := c.Get(ctx, client.ObjectKey{Name: binding.ContentSourceRef.Name}, contentSource); err != nil {
			logger.Error(err, "Failed to get ContentSource for VM reconciliation due to ContentSourceBinding watch")
			return nil
		}

		providerRef := contentSource.Spec.ProviderRef
		// Assume that only supported type is ContentLibraryProvider.
		clProviderFromBinding := vmopv1alpha1.ContentLibraryProvider{}
		if err := c.Get(ctx, client.ObjectKey{Name: providerRef.Name}, &clProviderFromBinding); err != nil {
			logger.Error(err, "Failed to get ContentLibraryProvider for VM reconciliation due to ContentSourceBinding watch")
			return nil
		}

		// Filter images that have an OwnerReference to this ContentLibraryProvider.
		imageList := &vmopv1alpha1.VirtualMachineImageList{}
		if err := c.List(ctx, imageList); err != nil {
			logger.Error(err, "Failed to list VirtualMachineImages for VM reconciliation due to ContentSourceBinding watch")
			return nil
		}

		imagesToReconcile := make(map[string]struct{})
		for _, img := range imageList.Items {
			for _, ownerRef := range img.OwnerReferences {
				if ownerRef.Kind == "ContentLibraryProvider" && ownerRef.UID == clProviderFromBinding.UID {
					imagesToReconcile[img.Name] = struct{}{}
				}
			}
		}

		// Filter VMs that reference the images from the content source.
		vmList := &vmopv1alpha1.VirtualMachineList{}
		if err := c.List(ctx, vmList, client.InNamespace(binding.Namespace)); err != nil {
			logger.Error(err, "Failed to list VirtualMachines for reconciliation due to ContentSourceBinding watch")
			return nil
		}

		var reconcileRequests []reconcile.Request
		for _, vm := range vmList.Items {
			if _, ok := imagesToReconcile[vm.Spec.ImageName]; ok {
				key := client.ObjectKey{Namespace: vm.Namespace, Name: vm.Name}
				reconcileRequests = append(reconcileRequests, reconcile.Request{NamespacedName: key})
			}
		}

		logger.V(4).Info("Returning VM reconcile requests due to ContentSourceBinding watch", "requests", reconcileRequests)
		return reconcileRequests
	}
}

// classBindingToVMMapperFn returns a mapper function that can be used to queue reconcile request
// for the VirtualMachines in response to an event on the VirtualMachineClassBinding resource.
func classBindingToVMMapperFn(ctx *context.ControllerManagerContext, c client.Client) func(o client.Object) []reconcile.Request {
	// For a given VirtualMachineClassBinding, return reconcile requests
	// for those VirtualMachines with corresponding VirtualMachinesClasses referenced
	return func(o client.Object) []reconcile.Request {
		classBinding := o.(*vmopv1alpha1.VirtualMachineClassBinding)
		logger := ctx.Logger.WithValues("name", classBinding.Name, "namespace", classBinding.Namespace)

		logger.V(4).Info("Reconciling all VMs referencing a VM class because of a VirtualMachineClassBinding watch")

		// Find all vms that match this vmclassbinding
		vmList := &vmopv1alpha1.VirtualMachineList{}
		if err := c.List(ctx, vmList, client.InNamespace(classBinding.Namespace)); err != nil {
			logger.Error(err, "Failed to list VirtualMachines for reconciliation due to VirtualMachineClassBinding watch")
			return nil
		}

		// Populate reconcile requests for vms matching the classbinding reference
		var reconcileRequests []reconcile.Request
		for _, vm := range vmList.Items {
			if vm.Spec.ClassName == classBinding.ClassRef.Name {
				key := client.ObjectKey{Namespace: vm.Namespace, Name: vm.Name}
				reconcileRequests = append(reconcileRequests, reconcile.Request{NamespacedName: key})
			}
		}

		logger.V(4).Info("Returning VM reconcile requests due to VirtualMachineClassBinding watch", "requests", reconcileRequests)
		return reconcileRequests
	}
}

func NewReconciler(
	client client.Client,
	numReconcilers int,
	logger logr.Logger,
	recorder record.Recorder,
	vmProvider vmprovider.VirtualMachineProviderInterface,
	prober prober.Manager) *Reconciler {
	// Limit the maximum number of VirtualMachine creates by the provider. Calculated as MAX_CREATE_VMS_ON_PROVIDER
	// (default 80) percent of the total number of reconciler threads.
	maxConcurrentCreateVMsOnProvider := int(math.Ceil((float64(numReconcilers) * float64(lib.MaxConcurrentCreateVMsOnProvider())) / float64(100)))

	return &Reconciler{
		Client:                           client,
		Logger:                           logger,
		Recorder:                         recorder,
		VMProvider:                       vmProvider,
		Prober:                           prober,
		MaxConcurrentCreateVMsOnProvider: maxConcurrentCreateVMsOnProvider,
	}
}

// Reconciler reconciles a VirtualMachine object.
type Reconciler struct {
	client.Client
	Logger     logr.Logger
	Recorder   record.Recorder
	VMProvider vmprovider.VirtualMachineProviderInterface
	Prober     prober.Manager

	// Hack to limit concurrent create operations because they block and can take a long time.
	mutex                            sync.Mutex
	NumVMsBeingCreatedOnProvider     int
	MaxConcurrentCreateVMsOnProvider int
}

// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachineclasses,verbs=get;list
// +kubebuilder:rbac:groups=vmware.com,resources=virtualnetworkinterfaces;virtualnetworkinterfaces/status,verbs=create;get;list;patch;delete;watch;update
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas;namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachineclassbindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=contentsources,verbs=get;list;watch
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=contentlibraryproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=contentsourcebindings,verbs=get;list;watch

func (r *Reconciler) Reconcile(ctx goctx.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	vm := &vmopv1alpha1.VirtualMachine{}
	if err := r.Get(ctx, req.NamespacedName, vm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	vmCtx := &context.VirtualMachineContext{
		Context: ctx,
		Logger:  ctrl.Log.WithName("VirtualMachine").WithValues("name", vm.NamespacedName()),
		VM:      vm,
	}

	// If the VM has a pause reconcile annotation, it is being restored on vCenter. Return here so our reconcile
	// does not replace the VM being restored on the vCenter inventory.
	//
	// Do not requeue the reconcile here since removing the pause annotation will trigger a reconcile anyway.
	if _, ok := vm.Annotations[vmopv1alpha1.PauseAnnotation]; ok {
		vmCtx.Logger.Info("Skipping reconcile since Pause annotation is set on the VM")
		return ctrl.Result{}, nil
	}

	patchHelper, err := patch.NewHelper(vm, r.Client)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to init patch helper for %s", vmCtx.String())
	}

	defer func() {
		if err := patchHelper.Patch(ctx, vm); err != nil {
			if reterr == nil {
				reterr = err
			}
			vmCtx.Logger.Error(err, "patch failed")
		}
	}()

	if !vm.DeletionTimestamp.IsZero() {
		err = r.ReconcileDelete(vmCtx)
		return ctrl.Result{}, err
	}

	if err := r.ReconcileNormal(vmCtx); err != nil {
		vmCtx.Logger.Error(err, "Failed to reconcile VirtualMachine")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueDelay(vmCtx)}, nil
}

// Determine if we should request a non-zero requeue delay in order to trigger a non-rate limited reconcile
// at some point in the future.  Use this delay-based reconcile to trigger a specific reconcile to discovery the VM IP
// address rather than relying on the resync period to do.
//
// TODO: It would be much preferable to determine that a non-error resync is required at the source of the determination that
// TODO: the VM IP isn't available rather than up here in the reconcile loop.  However, in the interest of time, we are making
// TODO: this determination here and will have to refactor at some later date.
func requeueDelay(ctx *context.VirtualMachineContext) time.Duration {
	// If the VM is in Creating phase, the reconciler has run out of threads to Create VMs on the provider. Do not queue
	// immediately to avoid exponential backoff.
	if ctx.VM.Status.Phase == vmopv1alpha1.Creating {
		return 10 * time.Second
	}

	if ctx.VM.Status.VmIp == "" && ctx.VM.Status.PowerState == vmopv1alpha1.VirtualMachinePoweredOn {
		return 10 * time.Second
	}

	return 0
}

func (r *Reconciler) deleteVM(ctx *context.VirtualMachineContext) (err error) {
	defer func() {
		r.Recorder.EmitEvent(ctx.VM, "Delete", err, false)
	}()

	err = r.VMProvider.DeleteVirtualMachine(ctx, ctx.VM)
	if err != nil {
		if apiErrors.IsNotFound(err) {
			ctx.Logger.Info("To be deleted VirtualMachine was not found")
			return nil
		}
		ctx.Logger.Error(err, "Provider failed to delete VirtualMachine")
		return err
	}

	ctx.Logger.V(4).Info("Deleted VirtualMachine")
	return nil
}

func (r *Reconciler) ReconcileDelete(ctx *context.VirtualMachineContext) error {
	vm := ctx.VM

	ctx.Logger.Info("Reconciling VirtualMachine Deletion")
	defer func() {
		ctx.Logger.Info("Finished Reconciling VirtualMachine Deletion")
	}()

	if controllerutil.ContainsFinalizer(vm, finalizerName) {
		vm.Status.Phase = vmopv1alpha1.Deleting

		if err := r.deleteVM(ctx); err != nil {
			return err
		}

		vm.Status.Phase = vmopv1alpha1.Deleted
		controllerutil.RemoveFinalizer(vm, finalizerName)
		ctx.Logger.Info("Provider Completed deleting Virtual Machine",
			"time", time.Now().Format(time.RFC3339))
	}

	// Remove the VM from prober manager if ReconcileDelete succeeds.
	r.Prober.RemoveFromProberManager(vm)

	return nil
}

// ReconcileNormal processes a level trigger for this VM: create if it doesn't exist otherwise update the existing VM.
func (r *Reconciler) ReconcileNormal(ctx *context.VirtualMachineContext) (reterr error) {
	if !controllerutil.ContainsFinalizer(ctx.VM, finalizerName) {
		// The finalizer must be present before proceeding in order to ensure that the VM will
		// be cleaned up. Return immediately after here to let the patcher helper update the
		// object, and then we'll proceed on the next reconciliation.
		controllerutil.AddFinalizer(ctx.VM, finalizerName)
		return nil
	}

	initialVMStatus := ctx.VM.Status.DeepCopy()
	ctx.Logger.Info("Reconciling VirtualMachine")
	// Defer block to handle logging for SLI items
	defer func() {
		// Log the reconcile time using the CR creation time and the time the VM reached the desired state
		if reterr == nil && !apiequality.Semantic.DeepEqual(initialVMStatus, &ctx.VM.Status) {
			ctx.Logger.Info("Finished Reconciling VirtualMachine with updates to the CR",
				"createdTime", ctx.VM.CreationTimestamp, "currentTime", time.Now().Format(time.RFC3339),
				"spec.PowerState", ctx.VM.Spec.PowerState, "status.PowerState", ctx.VM.Status.PowerState)
		} else {
			ctx.Logger.Info("Finished Reconciling VirtualMachine")
		}
		// Log the first time VM was assigned with an IP address successfully
		if initialVMStatus.VmIp != ctx.VM.Status.VmIp {
			ctx.Logger.Info("VM successfully got assigned with an IP address",
				"time", time.Now().Format(time.RFC3339))
		}
	}()

	if err := r.createOrUpdateVM(ctx); err != nil {
		ctx.Logger.Error(err, "Failed to reconcile VirtualMachine")
		return err
	}

	// Add this VM to prober manager if ReconcileNormal succeeds.
	r.Prober.AddToProberManager(ctx.VM)

	return nil
}

func (r *Reconciler) getStoragePolicyID(ctx *context.VirtualMachineContext) (string, error) {
	scName := ctx.VM.Spec.StorageClass
	if scName == "" {
		return "", nil
	}

	sc := &storagev1.StorageClass{}
	if err := r.Get(ctx, client.ObjectKey{Name: scName}, sc); err != nil {
		ctx.Logger.Error(err, "Failed to get StorageClass", "storageClass", scName)
		return "", err
	}

	return sc.Parameters["storagePolicyID"], nil
}

func (r *Reconciler) getContentLibraryProviderFromImage(ctx *context.VirtualMachineContext, image *vmopv1alpha1.VirtualMachineImage) (*vmopv1alpha1.ContentLibraryProvider, error) {
	for _, ownerRef := range image.OwnerReferences {
		if ownerRef.Kind == "ContentLibraryProvider" {
			clProvider := &vmopv1alpha1.ContentLibraryProvider{}
			if err := r.Get(ctx, client.ObjectKey{Name: ownerRef.Name}, clProvider); err != nil {
				ctx.Logger.Error(err, "error retrieving the ContentLibraryProvider from the API server", "clProviderName", ownerRef.Name)
				return nil, err
			}

			return clProvider, nil
		}
	}

	return nil, fmt.Errorf("VirtualMachineImage does not have an OwnerReference to the ContentLibraryProvider. imageName: %v", image.Name)
}

func (r *Reconciler) getContentSourceFromCLProvider(ctx *context.VirtualMachineContext, clProvider *vmopv1alpha1.ContentLibraryProvider) (*vmopv1alpha1.ContentSource, error) {
	for _, ownerRef := range clProvider.OwnerReferences {
		if ownerRef.Kind == "ContentSource" {
			cs := &vmopv1alpha1.ContentSource{}
			if err := r.Get(ctx, client.ObjectKey{Name: ownerRef.Name}, cs); err != nil {
				ctx.Logger.Error(err, "error retrieving the ContentSource from the API server", "contentSource", ownerRef.Name)
				return nil, err
			}

			return cs, nil
		}
	}

	return nil, fmt.Errorf("ContentLibraryProvider does not have an OwnerReference to the ContentSource. clProviderName: %v", clProvider.Name)
}

// getImageAndContentLibraryUUID fetches the VMImage content library UUID from the VM's image.
// This is done by checking the OwnerReference of the VirtualMachineImage resource. As a side effect, with VM service FSS,
// we also check if the VM's namespace has access to the VirtualMachineImage specified in the Spec. This is done by checking
// if a ContentSourceBinding existing in the namespace that points to the ContentSource corresponding to the specified image.
func (r *Reconciler) getImageAndContentLibraryUUID(ctx *context.VirtualMachineContext) (*vmopv1alpha1.VirtualMachineImage, string, error) {
	imageName := ctx.VM.Spec.ImageName

	vmImage := &vmopv1alpha1.VirtualMachineImage{}
	if err := r.Get(ctx, client.ObjectKey{Name: imageName}, vmImage); err != nil {
		msg := fmt.Sprintf("Failed to get VirtualMachineImage %s: %s", ctx.VM.Spec.ImageName, err)
		conditions.MarkFalse(ctx.VM,
			vmopv1alpha1.VirtualMachinePrereqReadyCondition,
			vmopv1alpha1.VirtualMachineImageNotFoundReason,
			vmopv1alpha1.ConditionSeverityError,
			msg)

		ctx.Logger.Error(err, "Failed to get VirtualMachineImage", "imageName", imageName)
		return nil, "", err
	}

	clProvider, err := r.getContentLibraryProviderFromImage(ctx, vmImage)
	if err != nil {
		return nil, "", err
	}

	clUUID := clProvider.Spec.UUID

	// With VM Service, we only allow deploying a VM from an image that a developer's namespace has access to.
	if lib.IsVMServiceFSSEnabled() {
		contentSource, err := r.getContentSourceFromCLProvider(ctx, clProvider)
		if err != nil {
			return nil, "", err
		}

		csBindingList := &vmopv1alpha1.ContentSourceBindingList{}
		if err := r.List(ctx, csBindingList, client.InNamespace(ctx.VM.Namespace)); err != nil {
			msg := fmt.Sprintf("Failed to list ContentSourceBindings in namespace: %s", ctx.VM.Namespace)
			conditions.MarkFalse(ctx.VM,
				vmopv1alpha1.VirtualMachinePrereqReadyCondition,
				vmopv1alpha1.ContentSourceBindingNotFoundReason,
				vmopv1alpha1.ConditionSeverityError,
				msg)
			ctx.Logger.Error(err, msg)
			return nil, "", errors.Wrap(err, msg)
		}

		// Filter the bindings for the specified VM Image.
		matchingContentSourceBinding := false
		for _, csBinding := range csBindingList.Items {
			if csBinding.ContentSourceRef.Kind == "ContentSource" && csBinding.ContentSourceRef.Name == contentSource.Name {
				matchingContentSourceBinding = true
				break
			}
		}

		if !matchingContentSourceBinding {
			msg := fmt.Sprintf("Namespace does not have access to VirtualMachineImage. imageName: %v, contentLibraryUUID: %v, namespace: %v",
				ctx.VM.Spec.ImageName, clUUID, ctx.VM.Namespace)
			conditions.MarkFalse(ctx.VM,
				vmopv1alpha1.VirtualMachinePrereqReadyCondition,
				vmopv1alpha1.ContentSourceBindingNotFoundReason,
				vmopv1alpha1.ConditionSeverityError,
				msg)
			ctx.Logger.Error(nil, msg)
			return nil, "", fmt.Errorf(msg)
		}
	}

	return vmImage, clUUID, nil
}

// getVMClass checks if a VM class specified by a VM spec is valid. When the VMServiceFSSEnabled is enabled,
// a valid VM Class binding for the class in the VM's namespace must exist.
func (r *Reconciler) getVMClass(ctx *context.VirtualMachineContext) (*vmopv1alpha1.VirtualMachineClass, error) {
	className := ctx.VM.Spec.ClassName

	vmClass := &vmopv1alpha1.VirtualMachineClass{}
	if err := r.Get(ctx, client.ObjectKey{Name: className}, vmClass); err != nil {
		msg := fmt.Sprintf("Failed to get VirtualMachineClass %s: %s", ctx.VM.Spec.ClassName, err)
		conditions.MarkFalse(ctx.VM,
			vmopv1alpha1.VirtualMachinePrereqReadyCondition,
			vmopv1alpha1.VirtualMachineClassNotFoundReason,
			vmopv1alpha1.ConditionSeverityError,
			msg)
		ctx.Logger.Error(err, "Failed to get VirtualMachineClass", "className", className)
		return nil, err
	}

	if lib.IsVMServiceFSSEnabled() {
		classBindingList := &vmopv1alpha1.VirtualMachineClassBindingList{}
		if err := r.List(ctx, classBindingList, client.InNamespace(ctx.VM.Namespace)); err != nil {
			msg := fmt.Sprintf("Failed to list VirtualMachineClassBindings in namespace: %s", ctx.VM.Namespace)
			conditions.MarkFalse(ctx.VM,
				vmopv1alpha1.VirtualMachinePrereqReadyCondition,
				vmopv1alpha1.VirtualMachineClassBindingNotFoundReason,
				vmopv1alpha1.ConditionSeverityError,
				msg)

			return nil, errors.Wrap(err, msg)
		}

		// Filter the bindings for the specified VM class.
		matchingClassBinding := false
		for _, classBinding := range classBindingList.Items {
			if classBinding.ClassRef.Kind == "VirtualMachineClass" && classBinding.ClassRef.Name == className {
				matchingClassBinding = true
				break
			}
		}

		if !matchingClassBinding {
			msg := fmt.Sprintf("Namespace does not have access to VirtualMachineClass. className: %v, namespace: %v", ctx.VM.Spec.ClassName, ctx.VM.Namespace)
			conditions.MarkFalse(ctx.VM,
				vmopv1alpha1.VirtualMachinePrereqReadyCondition,
				vmopv1alpha1.VirtualMachineClassBindingNotFoundReason,
				vmopv1alpha1.ConditionSeverityError,
				msg)

			return nil, fmt.Errorf("VirtualMachineClassBinding does not exist for VM Class %s in namespace %s", className, ctx.VM.Namespace)
		}
	}

	return vmClass, nil
}

func (r *Reconciler) getVMMetadata(ctx *context.VirtualMachineContext) (vmprovider.VMMetadata, error) {
	inMetadata := ctx.VM.Spec.VmMetadata
	outMetadata := vmprovider.VMMetadata{}

	if inMetadata == nil {
		return outMetadata, nil
	}

	// VmMetadata's ConfigMapName and SecretName are mutually exclusive.
	// Webhooks currently enforce this during create/update
	// Regardless check if both are set here and return err
	if inMetadata.ConfigMapName != "" && inMetadata.SecretName != "" {
		return outMetadata, fmt.Errorf("failed to get VM metadata. Both configMapName and secretName are specified")
	}

	if inMetadata.ConfigMapName != "" {
		vmMetadataConfigMap := &corev1.ConfigMap{}
		err := r.Get(ctx, client.ObjectKey{Name: inMetadata.ConfigMapName, Namespace: ctx.VM.Namespace}, vmMetadataConfigMap)
		if err != nil {
			return outMetadata, err
		}
		outMetadata.Data = vmMetadataConfigMap.Data
	}

	if inMetadata.SecretName != "" {
		vmMetadataSecret := &corev1.Secret{}
		err := r.Get(ctx, client.ObjectKey{Name: inMetadata.SecretName, Namespace: ctx.VM.Namespace}, vmMetadataSecret)
		if err != nil {
			return outMetadata, err
		}

		outMetadata.Data = make(map[string]string)
		for k, v := range vmMetadataSecret.Data {
			outMetadata.Data[k] = string(v)
		}
	}

	outMetadata.Transport = inMetadata.Transport
	return outMetadata, nil
}

func (r *Reconciler) getResourcePolicy(ctx *context.VirtualMachineContext) (*vmopv1alpha1.VirtualMachineSetResourcePolicy, error) {
	rpName := ctx.VM.Spec.ResourcePolicyName
	if rpName == "" {
		return nil, nil
	}

	resourcePolicy := &vmopv1alpha1.VirtualMachineSetResourcePolicy{}
	err := r.Get(ctx, client.ObjectKey{Name: rpName, Namespace: ctx.VM.Namespace}, resourcePolicy)
	if err != nil {
		ctx.Logger.Error(err, "Failed to get VirtualMachineSetResourcePolicy", "resourcePolicyName", rpName)
		return nil, err
	}

	// Make sure that the corresponding entities (RP and Folder) are created on the infra provider before
	// reconciling the VM. Requeue if the ResourcePool and Folders are not yet created for this ResourcePolicy.
	rpReady, err := r.VMProvider.IsVirtualMachineSetResourcePolicyReady(ctx, ctx.VM.Labels[topology.KubernetesTopologyZoneLabelKey], resourcePolicy)
	if err != nil {
		ctx.Logger.Error(err, "Failed to check if VirtualMachineSetResourcePolicy exists")
		return nil, err
	}
	if !rpReady {
		return nil, fmt.Errorf("VirtualMachineSetResourcePolicy is not yet ready")
	}

	return resourcePolicy, nil
}

func (r *Reconciler) findInstanceStorageVMPlacementStatus(vmCtx *context.VirtualMachineContext) (ready bool) {
	if !instancestorage.IsConfigured(vmCtx.VM) {
		return true
	}

	// TODO:
	// 1. Set the selected-node (if not set already) annotation for the volume controller to place PVCs on that node.

	// Check if all PVCs are realized, if not, inform reconcile handler to wait till the state is ready.
	if _, exists := vmCtx.VM.Annotations[constants.InstanceStoragePVCsBoundAnnotationKey]; !exists {
		vmCtx.Logger.V(5).WithValues(
			"reason", "Instance storage PVCs are not realized yet",
		).Info("Returning with not ready")
		return false
	}

	// Placement successful
	return true
}

// createOrUpdateVM calls into the VM provider to reconcile a VirtualMachine.
func (r *Reconciler) createOrUpdateVM(ctx *context.VirtualMachineContext) error {
	vmClass, err := r.getVMClass(ctx)
	if err != nil {
		return err
	}

	// Update VM spec with instance storage
	if err := r.reconcileInstanceStorageSpec(ctx, vmClass); err != nil {
		return err
	}

	vmImage, clUUID, err := r.getImageAndContentLibraryUUID(ctx)
	if err != nil {
		return err
	}

	vmMetadata, err := r.getVMMetadata(ctx)
	if err != nil {
		return err
	}

	resourcePolicy, err := r.getResourcePolicy(ctx)
	if err != nil {
		return err
	}

	storagePolicyID, err := r.getStoragePolicyID(ctx)
	if err != nil {
		return err
	}

	// Update VirtualMachine conditions to indicate all prereqs have been met.
	conditions.MarkTrue(ctx.VM, vmopv1alpha1.VirtualMachinePrereqReadyCondition)

	vm := ctx.VM
	vmConfigArgs := vmprovider.VMConfigArgs{
		VMClass:            *vmClass,
		VMImage:            vmImage,
		VMMetadata:         vmMetadata,
		ResourcePolicy:     resourcePolicy,
		StorageProfileID:   storagePolicyID,
		ContentLibraryUUID: clUUID,
	}

	exists, err := r.VMProvider.DoesVirtualMachineExist(ctx, vm)
	if err != nil {
		ctx.Logger.Error(err, "Failed to check if VirtualMachine exists from provider")
		return err
	}

	if !exists {
		// Set the phase to Creating first so we do not queue the reconcile immediately if we do not have threads available.
		vm.Status.Phase = vmopv1alpha1.Creating

		// Return and requeue the reconcile request so the provider has reconciler threads available to update the Status of
		// existing VirtualMachines.
		// Ignore overflow since we never expect this to go beyond 32 bits.
		r.mutex.Lock()

		if r.NumVMsBeingCreatedOnProvider >= r.MaxConcurrentCreateVMsOnProvider {
			ctx.Logger.Info("Not enough workers to update VirtualMachine status. Re-queueing the reconcile request")
			// Return nil here so we don't requeue immediately and cause an exponential backoff.
			r.mutex.Unlock()
			return nil
		}

		r.NumVMsBeingCreatedOnProvider++
		r.mutex.Unlock()

		defer func() {
			r.mutex.Lock()
			r.NumVMsBeingCreatedOnProvider--
			r.mutex.Unlock()
		}()

		// Check if the specified resource policy is in deleting state.
		if resourcePolicy != nil && !resourcePolicy.DeletionTimestamp.IsZero() {
			err = fmt.Errorf("cannot create VirtualMachine with its resource policy in DELETING state")
			ctx.Logger.Error(err, "resourcePolicyName", resourcePolicy.Name)
			r.Recorder.EmitEvent(vm, "Create", err, false)
			return err
		}

		err = r.VMProvider.CreateVirtualMachine(ctx, vm, vmConfigArgs)
		if err != nil {
			ctx.Logger.Error(err, "Provider failed to create VirtualMachine")
			r.Recorder.EmitEvent(vm, "Create", err, false)
			return err
		}
	}

	if lib.IsInstanceStorageFSSEnabled() {
		if !r.findInstanceStorageVMPlacementStatus(ctx) {
			return nil
		}
	}

	vm.Status.Phase = vmopv1alpha1.Created

	err = r.VMProvider.UpdateVirtualMachine(ctx, vm, vmConfigArgs)
	if err != nil {
		ctx.Logger.Error(err, "Provider failed to update VirtualMachine")
		r.Recorder.EmitEvent(vm, "Update", err, false)
		return err
	}

	return nil
}

// reconcileInstanceStorageSpec checks if VM class is configured with instance volumes and adds instance storage data in VM spec accordingly.
func (r *Reconciler) reconcileInstanceStorageSpec(
	ctx *context.VirtualMachineContext,
	vmClass *vmopv1alpha1.VirtualMachineClass) error {
	if !lib.IsInstanceStorageFSSEnabled() {
		return nil
	}

	if ctx.VM.Status.Phase == vmopv1alpha1.Created {
		ctx.Logger.V(5).WithValues(
			"reason",
			"VM created",
		).Info("Skipping instance volume patch")
		return nil
	}

	if instancestorage.IsConfigured(ctx.VM) {
		ctx.Logger.V(5).WithValues(
			"reason",
			"VM spec already updated",
		).Info("Skipping instance volume patch")
		return nil
	}

	instanceStorage := vmClass.Spec.Hardware.InstanceStorage

	if len(instanceStorage.Volumes) == 0 {
		ctx.Logger.V(5).WithValues(
			"reason", "VMClass is not configured with instance storage",
			"VMClass", vmClass.Name,
		).Info("Skipping instance volume patch")
		return nil
	}

	return r.addInstanceStorageSpec(ctx, instanceStorage)
}

// addInstanceStorageSpec modifies VM spec with instance volume data.
func (r *Reconciler) addInstanceStorageSpec(
	ctx *context.VirtualMachineContext,
	instanceStorage vmopv1alpha1.InstanceStorage) error {
	pvcs := []vmopv1alpha1.VirtualMachineVolume{}

	for _, isv := range instanceStorage.Volumes {
		uuid, err := uuid.NewUUID()
		if err != nil {
			return err
		}
		pvcName := constants.InstanceStoragePVCNamePrefix + uuid.String()
		vmv := vmopv1alpha1.VirtualMachineVolume{
			Name: pvcName,
			PersistentVolumeClaim: &vmopv1alpha1.PersistentVolumeClaimVolumeSource{
				PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
					ReadOnly:  false,
				},
				InstanceVolumeClaim: &vmopv1alpha1.InstanceVolumeClaimVolumeSource{
					StorageClass: instanceStorage.StorageClass,
					Size:         isv.Size,
				},
			},
		}
		pvcs = append(pvcs, vmv)
	}

	vm := ctx.VM
	// Append PVCs to existing virtual machine volume spec
	vm.Spec.Volumes = append(vm.Spec.Volumes, pvcs...)

	return nil
}
