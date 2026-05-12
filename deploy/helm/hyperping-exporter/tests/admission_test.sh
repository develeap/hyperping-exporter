#!/usr/bin/env bash
# kind PSS-restricted admission driver. Per Contract C1.4 the dispatch
# table below decides per-fixture whether to live-boot (kubectl apply +
# rollout status) or skip from the live path (CRD-bearing fixtures are
# kubeconform-validated separately).
#
# Usage:
#   bash admission_test.sh [<fixture-basename>]...
#   (no args -> run every fixture in the dispatch table)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "$HERE/.." && pwd)"
# shellcheck disable=SC1091
source "$HERE/admission_env.sh"

# Resolve absolute path used by the kind extraMounts.hostPath rewrite.
ADMISSION_CONFIG_DIR="$HERE/kind-pss-config"
TMP_KIND_CONFIG="$(mktemp)"
trap 'rm -f "$TMP_KIND_CONFIG"' EXIT

# Dispatch table (Contract C1.4). Two columns: fixture name, mode.
#   live_boot     - kubectl apply + rollout status (proves PSS admit + readonly-rootfs).
#   kubeconform   - skipped from this live driver; validated by `make helm-kubeconform`.
dispatch_mode() {
  case "$1" in
    pss-restricted)         echo "live_boot" ;;
    networkpolicy-default)  echo "live_boot" ;;
    external-secret)        echo "kubeconform" ;;
    networkpolicy-cilium)   echo "kubeconform" ;;
    *) echo "unknown" ;;
  esac
}

# Read kindest pin captured by tests/scripts/resolve_pins.sh.
KINDEST_VERSION_FILE="$HERE/artifacts/kindest-version.txt"
KINDEST_DIGEST_FILE="$HERE/artifacts/kindest-digest.txt"
if [ ! -r "$KINDEST_VERSION_FILE" ] || [ ! -r "$KINDEST_DIGEST_FILE" ]; then
  echo "FATAL: kindest version/digest artifacts missing. Run tests/scripts/resolve_pins.sh first." >&2
  exit 2
fi
KINDEST_VERSION="$(tr -d '[:space:]' < "$KINDEST_VERSION_FILE")"
KINDEST_DIGEST="$(tr -d '[:space:]' < "$KINDEST_DIGEST_FILE")"

# Rewrite kind config placeholders on a temp copy; the committed file
# keeps the placeholders so the workflow audit can grep for surviving __X__.
sed -e "s|__KINDEST_VERSION__|${KINDEST_VERSION}|g" \
    -e "s|__KINDEST_DIGEST__|${KINDEST_DIGEST}|g" \
    -e "s|__ABSPATH__|${ADMISSION_CONFIG_DIR}|g" \
    "$HERE/kind-pss.yaml" > "$TMP_KIND_CONFIG"

# Verify no placeholders survived.
if grep -E '__[A-Z_]+__' "$TMP_KIND_CONFIG" >/dev/null 2>&1; then
  echo "FATAL: kind-pss.yaml placeholder substitution incomplete" >&2
  cat "$TMP_KIND_CONFIG" >&2
  exit 2
fi

create_cluster_if_absent() {
  if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER_NAME"; then
    echo "kind cluster $KIND_CLUSTER_NAME already exists; reusing"
    return
  fi
  echo "Creating kind cluster $KIND_CLUSTER_NAME ..."
  kind create cluster --name "$KIND_CLUSTER_NAME" --config "$TMP_KIND_CONFIG" --wait 120s
}

ensure_namespace() {
  if ! kubectl get ns "$TEST_NAMESPACE" >/dev/null 2>&1; then
    kubectl create namespace "$TEST_NAMESPACE"
  fi
  kubectl label --overwrite ns "$TEST_NAMESPACE" \
    pod-security.kubernetes.io/enforce=restricted \
    pod-security.kubernetes.io/audit=restricted \
    pod-security.kubernetes.io/warn=restricted
}

cleanup_fixture() {
  kubectl delete --ignore-not-found -n "$TEST_NAMESPACE" \
    deploy,statefulset,svc,sa,role,rolebinding,cm,secret,networkpolicy,pdb \
    -l "app.kubernetes.io/instance=$TEST_RELEASE_NAME" || true
  kubectl wait --for=delete --timeout=60s -n "$TEST_NAMESPACE" \
    deploy,statefulset,pod,svc,sa,role,rolebinding,cm,secret,networkpolicy,pdb \
    -l "app.kubernetes.io/instance=$TEST_RELEASE_NAME" 2>/dev/null || true
}

run_live_boot() {
  local fixture="$1"
  echo "=== live-boot: $fixture ==="
  cleanup_fixture
  helm template "$TEST_RELEASE_NAME" "$CHART_DIR" \
       -f "$CHART_DIR/tests/fixtures/${fixture}.values.yaml" \
       --namespace "$TEST_NAMESPACE" \
       --set "image.tag=${LIVE_BOOT_IMAGE##*:}" \
       --set "image.repository=${LIVE_BOOT_IMAGE%:*}" \
    | kubectl apply -n "$TEST_NAMESPACE" -f -
  local deploy
  deploy="$(kubectl get deploy -n "$TEST_NAMESPACE" -l "app.kubernetes.io/instance=$TEST_RELEASE_NAME" -o name | head -n1)"
  if [ -z "$deploy" ]; then
    echo "FATAL: no Deployment matched after apply for $fixture" >&2
    exit 1
  fi
  kubectl rollout status -n "$TEST_NAMESPACE" "$deploy" --timeout=120s
  echo "PASS live-boot: $fixture"
}

FIXTURES=("$@")
if [ ${#FIXTURES[@]} -eq 0 ]; then
  FIXTURES=(pss-restricted networkpolicy-default external-secret networkpolicy-cilium)
fi

create_cluster_if_absent
ensure_namespace

for f in "${FIXTURES[@]}"; do
  mode="$(dispatch_mode "$f")"
  case "$mode" in
    live_boot)   run_live_boot "$f" ;;
    kubeconform) echo "SKIP live: $f (CRD-bearing; validated by make helm-kubeconform)" ;;
    unknown)     echo "FATAL: unknown fixture $f" >&2; exit 2 ;;
  esac
done

echo
echo "All admission_test fixtures complete."
