# Chart UX Hardening: Production-Readiness Defaults Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use trycycle-executing to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land all ten chart hardening items as one PR bumping the chart to **1.5.0** (skipping the binary's v1.2.x slot to keep the shared CHANGELOG strictly monotonic-descending and to track the just-released v1.4.1 binary), gated by an extended render harness, kubeconform schema validation, and a kind PSS-restricted admission CI job that exercises four configurations (defaults, externalSecret, replicas-zero, network-policy variants).

**Architecture:** Single-PR cutover on the existing `chore/chart-hardening-prod-defaults` worktree branch. The chart already implements partial versions of items 1, 2, 4, 5, 8; this work reconciles those surfaces against the user's contract plus adds ExternalSecret, mutual-exclusion `fail()` guards (secret-source, multi-replica, AND cacheTTL non-string), the Cilium NP variant (with proper Cilium-shaped `EndpointSelector` ingress), the `--cache-ttl` toJson render with explicit type-safe coercion, a Deployment-env gating path for `replicaCount: 0` scaled-to-zero, and a runtime-readOnlyRootFilesystem proof. Three CI surfaces gate the chart: the existing PyYAML render harness (extended fixture matrix, including all existing fixtures re-asserted with NetworkPolicy in the post-default-on render), `kubeconform` strict offline schema validation including CRDs, and a `kind` job that admits each of four representative fixtures into a PSS-restricted namespace AND boots one of those pods to prove the binary tolerates `readOnlyRootFilesystem: true` at runtime. Every commit on the branch must keep `python3 render_test.py` and `helm lint` green (no bisect-red windows).

**Tech Stack:** Helm v3.20.2 (pinned in CI, with substring-match assertions tolerant of v3.20.x patch drift), PyYAML 6.0.3, `kubeconform` v0.7.0, `kind` v0.30.0, Kubernetes 1.34 (PSS-restricted v1.33+ semantics), `external-secrets.io/v1` (default) with v1beta1 fallback (values-driven), `cilium.io/v2 CiliumNetworkPolicy`. Go binary unchanged.

---

## Scope Check

Single PR per user decision #4. All ten items target one Helm chart, share one test harness, and have interlocking semantics. No natural split survives the mutual-exclusion testing. Single PR is correct.

## Strategy Gate

Five judgment calls baked in (changes from the original plan called out explicitly so reviewers see why):

1. **Chart version: 1.5.0** (not 1.2.0). The CHANGELOG already has a `## [1.2.0] - 2026-04-25` entry for the binary's MCP-metrics release. Chart and binary share one CHANGELOG and one 1.x.y namespace; reusing 1.2.0 produces a duplicate-anchor collision and breaks the strictly descending version order. 1.5.0 is the next monotonic chart slot above the latest binary release v1.4.1.

2. **appVersion: "1.4.1"** (not "1.4.0"). The latest released binary is v1.4.1 (confirmed via `git tag --list 'v*'`). The chart's `appVersion` controls the default `image.tag` and the `app.kubernetes.io/version` label on every resource. Staying at 1.4.0 would default new installs to an older image. Bump to 1.4.1 and update the render harness's `EXPECTED_VERSION` literal.

3. **ExternalSecret apiVersion: `external-secrets.io/v1`** (default) with a values-driven override `externalSecret.apiVersion`. ESO v0.16+ graduated v1 from v1beta1; v1beta1 still works through the conversion webhook for ESO releases ≤ 0.18. Defaulting to v1 tracks the current upstream API; the override lets operators pin v1beta1 for legacy installations. Documented in values.yaml.

4. **Item 7 (multi-replica):** `fail()` guard chosen over leader-election sidecar. The binary has no leader-election support today; adding it would change the binary (forbidden by "WHAT NOT TO DO"). `fail()` is the only path.

5. **Item 5 Cilium variant:** `cilium.io/v2 CiliumNetworkPolicy` (singular, namespaced). Rendered only when `networkPolicy.fqdnRestriction.enabled: true`. Mutually exclusive with the vanilla NetworkPolicy. Be honest at apply time on non-Cilium clusters (`no matches for kind "CiliumNetworkPolicy"`) rather than silently permissive at render time. **Cilium ingress shape is NOT vanilla-NP-shaped:** `CiliumNetworkPolicy.spec.ingress[].fromEndpoints` expects a list of bare `EndpointSelector` objects (each one a `matchLabels`/`matchExpressions` map at the root), NOT vanilla `NetworkPolicyPeer` objects wrapping `podSelector`/`namespaceSelector`. The chart's `networkPolicy.ingressFrom` values default to vanilla `NetworkPolicyPeer` shape (used by the existing `templates/networkpolicy.yaml`). The Cilium template must NOT `toYaml` that value through as-is; it must walk each entry and extract the labels from `podSelector.matchLabels` (and merge in `io.kubernetes.pod.namespace` from `namespaceSelector.matchLabels` when present) into a flat `EndpointSelector`. Cases where the input can't be converted (e.g. selector-less peers or `ipBlock`-only peers) are intentionally dropped with a render-time `fail()` so misconfiguration is loud, not silent. If `networkPolicy.ingressFrom` is empty, the Cilium template's `ingress:` block is omitted entirely (egress-only policy).

`replicaCount: 0` is locked-in as a supported state: render `replicas: 0`, suppress PDB, **exempt from the secret-source `fail()` guard** because zero pods need no API key (operator workflow: scale to zero, then tear down secrets). Multi-replica `fail()` only fires for `replicaCount > 1`. **Deployment env contract:** when `replicaCount: 0` AND zero secret sources are configured, the Deployment's `env` block that references `secretKeyRef: name: <fullname>` must NOT be emitted (it would dangle to a missing Secret if the operator later scales to 1 without first configuring a source). The `env` entry is gated on `secretSourceCount > 0` so scaling 0 → 1 without first setting a source re-trips the `validateSecretSources` guard at the next render rather than silently CrashLoopBackOff-ing.

6. **Item 3 cacheTTL contract (hardened):** the chart treats `config.cacheTTL` as a **required string** representing a Go duration. To avoid both (a) the printf `%s` mis-format on bare integers (`%!s(int=60)`) and (b) the binary's `time.ParseDuration` rejecting unit-less values, the chart renders via `printf "--cache-ttl=%v" (toString .Values.config.cacheTTL) | toJson`. `toString` coerces ints/floats/bools to their string representation; `%v` is the universally-safe verb in Sprig's `printf`. A `validateCacheTTL` helper additionally fails the render with a clear error when `kindOf .Values.config.cacheTTL` is `int`/`float64` (i.e. unquoted in values.yaml), pointing the operator at the `"60s"` form. This is consistent with the multi-replica and secret-source `fail()` policy: footguns are hard-failed at render time, not documented and left for runtime.

7. **Cross-cutting TDD/bisect contract:** every commit on the branch must keep `python3 render_test.py` AND `helm lint` green. This means production-template changes and the test-expectation updates they invalidate land in **the same commit**, never in separate commits. Specifically: (a) Task 6's `templates/externalsecret.yaml` and Task 7's `validateSecretSources`/`validateReplicaCount`/secret.yaml guards land together as Task 6 (renamed to "ExternalSecret template + secret-source guards"), because the legacy two-line `apiKey || existingSecret` guard at the top of `deployment.yaml` would reject the ExternalSecret fixture otherwise. (b) Task 8's `networkPolicy.enabled: false → true` flip lands together with Case 1's `expected_versions` enumeration update (adding `NetworkPolicy` to the kinds), and with whichever of the pre-existing eight fixtures (`existing-secret`, `ascii-regex`, `quote-regex`, `readme-regex`, `single-quote-regex`, `both-flags`, `mcp-url`, `mcp-url-query`) need their assertions updated to tolerate the now-present NetworkPolicy. (c) Task 10's `appVersion: 1.4.0 → 1.4.1` lands together with `EXPECTED_IMAGE_DEFAULT` and `EXPECTED_VERSION` bumps to `1.4.1` in `render_test.py:160-161`, and with the CHANGELOG entry. Task 10 keeps only the **net-new** assertions (defaults extensions, fail-path cases, ExternalSecret cases, NP cases, replicas-zero, PDB, cache-ttl numeric round-trip) that don't invalidate pre-Task-11 state.

Empirical resources validation (item 1) uses the dev API key at `/home/khaledsa/projects/develeap/terraform-provider-hyperping/.env`. Measurement runs inside Docker with `--read-only --user 65534:65534` so the runtime envelope matches the chart's actual deployment (uid 65534, read-only root, no host writable-fs slack). This also doubles as the runtime proof that the binary tolerates `readOnlyRootFilesystem: true`.

PDB design (item 8): user said "default to maxUnavailable: 0", but `maxUnavailable: 0` blocks ALL node drains including kubectl drain for legitimate maintenance. We default to **`maxUnavailable: 1`** (which lets drains proceed on a single-replica deployment, since the chart aborts on `replicaCount > 1` anyway). The PDB template is suppressed when `replicaCount ≤ 1` because a PDB selecting a single replica with `maxUnavailable: 0` would be a footgun; suppression with `maxUnavailable: 1` is the right default. Values doc explains both choices. Net: in the supported single-replica architecture PDB never renders; if a future leader-election change permits multi-replica, the PDB defaults are already drain-safe.

NetworkPolicy egress (item 5): user contract is "TCP/443 to 0.0.0.0/0 plus DNS to kube-dns". The existing template's `ipBlock.except: [10/8, 172.16/12, 192.168/16]` is more restrictive than the user contract; we **replace** the egress rule with the unrestricted form so the chart matches the documented contract. The `cross-namespace scraping` comment block above `ingressFrom` is preserved verbatim via a targeted edit, not a block replacement.

No further direction change is warranted.

## File Structure

### Files created

- `deploy/helm/hyperping-exporter/templates/externalsecret.yaml` — renders `external-secrets.io/v1 ExternalSecret` (or v1beta1 per values override) when `externalSecret.enabled: true`. Mutually exclusive with `templates/secret.yaml`.
- `deploy/helm/hyperping-exporter/templates/networkpolicy-cilium.yaml` — renders `cilium.io/v2 CiliumNetworkPolicy` when `networkPolicy.enabled: true` AND `networkPolicy.fqdnRestriction.enabled: true`. Mutually exclusive with `templates/networkpolicy.yaml`.
- `deploy/helm/hyperping-exporter/tests/fixtures/pss-restricted.values.yaml` — minimal `apiKey` config; used by the PSS admission job AND a render-harness fixture. Asserts the default securityContext block passes restricted enforcement.
- `deploy/helm/hyperping-exporter/tests/fixtures/external-secret.values.yaml` — `externalSecret.enabled: true`, `secretStoreRef`, `remoteRef.key`, explicit `refreshInterval: 30m`.
- `deploy/helm/hyperping-exporter/tests/fixtures/external-secret-defaults.values.yaml` — `externalSecret.enabled: true` with NO `refreshInterval` override (proves the default `1h` propagates).
- `deploy/helm/hyperping-exporter/tests/fixtures/external-secret-missing-store.values.yaml` — `externalSecret.enabled: true` with no `secretStoreRef.name`; asserts the `required` failure message.
- `deploy/helm/hyperping-exporter/tests/fixtures/replicas-zero.values.yaml` — `replicaCount: 0` with NO secret source set (scaled-to-zero state is secret-source-exempt; see Task 6).
- `deploy/helm/hyperping-exporter/tests/fixtures/replicas-multi.values.yaml` — `replicaCount: 2`, `apiKey: x`; asserts the multi-replica `fail()`.
- `deploy/helm/hyperping-exporter/tests/fixtures/pdb-enabled.values.yaml` — `apiKey: x`, `podDisruptionBudget.enabled: true`, default `replicaCount: 1`; asserts PDB is suppressed even when enabled (the user-visible safety property).
- `deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-default.values.yaml` — `apiKey: x` only; default networkPolicy.enabled=true now renders the vanilla NP.
- `deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-cilium.values.yaml` — `apiKey: x`, `networkPolicy.fqdnRestriction.enabled: true`, `allowedHosts: [api.hyperping.io, mcp.hyperping.io]`.
- `deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-apikey-and-existing.values.yaml`.
- `deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-apikey-and-external.values.yaml`.
- `deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-existing-and-external.values.yaml`.
- `deploy/helm/hyperping-exporter/tests/fixtures/secret-source-missing.values.yaml` — explicit `externalSecret.enabled: false` (future-proof against the default flipping).
- `deploy/helm/hyperping-exporter/tests/fixtures/cache-ttl-numeric.values.yaml` — `cacheTTL: 60` (bare int) asserts the chart still emits a string and the binary's duration parser succeeds (caught at runtime in admission job).
- `deploy/helm/hyperping-exporter/tests/kind-pss.yaml` — `kind` cluster config consumed by helm/kind-action AND by the local `make helm-pss` target. Uses absolute paths via `${GITHUB_WORKSPACE}` placeholder rewritten by admission_test.sh.
- `deploy/helm/hyperping-exporter/tests/kind-pss-config/admission-config.yaml` — PSS admission plugin configuration mounted into the kind control-plane node.
- `deploy/helm/hyperping-exporter/tests/admission_test.sh` — bash driver. Takes a fixture argument; iterates over all four if called with no args. Creates the kind cluster only if absent, labels the test namespace `pod-security.kubernetes.io/enforce=restricted`, renders with `--namespace`, applies live (NOT dry-run) for the defaults case so the pod actually starts, dry-runs for variants that need CRDs not present. Verifies pod Ready for the live case (this is the runtime `readOnlyRootFilesystem` proof). Idempotent.
- `docs/plans/2026-05-12-chart-hardening-prod-defaults.md` — this plan (overwritten by this rewrite).

### Files modified

- `deploy/helm/hyperping-exporter/Chart.yaml`:
  - `version: 1.1.0 → 1.5.0`.
  - `appVersion: "1.4.0" → "1.4.1"` (tracks latest binary).
- `deploy/helm/hyperping-exporter/values.yaml` — comprehensive edits:
  - One-line comment next to `image.tag` noting chart version and binary version track separately.
  - Block comment above `securityContext:` explaining the PSS-restricted contract.
  - Comment under `cacheTTL` warning that bare integers like `60` (without unit) are rejected by the binary's Go-duration parser; always supply a unit.
  - Re-tune `resources` from empirical observation (Task 2).
  - Reconcile probes against user's contract (Task 3).
  - Flip `networkPolicy.enabled` default `false → true` and replace the egress rule with the unrestricted form (Task 7).
  - Add `networkPolicy.fqdnRestriction.enabled: false`, `allowedHosts: [api.hyperping.io]` (Task 7).
  - Add `externalSecret:` block with `apiVersion: external-secrets.io/v1`, `enabled: false`, `secretStoreRef`, `remoteRef.key`, `remoteRef.property`, `refreshInterval: "1h"` (Task 6).
  - Document the secret-source mutual exclusion above the secret block.
  - Document the multi-replica `fail()` and scaled-to-zero exemption above `replicaCount`.
- `deploy/helm/hyperping-exporter/templates/_helpers.tpl` — four new helpers (`secretSourceCount`, `validateSecretSources`, `validateReplicaCount`, `validateCacheTTL`). The `secretSourceCount` helper uses the `.Values.externalSecret | default dict` idiom so removing the entire block from a consumer override doesn't NPE. `validateCacheTTL` uses `kindOf` to fail loudly when the operator supplies `cacheTTL: 60` (bare int) instead of `cacheTTL: "60s"`.
- `deploy/helm/hyperping-exporter/templates/deployment.yaml`:
  - Replace the legacy two-line apiKey/existingSecret guard (lines 1-3) with the three validator includes (`validateSecretSources`, `validateReplicaCount`, `validateCacheTTL`). Use trailing `-}}` to suppress stray newlines.
  - Render `--cache-ttl` via `printf "--cache-ttl=%v" (toString .Values.config.cacheTTL) | toJson` (item 3 — `%v` and `toString` are belt-and-braces against any operator-supplied non-string sneaking past `validateCacheTTL`; rendered value still round-trips through YAML cleanly).
  - Gate the `env: HYPERPING_API_KEY` block with `secretSourceCount > 0` so a `replicaCount: 0` Deployment with no configured secret source doesn't dangle a `secretKeyRef` to a non-existent Secret.
- `deploy/helm/hyperping-exporter/templates/secret.yaml` — extend `if` guard to suppress on `externalSecret.enabled`.
- `deploy/helm/hyperping-exporter/templates/pdb.yaml` — wrap in `{{- if and .Values.podDisruptionBudget.enabled (gt (int .Values.replicaCount) 1) }}`. `maxUnavailable` default stays at `1` (drain-safe; rationale in values.yaml).
- `deploy/helm/hyperping-exporter/templates/networkpolicy.yaml`:
  - Wrap in `{{- if and .Values.networkPolicy.enabled (not (and .Values.networkPolicy.fqdnRestriction .Values.networkPolicy.fqdnRestriction.enabled)) }}` (Cilium-suppression).
  - Replace ONLY the egress 443 rule's `ipBlock` block (lines 41-47) so the existing cross-namespace scraping doc comment above `ingressFrom` is preserved. The new egress permits TCP/443 to `0.0.0.0/0` with no exceptions, matching the user's documented contract.
- `deploy/helm/hyperping-exporter/tests/render_test.py`:
  - **In Task 8's commit** (when networkPolicy.enabled flips to true): extend Case 1's `expected_versions` enumeration to also cover NetworkPolicy (now in default render). Audit the other eight pre-existing fixtures (`existing-secret`, `ascii-regex`, `quote-regex`, `readme-regex`, `single-quote-regex`, `both-flags`, `mcp-url`, `mcp-url-query`); none of them assert against the set of rendered kinds (they only assert against `deployment_args` and `assert_scalars_clean`), so the default-on NetworkPolicy is silently tolerated by their existing assertions. Confirm via `helm template ... -f <fixture> | grep '^kind:' | sort -u` in Task 8 Step 9. No fixture-by-fixture edit needed beyond Case 1.
  - **In Task 10's commit** (when appVersion/Chart.yaml bumps): bump `EXPECTED_VERSION` literal `"1.4.0" → "1.4.1"` AND `EXPECTED_IMAGE_DEFAULT` `"khaledsalhabdeveleap/hyperping-exporter:1.4.0" → "khaledsalhabdeveleap/hyperping-exporter:1.4.1"`. These three changes are atomic with the Chart.yaml bump so Case 1's `assert_eq(deployment_image(rendered), EXPECTED_IMAGE_DEFAULT, ...)` is never red.
  - **In Task 10's commit** (assertions-only): add `assert_fail` helper; six new `fail()` cases (three secret-conflicts, secret-source-missing, replicas-multi, cacheTTL-bare-int). Add `EXPECTED_CHART_LABEL = "hyperping-exporter-1.5.0"` constant and the `helm.sh/chart` label assertion (Case 1 extension): the new label flips `1.1.0 → 1.5.0` on every labelled resource. 13+ new positive cases (see Test Plan).
- `.github/workflows/helm-ci.yml` — single job remains named `helm` (preserves branch-protection identifier) but its body is extended with three named steps: `lint-and-render`, `kubeconform`, `pss-admission`. Branch-protection rules require ONLY the job-level check; step-level failures still fail the job. This avoids forcing every consumer to update branch-protection.
- `Makefile` — add `helm-ci`, `helm-render`, `helm-kubeconform`, `helm-pss` targets.
- `CHANGELOG.md` — add `## [1.5.0] - 2026-05-12` chart-section between `## [Unreleased]` and `## [1.4.1]`. Strict keep-a-changelog header (no parenthetical suffix). The "Chart only; binary unchanged" note moves into the Highlights bullet list.

### Files explicitly NOT modified

- `main.go`, `internal/**` — binary unchanged.
- `.github/workflows/ci.yml` — chart still in `paths-ignore`.
- `.github/workflows/release.yml`, `.goreleaser.yml` — no release pipeline impact.
- `deploy/grafana/**`, `deploy/prometheus/**`, `deploy/k8s/**` — out of scope.
- `README.md` — chart-specific docs live in `values.yaml`.

---

## Task Decomposition

### Task 1: Pre-flight — capture the current contract and verify tooling

**Files:** none (capture only). Test: `deploy/helm/hyperping-exporter/tests/render_test.py` (run as-is).

This task does NOT change the chart. It establishes the baseline.

- [ ] **Step 1: Run the existing render harness against the current chart**

```bash
cd /home/khaledsa/projects/develeap/hyperping-exporter/.worktrees/chart-hardening-prod-defaults
python3 deploy/helm/hyperping-exporter/tests/render_test.py
```
Expected: 9 PASS lines, ending with `ALL RENDER TESTS PASSED`.

- [ ] **Step 2: Run `helm lint`**

```bash
helm lint deploy/helm/hyperping-exporter/
```
Expected: `1 chart(s) linted, 0 chart(s) failed`.

- [ ] **Step 3: Capture baseline references**

```bash
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/default.values.yaml \
  > /tmp/render-baseline.yaml
wc -l /tmp/render-baseline.yaml
# Capture current Go test count so Task 10 doesn't hard-code a stale number.
go test ./... -count=1 2>&1 | tee /tmp/go-baseline.txt | tail -5
TEST_BASELINE=$(grep -c '^=== RUN' /tmp/go-baseline.txt || true)
echo "Go test baseline RUNs: $TEST_BASELINE" > /tmp/go-baseline-count.txt
cat /tmp/go-baseline-count.txt
```
Expected: file written; baseline test count recorded for use in Task 10.

- [ ] **Step 4: Verify tooling availability and capture versions**

```bash
which helm kind kubectl kubeconform docker 2>&1 | tee /tmp/tooling.txt
helm version --short
kind version 2>/dev/null || echo "kind missing"
kubeconform -v 2>/dev/null || echo "kubeconform missing"
docker version --format '{{.Client.Version}}'
```
Expected: helm v3.20.x present; docker present (required for Task 2's containerised measurement). Missing `kind`/`kubeconform` is acceptable locally (CI provides them); record and continue.

- [ ] **Step 5: Verify Hyperping dev API key**

```bash
test -r /home/khaledsa/projects/develeap/terraform-provider-hyperping/.env && \
  grep -q '^HYPERPING_API_KEY=' /home/khaledsa/projects/develeap/terraform-provider-hyperping/.env && \
  echo "OK key file readable" || echo "FAIL key file missing or has no HYPERPING_API_KEY="
```
Expected: `OK key file readable`.

- [ ] **Step 6: Resolve action SHA pins (CRITICAL pre-task — explicit verification, no `||`-fallback)**

The pins must be deterministic: each query resolves to an exact tag, an exact SHA for that tag, and an exact `kindest/node` digest for the matching `kind` release. The `||`-fallback pattern from the previous plan masked tag-drift (`helm/kind-action v1.12.0` is now superseded by `v1.14.0`; `kind v0.30.0` is now superseded by `v0.31.0`). Run each command separately; if any of them returns a tag that does not match the expected literal, ABORT and surface the mismatch to the user. Substitution into the workflow then uses ONLY values that came out of `gh api` in this step.

```bash
# 1. Resolve helm/kind-action latest tag. Expected to be at minimum v1.14.0
#    (per upstream as of plan synthesis). The pin used in Task 8 must equal
#    whatever this query returns.
HELM_KIND_ACTION_TAG=$(gh api repos/helm/kind-action/releases/latest --jq .tag_name)
echo "helm/kind-action latest tag: ${HELM_KIND_ACTION_TAG}"
test -n "${HELM_KIND_ACTION_TAG}" || { echo "FATAL: gh returned empty kind-action tag" >&2; exit 1; }
echo "${HELM_KIND_ACTION_TAG}" > /tmp/kind-action-tag.txt

# 2. Resolve the commit SHA for that exact tag (annotated-tag aware).
HELM_KIND_ACTION_SHA=$(gh api repos/helm/kind-action/git/ref/tags/${HELM_KIND_ACTION_TAG} --jq '.object.sha')
# If .object.type is "tag" (annotated tag), dereference once to get the
# underlying commit SHA. Lightweight tags return "commit" directly.
HELM_KIND_ACTION_OBJ_TYPE=$(gh api repos/helm/kind-action/git/ref/tags/${HELM_KIND_ACTION_TAG} --jq '.object.type')
if [ "${HELM_KIND_ACTION_OBJ_TYPE}" = "tag" ]; then
  HELM_KIND_ACTION_SHA=$(gh api repos/helm/kind-action/git/tags/${HELM_KIND_ACTION_SHA} --jq '.object.sha')
fi
echo "helm/kind-action ${HELM_KIND_ACTION_TAG} SHA: ${HELM_KIND_ACTION_SHA}"
test -n "${HELM_KIND_ACTION_SHA}" || { echo "FATAL: empty kind-action SHA" >&2; exit 1; }
echo "${HELM_KIND_ACTION_SHA}" > /tmp/kind-action-sha.txt

# 3. Resolve kubernetes-sigs/kind latest tag and require ≥ v0.31.0.
KIND_TAG=$(gh api repos/kubernetes-sigs/kind/releases/latest --jq .tag_name)
echo "kind latest tag: ${KIND_TAG}"
test -n "${KIND_TAG}" || { echo "FATAL: gh returned empty kind tag" >&2; exit 1; }
echo "${KIND_TAG}" > /tmp/kind-latest-tag.txt

# 4. Extract the kindest/node digest for v1.34.x from THAT release's body.
#    kind ships node images keyed to its own release.
KINDEST_DIGEST_LINE=$(gh api repos/kubernetes-sigs/kind/releases/tags/${KIND_TAG} --jq '.body' \
  | grep -Eo 'kindest/node:v1\.34\.[0-9]+@sha256:[0-9a-f]{64}' | head -1)
echo "kindest/node digest line: ${KINDEST_DIGEST_LINE}"
test -n "${KINDEST_DIGEST_LINE}" || { echo "FATAL: no kindest/node:v1.34.x digest in ${KIND_TAG} release notes" >&2; exit 1; }
echo "${KINDEST_DIGEST_LINE}" > /tmp/kindest-node-digest.txt

# 5. Cross-check helm/setup-helm, setup-python, checkout SHAs against
#    their currently-pinned tags in helm-ci.yml. These were verified in
#    PR 51 and remain valid; re-confirm here so reviewers see the audit.
ACTIONS_CHECKOUT_TAG=$(gh api repos/actions/checkout/releases/latest --jq .tag_name)
SETUP_HELM_TAG=$(gh api repos/azure/setup-helm/releases/latest --jq .tag_name)
SETUP_PYTHON_TAG=$(gh api repos/actions/setup-python/releases/latest --jq .tag_name)
echo "current pins: actions/checkout=v6 (latest=${ACTIONS_CHECKOUT_TAG})"
echo "              azure/setup-helm=v5.0.0 (latest=${SETUP_HELM_TAG})"
echo "              actions/setup-python=v6.2.0 (latest=${SETUP_PYTHON_TAG})"
# These are FYI only; the workflow keeps the pre-verified pins from PR51.
# A future PR can re-bump them.
```

**Failure policy:** if any of these commands surfaces a tag that does NOT match the values above (e.g. `helm/kind-action` rolled to `v1.15.0` mid-execution, or kind released `v0.32.0` with a new `kindest/node:v1.35` line), the executing subagent surfaces the mismatch to the user via the `USER DECISION REQUIRED:` channel rather than silently advancing with stale pins. The `||`-fallback construct previously used here is explicitly removed: it would have masked tag drift.

Captured values are used VERBATIM in Task 8 Step 4 (workflow YAML) and Task 8 Step 2 (`kind-pss.yaml` node digest). The plan does NOT hard-code `v1.12.0` or `v0.30.0` anywhere downstream; substitutions are sourced from `/tmp/kind-action-tag.txt`, `/tmp/kind-action-sha.txt`, `/tmp/kind-latest-tag.txt`, `/tmp/kindest-node-digest.txt`.

- [ ] **Step 7: Commit nothing — pre-flight only.** Move directly to Task 2.

---

### Task 2: Empirically observe binary resource use under chart-equivalent runtime

**Files:** Modify `deploy/helm/hyperping-exporter/values.yaml` (resources block). Test: `deploy/helm/hyperping-exporter/tests/render_test.py` (assertion to be added in Task 10 once all values are settled).

The user explicitly required empirical validation against the live Hyperping API. Measurement runs **inside Docker** with `--read-only --user 65534:65534 --tmpfs /tmp` so the runtime envelope matches the chart's deployment exactly (uid 65534, read-only root, restricted writable surfaces). This task simultaneously **proves the binary tolerates `readOnlyRootFilesystem: true`** (item 4); if the binary aborts or hangs, this task fails-fast and the chart adds an `emptyDir` mount before proceeding.

- [ ] **Step 1: Build a local Docker image of the binary**

```bash
cd /home/khaledsa/projects/develeap/hyperping-exporter/.worktrees/chart-hardening-prod-defaults
make docker-build
docker images hyperping-exporter:dev 2>/dev/null | head -2
```
Expected: image present. If `make docker-build` produces a different tag, capture and reuse below.

- [ ] **Step 2: Launch the binary inside Docker matching chart runtime**

```bash
set -a
. /home/khaledsa/projects/develeap/terraform-provider-hyperping/.env
set +a
mkdir -p /tmp/hpe-obs
# Chart-equivalent: read-only root, distinct uid, /tmp as tmpfs for any
# transient files Go runtime might want. This is the runtime proof for item 4.
docker run -d --rm \
  --name hpe-obs \
  --read-only \
  --user 65534:65534 \
  --tmpfs /tmp:rw,size=16m \
  --cap-drop=ALL \
  --security-opt no-new-privileges \
  -p 9312:9312 \
  -e HYPERPING_API_KEY="$HYPERPING_API_KEY" \
  hyperping-exporter:dev \
  --cache-ttl=60s --log-level=info --log-format=json
sleep 5
docker ps --filter name=hpe-obs --format '{{.Status}}'
curl -fsS http://127.0.0.1:9312/healthz && echo
```
Expected: container `Up`, `/healthz` returns 200. If the container exits with "permission denied" or similar, **stop**: item 4 requires either an `emptyDir` mount in the deployment template OR a binary change. Surface to the user before continuing.

- [ ] **Step 3: Sample CPU/RSS every 10s for 30 minutes**

```bash
for i in $(seq 1 180); do
  docker stats --no-stream --format '{{.CPUPerc}} {{.MemUsage}}' hpe-obs >> /tmp/hpe-obs/samples.txt
  curl -fsS http://127.0.0.1:9312/metrics > /dev/null || echo "scrape failed at sample $i"
  sleep 10
done
echo "done sampling"
wc -l /tmp/hpe-obs/samples.txt
```
Expected: 180 samples; all scrapes succeed.

- [ ] **Step 4: Compute peak and 99th-percentile values**

```bash
# Peak RSS in MiB (docker stats prints "37.5MiB / 256MiB" -> first field).
awk '{print $2}' /tmp/hpe-obs/samples.txt \
  | sed 's/MiB.*//; s/KiB.*//; s/GiB.*/000/' | sort -n | tail -1
# p99 CPU percent.
awk '{gsub("%",""); print $1}' /tmp/hpe-obs/samples.txt \
  | sort -n | awk 'NR==int(NR*0.99)+1{print; exit}'
docker stop hpe-obs
```
Record both numbers in the commit message.

- [ ] **Step 5: Determine defaults**

Rules (unchanged from original plan):
- `requests.memory` = `ceil(peak_RSS_Mi * 1.5)` rounded to nearest 16Mi.
- `limits.memory` = `ceil(peak_RSS_Mi * 4)` rounded to nearest 32Mi.
- `requests.cpu` = `max(50m, p99_cpu_pct * 10m)`.
- `limits.cpu` = `max(200m, p99_cpu_pct * 20m)`.

Typical expected outcome (peak RSS ≤ 40Mi, p99 ≤ 5%): `requests {cpu: 50m, memory: 64Mi}`, `limits {cpu: 200m, memory: 256Mi}`. **The measured values win** regardless of expectation; record measured peak/p99 in commit and write them into both values.yaml AND the Task 10 render assertion.

- [ ] **Step 6: Update `values.yaml`**

Replace the `resources` block with the computed values. Use this template (substituting measured numbers):

```yaml
resources:
  requests:
    cpu: <COMPUTED>      # measured p99 + safety margin
    memory: <COMPUTED>   # measured peak * 1.5
  limits:
    cpu: <COMPUTED>      # 4x requests, allows brief bursts
    memory: <COMPUTED>   # 4x peak; OOMKills surface as a clear failure
```

Record the measured values in a file `/tmp/hpe-obs/computed-defaults.yaml` for use by Task 10's assertion update.

- [ ] **Step 7: Commit**

```bash
cd /home/khaledsa/projects/develeap/hyperping-exporter/.worktrees/chart-hardening-prod-defaults
git add deploy/helm/hyperping-exporter/values.yaml
git commit -m "feat(helm): set resources defaults from 30min containerized live API observation

Measured peak RSS <P>MiB, p99 CPU <X>% inside Docker with --read-only
and uid 65534 (matching chart runtime). Also proves the binary tolerates
readOnlyRootFilesystem: true; no emptyDir mount required.

requests {cpu: <CPU_REQ>, memory: <MEM_REQ>}; limits {cpu: <CPU_LIM>,
memory: <MEM_LIM>}. Render harness assertion follows in Task 10."
```
Substitute measured values. The render-time literal assertion lands in Task 10 (after all values stabilise) to avoid the TDD-discipline footgun of asserting against partial values.

---

### Task 3: Reconcile liveness/readiness probe defaults

**Files:** Modify `deploy/helm/hyperping-exporter/values.yaml`. Test: render assertion in Task 10.

User's contract: liveness `10/10/3`, readiness `5/5/3`. The current chart ships `5/30` liveness and `10/15` readiness with no `failureThreshold`. The binary's HTTP server is up in sub-second; `initialDelaySeconds: 10` for liveness is conservative (slightly slower pod-ready, no benefit), but it matches the user-stated contract and is overridable. Document the rationale in the values comment so future maintainers can tighten without re-deriving.

- [ ] **Step 1: Update `values.yaml` probes block**

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: http
  # User contract: 10/10/3 — conservative because liveness restarts are
  # expensive. Override to 5/5/3 if your environment tolerates faster
  # restarts on transient backpressure.
  initialDelaySeconds: 10
  periodSeconds: 10
  failureThreshold: 3

readinessProbe:
  httpGet:
    path: /readyz
    port: http
  # User contract: 5/5/3 — tighter than liveness so a flapping backend
  # pulls the pod out of Service endpoints quickly without a full
  # restart cycle.
  initialDelaySeconds: 5
  periodSeconds: 5
  failureThreshold: 3
```

- [ ] **Step 2: Run harness as smoke check (no assertion yet)**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
```
Expected: all existing assertions still pass. Probe assertions land in Task 10.

- [ ] **Step 3: Commit**

```bash
git add deploy/helm/hyperping-exporter/values.yaml
git commit -m "feat(helm): set liveness 10/10/3 and readiness 5/5/3 probe defaults

User contract: liveness on /healthz at 10s initial / 10s period /
failureThreshold 3; readiness on /readyz tighter (5/5/3) so a flapping
backend pulls the pod out of Service endpoints quickly. Both fully
overridable; rationale documented inline in values.yaml."
```

---

### Task 4: Render `--cache-ttl` via toJson with type-safe coercion, add validateCacheTTL guard

**Files:** Modify `deploy/helm/hyperping-exporter/templates/deployment.yaml:45`. Modify `deploy/helm/hyperping-exporter/templates/_helpers.tpl`. Modify `deploy/helm/hyperping-exporter/values.yaml` (cacheTTL comment).

**Why both `%v`/`toString` AND a validator:** the operator-facing UX is a single error message at render time pointing them at the `"60s"` form. The `printf` defensive form (`%v` + `toString`) is belt-and-braces; even if a future template change drops the validator, the cache-ttl arg would still render as a parseable string (`--cache-ttl=60`) rather than as Go's `%!s(int=60)` poison value. Both layers together: validator is the operator-visible UX; defensive printf is a safety net.

- [ ] **Step 1: Append `validateCacheTTL` to `_helpers.tpl`**

```yaml
{{/*
Render-time validation: cacheTTL must be a Go duration STRING (e.g. "60s",
"5m"). A bare integer like `cacheTTL: 60` is parsed by YAML as int and
the binary's time.ParseDuration would reject it with "missing unit in
duration". Fail loudly rather than ship a runtime CrashLoopBackOff.

Uses Sprig's kindOf which returns "int", "float64", "string", "bool",
"slice", "map". The guard accepts only "string".
*/}}
{{- define "hyperping-exporter.validateCacheTTL" -}}
{{- $v := .Values.config.cacheTTL -}}
{{- $k := kindOf $v -}}
{{- if ne $k "string" -}}
{{- fail (printf "Configuration error: config.cacheTTL must be a quoted Go duration string (e.g. \"60s\", \"5m\"). Got %v of kind %s. Did you forget the quotes around the value in values.yaml?" $v $k) -}}
{{- end -}}
{{- end }}
```

- [ ] **Step 2: Replace the cache-ttl line in `deployment.yaml`**

Change line 45 from:
```yaml
            - "--cache-ttl={{ .Values.config.cacheTTL }}"
```
to:
```yaml
            - {{ printf "--cache-ttl=%v" (toString .Values.config.cacheTTL) | toJson }}
```

`%v` is Sprig/Go's universally-safe verb (works for any reflect kind); `toString` coerces to string regardless of input kind so the printf never produces `%!s(int=60)`. The `validateCacheTTL` helper (wired in Step 4 below) is the operator-visible error path; this rendering is the safety net.

- [ ] **Step 3: Add the cacheTTL numeric-warning comment to values.yaml**

Above the `cacheTTL:` line, insert:
```yaml
  # IMPORTANT: cacheTTL must be a quoted Go duration string (e.g. "60s",
  # "5m"). The chart fails the render (validateCacheTTL helper) when this
  # value is supplied as a bare integer like `cacheTTL: 60`, because the
  # binary's time.ParseDuration rejects unit-less values at startup.
  cacheTTL: "60s"
```
(Replace the existing `cacheTTL: "60s"` line with the commented block above.)

- [ ] **Step 4: Wire `validateCacheTTL` into `deployment.yaml`'s top-of-file guard block**

This task's commit is BEFORE Task 6/7 (where `validateSecretSources` / `validateReplicaCount` are introduced), so the file's existing guard structure must NOT be disturbed yet. Insert the `validateCacheTTL` include immediately AFTER the existing legacy `if and (not apiKey) (not existingSecret)` block at the very top:

```
{{- if and (not .Values.config.apiKey) (not .Values.config.existingSecret) -}}
{{- fail "must provide either config.apiKey or config.existingSecret" -}}
{{- end -}}
{{- include "hyperping-exporter.validateCacheTTL" . -}}
```

The legacy two-line guard is later replaced wholesale in Task 6 (renamed) when `validateSecretSources` lands; at that point this `validateCacheTTL` include is preserved as the second of three sibling includes. Document this intent inline as a `{{- /* … */ -}}` comment so the executor doesn't accidentally drop it on the Task 6 rewrite.

- [ ] **Step 5: Run the harness — BASELINE_ARGS still match**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
helm lint deploy/helm/hyperping-exporter/
```
Expected: every existing case still passes. `printf "--cache-ttl=%v" (toString "60s")` produces `"--cache-ttl=60s"`; `toJson` quotes it; the rendered arg matches baseline `--cache-ttl=60s` byte-for-byte. The new validateCacheTTL helper does not fire on the string default. `helm lint` clean.

- [ ] **Step 6: Smoke-test the validator fires on bare int**

```bash
cat > /tmp/cache-ttl-bare-int.yaml <<'EOF'
config:
  apiKey: x
  cacheTTL: 60
EOF
helm template testrel deploy/helm/hyperping-exporter/ -f /tmp/cache-ttl-bare-int.yaml 2>&1 \
  | grep -E 'cacheTTL must be a quoted Go duration string' >/dev/null && echo "PASS validator fires" || { echo "FAIL validator did not fire"; exit 1; }
rm -f /tmp/cache-ttl-bare-int.yaml
```
Expected: `PASS validator fires`. The render-harness fixture for this case lands in Task 10.

- [ ] **Step 7: Commit**

```bash
git add deploy/helm/hyperping-exporter/templates/deployment.yaml \
        deploy/helm/hyperping-exporter/templates/_helpers.tpl \
        deploy/helm/hyperping-exporter/values.yaml
git commit -m "refactor(helm): render --cache-ttl via toJson with type-safe coercion + validateCacheTTL guard

All optional-flag args (--exclude-name-pattern, --mcp-url) already go
through 'printf ... | toJson'. Bring --cache-ttl onto the same path
using 'printf %v' and 'toString' so non-string operator inputs never
become Go fmt poison values (%!s(int=60)).

validateCacheTTL helper fails the render with a clear error when the
operator supplies a bare integer like 'cacheTTL: 60' instead of '60s'.
This is the operator-visible UX; the printf coercion is the safety net.

Inline values.yaml comment points operators at the quoted-string
requirement. Smoke-tested both paths."
```

---

### Task 5: Document the existing PSS-restricted securityContext

**Files:** Modify `deploy/helm/hyperping-exporter/values.yaml`. Test: assertion in Task 10.

The chart already satisfies PSS-restricted; this task adds the explanatory comment block. The actual lock-in assertion belongs in Task 10 alongside every other defaults-case assertion, because writing it here violates TDD discipline (no red state, since the chart already complies).

- [ ] **Step 1: Audit chart values against PSS-restricted (v1.33) profile**

PSS Restricted requires:
- `allowPrivilegeEscalation: false`
- `capabilities.drop` contains `ALL`
- `runAsNonRoot: true`
- `seccompProfile.type: RuntimeDefault` (or `Localhost`)
- `runAsUser != 0`

The chart satisfies all of these today. No production change required.

- [ ] **Step 2: Add explanatory comment above `securityContext:` in values.yaml**

```yaml
# Pod and container securityContext defaults satisfy the Kubernetes Pod
# Security Standard "restricted" profile (v1.33+). The render harness
# (Task 10) locks the exact field values; the kind PSS admission CI job
# admits these into a pod-security.kubernetes.io/enforce=restricted
# namespace AND boots the pod live to prove the binary tolerates
# readOnlyRootFilesystem: true at runtime. Weakening any of these fields
# may cause PSS admission rejection in production clusters.
securityContext:
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  ...
```

- [ ] **Step 3: Commit**

```bash
git add deploy/helm/hyperping-exporter/values.yaml
git commit -m "docs(helm): document the PSS-restricted contract on the securityContext block

Explanatory comment above securityContext: explains what the field
defaults are doing and what proves them (the render harness locks
exact field values, the kind PSS admission job boots the pod live).
No production change."
```

---

### Task 6: ExternalSecret template + secret-source guards + multi-replica guard (single commit)

**Files:** Create `deploy/helm/hyperping-exporter/templates/externalsecret.yaml`. Create three external-secret fixtures, five conflict/missing fixtures (including replicas-multi). Modify `deploy/helm/hyperping-exporter/templates/_helpers.tpl`, `templates/deployment.yaml`, `templates/secret.yaml`, `values.yaml`. Test: smoke checks at Step 7 below; full harness assertions land in Task 10.

**Why these land in a single commit (R2-11 fix):** the prior plan split this work between Task 6 (template + values) and Task 7 (guards + legacy-guard replacement). At the Task 6 commit point in that split, an `external-secret.values.yaml` fixture (no `apiKey`, no `existingSecret`) would trip the LEGACY two-line guard at the top of `deployment.yaml` (`fail "must provide either config.apiKey or config.existingSecret"`); Task 6's own smoke test would exit non-zero, and the branch would be in a bisect-red state between commits 6 and 7. Combining them ensures every individual commit on the branch keeps `helm template -f <any-fixture>` green for fixtures that are supposed to render and red (with the correct message) for fixtures that are supposed to fail.

**Why this combined task comes BEFORE the rest of Task 7's body (now Task 7 = PDB/NP/replicas-zero):** the ExternalSecret path + secret-source guards form one self-contained semantic unit. Re-numbering: this is now Task 6; the prior Task 7's PDB/NP/Cilium/replicas-zero body becomes Task 7 (unchanged semantically, just renumbered).

- [ ] **Step 1: Add `externalSecret` block to `values.yaml`**

Insert after the `config:` block (before `service:`). Block comment above explains mutual exclusion:

```yaml
# Mutually-exclusive secret sources for the Hyperping API key:
#   - config.apiKey         (inline; dev/test only — stored plaintext in release metadata)
#   - config.existingSecret (operator manages a Secret out-of-band)
#   - externalSecret.enabled (External Secrets Operator path; below)
# Setting more than one (or none at replicaCount>=1) aborts install with
# a clear error. replicaCount: 0 is exempt from the "must set one" check
# (scaled-to-zero state needs no API key).
externalSecret:
  # External Secrets Operator API version. Default tracks ESO v0.16+
  # (which graduates v1 from v1beta1). Set to "external-secrets.io/v1beta1"
  # for legacy ESO installations (≤ v0.15). The conversion webhook in
  # ESO ≤ 0.18 still serves v1beta1 transparently.
  apiVersion: external-secrets.io/v1
  enabled: false
  # Reference to an existing (Cluster)SecretStore reachable by ESO.
  # Required when enabled is true; render aborts otherwise.
  secretStoreRef:
    name: ""
    kind: SecretStore   # or ClusterSecretStore
  # Remote key under which the Hyperping API key is stored in the
  # backing secret manager. Maps onto the chart-managed Secret as the
  # `api-key` field that the Deployment reads via HYPERPING_API_KEY.
  remoteRef:
    key: ""
    # Optional. Provider-specific sub-key when the backing secret is a
    # structured blob rather than a plain string.
    property: ""
  # How often ESO re-syncs the remote value into the in-cluster Secret.
  refreshInterval: "1h"
```

- [ ] **Step 2: Write the template `templates/externalsecret.yaml`**

```yaml
{{- /*
ExternalSecret variant: writes the Hyperping API key into a Secret of
the same name the Deployment's HYPERPING_API_KEY env var already
references. Mutually exclusive with config.apiKey and config.existingSecret
(enforced by templates/secret.yaml's guard + the validateSecretSources
helper added in Task 6).

REQUIRES the External Secrets Operator installed cluster-wide. Apply-time
"no matches for kind ExternalSecret" is the honest failure mode on
clusters where ESO is absent; the chart cannot detect operator presence
at render time.
*/ -}}
{{- if and .Values.externalSecret .Values.externalSecret.enabled -}}
apiVersion: {{ .Values.externalSecret.apiVersion | default "external-secrets.io/v1" }}
kind: ExternalSecret
metadata:
  name: {{ include "hyperping-exporter.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "hyperping-exporter.labels" . | nindent 4 }}
spec:
  refreshInterval: {{ .Values.externalSecret.refreshInterval | default "1h" }}
  secretStoreRef:
    name: {{ required "externalSecret.secretStoreRef.name is required when externalSecret.enabled" .Values.externalSecret.secretStoreRef.name }}
    kind: {{ .Values.externalSecret.secretStoreRef.kind | default "SecretStore" }}
  target:
    name: {{ include "hyperping-exporter.fullname" . }}
    creationPolicy: Owner
  data:
    - secretKey: api-key
      remoteRef:
        key: {{ required "externalSecret.remoteRef.key is required when externalSecret.enabled" .Values.externalSecret.remoteRef.key }}
        {{- with .Values.externalSecret.remoteRef.property }}
        property: {{ . }}
        {{- end }}
{{- end -}}
```

- [ ] **Step 3: Write three ExternalSecret fixtures**

`external-secret.values.yaml` (explicit override of `refreshInterval`, used by the primary render assertion):
```yaml
externalSecret:
  enabled: true
  secretStoreRef:
    name: vault-store
    kind: SecretStore
  remoteRef:
    key: hyperping/api-key
  refreshInterval: "30m"
```

`external-secret-defaults.values.yaml` (no `refreshInterval` override; proves default `1h` propagates):
```yaml
externalSecret:
  enabled: true
  secretStoreRef:
    name: vault-store
    kind: SecretStore
  remoteRef:
    key: hyperping/api-key
```

`external-secret-missing-store.values.yaml` (no `secretStoreRef.name`; asserts the `required` failure):
```yaml
externalSecret:
  enabled: true
  remoteRef:
    key: hyperping/api-key
```

- [ ] **Step 4: Append `secretSourceCount`, `validateSecretSources`, `validateReplicaCount` to `_helpers.tpl`**

(Sibling of `validateCacheTTL` from Task 4.)

```yaml
{{/*
Count configured secret sources. Exactly one of apiKey, existingSecret,
externalSecret.enabled must be set when replicaCount >= 1.
Defensive against missing externalSecret block in consumer overrides.
*/}}
{{- define "hyperping-exporter.secretSourceCount" -}}
{{- $count := 0 -}}
{{- if .Values.config.apiKey -}}{{- $count = add $count 1 -}}{{- end -}}
{{- if .Values.config.existingSecret -}}{{- $count = add $count 1 -}}{{- end -}}
{{- $es := (.Values.externalSecret | default dict) -}}
{{- if and $es $es.enabled -}}{{- $count = add $count 1 -}}{{- end -}}
{{- $count -}}
{{- end }}

{{/*
Render-time validation: fail loudly when secret sources are misconfigured.
Exempt: replicaCount: 0 (scaled-to-zero needs no key); skipping the
"must set one" check lets operators tear down secrets before final
removal without a render error.
*/}}
{{- define "hyperping-exporter.validateSecretSources" -}}
{{- $count := include "hyperping-exporter.secretSourceCount" . | int -}}
{{- if gt $count 1 -}}
{{- fail "Configuration error: set exactly one of .Values.config.apiKey, .Values.config.existingSecret, or .Values.externalSecret.enabled. Got multiple." -}}
{{- end -}}
{{- if and (eq $count 0) (gt (int .Values.replicaCount) 0) -}}
{{- fail "Configuration error: must set one of .Values.config.apiKey, .Values.config.existingSecret, or .Values.externalSecret.enabled." -}}
{{- end -}}
{{- end }}

{{/*
Render-time validation: the chart is single-poller by design. Multiple
replicas would mean N independent pollers hammering the Hyperping API
in parallel. replicaCount: 0 remains valid (scaled-to-zero state).
*/}}
{{- define "hyperping-exporter.validateReplicaCount" -}}
{{- if gt (int .Values.replicaCount) 1 -}}
{{- fail (printf "Configuration error: replicaCount=%d is unsupported. The exporter is single-poller by design; multiple replicas would each independently poll the Hyperping API. Set replicaCount to 1 (or 0 to scale to zero)." (int .Values.replicaCount)) -}}
{{- end -}}
{{- end }}
```

- [ ] **Step 5: Replace the legacy guard in `deployment.yaml` and gate the env block on `secretSourceCount > 0`**

(a) Replace the legacy two-line `if and (not apiKey) (not existingSecret) → fail` block at the very top of `deployment.yaml` with the three validator includes (preserving the `validateCacheTTL` include added in Task 4):

```
{{- include "hyperping-exporter.validateSecretSources" . -}}
{{- include "hyperping-exporter.validateReplicaCount" . -}}
{{- include "hyperping-exporter.validateCacheTTL" . -}}
```

Trailing `-}}` on all three lines so no stray blank document precedes `apiVersion:`.

(b) **R2-8 fix:** Gate the existing `env:` block that emits `HYPERPING_API_KEY` valueFrom `secretKeyRef` so it is suppressed when `secretSourceCount == 0` (the `replicaCount: 0` scaled-to-zero state is the only configuration that hits this branch, because `validateSecretSources` already aborts the render for `secretSourceCount == 0 AND replicaCount >= 1`). Wrap the relevant lines in `deployment.yaml` (around lines 56–64; locate the `env:` block emitting `HYPERPING_API_KEY`):

```yaml
        {{- $sources := include "hyperping-exporter.secretSourceCount" . | int }}
        {{- if gt $sources 0 }}
          env:
            - name: HYPERPING_API_KEY
              valueFrom:
                secretKeyRef:
                  name: {{ default (include "hyperping-exporter.fullname" .) .Values.config.existingSecret }}
                  key: api-key
        {{- end }}
```

Rationale: a `replicaCount: 0` Deployment with no configured secret source currently emits a `secretKeyRef` pointing at a non-existent Secret. The kubelet won't try to resolve it (no Pods scheduled) but a future scale 0 → 1 without first configuring a source would CrashLoopBackOff silently. With the gating in place, the operator hits `validateSecretSources` at the next render and gets a clear error.

- [ ] **Step 6: Extend `secret.yaml` guard**

Change line 1 of `secret.yaml` to:
```yaml
{{- if and .Values.config.apiKey (not .Values.config.existingSecret) (not (and .Values.externalSecret .Values.externalSecret.enabled)) }}
```

- [ ] **Step 7: Write five conflict / missing fixtures**

`secret-conflict-apikey-and-existing.values.yaml`:
```yaml
config:
  apiKey: x
  existingSecret: my-external-secret
```

`secret-conflict-apikey-and-external.values.yaml`:
```yaml
config:
  apiKey: x
externalSecret:
  enabled: true
  secretStoreRef:
    name: vault-store
    kind: SecretStore
  remoteRef:
    key: hyperping/api-key
```

`secret-conflict-existing-and-external.values.yaml`:
```yaml
config:
  existingSecret: my-external-secret
externalSecret:
  enabled: true
  secretStoreRef:
    name: vault-store
    kind: SecretStore
  remoteRef:
    key: hyperping/api-key
```

`secret-source-missing.values.yaml` (explicit `externalSecret.enabled: false` so future default flips don't silently change this fixture's meaning):
```yaml
config:
  apiKey: ""
  existingSecret: ""
externalSecret:
  enabled: false
```

`replicas-multi.values.yaml`:
```yaml
replicaCount: 2
config:
  apiKey: x
```

- [ ] **Step 8: Smoke-test that both render-success and render-fail paths work**

(R2-11 fix — these smoke tests now run AFTER the legacy-guard replacement, so the ExternalSecret fixture's `helm template` actually succeeds.)

```bash
# (a) ExternalSecret path now renders successfully (legacy guard replaced).
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/external-secret.values.yaml \
  | grep -E '^(apiVersion|kind|  name|  refreshInterval): '
# Expected: kind: ExternalSecret, apiVersion: external-secrets.io/v1,
# name: testrel-hyperping-exporter, refreshInterval: 30m.
# Plain Secret should NOT be present (secret.yaml guard suppresses it).
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/external-secret.values.yaml \
  | grep -c '^kind: Secret$' | grep -q '^0$' && echo "PASS plain Secret suppressed" || { echo "FAIL plain Secret leaked"; exit 1; }

# (b) Existing render harness still green (no regressions on pre-Task-6 fixtures).
python3 deploy/helm/hyperping-exporter/tests/render_test.py

# (c) Conflict / missing / multi-replica all fail with the right messages.
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-apikey-and-existing.values.yaml 2>&1 \
  | grep -q "Got multiple" && echo "PASS conflict apikey+existing" || { echo "FAIL conflict apikey+existing"; exit 1; }
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-apikey-and-external.values.yaml 2>&1 \
  | grep -q "Got multiple" && echo "PASS conflict apikey+external" || { echo "FAIL conflict apikey+external"; exit 1; }
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-existing-and-external.values.yaml 2>&1 \
  | grep -q "Got multiple" && echo "PASS conflict existing+external" || { echo "FAIL conflict existing+external"; exit 1; }
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/secret-source-missing.values.yaml 2>&1 \
  | grep -q "must set one of" && echo "PASS missing source" || { echo "FAIL missing source"; exit 1; }
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/replicas-multi.values.yaml 2>&1 \
  | grep -q "replicaCount=2 is unsupported" && echo "PASS replicas-multi" || { echo "FAIL replicas-multi"; exit 1; }
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/external-secret-missing-store.values.yaml 2>&1 \
  | grep -q "externalSecret.secretStoreRef.name is required" && echo "PASS required-store" || { echo "FAIL required-store"; exit 1; }

# (d) Helm lint still clean.
helm lint deploy/helm/hyperping-exporter/
```

Expected: all `PASS` lines, render harness green, `helm lint` clean. The full assertion harness lands in Task 10; this step is the bisect-safety gate for the combined commit.

- [ ] **Step 9: Commit (single commit — template + helpers + guards + fixtures all together)**

```bash
git add deploy/helm/hyperping-exporter/templates/externalsecret.yaml \
        deploy/helm/hyperping-exporter/templates/_helpers.tpl \
        deploy/helm/hyperping-exporter/templates/deployment.yaml \
        deploy/helm/hyperping-exporter/templates/secret.yaml \
        deploy/helm/hyperping-exporter/values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/external-secret.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/external-secret-defaults.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/external-secret-missing-store.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-apikey-and-existing.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-apikey-and-external.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-existing-and-external.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/secret-source-missing.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/replicas-multi.values.yaml
git commit -m "feat(helm): ExternalSecret template, secret-source guards, multi-replica guard, env-block gating

Single commit so every fixture's render result remains consistent at
every bisect point. Combines the ExternalSecret template (which lets
the externalSecret.values.yaml fixture render at all) with:

- secretSourceCount / validateSecretSources / validateReplicaCount
  helpers in _helpers.tpl.
- Legacy two-line apiKey/existingSecret guard at the top of
  deployment.yaml replaced with the three validator includes (plus
  the validateCacheTTL include preserved from the prior commit).
- deployment.yaml env: HYPERPING_API_KEY block gated on
  secretSourceCount > 0 so a replicaCount: 0 Deployment without a
  configured secret source does not dangle a secretKeyRef to a
  missing Secret (operator scaling 0 -> 1 without first configuring
  a source re-trips validateSecretSources at the next render).
- secret.yaml guard extended to suppress the chart-managed Secret
  when externalSecret.enabled is true.
- Five new fixtures (three conflicts, missing source, replicas-multi).

apiVersion is values-driven (default external-secrets.io/v1; set to
v1beta1 for legacy ESO <= 0.15). Required fields (secretStoreRef.name,
remoteRef.key) enforced via Helm's 'required' helper.

The full assertion harness lands in Task 10."
```

---

### Task 7: PDB guard, NetworkPolicy default-on (with Case 1 enumeration update), Cilium FQDN variant, replicaCount:0 support

**Files:** Modify `deploy/helm/hyperping-exporter/templates/pdb.yaml`, `templates/networkpolicy.yaml`. Create `templates/networkpolicy-cilium.yaml`. Modify `deploy/helm/hyperping-exporter/values.yaml`, `tests/render_test.py`. Create four fixtures.

**Coupling note:** the multi-replica `fail()` from Task 6 means PDB rendering never occurs in supported configs today. The PDB template is kept correct (drain-safe defaults) so a future leader-election change can flip the multi-replica behaviour without revisiting pdb.yaml. The fixture asserts the user-visible safety property: PDB stays suppressed for `replicaCount ≤ 1` even when explicitly enabled.

**R2-2 / R2-5 / R2-10 commit-atomicity contract:**
- (R2-10) Case 1's `expected_versions` enumeration in `render_test.py` is extended to include `NetworkPolicy` in **this same commit** as the `networkPolicy.enabled: false → true` flip. Without that, the very next harness run on this commit would fail at Case 1's `assert_eq(versions, expected_versions, ...)`. Bisect-red eliminated.
- (R2-5) Before committing, the executor runs `helm template testrel <chart> -f <fixture>` for each of the eight pre-existing fixtures (`default`, `existing-secret`, `ascii-regex`, `quote-regex`, `readme-regex`, `single-quote-regex`, `both-flags`, `mcp-url`, `mcp-url-query`) and confirms via `python3 deploy/helm/hyperping-exporter/tests/render_test.py` that all of them remain green with NetworkPolicy now in the render. The harness today never enumerates rendered kinds for those eight fixtures (it asserts only `deployment_args` and `assert_scalars_clean`); the added NetworkPolicy is silently tolerated. Documented here so future maintainers know why no fixture-by-fixture assertion update was needed.
- (R2-2) The Cilium NetworkPolicy template does NOT `toYaml` the vanilla-NP-shaped `networkPolicy.ingressFrom` into `fromEndpoints`. Cilium's `fromEndpoints` expects bare `EndpointSelector` objects (a `matchLabels`/`matchExpressions` map at the root), not vanilla `NetworkPolicyPeer` objects wrapping `podSelector`/`namespaceSelector`. The template walks each entry and emits a converted `EndpointSelector`; entries that can't be converted abort the render with a clear error. If `ingressFrom` is empty, the `ingress:` block is omitted entirely (egress-only). See Step 3 below for the conversion logic.

- [ ] **Step 1: Update `pdb.yaml`**

```yaml
{{- /*
PDB is rendered only when both conditions hold:
  - podDisruptionBudget.enabled: true (operator opt-in)
  - replicaCount > 1 (a PDB selecting a single replica with
    maxUnavailable: 0 would block voluntary disruptions including
    kubectl drain, which is worse than no PDB at all)
With the multi-replica fail() guard in Task 6 this template effectively
never emits today. The guard is kept correct so a future leader-election
architecture can remove the fail() without revisiting pdb.yaml.
*/ -}}
{{- if and .Values.podDisruptionBudget.enabled (gt (int .Values.replicaCount) 1) }}
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ include "hyperping-exporter.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "hyperping-exporter.labels" . | nindent 4 }}
spec:
  maxUnavailable: {{ .Values.podDisruptionBudget.maxUnavailable | default 1 }}
  selector:
    matchLabels:
      {{- include "hyperping-exporter.selectorLabels" . | nindent 6 }}
{{- end }}
```

Default `maxUnavailable: 1` (NOT 0) so when a future architecture renders PDB, node drains still proceed; explained in values.yaml.

- [ ] **Step 2: Targeted edit of `networkpolicy.yaml`**

Make two minimal edits (NOT a block replacement, so the existing cross-namespace scraping doc above `ingressFrom` and the file's overall shape are preserved):

1. Replace line 1 (`{{- if .Values.networkPolicy.enabled }}`) with:
```
{{- if and .Values.networkPolicy.enabled (not (and .Values.networkPolicy.fqdnRestriction .Values.networkPolicy.fqdnRestriction.enabled)) }}
```

2. Replace the egress 443 rule's `to:` block (the `ipBlock` with `cidr: 0.0.0.0/0` and the `except:` list, lines 41-47) with:
```yaml
    # TCP/443 to any destination. NetworkPolicy cannot filter by FQDN;
    # use networkPolicy.fqdnRestriction.enabled with Cilium for FQDN
    # restrictions instead.
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
      ports:
        - protocol: TCP
          port: 443
```

(The DNS rule above and the `policyTypes:`/`ingress:` blocks are untouched.)

- [ ] **Step 3: Create `templates/networkpolicy-cilium.yaml` (with NP-shape → EndpointSelector conversion)**

(R2-2 fix.) The template converts each `networkPolicy.ingressFrom` entry — which the chart's existing default and the existing vanilla `networkpolicy.yaml` consume as a `NetworkPolicyPeer` object with `podSelector.matchLabels` (and optionally `namespaceSelector.matchLabels`) — into a Cilium `EndpointSelector` (a flat `matchLabels` map at the root). Entries that don't fit this shape (e.g. `ipBlock`-only peers, peers missing both selectors) abort the render with a clear `fail()` call; this is consistent with the rest of the chart's "loud failure on misconfiguration" policy.

```yaml
{{- if and .Values.networkPolicy.enabled .Values.networkPolicy.fqdnRestriction .Values.networkPolicy.fqdnRestriction.enabled }}
{{- /*
CiliumNetworkPolicy variant: restricts egress to specific FQDNs.
REQUIRES the Cilium CNI with cilium.io/v2 CRDs installed. On clusters
without Cilium, helm apply will fail with "no matches for kind
CiliumNetworkPolicy"; intentional honest behaviour.
The vanilla networkpolicy.yaml is suppressed when this path is active.

Ingress shape conversion: Cilium's spec.ingress[].fromEndpoints expects
bare EndpointSelector objects (matchLabels / matchExpressions at the
root). The chart's networkPolicy.ingressFrom values default to vanilla
NetworkPolicyPeer shape (podSelector / namespaceSelector wrappers).
This template walks each peer, extracts podSelector.matchLabels and
merges io.kubernetes.pod.namespace from namespaceSelector.matchLabels
when present, and emits an EndpointSelector. Peers without a
podSelector or namespaceSelector abort the render.
*/ -}}
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: {{ include "hyperping-exporter.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "hyperping-exporter.labels" . | nindent 4 }}
spec:
  endpointSelector:
    matchLabels:
      {{- include "hyperping-exporter.selectorLabels" . | nindent 6 }}
  {{- if .Values.networkPolicy.ingressFrom }}
  ingress:
    - fromEndpoints:
        {{- range $i, $peer := .Values.networkPolicy.ingressFrom }}
        {{- $labels := dict }}
        {{- with $peer.podSelector }}
        {{- with .matchLabels }}
        {{- $labels = merge $labels . }}
        {{- end }}
        {{- end }}
        {{- with $peer.namespaceSelector }}
        {{- with .matchLabels }}
        {{- range $k, $v := . }}
        {{- /* Cilium represents namespace match-by-label via the well-known
        io.kubernetes.pod.namespace-labels.<key> label. The common
        ergonomic case (namespace name) uses the
        kubernetes.io/metadata.name label which becomes
        io.kubernetes.pod.namespace.label.kubernetes.io/metadata.name on
        the Cilium side. To keep this conversion honest and avoid
        misinterpretation, fail if the consumer supplied any
        namespaceSelector beyond a simple kubernetes.io/metadata.name
        equality; document the supported shape in values.yaml. */}}
        {{- if ne $k "kubernetes.io/metadata.name" }}
        {{- fail (printf "networkPolicy.ingressFrom[%d].namespaceSelector.matchLabels only supports kubernetes.io/metadata.name when fqdnRestriction.enabled=true; got key %q. Use podSelector for in-namespace selection, or extend the chart for richer Cilium endpoint matching." $i $k) }}
        {{- end }}
        {{- $labels = set $labels "io.kubernetes.pod.namespace" $v }}
        {{- end }}
        {{- end }}
        {{- end }}
        {{- if eq (len $labels) 0 }}
        {{- fail (printf "networkPolicy.ingressFrom[%d] cannot be converted to a Cilium EndpointSelector: needs at least a podSelector.matchLabels or namespaceSelector.matchLabels (kubernetes.io/metadata.name)." $i) }}
        {{- end }}
        - matchLabels:
            {{- toYaml $labels | nindent 12 }}
        {{- end }}
      toPorts:
        - ports:
            - port: "{{ .Values.service.port }}"
              protocol: TCP
  {{- end }}
  egress:
    - toEndpoints:
        - matchLabels:
            io.kubernetes.pod.namespace: kube-system
            k8s-app: kube-dns
      toPorts:
        - ports:
            - port: "53"
              protocol: UDP
            - port: "53"
              protocol: TCP
          rules:
            dns:
              - matchPattern: "*"
    - toFQDNs:
        {{- range .Values.networkPolicy.fqdnRestriction.allowedHosts }}
        - matchName: {{ . | quote }}
        {{- end }}
      toPorts:
        - ports:
            - port: "443"
              protocol: TCP
{{- end }}
```

The vanilla `networkPolicy.ingressFrom` default — `[{podSelector: {matchLabels: {app.kubernetes.io/name: prometheus}}}]` — converts cleanly to `[{matchLabels: {app.kubernetes.io/name: prometheus}}]` on the Cilium side. The `networkpolicy-cilium.values.yaml` fixture exercises this conversion; Case 19 in Task 10 asserts the rendered EndpointSelector shape literally so the conversion can't silently break.

- [ ] **Step 4: Update `values.yaml` networkPolicy block in place**

Targeted line edits (NOT block replacement so the cross-namespace doc comment block above `ingressFrom` is preserved verbatim):

- Change `networkPolicy.enabled: false` to `networkPolicy.enabled: true`.
- Insert `fqdnRestriction:` sub-block right after the `enabled: true` line:

```yaml
networkPolicy:
  # Default ON: ship a vanilla NetworkPolicy that denies all egress
  # except DNS to kube-system kube-dns/coredns and TCP/443 to any
  # destination. NetworkPolicy cannot filter by FQDN; this is the
  # strongest constraint expressible in vanilla Kubernetes. For
  # FQDN-restricted egress (e.g., only api.hyperping.io), see
  # fqdnRestriction below.
  enabled: true
  fqdnRestriction:
    # Set to true on Cilium clusters to render a CiliumNetworkPolicy
    # instead of the vanilla NetworkPolicy. REQUIRES Cilium CNI with
    # cilium.io/v2 CRDs installed; rejected by the API server on other
    # CNIs. Mutually exclusive with the vanilla path.
    enabled: false
    allowedHosts:
      - api.hyperping.io
  # (existing cross-namespace scraping comment block preserved here)
  ingressFrom:
    - podSelector:
        matchLabels:
          app.kubernetes.io/name: prometheus
```

- [ ] **Step 5: Update `values.yaml` podDisruptionBudget block**

Replace the existing block with:
```yaml
podDisruptionBudget:
  # PDB is rendered ONLY when enabled AND replicaCount > 1; today the
  # multi-replica fail() guard means PDB never renders in supported
  # configurations. The template is kept correct in case a future
  # leader-election change permits multi-replica.
  enabled: false
  # maxUnavailable: 1 lets kubectl drain proceed (single-replica drain
  # would otherwise block on maxUnavailable: 0). When PDB is rendered
  # (replicaCount > 1, future use), 1 means "tolerate one disruption".
  maxUnavailable: 1
```

- [ ] **Step 6: Add multi-replica/scaled-to-zero comment to values.yaml**

Above `replicaCount:`:
```yaml
# Single-poller by design: the chart's fail() guards reject
# replicaCount > 1 (N independent pollers would hammer the Hyperping
# API). replicaCount: 0 is supported (scaled-to-zero state) and is
# exempt from the secret-source 'must set one' check, so operators
# can scale to zero before tearing down secrets.
replicaCount: 1
```

- [ ] **Step 7: Add image.tag comment to values.yaml (item 9)**

Change the `tag: ""` line to:
```yaml
  tag: ""  # Defaults to chart appVersion. NOTE: chart version (Chart.yaml: version) and binary version (Chart.yaml: appVersion / image.tag) track separately; bumps to one don't imply bumps to the other.
```

- [ ] **Step 8: Create four fixtures**

`networkpolicy-default.values.yaml`:
```yaml
config:
  apiKey: x
```

`networkpolicy-cilium.values.yaml`:
```yaml
config:
  apiKey: x
networkPolicy:
  fqdnRestriction:
    enabled: true
    allowedHosts:
      - api.hyperping.io
      - mcp.hyperping.io
```

`replicas-zero.values.yaml` (no secret source; scaled-to-zero exempts the must-set-one guard):
```yaml
replicaCount: 0
config:
  apiKey: ""
  existingSecret: ""
externalSecret:
  enabled: false
```

`pdb-enabled.values.yaml` (single replica + enabled PDB; asserts suppression):
```yaml
config:
  apiKey: x
podDisruptionBudget:
  enabled: true
```

- [ ] **Step 9: Update Case 1's `expected_versions` enumeration (R2-10) in render_test.py**

In the same edit window as the production-template changes, replace Case 1's existing `expected_versions` build (currently iterates only `Secret`, `Service`, `Deployment`) with the post-Task-7 enumeration that also includes `NetworkPolicy`:

```python
    expected_versions: dict[str, str] = {}
    for kind in ("Secret", "Service", "Deployment", "NetworkPolicy"):
        for d in find_all(rendered, kind):
            expected_versions[f"{kind}/{d['metadata']['name']}"] = EXPECTED_VERSION
```

This is the ONLY harness change in Task 7. Lock-in of resources/probes/securityContext/helm.sh-chart-label assertions still lands in Task 10.

- [ ] **Step 10: Smoke-test rendering (positive + regression sweep)**

```bash
# (a) replicaCount: 0 fixture: NP rendered (now default-on), PDB absent, replicas: 0.
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/replicas-zero.values.yaml \
  | grep -E '^(kind|  replicas):' | sort -u
# Expected: kind Deployment, NetworkPolicy, Service. NO PodDisruptionBudget. replicas: 0.

# (b) Cilium variant: CiliumNetworkPolicy rendered, vanilla NetworkPolicy NOT.
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-cilium.values.yaml \
  | grep '^kind:' | sort -u
# Expected: Deployment, Secret, Service, CiliumNetworkPolicy. NOT NetworkPolicy.

# (c) Cilium variant ingress conversion: assert the rendered fromEndpoints is
#     an EndpointSelector ({matchLabels: {...}}), NOT a NetworkPolicyPeer
#     ({podSelector: {matchLabels: {...}}}).
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-cilium.values.yaml \
  | python3 -c '
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d and d.get("kind") == "CiliumNetworkPolicy"]
assert len(docs) == 1, f"expected 1 CiliumNetworkPolicy, got {len(docs)}"
cnp = docs[0]
ingress = cnp["spec"].get("ingress", [])
assert ingress, "Cilium policy must have an ingress block for the default ingressFrom value"
peers = ingress[0]["fromEndpoints"]
for p in peers:
    assert "matchLabels" in p, f"Cilium fromEndpoints peer must be flat EndpointSelector, got {p}"
    assert "podSelector" not in p, f"Cilium fromEndpoints must NOT wrap in podSelector, got {p}"
print("PASS Cilium ingress conversion: bare EndpointSelector shape")
'

# (d) R2-5 regression sweep — every pre-existing fixture still renders
#     and the harness still passes with NetworkPolicy now in the default render.
for f in default existing-secret ascii-regex quote-regex readme-regex \
         single-quote-regex both-flags mcp-url mcp-url-query; do
  helm template testrel deploy/helm/hyperping-exporter/ \
    -f deploy/helm/hyperping-exporter/tests/fixtures/${f}.values.yaml >/dev/null \
    || { echo "FAIL fixture ${f} no longer renders"; exit 1; }
  echo "PASS fixture ${f} still renders"
done

# (e) Full harness green — Case 1's NP-enumeration update covers the new resource.
python3 deploy/helm/hyperping-exporter/tests/render_test.py
# Expected: ALL RENDER TESTS PASSED. helm lint clean.
helm lint deploy/helm/hyperping-exporter/
```

- [ ] **Step 11: Commit**

```bash
git add deploy/helm/hyperping-exporter/templates/pdb.yaml \
        deploy/helm/hyperping-exporter/templates/networkpolicy.yaml \
        deploy/helm/hyperping-exporter/templates/networkpolicy-cilium.yaml \
        deploy/helm/hyperping-exporter/values.yaml \
        deploy/helm/hyperping-exporter/tests/render_test.py \
        deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-default.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-cilium.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/replicas-zero.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/pdb-enabled.values.yaml
git commit -m "feat(helm): NetworkPolicy default-on with Cilium variant, drain-safe PDB guard, replicas:0 support

- networkPolicy.enabled flips to true by default; vanilla NetworkPolicy
  denies all egress except DNS to kube-system and TCP/443 to 0.0.0.0/0
  (no RFC1918 except — matches the documented egress contract).
- networkPolicy.fqdnRestriction.enabled: true renders a
  cilium.io/v2 CiliumNetworkPolicy with toFQDNs rules; mutually
  exclusive with the vanilla path. ingressFrom values (vanilla
  NetworkPolicyPeer shape) are converted to Cilium EndpointSelector
  shape at render time; unsupported shapes abort with a clear error.
  Requires Cilium CNI at apply time.
- PDB is rendered only when replicaCount > 1 AND
  podDisruptionBudget.enabled: true. maxUnavailable default stays at 1
  (drain-safe). With the multi-replica fail() guard, PDB never renders
  in supported configurations today; the path stays correct for a
  future leader-election architecture.
- replicaCount: 0 supported (scaled-to-zero) and exempt from the
  must-set-one secret-source guard, so operators can tear down secrets
  cleanly.
- image.tag comment documents chart-vs-binary version independence.

render_test.py Case 1 expected_versions enumeration extended to
include NetworkPolicy so the harness stays green at THIS commit
(default render now emits a NetworkPolicy). Pre-existing fixtures
audited and unaffected (none of them assert on the kinds-list)."
```

---

### Task 8: kubeconform schema validation + kind PSS admission CI (extended job, with sed-rewrite step and stale-cluster cleanup)

**Files:** Modify `.github/workflows/helm-ci.yml`. Create `deploy/helm/hyperping-exporter/tests/kind-pss.yaml`. Create `deploy/helm/hyperping-exporter/tests/kind-pss-config/admission-config.yaml`. Create `deploy/helm/hyperping-exporter/tests/admission_test.sh`. Modify `Makefile`.

The existing job key `helm` is **preserved** so existing branch-protection identifiers continue to work. The job's body grows to run three named steps in sequence (`lint-and-render`, `kubeconform`, `pss-admission`); step failures fail the job.

Substitute the action SHAs and kindest/node digest captured in Task 1 Step 6 below; the plan uses `<KIND_ACTION_SHA>`, `<KIND_ACTION_TAG>`, `<KINDEST_NODE_DIGEST>` as **mandatory substitution placeholders**, not as literal commit-as-is values.

- [ ] **Step 1: Create `tests/kind-pss-config/admission-config.yaml`**

```yaml
apiVersion: apiserver.config.k8s.io/v1
kind: AdmissionConfiguration
plugins:
  - name: PodSecurity
    configuration:
      apiVersion: pod-security.admission.config.k8s.io/v1
      kind: PodSecurityConfiguration
      defaults:
        enforce: privileged
        enforce-version: latest
      exemptions:
        usernames: []
        runtimeClasses: []
        namespaces: [kube-system]
```

Default cluster enforcement stays `privileged`; the test namespace is labelled `restricted` explicitly so the test is scoped.

- [ ] **Step 2: Create `tests/kind-pss.yaml`**

Note: kind resolves relative `hostPath` against its own cwd, not `$GITHUB_WORKSPACE`. `admission_test.sh` (Step 3 below) rewrites the file in place at run time, substituting the absolute path. The committed file uses a placeholder.

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: hyperping-pss
nodes:
  - role: control-plane
    image: kindest/node:v1.34.0@<KINDEST_NODE_DIGEST>
    kubeadmConfigPatches:
      - |
        kind: ClusterConfiguration
        apiVersion: kubeadm.k8s.io/v1beta4
        apiServer:
          extraArgs:
            - name: admission-control-config-file
              value: /etc/kubernetes/policies/admission-config.yaml
        extraVolumes:
          - name: admission-config
            hostPath: /etc/kubernetes/policies
            mountPath: /etc/kubernetes/policies
            readOnly: true
            pathType: DirectoryOrCreate
    extraMounts:
      - hostPath: __ABSPATH_PLACEHOLDER__/deploy/helm/hyperping-exporter/tests/kind-pss-config
        containerPath: /etc/kubernetes/policies
```

(Note: `apiServer.extraArgs` is a list under kubeadm v1beta4. The previous map syntax breaks on K8s 1.34.)

- [ ] **Step 3: Create `tests/admission_test.sh`**

```bash
#!/usr/bin/env bash
# Render the chart and admit it into a PSS-restricted namespace inside
# kind. For the defaults fixture, the pod is booted live so the binary
# is exercised under readOnlyRootFilesystem: true (item 4 runtime proof).
# For ExternalSecret/Cilium variants the run is dry-server (CRDs absent
# in stock kind).
set -euo pipefail

command -v kind >/dev/null 2>&1 || { echo "kind missing; install from https://github.com/kubernetes-sigs/kind/releases" >&2; exit 2; }
command -v kubectl >/dev/null 2>&1 || { echo "kubectl missing" >&2; exit 2; }
command -v helm >/dev/null 2>&1 || { echo "helm missing" >&2; exit 2; }

CHART_DIR="${CHART_DIR:-deploy/helm/hyperping-exporter}"
REPO_ROOT="$(cd "$(dirname "$0")/../../../.." && pwd)"
NS="hyperping-pss-test"

# Rewrite the kind-pss.yaml placeholder with the absolute repo path so
# kind resolves the admission-config hostMount correctly. Done on a copy
# so the committed file stays portable.
KIND_CFG="$(mktemp)"
sed "s#__ABSPATH_PLACEHOLDER__#${REPO_ROOT}#g" \
    "$CHART_DIR/tests/kind-pss.yaml" > "$KIND_CFG"

if ! kind get clusters 2>/dev/null | grep -q '^hyperping-pss$'; then
  kind create cluster --config "$KIND_CFG" --wait 120s
fi

kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl label namespace "$NS" \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/enforce-version=latest \
  --overwrite

# Determine fixtures to exercise. Pass a fixture path as $1 to override.
DEFAULT_FIXTURES=(
  "$CHART_DIR/tests/fixtures/pss-restricted.values.yaml"
  "$CHART_DIR/tests/fixtures/networkpolicy-default.values.yaml"
  "$CHART_DIR/tests/fixtures/replicas-zero.values.yaml"
  "$CHART_DIR/tests/fixtures/external-secret.values.yaml"
)
FIXTURES=("${@:-${DEFAULT_FIXTURES[@]}}")

for f in "${FIXTURES[@]}"; do
  basef=$(basename "$f")
  echo "::group::admission $basef"
  # Strip namespace: from the rendered manifest so kubectl apply -n "$NS"
  # actually targets $NS (manifest-namespace wins over -n otherwise).
  # We pass --namespace at helm template time to get the right Release.Namespace
  # AND strip the resulting metadata.namespace lines so kubectl is free to
  # set its own.
  RENDERED=$(mktemp)
  helm template testrel "$CHART_DIR" --namespace "$NS" -f "$f" \
    | grep -v '^  namespace:' > "$RENDERED"

  case "$basef" in
    external-secret*|networkpolicy-cilium*)
      # Variants that emit CRDs absent from stock kind: server-side dry-run only.
      kubectl apply --server-side --force-conflicts --dry-run=server \
        -n "$NS" --validate=false -f "$RENDERED"
      ;;
    *)
      # Live apply — pod boots, runtime readOnlyRootFilesystem proves itself.
      kubectl apply --server-side --force-conflicts -n "$NS" -f "$RENDERED"
      # Wait for at least one Pod to become Ready (or for the
      # zero-replica case to settle with 0 desired).
      if grep -q '^  replicas: 0' "$RENDERED"; then
        echo "  replicaCount=0 fixture; skipping pod-ready wait."
      else
        kubectl -n "$NS" rollout status deploy/testrel-hyperping-exporter --timeout=120s
      fi
      kubectl -n "$NS" delete -f "$RENDERED" --ignore-not-found --wait=false || true
      ;;
  esac
  rm -f "$RENDERED"
  echo "::endgroup::"
done

echo "PASS admission: PSS-restricted namespace admitted all configurations"
```

Make executable: `chmod +x deploy/helm/hyperping-exporter/tests/admission_test.sh`.

- [ ] **Step 4: Extend `.github/workflows/helm-ci.yml`** (preserving the existing job key `helm`)

(R2-1 fix.) The committed `tests/kind-pss.yaml` contains the literal `__ABSPATH_PLACEHOLDER__` so the file stays portable. `helm/kind-action` consumes the file VERBATIM (it does not run the test script); the placeholder MUST be substituted before kind-action reads it, otherwise kind silently falls back to no hostMount and the PSS test runs against vanilla admission (vacuous pass). The fix is a **discrete workflow step that runs `sed -i` IN PLACE on the file** before the kind-action step. Substitution uses `${GITHUB_WORKSPACE}` (set by GitHub Actions to the absolute repo checkout root). Locally, `admission_test.sh` still does the equivalent on a copy so the committed file stays clean.

(R2-7 fix.) The `helm/kind-action` step does NOT include any `kind delete cluster` teardown. If a prior CI step on the same runner left a stale `hyperping-pss` cluster behind, `kind-action` would silently reuse it (with potentially stale config). A defensive `kind delete cluster --name hyperping-pss --quiet` step before kind-action ensures we always boot a fresh cluster against the freshly-substituted config. Local runs get the same safety via `admission_test.sh`'s `kind get clusters | grep` guard, which we also extend with an optional `make helm-pss-clean` target for explicit teardown.

The SHA/tag substitutions in the YAML below come from Task 1 Step 6's `/tmp/kind-action-sha.txt`, `/tmp/kind-action-tag.txt`, `/tmp/kind-latest-tag.txt`, `/tmp/kindest-node-digest.txt`. The executor MUST replace every `<…>` placeholder with the corresponding captured value before committing; no placeholder survives into the committed workflow file. If any captured value differs from the expectations documented in Task 1 Step 6, the executor surfaces the mismatch via `USER DECISION REQUIRED:` instead of silently substituting.

Replace the entire `jobs:` block:

```yaml
jobs:
  helm:
    name: Helm chart CI
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd  # v6
      - name: Set up Helm
        uses: azure/setup-helm@dda3372f752e03dde6b3237bc9431cdc2f7a02a2  # v5.0.0
        with:
          version: v3.20.2
      - name: Set up Python
        uses: actions/setup-python@a309ff8b426b58ec0e2a45f0f869d46889d02405  # v6.2.0
        with:
          python-version: '3.12'
      - name: Install PyYAML
        run: python -m pip install --no-cache-dir 'pyyaml==6.0.3'
      - name: Helm lint
        run: helm lint deploy/helm/hyperping-exporter/
      - name: Render tests (PyYAML harness)
        run: python3 deploy/helm/hyperping-exporter/tests/render_test.py
      - name: Install kubeconform
        run: |
          curl -sSL https://github.com/yannh/kubeconform/releases/download/v0.7.0/kubeconform-linux-amd64.tar.gz \
            | sudo tar xz -C /usr/local/bin kubeconform
          kubeconform -v
      - name: Schema-validate every passing fixture (kubeconform)
        run: |
          set -e
          CHART=deploy/helm/hyperping-exporter
          # datreeio CRDs-catalog uses lowercase kind in filenames.
          SCHEMAS='https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind | lower}}_{{.ResourceAPIVersion}}.json'
          for f in $CHART/tests/fixtures/*.values.yaml; do
            case "$(basename "$f")" in
              # Fixtures that intentionally fail render; skip kubeconform.
              secret-conflict-*|secret-source-missing.values.yaml|replicas-multi.values.yaml|external-secret-missing-store.values.yaml|cache-ttl-numeric.values.yaml) continue ;;
            esac
            echo "::group::kubeconform $f"
            helm template testrel "$CHART" -f "$f" \
              | kubeconform -strict -summary \
                  -kubernetes-version 1.34.0 \
                  -schema-location default \
                  -schema-location "$SCHEMAS"
            echo "::endgroup::"
          done
      # (R2-1) Rewrite the kind-pss.yaml placeholder with the absolute
      # repo root BEFORE helm/kind-action consumes it. Without this step
      # kind would either reject the literal __ABSPATH_PLACEHOLDER__ path
      # or silently mount nothing (admission-config absent → PSS test
      # runs against vanilla admission → vacuous pass).
      - name: Rewrite kind-pss.yaml hostPath placeholder
        run: |
          set -e
          CFG=deploy/helm/hyperping-exporter/tests/kind-pss.yaml
          sed -i "s#__ABSPATH_PLACEHOLDER__#${GITHUB_WORKSPACE}#g" "$CFG"
          # Verify substitution actually happened — any remaining
          # placeholder must abort the job, not silently fall through.
          if grep -q '__ABSPATH_PLACEHOLDER__' "$CFG"; then
            echo "FATAL: placeholder __ABSPATH_PLACEHOLDER__ still present in $CFG" >&2
            exit 1
          fi
          echo "Substituted hostPath -> ${GITHUB_WORKSPACE} in $CFG"
          grep -E '^\s*hostPath:' "$CFG"
      # (R2-7) Defensive teardown — if a prior step left a stale cluster
      # of the same name on the runner, kind-action would silently reuse
      # it with the prior config. Always start from a clean slate.
      - name: Tear down any stale kind cluster
        run: |
          if kind get clusters 2>/dev/null | grep -q '^hyperping-pss$'; then
            echo "Deleting stale hyperping-pss cluster"
            kind delete cluster --name hyperping-pss
          else
            echo "No stale hyperping-pss cluster present"
          fi
      - name: Set up kind cluster
        uses: helm/kind-action@<KIND_ACTION_SHA>  # <KIND_ACTION_TAG>
        with:
          version: <KIND_LATEST_TAG>
          config: deploy/helm/hyperping-exporter/tests/kind-pss.yaml
          cluster_name: hyperping-pss
          wait: 120s
      - name: Admit chart into PSS-restricted namespace (four fixtures)
        run: bash deploy/helm/hyperping-exporter/tests/admission_test.sh
      # Always-run cleanup (best-effort) so the runner doesn't leak the
      # cluster across job retries on self-hosted runners.
      - name: Tear down kind cluster
        if: always()
        run: kind delete cluster --name hyperping-pss || true
```

**Substitution policy (no placeholders may survive):** every `<KIND_ACTION_SHA>`, `<KIND_ACTION_TAG>`, `<KIND_LATEST_TAG>` literal in the YAML above is replaced with the value captured by Task 1 Step 6 BEFORE the executor stages the file for commit. A `grep '<KIND' .github/workflows/helm-ci.yml && exit 1` post-edit check in Task 8 Step 8 below confirms no placeholder leaked into the committed workflow. Same for the `kindest/node:v1.34.x@<KINDEST_NODE_DIGEST>` substitution in `tests/kind-pss.yaml` (Step 2 of this task).

**Branch-protection note:** the workflow keeps the `helm` job key, so existing branch-protection rules referencing the job-key check name continue to bind. The job's `name:` is `Helm chart CI`; if branch-protection matches by job `name:` rather than by `id:`, the rule needs updating. Confirm before merge via `gh api repos/develeap/hyperping-exporter/branches/main/protection --jq '.required_status_checks.contexts' 2>/dev/null` and reconcile in the PR body (Task 12).

- [ ] **Step 5: Add Makefile targets** (including `helm-pss-clean` for stale-cluster teardown — R2-7)

Append to `Makefile`:

```make
.PHONY: helm-ci helm-render helm-kubeconform helm-pss helm-pss-clean
helm-ci: helm-render helm-kubeconform helm-pss

helm-render:
	helm lint deploy/helm/hyperping-exporter/
	python3 deploy/helm/hyperping-exporter/tests/render_test.py

helm-kubeconform:
	@command -v kubeconform >/dev/null || { echo "kubeconform missing; install from https://github.com/yannh/kubeconform/releases"; exit 2; }
	@for f in deploy/helm/hyperping-exporter/tests/fixtures/*.values.yaml; do \
		case $$(basename $$f) in secret-conflict-*|secret-source-missing.values.yaml|replicas-multi.values.yaml|external-secret-missing-store.values.yaml|cache-ttl-numeric.values.yaml) continue ;; esac ; \
		echo "kubeconform $$f" ; \
		helm template testrel deploy/helm/hyperping-exporter -f $$f \
		  | kubeconform -strict -summary -kubernetes-version 1.34.0 \
		      -schema-location default \
		      -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind | lower}}_{{.ResourceAPIVersion}}.json' \
		  || exit 1 ; \
	done

helm-pss:
	@command -v kind >/dev/null || { echo "kind missing; install from https://github.com/kubernetes-sigs/kind/releases"; exit 2; }
	bash deploy/helm/hyperping-exporter/tests/admission_test.sh

helm-pss-clean:
	@command -v kind >/dev/null || { echo "kind missing; nothing to delete"; exit 0; }
	@if kind get clusters 2>/dev/null | grep -q '^hyperping-pss$$'; then \
		echo "Deleting hyperping-pss cluster"; \
		kind delete cluster --name hyperping-pss; \
	else \
		echo "No hyperping-pss cluster present"; \
	fi
```

- [ ] **Step 6: Create the pss-restricted fixture**

`tests/fixtures/pss-restricted.values.yaml`:
```yaml
# Minimal config used by the PSS admission job AND by render-harness
# Case for explicit defaults-equivalent verification.
config:
  apiKey: x
```

- [ ] **Step 7: Run the local equivalents (if tools available)**

```bash
make helm-render
make helm-kubeconform 2>&1 | tail -20    # may not be available locally
make helm-pss 2>&1 | tail -40            # requires docker + kind locally
```
Expected: `helm-render` clean. `helm-kubeconform` / `helm-pss` either clean or surface tooling-missing message (CI will exercise the real path).

- [ ] **Step 8: Post-edit placeholder audit (mandatory)**

```bash
# Confirm no Task-1-Step-6 placeholders leaked into the committed files.
# The committed kind-pss.yaml MUST retain __ABSPATH_PLACEHOLDER__ (it is
# rewritten at run time); everything else must be a concrete value.
WORKFLOW=.github/workflows/helm-ci.yml
KIND_CFG=deploy/helm/hyperping-exporter/tests/kind-pss.yaml
if grep -E '<KIND_ACTION_(SHA|TAG)>|<KIND_LATEST_TAG>|<KINDEST_NODE_DIGEST>' "$WORKFLOW" "$KIND_CFG"; then
  echo "FATAL: substitution placeholders still present in committed files" >&2
  exit 1
fi
# kind-pss.yaml is allowed (required) to keep __ABSPATH_PLACEHOLDER__.
grep -q '__ABSPATH_PLACEHOLDER__' "$KIND_CFG" || { echo "FATAL: kind-pss.yaml lost its hostPath placeholder" >&2; exit 1; }
echo "Placeholder audit clean."
```

- [ ] **Step 9: Commit**

```bash
git add .github/workflows/helm-ci.yml \
        deploy/helm/hyperping-exporter/tests/kind-pss.yaml \
        deploy/helm/hyperping-exporter/tests/kind-pss-config/admission-config.yaml \
        deploy/helm/hyperping-exporter/tests/admission_test.sh \
        deploy/helm/hyperping-exporter/tests/fixtures/pss-restricted.values.yaml \
        Makefile
git commit -m "ci(helm): add kubeconform schema validation and kind PSS admission to helm CI

Existing 'helm' job extended with new steps (preserves branch
protection): kubeconform validates every passing fixture against
k8s 1.34 + datreeio CRDs (lowercase kind filenames), and a kind
cluster boots the rendered chart into a PSS-restricted namespace.

The admission test exercises four fixtures (defaults, networkpolicy,
replicas-zero, externalSecret). The defaults case applies LIVE so the
pod actually starts under readOnlyRootFilesystem: true; rollout-status
wait proves the binary tolerates the read-only root at runtime.

A discrete workflow step rewrites the kind-pss.yaml __ABSPATH_PLACEHOLDER__
to \$GITHUB_WORKSPACE BEFORE helm/kind-action consumes the file, so
the admission-config hostMount is always honoured. A defensive
'kind delete cluster' step runs before kind-action to drop any stale
cluster from prior steps on the same runner.

kind config uses kubeadm v1beta4 (required by k8s 1.34) and pins
kindest/node by digest. Action SHAs and the kindest/node digest are
captured from upstream at execution time (Task 1 Step 6) with no
||-fallback; a tag mismatch surfaces to the user via USER DECISION
REQUIRED rather than silently substituting stale values."
```

---

### Task 9: Chart.yaml bump, EXPECTED constants bump, and CHANGELOG entry (single commit — R2-3)

**Files:** Modify `deploy/helm/hyperping-exporter/Chart.yaml`, `deploy/helm/hyperping-exporter/tests/render_test.py`, `CHANGELOG.md`.

(Renumbered from the prior plan's Task 10 (CHANGELOG-only commit). Combines the chart-version/appVersion bumps with the test-expectation bumps in render_test.py so Case 1's `deployment_image` assertion is never red between commits — see Strategy Gate item 7.)

- [ ] **Step 1: Bump chart and appVersion**

In `deploy/helm/hyperping-exporter/Chart.yaml`:
- Change `version: 1.1.0` to `version: 1.5.0`.
- Change `appVersion: "1.4.0"` to `appVersion: "1.4.1"`.

- [ ] **Step 1b: Bump EXPECTED constants in render_test.py (R2-3 fix — same commit)**

In `deploy/helm/hyperping-exporter/tests/render_test.py`, lines 160-161:

```python
EXPECTED_IMAGE_DEFAULT = "khaledsalhabdeveleap/hyperping-exporter:1.4.1"
EXPECTED_VERSION = "1.4.1"
```

(Previously `1.4.0` for both.) The harness's Case 1 `assert_eq(deployment_image(rendered), EXPECTED_IMAGE_DEFAULT, ...)` would otherwise be red against the Chart.yaml bump for one commit until Task 10 fixed it. Combining both edits in this commit eliminates the bisect-red window.

Run the harness to confirm: `python3 deploy/helm/hyperping-exporter/tests/render_test.py` → ALL RENDER TESTS PASSED.

- [ ] **Step 2: Write CHANGELOG section**

Insert immediately after `## [Unreleased]`, before `## [1.4.1] - 2026-05-12`. **Strict keep-a-changelog header** (no parenthetical suffix; the chart-only nature moves into a Highlights bullet):

```markdown
## [1.5.0] - 2026-05-12

### Highlights

- Chart-only release; binary unchanged (chart now tracks the just-released v1.4.1 image).
- Production-readiness defaults: tuned resources from a 30-minute containerised live API observation (uid 65534, read-only root), reconciled probe defaults to the peer-review contract (10/10/3 and 5/5/3), PSS-restricted securityContext locked-in by render tests AND proven at runtime via a kind admission CI job that boots the pod.
- ExternalSecret support via `external-secrets.io/v1` (default; `v1beta1` available via `externalSecret.apiVersion` override for legacy ESO ≤ 0.15). Mutually exclusive with inline `apiKey` and `existingSecret`.
- NetworkPolicy on by default; vanilla NP permits DNS to kube-system and TCP/443 to any destination (no RFC1918 except — matches the documented egress contract). Optional `cilium.io/v2 CiliumNetworkPolicy` variant for FQDN-restricted egress.
- Four render-time `fail()` guards prevent silent misconfiguration: multiple secret sources, missing secret sources (when `replicaCount ≥ 1`), `replicaCount > 1`, and bare-integer `cacheTTL` (e.g. `cacheTTL: 60` instead of `"60s"`). `replicaCount: 0` is supported and exempt from the secret-source check (scaled-to-zero state); the Deployment's API-key env block is gated on a configured secret source so a `0 → 1` scale-up without a source re-trips the secret-source guard.
- New CI gates on chart-touching PRs: `kubeconform` schema validation against k8s 1.34 + datreeio CRDs catalog, and a `kind` cluster with PSS-restricted enforced on a test namespace that exercises four representative fixtures (one boots the pod live).

### Added

- `externalSecret` values block + `templates/externalsecret.yaml`. `apiVersion` (default `external-secrets.io/v1`), `secretStoreRef`, `remoteRef.key`, `remoteRef.property`, `refreshInterval`. Required fields enforced via Helm's `required` helper.
- `networkPolicy.fqdnRestriction.enabled` + `allowedHosts` + `templates/networkpolicy-cilium.yaml` for FQDN-restricted egress on Cilium clusters. NP-shape `ingressFrom` peers are converted at render time to Cilium's flat `EndpointSelector` shape; unsupported shapes abort the render.
- Multi-replica `fail()` guard, secret-source mutual-exclusion `fail()` guard, and `validateCacheTTL` (rejects bare-integer cacheTTL) in `_helpers.tpl`.
- Deployment `env` block gating: the `secretKeyRef` reference to the API-key Secret is suppressed when no secret source is configured (only reachable for `replicaCount: 0`), so a future scale-up to `replicaCount: 1` without first configuring a source re-trips `validateSecretSources` rather than silently CrashLoopBackOff-ing.
- 11+ new render-harness cases covering ExternalSecret (three fixtures), NetworkPolicy default-on, Cilium variant, replicas-zero, PDB safety, the conflict/missing `fail()` paths, and an explicit `helm.sh/chart` label assertion.
- `kubeconform` step in Helm CI, validating every passing fixture against k8s 1.34 schemas + datreeio CRDs catalog (lowercase kind filename convention).
- `pss-admission` step in Helm CI, applying four fixtures into a PSS-restricted namespace inside `kind`. The defaults fixture applies live (rollout-status wait) so the binary is proven to tolerate `readOnlyRootFilesystem: true` at runtime.
- `make helm-ci` (and `helm-render`, `helm-kubeconform`, `helm-pss`) for local parity with CI.
- `image.tag` comment noting chart-vs-binary version independence.

### Changed

- `Chart.yaml` `version` `1.1.0` → `1.5.0`. `appVersion` `"1.4.0"` → `"1.4.1"` to track the latest released binary. Side-effect: `app.kubernetes.io/version` label flips `"1.4.0"` → `"1.4.1"` on every resource; `helm.sh/chart` flips `hyperping-exporter-1.1.0` → `hyperping-exporter-1.5.0`.
- `resources` defaults tuned from empirical observation (see commit message for measured peak RSS and p99 CPU).
- `livenessProbe` defaults: `initialDelaySeconds 10`, `periodSeconds 10`, `failureThreshold 3`.
- `readinessProbe` defaults: `initialDelaySeconds 5`, `periodSeconds 5`, `failureThreshold 3`.
- `networkPolicy.enabled` default `false` → `true`. Egress 443 rule no longer excludes RFC1918 (matches the documented contract).
- `podDisruptionBudget.maxUnavailable` default kept at `1` (drain-safe). PDB is now suppressed when `replicaCount ≤ 1` regardless of `enabled` (a PDB selecting a single replica blocks node drains).
- `--cache-ttl` rendered via `printf "%v" + toString + toJson` for consistency with the other optional flags AND for type-safe coercion (bare ints no longer become Go fmt's `%!s(int=N)` poison). `values.yaml` documents the quoted-string requirement, and `validateCacheTTL` aborts the render with a clear error if a bare integer slips through.

### Upgrade notes

- **Image version flips**: `app.kubernetes.io/version` label moves `"1.4.0"` → `"1.4.1"` on every resource, and the default `image.tag` resolves to `1.4.1`. Pin `image.tag` explicitly if you must stay on the older binary.
- **Chart label churn**: `helm.sh/chart` label flips `hyperping-exporter-1.1.0` → `hyperping-exporter-1.5.0`; diffs in your GitOps tool are expected.
- **NetworkPolicy now enabled by default**: clusters that previously relied on the chart NOT applying a NetworkPolicy must explicitly set `networkPolicy.enabled: false` in their values. Audit `ingressFrom` if Prometheus lives in a different namespace.
- **`replicaCount > 1` aborts render**: installs that previously set `replicaCount: 2+` were silently misconfigured (N independent pollers hammering Hyperping). Set `replicaCount: 1` and accept the single-poller architecture. `replicaCount: 0` is supported.
- **Multiple/missing secret sources abort render** (except when `replicaCount: 0`): pick exactly one of `apiKey` / `existingSecret` / `externalSecret.enabled`.
- **Bare-integer `cacheTTL` aborts render**: use a quoted Go duration string like `"60s"` or `"5m"`. The chart no longer silently passes bare integers through to the binary.
- **ExternalSecret apiVersion default is v1**: legacy ESO installations (≤ v0.15) must set `externalSecret.apiVersion: external-secrets.io/v1beta1`.
- **PSS-restricted is the assumed namespace policy**: the chart does NOT label namespaces. Operators must label the target namespace `pod-security.kubernetes.io/enforce=restricted` themselves (or use a higher-level admission controller). The chart's securityContext defaults already satisfy the profile; overrides that weaken it will be rejected at apply time on PSS-restricted namespaces.
- **Helm-to-ESO upgrade caveat**: switching an existing release from `config.apiKey` (or `existingSecret`) to `externalSecret.enabled: true` deletes the chart-managed Secret on upgrade; ESO must reconcile the new Secret quickly enough that the Deployment doesn't restart with an unresolvable env reference. Plan the cutover during a maintenance window or stage via `helm upgrade --post-renderer` to apply the ExternalSecret first.
```

- [ ] **Step 3: Commit (single commit covers Chart bump + harness expectation bump + CHANGELOG)**

```bash
git add deploy/helm/hyperping-exporter/Chart.yaml \
        deploy/helm/hyperping-exporter/tests/render_test.py \
        CHANGELOG.md
git commit -m "chore(helm): bump chart to 1.5.0, appVersion to 1.4.1, harness expectations, CHANGELOG

Chart 1.1.0 -> 1.5.0; appVersion 1.4.0 -> 1.4.1 to track the latest
released binary. render_test.py EXPECTED_IMAGE_DEFAULT and
EXPECTED_VERSION bumped to 1.4.1 in the same commit so the Case 1
deployment_image assertion never goes red mid-bisect.

CHANGELOG section sits between [Unreleased] and [1.4.1], preserving
strictly descending order. Highlights cover the production-readiness
defaults, ExternalSecret support, fail() guards, and the new CI
gates. Upgrade notes flag the appVersion / chart-label flip, the NP
default change, multi-replica rejection, secret-source constraints,
ESO apiVersion default, the PSS labelling responsibility, and the
ESO cutover caveat."
```

---

### Task 10: Extend render harness with all net-new assertions and run the full suite

**Files:** Modify `deploy/helm/hyperping-exporter/tests/render_test.py`. Create `deploy/helm/hyperping-exporter/tests/fixtures/cache-ttl-numeric.values.yaml`. No production-template changes.

(Renumbered from the prior plan's Task 11 (assertions-only commit). This task contains ONLY the net-new assertions that don't invalidate any prior-commit state. The bumps that previously lived in "Step 1" and "Step 3" of this task have moved to Tasks 9 (`EXPECTED_VERSION` / `EXPECTED_IMAGE_DEFAULT`) and 7 (Case 1 `expected_versions` enumeration for NetworkPolicy) respectively, eliminating the bisect-red windows R2-3 and R2-10 identified.)

This task lands the assertions for resources/probes/securityContexts (measured values from Task 2 are now fully settled), the `helm.sh/chart` label lock-in, the six `fail()`-path cases, the ExternalSecret cases, the NP and Cilium cases, the replicas-zero case, the PDB case, and the cacheTTL numeric round-trip — all on top of a chart state that is already complete.

- [ ] **Step 1: Add `EXPECTED_CHART_LABEL` constant only** (EXPECTED_VERSION/EXPECTED_IMAGE_DEFAULT already at 1.4.1 from Task 9)

After the existing `EXPECTED_VERSION = "1.4.1"` line in `render_test.py`, add:
```python
EXPECTED_CHART_LABEL = "hyperping-exporter-1.5.0"
```

- [ ] **Step 2: Extend Case 1 (defaults) with locked-in assertions**

After the existing `assert_eq(versions, expected_versions, ...)` line, append (substituting the measured values from Task 2 Step 5; the file `/tmp/hpe-obs/computed-defaults.yaml` has them):

```python
    # Defaults-case extensions: resources, probes, securityContexts,
    # helm.sh/chart label. All literal so any future drift surfaces in CI.
    EXPECTED_RESOURCES = {
        "requests": {"cpu": "<CPU_REQ>",  "memory": "<MEM_REQ>"},
        "limits":   {"cpu": "<CPU_LIM>",  "memory": "<MEM_LIM>"},
    }
    EXPECTED_LIVENESS = {
        "httpGet": {"path": "/healthz", "port": "http"},
        "initialDelaySeconds": 10,
        "periodSeconds": 10,
        "failureThreshold": 3,
    }
    EXPECTED_READINESS = {
        "httpGet": {"path": "/readyz", "port": "http"},
        "initialDelaySeconds": 5,
        "periodSeconds": 5,
        "failureThreshold": 3,
    }
    EXPECTED_CONTAINER_SC = {
        "readOnlyRootFilesystem": True,
        "allowPrivilegeEscalation": False,
        "runAsNonRoot": True,
        "runAsUser": 65534,
        "runAsGroup": 65534,
        "capabilities": {"drop": ["ALL"]},
        "seccompProfile": {"type": "RuntimeDefault"},
    }
    EXPECTED_POD_SC = {
        "runAsNonRoot": True,
        "runAsUser": 65534,
        "runAsGroup": 65534,
        "fsGroup": 65534,
        "seccompProfile": {"type": "RuntimeDefault"},
    }
    deployment = find_deployment(rendered)
    container = deployment["spec"]["template"]["spec"]["containers"][0]
    pod = deployment["spec"]["template"]["spec"]
    assert_eq(container["resources"], EXPECTED_RESOURCES,
              "defaults: resources match empirical envelope")
    assert_eq(container["livenessProbe"], EXPECTED_LIVENESS,
              "defaults: livenessProbe matches user contract (10/10/3)")
    assert_eq(container["readinessProbe"], EXPECTED_READINESS,
              "defaults: readinessProbe matches user contract (5/5/3)")
    assert_eq(container["securityContext"], EXPECTED_CONTAINER_SC,
              "defaults: container securityContext is PSS-restricted compliant")
    assert_eq(pod["securityContext"], EXPECTED_POD_SC,
              "defaults: pod securityContext is PSS-restricted compliant")
    # helm.sh/chart label flips with each chart-version bump; lock it.
    chart_labels = {
        f"{d.get('kind')}/{d['metadata']['name']}": d['metadata']['labels']['helm.sh/chart']
        for d in docs(rendered)
        if (d.get('metadata') or {}).get('labels', {}).get('helm.sh/chart')
    }
    expected_chart_labels = {k: EXPECTED_CHART_LABEL for k in chart_labels}
    assert_eq(chart_labels, expected_chart_labels,
              "defaults: helm.sh/chart label is hyperping-exporter-1.5.0 on every labelled resource")
```

- [ ] **Step 3: (intentionally empty — Case 1 `expected_versions` enumeration already landed in Task 7)**

Skipped: the `expected_versions` enumeration was extended to include `NetworkPolicy` in Task 7's commit alongside the `networkPolicy.enabled` default flip, eliminating the bisect-red window R2-10 identified. No edit needed here.

- [ ] **Step 4: Add the `assert_fail` helper**

After the existing `assert_eq` helper:

```python
def assert_fail(fixture: str, expected_substring: str, label: str) -> None:
    """Render with the given fixture and assert helm exits non-zero
    AND the stderr contains the expected substring."""
    try:
        completed = subprocess.run(
            [HELM, "template", "testrel", str(CHART), "-f", str(FIXTURES / fixture)],
            check=False, capture_output=True, text=True,
        )
    except FileNotFoundError as exc:
        print(f"FAIL {label}: helm not on PATH: {exc}", file=sys.stderr)
        sys.exit(1)
    if completed.returncode == 0:
        print(f"FAIL {label}: helm template exited 0 but should have failed", file=sys.stderr)
        print(f"  stdout (head): {completed.stdout[:400]!r}", file=sys.stderr)
        sys.exit(1)
    if expected_substring not in completed.stderr:
        print(f"FAIL {label}: expected substring not in stderr", file=sys.stderr)
        print(f"  expected substring: {expected_substring!r}", file=sys.stderr)
        print(f"  actual stderr:      {completed.stderr!r}", file=sys.stderr)
        sys.exit(1)
    print(f"PASS {label}: helm template fails as expected")
```

`assert_fail` substring search is intentionally loose (substring, not literal); Helm v3.20.x patch-level changes to fail() framing won't break it.

- [ ] **Step 5: Add kind-presence helpers**

```python
def find_external_secret(rendered: str) -> dict | None:
    for d in docs(rendered):
        if d.get("kind") == "ExternalSecret":
            return d
    return None


def find_pdb(rendered: str) -> dict | None:
    for d in docs(rendered):
        if d.get("kind") == "PodDisruptionBudget":
            return d
    return None


def find_networkpolicy(rendered: str) -> dict | None:
    for d in docs(rendered):
        if d.get("kind") == "NetworkPolicy":
            return d
    return None


def find_cilium_networkpolicy(rendered: str) -> dict | None:
    for d in docs(rendered):
        if d.get("kind") == "CiliumNetworkPolicy":
            return d
    return None


def kinds_in(rendered: str) -> set[str]:
    return {d.get("kind") for d in docs(rendered) if d.get("kind")}
```

- [ ] **Step 6: Append all new cases after Case 9**

```python
    # Case 10–14 — secret-source / replica fail() paths.
    assert_fail("secret-conflict-apikey-and-existing.values.yaml",
                "Got multiple",
                "secret-conflict apikey+existingSecret aborts render")
    assert_fail("secret-conflict-apikey-and-external.values.yaml",
                "Got multiple",
                "secret-conflict apikey+externalSecret aborts render")
    assert_fail("secret-conflict-existing-and-external.values.yaml",
                "Got multiple",
                "secret-conflict existing+externalSecret aborts render")
    assert_fail("secret-source-missing.values.yaml",
                "must set one of",
                "secret-source missing aborts render")
    assert_fail("replicas-multi.values.yaml",
                "replicaCount=2 is unsupported",
                "replicaCount > 1 aborts render")
    # cacheTTL bare-int aborts render (R2-4/R2-9 fix — validateCacheTTL
    # helper fails loudly when kindOf cacheTTL != "string").
    assert_fail("cache-ttl-numeric.values.yaml",
                "cacheTTL must be a quoted Go duration string",
                "cacheTTL bare integer aborts render")

    # Case 15 — ExternalSecret rendered, plain Secret absent.
    rendered = helm_template("external-secret.values.yaml")
    es = find_external_secret(rendered)
    assert es is not None, "FAIL external-secret: ExternalSecret should be rendered"
    print("PASS external-secret: ExternalSecret rendered")
    assert find_secret(rendered) is None, \
        "FAIL external-secret: chart-managed Secret must NOT be present"
    print("PASS external-secret: chart-managed Secret absent (ES path consumed)")
    assert_eq(es["apiVersion"], "external-secrets.io/v1",
              "external-secret: v1 ExternalSecret rendered by default")
    assert_eq(es["spec"]["refreshInterval"], "30m",
              "external-secret: refreshInterval propagates from values")
    assert_eq(es["spec"]["target"]["name"], "testrel-hyperping-exporter",
              "external-secret: target Secret name matches Deployment env ref")
    assert_eq(es["spec"]["data"][0]["secretKey"], "api-key",
              "external-secret: writes into the api-key field the Deployment reads")
    assert_eq(es["spec"]["secretStoreRef"]["name"], "vault-store",
              "external-secret: secretStoreRef.name propagates")
    assert_eq(es["spec"]["data"][0]["remoteRef"]["key"], "hyperping/api-key",
              "external-secret: remoteRef.key propagates")
    env = find_deployment(rendered)["spec"]["template"]["spec"]["containers"][0]["env"]
    secret_ref = env[0]["valueFrom"]["secretKeyRef"]["name"]
    assert_eq(secret_ref, "testrel-hyperping-exporter",
              "external-secret: container env references the ESO-target name")
    assert_scalars_clean(rendered, "external-secret")

    # Case 16 — ExternalSecret default refreshInterval propagates.
    rendered = helm_template("external-secret-defaults.values.yaml")
    es = find_external_secret(rendered)
    assert_eq(es["spec"]["refreshInterval"], "1h",
              "external-secret-defaults: refreshInterval falls through to 1h default")

    # Case 17 — missing secretStoreRef.name fires the required() error.
    assert_fail("external-secret-missing-store.values.yaml",
                "externalSecret.secretStoreRef.name is required",
                "external-secret-missing-store: required() fires for missing store")

    # Case 18 — vanilla NetworkPolicy is rendered when networkPolicy.enabled
    # is true (now the default) and fqdnRestriction is off. Egress allows
    # DNS to kube-system plus TCP/443 to 0.0.0.0/0 with NO RFC1918 except.
    rendered = helm_template("networkpolicy-default.values.yaml")
    np = find_networkpolicy(rendered)
    assert np is not None, "FAIL np-default: vanilla NetworkPolicy must render"
    print("PASS np-default: vanilla NetworkPolicy rendered")
    assert find_cilium_networkpolicy(rendered) is None, \
        "FAIL np-default: CiliumNetworkPolicy must NOT render in vanilla path"
    print("PASS np-default: CiliumNetworkPolicy absent")
    # Egress: DNS (UDP+TCP 53) and TCP/443.
    egress_ports = sorted(
        port["port"] for rule in np["spec"]["egress"]
        for port in rule.get("ports", [])
    )
    assert_eq(egress_ports, [53, 53, 443],
              "np-default: egress allows DNS (UDP+TCP/53) and TCP/443 only")
    # Locate the 443 egress rule and assert its ipBlock cidr is 0.0.0.0/0
    # with NO except (user contract).
    egress_443 = [rule for rule in np["spec"]["egress"]
                  if any(p["port"] == 443 for p in rule.get("ports", []))][0]
    ipb = egress_443["to"][0]["ipBlock"]
    assert_eq(ipb.get("cidr"), "0.0.0.0/0",
              "np-default: egress 443 targets 0.0.0.0/0")
    assert "except" not in ipb, \
        "FAIL np-default: egress 443 ipBlock must NOT have an except list"
    print("PASS np-default: egress 443 has no RFC1918 except (matches contract)")

    # Case 19 — Cilium variant. CiliumNetworkPolicy rendered; vanilla absent.
    rendered = helm_template("networkpolicy-cilium.values.yaml")
    cnp = find_cilium_networkpolicy(rendered)
    assert cnp is not None, "FAIL np-cilium: CiliumNetworkPolicy must render"
    print("PASS np-cilium: CiliumNetworkPolicy rendered")
    assert find_networkpolicy(rendered) is None, \
        "FAIL np-cilium: vanilla NetworkPolicy must NOT render alongside Cilium"
    print("PASS np-cilium: vanilla NetworkPolicy absent (mutually exclusive)")
    assert_eq(cnp["apiVersion"], "cilium.io/v2",
              "np-cilium: cilium.io/v2 apiVersion")
    fqdns = sorted(r["matchName"] for rule in cnp["spec"]["egress"]
                   for r in rule.get("toFQDNs", []))
    assert_eq(fqdns, ["api.hyperping.io", "mcp.hyperping.io"],
              "np-cilium: toFQDNs lists both allowedHosts")
    # R2-2 fix — Cilium ingress fromEndpoints must be flat EndpointSelector
    # ({matchLabels: {...}}), NOT vanilla NetworkPolicyPeer wrapped in
    # podSelector / namespaceSelector. The chart converts NP-shape to
    # Cilium-shape at render time.
    ingress = cnp["spec"].get("ingress", [])
    assert ingress, "FAIL np-cilium: ingress block missing (default ingressFrom has 1 peer)"
    peers = ingress[0]["fromEndpoints"]
    assert peers, "FAIL np-cilium: ingress.fromEndpoints empty"
    for i, p in enumerate(peers):
        assert "matchLabels" in p, \
            f"FAIL np-cilium: fromEndpoints[{i}] missing matchLabels: {p!r}"
        assert "podSelector" not in p, \
            f"FAIL np-cilium: fromEndpoints[{i}] must be flat EndpointSelector, not NetworkPolicyPeer: {p!r}"
        assert "namespaceSelector" not in p, \
            f"FAIL np-cilium: fromEndpoints[{i}] must be flat EndpointSelector, not NetworkPolicyPeer: {p!r}"
    # The default networkPolicy.ingressFrom is
    #   [{podSelector: {matchLabels: {app.kubernetes.io/name: prometheus}}}]
    # which converts to
    #   [{matchLabels: {app.kubernetes.io/name: prometheus}}].
    assert_eq(peers[0]["matchLabels"], {"app.kubernetes.io/name": "prometheus"},
              "np-cilium: default ingressFrom converted to flat EndpointSelector")

    # Case 20 — replicaCount: 0 renders Deployment with replicas:0,
    # suppresses PDB even when enabled, and does NOT fail on missing
    # secret-source (scaled-to-zero is exempt).
    rendered = helm_template("replicas-zero.values.yaml")
    deployment = find_deployment(rendered)
    assert_eq(deployment["spec"]["replicas"], 0,
              "replicas-zero: Deployment has replicas: 0")
    assert find_pdb(rendered) is None, \
        "FAIL replicas-zero: PDB must NOT render for replicaCount=0"
    print("PASS replicas-zero: PDB suppressed for zero-replica state")
    # No Secret expected (no apiKey set) but Deployment/Service/NetworkPolicy render.
    expected_kinds_zero = {"Deployment", "Service", "NetworkPolicy"}
    assert expected_kinds_zero.issubset(kinds_in(rendered)), \
        f"FAIL replicas-zero: missing kinds, got {kinds_in(rendered)}"
    print("PASS replicas-zero: Deployment/Service/NetworkPolicy present (no Secret needed)")

    # Case 21 — PDB enabled at replicaCount=1 (default) is suppressed.
    rendered = helm_template("pdb-enabled.values.yaml")
    assert find_pdb(rendered) is None, \
        "FAIL pdb-enabled: PDB must NOT render with replicaCount=1"
    print("PASS pdb-enabled: PDB suppressed at replicaCount=1 (drain safety)")

    # (Cache-ttl bare-int is asserted as a FAIL case above, alongside the
    # other validate*** guards. The chart's validateCacheTTL helper aborts
    # the render before the template ever emits a bad arg, so there's no
    # "render-and-document" case for the bare-int form.)

    # Case 22 — cacheTTL value rendered via toJson with toString coercion
    # for a quoted-string fixture that overrides the default. Confirms the
    # printf "%v" + toString path produces a clean string arg byte-for-byte.
    rendered = helm_template("cache-ttl-string-override.values.yaml")
    args = deployment_args(rendered)
    cache_arg = [a for a in args if a.startswith("--cache-ttl=")][0]
    assert_eq(cache_arg, "--cache-ttl=5m",
              "cache-ttl override: chart emits --cache-ttl=<duration string> verbatim")
```

Add two fixtures:

`cache-ttl-numeric.values.yaml` (drives the fail-case above):
```yaml
config:
  apiKey: x
  cacheTTL: 60
```

`cache-ttl-string-override.values.yaml` (drives the round-trip-string positive case):
```yaml
config:
  apiKey: x
  cacheTTL: "5m"
```

- [ ] **Step 7: Run the harness**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
```
Expected: every PASS, ending with `ALL RENDER TESTS PASSED`. Print final line count and case-count via `grep -c ^PASS` output of a captured run.

- [ ] **Step 8: Run all verification stages**

```bash
# Helm lint
helm lint deploy/helm/hyperping-exporter/
# kubeconform locally if tooling present
which kubeconform >/dev/null && make helm-kubeconform || echo "kubeconform not installed locally; CI exercises it"
# kind PSS admission locally if tooling present (slow; optional)
which kind >/dev/null && make helm-pss || echo "kind not installed locally; CI exercises it"
# Go regression smoke (Go code unchanged but verify nothing crept in)
go test ./... -race -count=1 | tee /tmp/go-final.txt | tail -5
diff <(grep -c '^=== RUN' /tmp/go-baseline.txt) <(grep -c '^=== RUN' /tmp/go-final.txt) && \
  echo "Go test count unchanged from baseline" || \
  echo "WARN Go test count changed from baseline; investigate"
```
Expected: all green. The diff comparison uses the dynamic baseline captured in Task 1 Step 3, not a hard-coded number.

- [ ] **Step 9: Confirm git status and branch state**

```bash
git -C /home/khaledsa/projects/develeap/hyperping-exporter/.worktrees/chart-hardening-prod-defaults status --short
git -C /home/khaledsa/projects/develeap/hyperping-exporter/.worktrees/chart-hardening-prod-defaults log --oneline main..HEAD
git -C /home/khaledsa/projects/develeap/hyperping-exporter/.worktrees/chart-hardening-prod-defaults diff --stat main..HEAD
```
Expected: clean working tree; 9 commits on the branch (Tasks 2/3/4/5/6/7/8/9 plus this one), files-changed list covers the chart, the CI workflow, the Makefile, the CHANGELOG, the docs/plans entry.

- [ ] **Step 10: Commit the harness extension**

```bash
git add deploy/helm/hyperping-exporter/tests/render_test.py \
        deploy/helm/hyperping-exporter/tests/fixtures/cache-ttl-numeric.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/cache-ttl-string-override.values.yaml
git commit -m "test(helm): lock all production-readiness defaults via extended render harness

Net-new assertions only; no production code changes. EXPECTED constants
(EXPECTED_VERSION, EXPECTED_IMAGE_DEFAULT) and Case 1 expected_versions
enumeration already settled in Tasks 7 and 9 so this commit never
introduces a bisect-red window.

Adds EXPECTED_CHART_LABEL=hyperping-exporter-1.5.0 and asserts the
helm.sh/chart label on every labelled resource. 13+ new cases cover:
- resources / livenessProbe / readinessProbe / container & pod
  securityContext literal lock-in on the defaults render.
- six fail() paths (three secret-conflicts, secret-source-missing,
  replicas-multi, cacheTTL bare-int).
- ExternalSecret rendered with the v1 apiVersion + propagated
  refreshInterval; the plain Secret suppressed; ESO target name
  matches the Deployment env reference. external-secret-defaults
  fixture proves the 1h refreshInterval default propagates.
  external-secret-missing-store fires the required() error.
- vanilla NetworkPolicy rendered with egress 443 to 0.0.0.0/0 (no
  RFC1918 except), DNS to kube-system on UDP+TCP/53.
- Cilium variant rendered with toFQDNs allowedHosts and a flat
  EndpointSelector ingress (NP-shape -> EndpointSelector conversion
  verified literally so the misconfiguration can't slip back in).
- replicas-zero renders Deployment with replicas: 0, suppresses PDB,
  exempts from must-set-one secret-source guard.
- pdb-enabled at replicaCount=1 suppresses PDB.
- cacheTTL override fixture renders --cache-ttl=5m verbatim via the
  printf %v + toString + toJson path."
```

---

### Task 10: Open the PR

**Files:** none (orchestration only).

- [ ] **Step 1: Push the branch**

```bash
git -C /home/khaledsa/projects/develeap/hyperping-exporter/.worktrees/chart-hardening-prod-defaults push -u origin chore/chart-hardening-prod-defaults
```

- [ ] **Step 2: Open PR via `gh`**

```bash
gh pr create \
  --title "feat(helm): chart 1.5.0 — production-readiness defaults and CI hardening" \
  --body "$(cat <<'EOF'
## Summary

Chart-only release bumping `version` to 1.5.0 and tracking the just-released binary v1.4.1 via `appVersion`. Implements the ten production-readiness items from the v1.4.1 peer review.

### What changed and why

- **Resources defaults tuned from a 30-minute containerised observation** against the live Hyperping API (Docker `--read-only --user 65534:65534`, matching chart runtime). Effect: requests/limits reflect actual binary use under restricted constraints; render harness asserts the literal envelope.
- **Probes reconciled** to the peer-review contract (10/10/3 liveness, 5/5/3 readiness). Effect: pods reach Ready faster and unhealthy pods are pulled from Service endpoints sooner.
- **`--cache-ttl` renders via `printf "%v" | toString | toJson`** with a `validateCacheTTL` helper that aborts the render when the operator supplies a bare integer (e.g. `cacheTTL: 60`). Effect: every optional-flag arg in the chart now goes through one canonical path AND a misconfigured cacheTTL fails loudly at render time instead of producing a runtime CrashLoopBackOff or Go's `%!s(int=N)` poison value.
- **PSS-restricted securityContext locked-in by render tests AND admitted live in CI**. The kind admission job applies the defaults fixture LIVE so the pod boots under `readOnlyRootFilesystem: true`; this is the runtime proof item 4 demanded.
- **NetworkPolicy on by default**. Vanilla NP permits DNS to kube-system and TCP/443 to 0.0.0.0/0 (no RFC1918 except — matches the documented egress contract). Optional `cilium.io/v2 CiliumNetworkPolicy` variant for FQDN-restricted egress. The Cilium template converts the chart's vanilla-NP-shaped `ingressFrom` values (`{podSelector: {matchLabels: …}}`) to Cilium's flat `EndpointSelector` shape at render time; unsupported shapes abort with a clear error rather than producing a CRD-schema-invalid resource.
- **ExternalSecret support** via `external-secrets.io/v1` (default) with `externalSecret.apiVersion` override for legacy ESO ≤ 0.15. Mutually exclusive with `apiKey` / `existingSecret`.
- **Four render-time `fail()` guards**: multi-source secret config, missing secret source at replicaCount≥1, `replicaCount > 1`, and bare-integer `cacheTTL`. `replicaCount: 0` is supported and secret-source-exempt; the Deployment's `env` block referencing the API-key Secret is gated on `secretSourceCount > 0` so a `0 → 1` scale-up without configuring a source re-trips the secret-source guard cleanly.
- **PDB guard**: PDB suppressed when `replicaCount ≤ 1` regardless of `enabled`. `maxUnavailable` default kept at `1` (drain-safe).
- **`image.tag` comment** noting chart-vs-binary version independence.

### Version policy

- Chart `version`: 1.1.0 → **1.5.0** (skips past the binary's 1.2.x slot to keep the shared CHANGELOG monotonic; the binary's `[1.2.0] - 2026-04-25` entry is untouched).
- Chart `appVersion`: "1.4.0" → **"1.4.1"** (tracks the just-released binary v1.4.1).

### CI / verification

The existing `helm` job is extended with three new steps inside the same job (no branch-protection migration required):

- `helm lint` + PyYAML render harness (22+ cases including six new `fail()` paths and an explicit `helm.sh/chart` label assertion).
- `kubeconform`: every passing fixture validated against k8s 1.34 schemas + datreeio CRDs catalog (lowercase kind filename convention).
- `kind` cluster boots `kindest/node:v1.34.0@sha256:...` (digest-pinned), labels a namespace `pod-security.kubernetes.io/enforce=restricted`, applies four fixtures (defaults live with rollout-status wait; ExternalSecret/Cilium via server-side dry-run).

### Caveats explicitly documented

- The chart does NOT create or label the target Namespace. Operators must label `pod-security.kubernetes.io/enforce=restricted` themselves; the upgrade notes spell this out.
- NetworkPolicy semantic correctness (does the policy actually permit traffic to api.hyperping.io?) is NOT verified by kubeconform or PSS. The egress rule is constructed to match the documented contract; manual smoke verification post-merge is the operator's responsibility for FQDN-restricted Cilium installs.
- Helm-to-ESO upgrade requires a brief window where the chart-managed Secret is deleted and ESO reconciles a new one; staged cutover guidance is in the CHANGELOG upgrade notes.

### Upgrade notes (see CHANGELOG for full text)

- `app.kubernetes.io/version` label flips `1.4.0 → 1.4.1`; `helm.sh/chart` flips `hyperping-exporter-1.1.0 → hyperping-exporter-1.5.0`.
- `networkPolicy.enabled` flips to `true` by default — set `false` explicitly to opt out.
- `replicaCount > 1`, multi-source secret configs, and missing secret-source at replicaCount≥1 now abort install.
- `externalSecret.apiVersion` defaults to `external-secrets.io/v1` — set to `v1beta1` for legacy ESO ≤ 0.15.
- PSS-restricted enforcement is the assumed namespace policy; the chart does NOT label the namespace.
EOF
)"
```

- [ ] **Step 3: Watch CI**

```bash
gh pr checks --watch
```
Expected: the `helm` job (with all three new steps) passes. Go-side CI is skipped via the existing `paths-ignore` filter.

- [ ] **Step 4: Report PR URL and stop**

Do NOT merge. The user explicitly required PR-only.

---

## Risks and Cutover Notes

- **Action SHA pinning**: Task 1 Step 6 resolves the live SHAs and digests with no `||`-fallback; a tag mismatch from upstream (e.g. helm/kind-action rolled past the captured version mid-execution) surfaces to the user via `USER DECISION REQUIRED:` rather than silently substituting stale values. Task 8 Step 8's post-edit audit `grep`s the committed workflow + `kind-pss.yaml` for surviving placeholders and aborts the commit on any leak.
- **kubeconform CRDs catalog availability**: the workflow pulls schemas from `raw.githubusercontent.com/datreeio/CRDs-catalog`. Lowercase-kind filename convention is now used. If GitHub rate-limits the runner, the job will fail intermittently; document if encountered and pin a tagged catalog release.
- **kind boot time + live admission**: ~60-90 seconds on GitHub-hosted runners plus a rollout-status wait of up to 120 seconds for the defaults fixture pod. Total job time ~3-5 minutes; acceptable.
- **kind admission-config mount (R2-1 fix)**: kind resolves relative `hostPath` against its own cwd, not `$GITHUB_WORKSPACE`, and `helm/kind-action` consumes its `config:` input VERBATIM. The committed `tests/kind-pss.yaml` keeps `__ABSPATH_PLACEHOLDER__` so the file stays portable. **A discrete workflow step (`Rewrite kind-pss.yaml hostPath placeholder`) runs `sed -i "s#__ABSPATH_PLACEHOLDER__#${GITHUB_WORKSPACE}#g"` IN PLACE BEFORE the `helm/kind-action@…` step**, with an explicit post-substitution `grep` that aborts the job if the placeholder survived. Without that step, kind would either reject the literal path or silently mount nothing (admission-config absent → vacuous-pass PSS test). For local runs, `admission_test.sh` does the equivalent on a temp copy so the committed file stays clean. The full YAML for the rewrite step is in Task 8 Step 4.
- **Stale kind cluster (R2-7 fix)**: a defensive `kind delete cluster --name hyperping-pss` step runs BEFORE `helm/kind-action` to ensure CI never reuses a stale cluster from a prior step (e.g. on a self-hosted runner). A second always-run cleanup at job end prevents leak across retries. Locally, `make helm-pss-clean` is the explicit teardown target, and `admission_test.sh`'s "create-if-absent" guard reuses the local cluster intentionally (faster iteration when the operator is debugging the script itself).
- **Namespace mismatch in `kubectl apply`**: rendered manifests carry `namespace: <Release.Namespace>`. The admission script renders with `--namespace "$NS"` so Release.Namespace becomes the test namespace, AND strips `^  namespace:` lines as belt-and-braces so the `-n "$NS"` flag has authority. Either alone would suffice; both together are robust.
- **PSS rejection for fields we don't set**: the chart never sets `hostNetwork`, `hostPID`, `procMount`, sysctl, AppArmor — all PSS-friendly defaults. If a future change accidentally adds `hostPort` or similar, the PSS admission job will catch it.
- **External Secrets Operator presence at apply time**: same trade-off as the Cilium variant: chart cannot detect operator presence at render time; ExternalSecret apply fails with `no matches for kind` on clusters without ESO. The admission test exercises the ExternalSecret render via `--dry-run=server` (CRDs absent in stock kind), which validates the manifest shape against the apiserver but not against the CRD schema; kubeconform provides that via the datreeio catalog.
- **Helm template function availability**: `add`, `default dict`, `int`, `gt`, `printf`, `toJson`, `required` are all in Helm v3.20.x Sprig; verified by smoke tests in Tasks 6-8.
- **`replicaCount: 0` integer rendering**: Helm/Go YAML serialises the integer literally, producing `replicas: 0` (not `null` or `""`). Verified by Case 20.
- **Backwards-compat**: no existing value name renamed. New keys (`externalSecret`, `networkPolicy.fqdnRestriction`) are additive. The new `fail()` guards intentionally reject previously-broken configurations; that's the point.
- **Branch-protection identifier**: the workflow keeps the `helm` job key, so existing branch-protection rules referencing the `Helm chart lint and render` check name continue to bind. The job's `name:` is updated to `Helm chart CI`; if branch-protection matches by `name`, the rule needs the new name. Confirm before merge by running `gh api repos/develeap/hyperping-exporter/branches/main/protection --jq '.required_status_checks.contexts' 2>/dev/null` and reconciling.

## Why this plan is bold and direct

- One PR, ten items, single cutover.
- Empirical numbers measured under chart-equivalent runtime (Docker `--read-only`, uid 65534).
- Every edge case (replicas:0, NP-conflict, ExternalSecret-conflict, multi-replica, missing-store, bare-int cacheTTL) is tested as a `fail()` path or admission path, not characterised into best-effort warnings.
- Real CI gates: live pod boot for the readOnlyRootFilesystem proof, kubeconform for offline schema validation including CRDs, PSS admission for restricted-namespace enforcement.
- **Strict bisect contract**: every commit on the branch keeps `python3 render_test.py` AND `helm lint` green. Production-template changes land in the same commit as the harness-expectation updates they invalidate (Task 7: NP-default-on + Case 1 enumeration extension; Task 9: appVersion bump + EXPECTED_IMAGE_DEFAULT/EXPECTED_VERSION bump). Task 6 combines the ExternalSecret template with the secret-source / multi-replica guards so no commit leaves the chart in a state where `external-secret.values.yaml` would fail the legacy guard.
- Action SHAs, kindest/node digest, and ESO apiVersion are all resolved against upstream at execution time (Task 1 Step 6) WITHOUT `||`-fallback so tag drift surfaces loudly; a post-edit `grep` in Task 8 Step 8 audits the committed files for surviving placeholders.
- The `kind-pss.yaml` `__ABSPATH_PLACEHOLDER__` is rewritten by a discrete workflow step BEFORE `helm/kind-action` consumes the file; no implicit dependency on `admission_test.sh` running first.
- Cilium ingress shape conversion is type-correct: vanilla NP `NetworkPolicyPeer` shape is walked and converted to a flat Cilium `EndpointSelector`, with unsupported shapes aborting the render rather than producing a CRD-schema-invalid resource.
- Cache-TTL handling is two-layer defence: an operator-visible `validateCacheTTL` `fail()` for clear UX, AND a `printf "%v" + toString` defensive coercion so even if the validator is ever removed, the rendered arg stays clean.
- `replicaCount: 0` is a fully-supported state: render the Deployment with `replicas: 0`, suppress PDB, gate the env block on `secretSourceCount > 0`, exempt from `validateSecretSources`. A future `0 → 1` scale-up without first configuring a source re-trips the guard cleanly instead of CrashLoopBackOff.
- Version policy is explicit: chart 1.5.0 (not 1.2.0, which collides with the binary slot), appVersion 1.4.1 (tracks the latest released binary).
- Caveats the chart cannot fix (operator-side namespace labelling, NP connectivity semantics, ESO operator presence, kindest/node tag mutability between digest pins) are surfaced in the upgrade notes rather than papered over.
