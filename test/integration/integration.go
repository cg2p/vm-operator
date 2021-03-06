// Copyright (c) 2019-2020 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"context"
	"flag"
	"fmt"
	stdlog "log"
	"os"
	"path"
	"path/filepath"
	"strconv"

	. "github.com/onsi/gomega"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vapi/library"
	"github.com/vmware/govmomi/vapi/vcenter"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
	"k8s.io/klog/klogr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	vmopv1alpha1 "github.com/vmware-tanzu/vm-operator-api/api/v1alpha1"
	ncpv1alpha1 "github.com/vmware-tanzu/vm-operator/external/ncp/api/v1alpha1"

	topologyv1 "github.com/vmware-tanzu/vm-operator/external/tanzu-topology/api/v1alpha1"

	netopv1alpha1 "github.com/vmware-tanzu/vm-operator/external/net-operator/api/v1alpha1"
	cnsv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-csi-driver/pkg/syncer/cnsoperator/apis/cnsnodevmattachment/v1alpha1"

	"github.com/vmware-tanzu/vm-operator/pkg/lib"
	"github.com/vmware-tanzu/vm-operator/pkg/vmprovider"
	"github.com/vmware-tanzu/vm-operator/pkg/vmprovider/providers/vsphere"
	vmopclient "github.com/vmware-tanzu/vm-operator/pkg/vmprovider/providers/vsphere/client"
	"github.com/vmware-tanzu/vm-operator/pkg/vmprovider/providers/vsphere/config"
	"github.com/vmware-tanzu/vm-operator/pkg/vmprovider/providers/vsphere/contentlibrary"
	"github.com/vmware-tanzu/vm-operator/pkg/vmprovider/providers/vsphere/credentials"
	"github.com/vmware-tanzu/vm-operator/pkg/vmprovider/providers/vsphere/session"
	"github.com/vmware-tanzu/vm-operator/test/builder"
	"github.com/vmware-tanzu/vm-operator/test/testutil"
)

const (
	IntegrationContentLibraryItemName = "test-item"
	DefaultNamespace                  = "default"
	SecretName                        = "wcp-vmop-sa-vc-auth" // nolint:gosec
	ContentSourceName                 = "vmop-test-integration-cl"
)

var (
	ContentSourceID string
	log             = logf.Log.WithName("integration")
	Log             = log
	vmProvider      vmprovider.VirtualMachineProviderInterface
)

func setContentSourceID(id string) {
	ContentSourceID = id
}

func GetContentSourceID() string {
	return ContentSourceID
}

func NewIntegrationVMOperatorConfig(vcAddress string, vcPort int) *config.VSphereVMProviderConfig {
	var dcMoID, rpMoID, folderMoID string
	for _, dc := range simulator.Map.All("Datacenter") {
		if dc.Entity().Name == "DC0" {
			dcMoID = dc.Reference().Value
			break
		}
	}
	for _, cl := range simulator.Map.All("ClusterComputeResource") {
		if cl.Entity().Name == "DC0_C0" {
			rpMoID = cl.(*simulator.ClusterComputeResource).ResourcePool.Reference().Value
			break
		}
	}
	for _, folder := range simulator.Map.All("Folder") {
		if folder.Entity().Name == "vm" {
			folderMoID = folder.Reference().Value
			break
		}
	}

	return &config.VSphereVMProviderConfig{
		VcPNID:                      vcAddress,
		VcPort:                      strconv.Itoa(vcPort),
		VcCreds:                     NewIntegrationVMOperatorCredentials(),
		Datacenter:                  dcMoID,
		ResourcePool:                rpMoID,
		Datastore:                   "/DC0/datastore/LocalDS_0",
		Folder:                      folderMoID,
		UseInventoryAsContentSource: true,
		InsecureSkipTLSVerify:       true,
	}
}

func NewIntegrationVMOperatorCredentials() *credentials.VSphereVMProviderCredentials {
	// User and password can be anything for vcSim
	return &credentials.VSphereVMProviderCredentials{
		Username: "Administrator@vsphere.local",
		Password: "Admin!23",
	}
}

func enableDebugLogging() {
	strVal, ok := os.LookupEnv("ENABLE_DEBUG_MODE")
	if ok {
		stdlog.Println("Debug logging is enabled")
		klog.InitFlags(nil)
		dbgEnabled, err := strconv.ParseBool(strVal)
		if err != nil {
			stdlog.Fatalf("Failed to print ENABLE_DEBUG_MODE env variable '%s': %v", strVal, err)
		}
		if dbgEnabled {
			if err := flag.Set("alsologtostderr", "true"); err != nil {
				stdlog.Fatalf("failed to set klog logtostderr flag: %v", err)
			}
			if err := flag.Set("v", "4"); err != nil {
				stdlog.Fatalf("failed to set klog level flag: %v", err)
			}
			flag.Parse()
			logf.Log.Fulfill(klogr.New())
			return
		}
	}
	stdlog.Println("Debug logging is disabled")
	flag.Parse()
}

// GetCtrlRuntimeClient gets a vm-operator-api client
func GetCtrlRuntimeClient(config *rest.Config) (client.Client, error) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = vmopv1alpha1.AddToScheme(s)
	_ = ncpv1alpha1.AddToScheme(s)
	_ = netopv1alpha1.AddToScheme(s)
	_ = topologyv1.AddToScheme(s)
	_ = cnsv1alpha1.SchemeBuilder.AddToScheme(s)
	return client.New(config, client.Options{Scheme: s})
}

func SetupIntegrationEnv(namespaces []string) (*envtest.Environment, *config.VSphereVMProviderConfig, client.Client, *VcSimInstance, *vmopclient.Client, vmprovider.VirtualMachineProviderInterface) {
	Expect(namespaces).ToNot(BeEmpty())

	enableDebugLogging()
	rootDir, err := testutil.GetRootDir()
	Expect(err).ToNot(HaveOccurred())

	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join(rootDir, "config", "crd", "bases"),
			filepath.Join(rootDir, "config", "crd", "external-crds"),
		},
	}

	cfg, err := testEnv.Start()
	Expect(err).NotTo(HaveOccurred())

	stdlog.Print("setting up the integration test env...")

	k8sClient, err := GetCtrlRuntimeClient(cfg)
	Expect(err).NotTo(HaveOccurred())

	// Set up fake event recorder
	recorder, _ := builder.NewFakeRecorder()

	// Register the vSphere provider
	log.Info("setting up vSphere Provider")
	vmProvider = vsphere.NewVSphereVMProviderFromClient(k8sClient, recorder)

	vcSim := NewVcSimInstance()

	address, port := vcSim.Start()
	vSphereConfig := NewIntegrationVMOperatorConfig(address, port)
	Expect(vSphereConfig).ToNot(BeNil())

	vmopClient, err := SetupVcSimEnv(vSphereConfig, k8sClient)
	Expect(err).NotTo(HaveOccurred())

	err = os.Setenv(contentlibrary.EnvContentLibAPIWaitSecs, "1")
	Expect(err).NotTo(HaveOccurred())

	// Create a default AZ with the namespaces in it.
	// NOTE: Even though for these tests the FSS is (generally) off, GetAvailabilityZones() will
	// return any AZs if they exist regardless of the FSS value.
	az := &topologyv1.AvailabilityZone{
		ObjectMeta: metav1.ObjectMeta{
			Name: "availabilityzone",
		},
		Spec: topologyv1.AvailabilityZoneSpec{
			ClusterComputeResourceMoId: simulator.Map.All("ClusterComputeResource")[0].Reference().Value,
			Namespaces:                 map[string]topologyv1.NamespaceInfo{},
		},
	}
	for _, ns := range namespaces {
		az.Spec.Namespaces[ns] = topologyv1.NamespaceInfo{
			PoolMoId:   vSphereConfig.ResourcePool,
			FolderMoId: vSphereConfig.Folder,
		}
	}
	Expect(k8sClient.Create(context.Background(), az)).To(Succeed())

	return testEnv, vSphereConfig, k8sClient, vcSim, vmopClient, vmProvider
}

func TeardownIntegrationEnv(testEnv *envtest.Environment, vcSim *VcSimInstance) {
	TeardownVcSimEnv(vcSim)

	if testEnv != nil {
		stdlog.Print("stopping the test environment...")
		err := testEnv.Stop()
		Expect(err).NotTo(HaveOccurred())
	}
}

func SetupVcSimEnv(
	vSphereConfig *config.VSphereVMProviderConfig,
	client client.Client) (*vmopclient.Client, error) {

	// Support for bootstrapping VM operator resource requirements in Kubernetes.
	// Generate a fake vsphere provider config that is suitable for the integration test environment.
	// Post the resultant config map to the API Master for consumption by the VM operator
	log.Info("Installing a bootstrap config map for use in integration tests.")

	// Configure the environment with the location of the vmop config.
	err := lib.SetVMOpNamespaceEnv(DefaultNamespace)
	if err != nil {
		return nil, fmt.Errorf("failed to install vm operator config: %v", err)
	}

	// Support for bootstrapping VM operator resource requirements in Kubernetes.
	// Generate a fake vsphere provider config that is suitable for the integration test environment.
	// Post the resultant config map to the API Master for consumption by the VM operator
	klog.Infof("Installing a bootstrap config map for use in integration tests.")
	err = config.InstallVSphereVMProviderConfig(client, DefaultNamespace, vSphereConfig, SecretName)
	if err != nil {
		return nil, fmt.Errorf("failed to install vm operator config: %v", err)
	}

	// Setup content library once.  The first namespace is sufficient to use
	vmopClient, err := vmProvider.(vsphere.VSphereVMProviderGetSessionHack).GetClient(context.TODO())
	if err != nil {
		return nil, fmt.Errorf("failed to get vm provider client: %v", err)
	}

	if err := SetupContentLibrary(client, vmopClient); err != nil {
		return nil, fmt.Errorf("failed to setup the VC Simulator: %v", err)
	}

	return vmopClient, nil
}

func TeardownVcSimEnv(vcSim *VcSimInstance) {
	if vcSim != nil {
		vcSim.Stop()
	}
}

func CreateLibraryItem(ctx context.Context, vmopClient *vmopclient.Client, name, kind, libraryID, ovfPath string) error {
	libraryItem := library.Item{
		Name:      name,
		Type:      kind,
		LibraryID: libraryID,
	}
	return vmopClient.ContentLibClient().CreateLibraryItem(ctx, libraryItem, ovfPath)
}

// SetupContentLibrary creates ContentSource and ContentLibraryProvider resources for the vSphere content library.
func SetupContentLibrary(client client.Client, vmopClient *vmopclient.Client) error {
	stdlog.Printf("Setting up ContentLibraryPrvider and ContentSource for integration tests")
	ctx := context.Background()

	var datastoreID string
	for _, dc := range simulator.Map.All("Datastore") {
		if dc.Entity().Name == "LocalDS_0" {
			datastoreID = dc.Reference().Value
			break
		}
	}

	libID, err := vmopClient.ContentLibClient().CreateLibrary(ctx, ContentSourceName, datastoreID)
	if err != nil {
		return err
	}

	if err := CreateLibraryItem(
		ctx,
		vmopClient,
		IntegrationContentLibraryItemName,
		"ovf",
		libID,
		path.Join(
			testutil.GetRootDirOrDie(),
			"images",
			"ttylinux-pc_i486-16.1.ovf",
		)); err != nil {

		return err
	}

	// Assign ContentSourceID to be used for integration tests
	setContentSourceID(libID)

	clProvider := &vmopv1alpha1.ContentLibraryProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: libID,
		},
		Spec: vmopv1alpha1.ContentLibraryProviderSpec{
			UUID: libID,
		},
	}

	cs := &vmopv1alpha1.ContentSource{
		ObjectMeta: metav1.ObjectMeta{
			Name: libID,
		},
		Spec: vmopv1alpha1.ContentSourceSpec{
			ProviderRef: vmopv1alpha1.ContentProviderReference{
				Name: clProvider.ObjectMeta.Name,
				Kind: "ContentLibraryProvider",
			},
		},
	}

	// Create ContentSource and ContentLibraryProvider resources for the content library.
	if err := client.Create(ctx, clProvider); err != nil {
		return err
	}

	return client.Create(ctx, cs)
}

func CloneVirtualMachineToLibraryItem(ctx context.Context, cfg *config.VSphereVMProviderConfig, s *session.Session, src, name string) error {
	vm, err := s.Finder.VirtualMachine(ctx, src)
	if err != nil {
		return err
	}

	pool, err := vm.ResourcePool(ctx)
	if err != nil {
		return err
	}

	restClient := s.Client.RestClient()

	spec := vcenter.Template{
		Name:     name,
		Library:  GetContentSourceID(),
		SourceVM: vm.Reference().Value,
		Placement: &vcenter.Placement{
			Folder:       cfg.Folder,
			ResourcePool: pool.Reference().Value,
		},
	}

	id, err := vcenter.NewManager(restClient).CreateTemplate(ctx, spec)
	if err != nil {
		return err
	}
	stdlog.Printf("Created vmtx %s in library %s", id, GetContentSourceID())

	return nil
}
