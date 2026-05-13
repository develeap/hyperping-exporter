#!/usr/bin/env bash
# audit_anchors.sh — Contract C5 anchor audit.
#
# Greps a closed set of source files for any literal occurrence of the
# admission-test cluster / namespace / release / image anchors. Each
# anchor MUST be sourced from `tests/admission_env.sh` (Contract C5.1)
# or `tests/pins.expected.yaml`, never spelled inline. Lives in its own
# script (separate from the workflow YAML) so the audit's own pattern
# text cannot self-match in the workflow file (R2). The workflow's
# Audit pins step invokes this script.
#
# Anchor list (kept in lockstep with tests/admission_env.sh):
#   - hyperping-pss       (KIND_CLUSTER_NAME)
#   - hyperping-pss-test  (TEST_NAMESPACE)
#   - khaledsalhabdeveleap/hyperping-exporter:1.4.1  (LIVE_BOOT_IMAGE)
#
# Permitted occurrences:
#   - tests/admission_env.sh   (sole declarer)
#   - tests/pins.expected.yaml (image repo + tag are split there)
#   - this script (audit_anchors.sh) by construction.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$HERE/../../../../.." && pwd)"

# Files audited against the anchor list. The workflow's Audit pins step
# greps these paths (and only these) for literal anchor strings.
TARGETS=(
  ".github/workflows/helm-ci.yml"
  "Makefile"
  "deploy/helm/hyperping-exporter/tests/kind-pss.yaml"
)

# Patterns are kept here (out of the workflow YAML) so the audit step
# can never grep its own pattern text.
PATTERNS=(
  # Match the cluster name as a whole word, but the test namespace name
  # (which contains the cluster name) is also matched separately. Use
  # word boundaries to avoid false matches inside paths or comments.
  'hyperping-pss-test'
  'hyperping-pss[^-]'
  'khaledsalhabdeveleap/hyperping-exporter:1\.4\.1'
)

fail=0
for tgt in "${TARGETS[@]}"; do
  path="$REPO_ROOT/$tgt"
  if [ ! -f "$path" ]; then
    echo "WARNING: audit target missing: $tgt (skipping)" >&2
    continue
  fi
  for pat in "${PATTERNS[@]}"; do
    if grep -nE "$pat" "$path"; then
      echo "FATAL: anchor literal /$pat/ embedded in $tgt; source from admission_env.sh / pins.expected.yaml" >&2
      fail=1
    fi
  done
done

if [ "$fail" -ne 0 ]; then
  exit 1
fi

echo "OK anchor audit: no literal anchors leaked into workflow / Makefile / kind-pss.yaml"
