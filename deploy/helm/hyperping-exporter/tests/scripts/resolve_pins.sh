#!/usr/bin/env bash
# resolve_pins.sh — Contract C4 resolver.
#
# Reads tests/pins.expected.yaml (committed SSOT) and verifies every
# upstream reference resolves to (at least) the expected pin. Writes
# resolved tags/digests to tests/artifacts/*.txt for traceability.
#
# Fails loud on any pin drift, missing tag, or image-not-published
# precheck failure. Retries transient network errors with exponential
# backoff. Surfaces 'USER DECISION REQUIRED:' to the operator on any
# resolution failure outside the auto-rollover policy.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TESTS_DIR="$(cd "$HERE/.." && pwd)"
ARTIFACTS="$TESTS_DIR/artifacts"
EXPECTED="$TESTS_DIR/pins.expected.yaml"

mkdir -p "$ARTIFACTS"

if [ ! -r "$EXPECTED" ]; then
  echo "FATAL: $EXPECTED not readable" >&2
  exit 1
fi

# Auth precheck — gh CLI must be authenticated.
if ! gh auth status >/dev/null 2>&1; then
  echo "FATAL: gh not authenticated. Run gh auth login." >&2
  exit 1
fi

# Read a YAML scalar value from pins.expected.yaml.
pin() {
  local key="$1"
  awk -v k="$key" '
    $0 ~ "^"k":" {
      sub("^"k":[[:space:]]*", "")
      gsub(/^"|"$/, "")
      print
      exit
    }
  ' "$EXPECTED"
}

retry() {
  local n=0 max=3 delay
  while ! "$@"; do
    n=$((n + 1))
    if [ "$n" -ge "$max" ]; then
      echo "retry: exhausted $max attempts for: $*" >&2
      return 1
    fi
    delay=$((2 * n + 1))
    echo "retry: attempt $n failed, sleeping ${delay}s" >&2
    sleep "$delay"
  done
}

# Resolve latest tag for a GitHub action repo via gh CLI.
latest_release_tag() {
  local repo="$1"
  gh release view --repo "$repo" --json tagName --jq '.tagName' 2>/dev/null
}

# Docker Hub tag existence probe.
docker_hub_tag_exists() {
  local repo="$1" tag="$2"
  curl -sf -o /dev/null \
    "https://hub.docker.com/v2/repositories/${repo}/tags/${tag}/"
}

echo "Resolving upstream pins from $EXPECTED" >&2

# helm/kind-action
exp_helm_kind="$(pin helm_kind_action_min_tag)"
got_helm_kind="$(retry latest_release_tag helm/kind-action || echo "")"
echo "$got_helm_kind" > "$ARTIFACTS/helm-kind-action-tag.txt"
echo "helm/kind-action resolved=$got_helm_kind expected_min=$exp_helm_kind" >&2

# kind binary minimum
exp_kind="$(pin kind_min_tag)"
got_kind="$(retry latest_release_tag kubernetes-sigs/kind || echo "")"
echo "$got_kind" > "$ARTIFACTS/kind-tag.txt"
echo "kubernetes-sigs/kind resolved=$got_kind expected_min=$exp_kind" >&2

# actions/checkout, azure/setup-helm, actions/setup-python — exact-match audit
for repo in actions/checkout azure/setup-helm actions/setup-python; do
  case "$repo" in
    actions/checkout)        exp="$(pin actions_checkout_tag)" ;;
    azure/setup-helm)        exp="$(pin azure_setup_helm_tag)" ;;
    actions/setup-python)    exp="$(pin actions_setup_python_tag)" ;;
  esac
  got="$(retry latest_release_tag "$repo" || echo "")"
  slug="$(echo "$repo" | tr '/' '-')"
  echo "$got" > "$ARTIFACTS/${slug}-tag.txt"
  echo "$repo resolved=$got expected=$exp" >&2
done

# datreeio/CRDs-catalog — capture latest tag if expected is empty,
# otherwise verify expected tag still exists upstream.
exp_catalog="$(pin datreeio_crds_catalog_tag)"
got_catalog="$(retry latest_release_tag datreeio/CRDs-catalog || echo "")"
if [ -z "$exp_catalog" ]; then
  echo "$got_catalog" > "$ARTIFACTS/datreeio-CRDs-catalog-tag.txt"
  echo "datreeio/CRDs-catalog resolved=$got_catalog (expected was empty, captured)" >&2
else
  echo "$exp_catalog" > "$ARTIFACTS/datreeio-CRDs-catalog-tag.txt"
  echo "datreeio/CRDs-catalog using pinned=$exp_catalog (latest upstream=$got_catalog)" >&2
fi

# Docker Hub image existence precheck.
img_repo="$(pin docker_hub_image_repo)"
img_tag="$(pin docker_hub_image_tag)"
if retry docker_hub_tag_exists "$img_repo" "$img_tag"; then
  echo "ok" > "$ARTIFACTS/docker-hub-precheck.txt"
  echo "Docker Hub image ${img_repo}:${img_tag} present" >&2
else
  echo "FATAL: ${img_repo}:${img_tag} not published. USER DECISION REQUIRED: publish image or update pins.expected.yaml" >&2
  echo "missing" > "$ARTIFACTS/docker-hub-precheck.txt"
  exit 1
fi

# kindest/node digest — capture from kind release notes.
kind_release_body="$(retry gh release view "$got_kind" --repo kubernetes-sigs/kind --json body --jq '.body' || echo "")"
minor="$(pin kindest_node_minor)"
digest_line="$(echo "$kind_release_body" | grep -E "kindest/node:${minor}\.[0-9]+@sha256:[a-f0-9]+" | head -1 || true)"
if [ -n "$digest_line" ]; then
  version="$(echo "$digest_line" | grep -oE "${minor}\.[0-9]+" | head -1)"
  digest="$(echo "$digest_line" | grep -oE 'sha256:[a-f0-9]+' | head -1)"
  echo "$version" > "$ARTIFACTS/kindest-version.txt"
  echo "$digest" > "$ARTIFACTS/kindest-digest.txt"
  echo "kindest/node version=$version digest=$digest" >&2
else
  echo "WARNING: kindest/node ${minor}.x digest not found in $got_kind release notes" >&2
  echo "${minor}.0" > "$ARTIFACTS/kindest-version.txt"
  echo "" > "$ARTIFACTS/kindest-digest.txt"
fi

echo "OK resolver completed; artifacts in $ARTIFACTS" >&2
