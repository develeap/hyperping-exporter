#!/usr/bin/env bash
# Documentation smoke tests for the multi-project rollout
# (work item: exporter-docs-changelog).
#
# These checks are deliberately coarse: they only assert the load-bearing
# strings exist in the operator-facing docs. The intent is to catch a doc
# regression that drops the project-label migration notes or the
# --projects-file flag row, NOT to validate prose quality.
#
# Run from the repo root:
#   bash tests/docs_smoke.sh
#
# Exits 0 on success, 1 on any missing marker.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
readme="${repo_root}/README.md"
changelog="${repo_root}/CHANGELOG.md"

fail=0

check() {
    local file="$1" pattern="$2" label="$3"
    if grep -qE "$pattern" "$file"; then
        echo "PASS ${label}"
    else
        echo "FAIL ${label}: pattern ${pattern@Q} not found in ${file}"
        fail=1
    fi
}

# README must document the new --projects-file flag in the configuration
# table and at least one prose paragraph explaining the project label.
check "$readme" 'projects-file' "README mentions --projects-file flag"
check "$readme" '[Mm]ulti-project' "README mentions multi-project deployment"

# CHANGELOG must have an [Unreleased] section flagging the new label as
# a BREAKING change for downstream dashboards / alerts.
check "$changelog" '\[Unreleased\]' "CHANGELOG carries an [Unreleased] section"
check "$changelog" 'project label' "CHANGELOG mentions the new project label"
check "$changelog" 'BREAKING' "CHANGELOG flags the dashboard/alert migration as BREAKING"
check "$changelog" 'projects-file' "CHANGELOG documents the --projects-file flag"

if [[ "$fail" -ne 0 ]]; then
    echo
    echo "DOCS SMOKE FAILED" >&2
    exit 1
fi
echo
echo "ALL DOCS SMOKE PASSED"
