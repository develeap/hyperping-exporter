#!/usr/bin/env bash
# SSOT for admission-test cluster / namespace / release / image names
# (Contract C5.1). Every consumer (admission_test.sh, make helm-pss*
# targets, .github/workflows/helm-ci.yml) sources this file and never
# spells these values literally elsewhere.
export KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-hyperping-pss}"
export TEST_NAMESPACE="${TEST_NAMESPACE:-hyperping-pss-test}"
export TEST_RELEASE_NAME="${TEST_RELEASE_NAME:-testrel}"
export LIVE_BOOT_IMAGE="${LIVE_BOOT_IMAGE:-khaledsalhabdeveleap/hyperping-exporter:1.4.1}"
