#!/usr/bin/env bash

set -o errexit
set -o pipefail
set -o nounset

go test -v ./cmd/... ./pkg/...
