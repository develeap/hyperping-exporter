# Chart UX Hardening: Production-Readiness Defaults Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use trycycle-executing to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land all ten chart hardening items from the v1.4.1 peer review as one PR that bumps the chart to **1.5.0** (tracks binary v1.4.1 via `appVersion`), gated by an extended PyYAML render harness, `kubeconform` schema validation (with CRDs), and a `kind` PSS-restricted admission CI job that boots one representative pod live to prove `readOnlyRootFilesystem` tolerance.

**Architecture:** Single-PR cutover on the existing `chore/chart-hardening-prod-defaults` worktree branch. The chart already partially implements items 1, 2, 4, 5, 8; this work reconciles those surfaces against the user's contract and adds ExternalSecret support, three render-time `fail()` guards (secret-source mutual-exclusion, multi-replica, cacheTTL-non-string), a `cilium.io/v2 CiliumNetworkPolicy` variant with explicit peer-shape conversion, a uniform safe-arg rendering pipeline (`toString | toJson` everywhere `printf`'d), a `replicaCount: 0` scaled-to-zero path with secret-source exemption, and a runtime read-only-rootfs proof. Three CI surfaces gate the chart inside a single `helm` job: the PyYAML render harness, offline `kubeconform` validation, and `kind` PSS-restricted live-admission. Every commit on the branch keeps `python3 render_test.py` AND `helm lint` AND `kubeconform` green (no bisect-red windows).

**Tech Stack:** Helm v3.20.2 (assertions use substring-match on `v3.20.` so v3.20.x patch drift does not break tests), PyYAML 6.0.3, `kubeconform` v0.7.0, `kind` (latest at execution time, captured by Task 1 Step 6; minimum v0.31.0 required for `kindest/node:v1.34.x`), Kubernetes 1.34 (PSS-restricted semantics), `external-secrets.io/v1` (default, with `v1beta1` override for ESO ≤ 0.15), `cilium.io/v2 CiliumNetworkPolicy`. Go binary unchanged.

---

## Scope Check

Single PR per user decision #4. All ten items target one Helm chart, share one test harness, and have interlocking semantics (secret-source mutual exclusion crosses ExternalSecret, multi-replica, and replicaCount:0; NetworkPolicy default-on touches every render-harness fixture's expected label set). No natural split survives the mutual-exclusion testing. Single PR is correct.

---

## Plan Invariants & Single Sources of Truth (SSOT)

This plan's correctness depends on these literal anchors. **Every occurrence of these values in the plan, in test code, in fixtures, in workflows, in `Makefile`, in error messages, and in CHANGELOG text MUST be sourced from these anchors and updated in the same commit when an anchor changes.** Executor MUST grep the worktree for any literal occurrence not sourced from the anchor list and fail the task if drift is found.

| Anchor | Value | Where it appears (must be kept in lockstep) |
|---|---|---|
| `CHART_VERSION` | `1.5.0` | `Chart.yaml`, `render_test.py` `EXPECTED_CHART_LABEL` constant, CHANGELOG `## [1.5.0] - 2026-05-12` header, PR body |
| `APP_VERSION` | `1.4.1` | `Chart.yaml`, `render_test.py` `EXPECTED_VERSION` and `EXPECTED_IMAGE_DEFAULT` constants, assertion message strings (Case 1: `f"app.kubernetes.io/version label must be {EXPECTED_VERSION}"`), upgrade notes |
| `CHANGELOG_DATE` | `2026-05-12` | CHANGELOG header. Same date as v1.4.1 entry; explicit `[Chart 1.5.0]` qualifier added to the header to defeat date-only sorters. See Contract C9 below. |
| `KIND_CLUSTER_NAME` | `hyperping-pss` | `tests/admission_env.sh` (sole producer), referenced by `kind-pss.yaml`, workflow `cluster_name`, defensive teardown, `make helm-pss-clean`, `admission_test.sh` |
| `TEST_RELEASE_NAME` | `testrel` | `tests/admission_env.sh`, every `kubectl rollout status deploy/...` reference |
| `TEST_NAMESPACE` | `hyperping-pss-test` | `tests/admission_env.sh`, every `kubectl -n` reference, `--namespace` flag to `helm template` |
| `LIVE_BOOT_IMAGE` | `khaledsalhabdeveleap/hyperping-exporter:1.4.1` | `render_test.py` `EXPECTED_IMAGE_DEFAULT`, admission live-boot fixture |
| `KUBECONFORM_CATALOG_REF` | (tagged release captured at Task 1 Step 6) | workflow `helm-ci.yml`, `Makefile` `helm-kubeconform` target. **NOT `main`.** See Contract C5. |
| `ADMISSION_FIXTURES` | `pss-restricted, external-secret, networkpolicy-default, networkpolicy-cilium` | `admission_test.sh`'s default fixture list, the workflow matrix, the per-fixture dispatch table |
| `LIVE_BOOT_FIXTURE` | `pss-restricted` (and `networkpolicy-default` post-Task-8 — both boot live) | `admission_test.sh` dispatch table; the only fixtures with a `kubectl rollout status` wait |

The plan owns the LIST of anchors; the worktree owns the VALUES. `tests/admission_env.sh` is the runtime SSOT for cluster/namespace/release/image, sourced by `admission_test.sh`, the `make helm-pss*` targets, and the workflow's bash steps. The plan and tests never re-spell these values; they reference the env var name.

---

## Durable Contracts

These contracts are the verification rules that bind every task. Each contract states the invariant, the gate that enforces it, and the test that proves the gate works. The findings memo (R3-1..R3-46) is the audit checklist these contracts must satisfy collectively, not a list of one-off patches.

### Contract C1 — Bisect-Green Atomicity (covers R3-1, R3-15, R3-21, R3-22, R3-34)

**Invariant:** Every commit on the branch keeps `python3 render_test.py` AND `helm lint` AND (from Task 9 onward) `make helm-kubeconform` green. No commit leaves an orphan helper, an unreferenced template, a stale expected-version, or a test-vs-template impedance mismatch.

**Mechanics:**

1. **Production-change atomicity:** any change that invalidates a render-harness assertion lands in the SAME commit as the assertion update. The plan's task boundaries are drawn to honour this. Specifically:
   - **Task 6** (ExternalSecret template + secret-source guards + replicaCount guard) lands the legacy two-line `apiKey || existingSecret` guard removal in the SAME commit as the new `validateSecretSources` / `validateReplicaCount` helpers and the `external-secret.values.yaml` fixture acceptance. The legacy guard would otherwise reject the ExternalSecret fixture between commits.
   - **Task 7** (PDB + NetworkPolicy default-on + Cilium variant + replicaCount:0) lands the `networkPolicy.enabled: false → true` flip in the SAME commit as Case 1's `expected_versions` enumeration extension and the `helm.sh/chart` label update.
   - **Task 9** (Chart.yaml bump) lands `Chart.yaml version`, `Chart.yaml appVersion`, `render_test.py` `EXPECTED_*` constants, the `f"...{EXPECTED_VERSION}..."` assertion message strings, AND the CHANGELOG header all in ONE commit.

2. **Helper hygiene:** when a template change replaces or supersedes a `_helpers.tpl` define, the same commit either DELETES the orphaned helper or migrates remaining callsites. The `_helpers.tpl` after each commit MUST satisfy: every defined name has at least one `include` callsite in the same commit's tree. Executor uses `grep -RhoE 'include "hyperping-exporter\.[a-zA-Z]+"' deploy/helm/hyperping-exporter/templates/` to enumerate references and compares against `_helpers.tpl`'s `define` list before each commit.

3. **Template-edit grammar:** template patches in the plan name the EXACT text to be replaced (full unedited block), not a line-number range. If the source file's content has shifted (whitespace, line number), the executor patches the unique full-text occurrence; if there is more than one occurrence (ambiguous), execution stops and surfaces the ambiguity. This eliminates "lines 41-47" hedges. The `networkpolicy.yaml` egress-rule edit (Task 7) and the `deployment.yaml` env-block edit (Task 6) are written this way.

4. **CRD-bearing fixture dispatch table** (R3-15 collapse): `admission_test.sh` resolves each fixture's admission mode via an explicit dispatch table — `pss-restricted` and `networkpolicy-default` boot LIVE with rollout-status; `external-secret` and `networkpolicy-cilium` are SKIPPED from the live job entirely and validated via `kubeconform` (which doesn't need CRDs installed) plus a kubeconform-only CI step. `--dry-run=server` is NOT used because it still consults the REST mapper and would abort on missing CRDs. The dispatch table is the SSOT; no case branch in shell duplicates it.

**Gate:** post-commit hook (documented as Task N Step "Gate") runs the harness + lint + (from Task 9) kubeconform and refuses to advance.

**Test:** `git rebase --exec 'python3 deploy/helm/hyperping-exporter/tests/render_test.py && helm lint deploy/helm/hyperping-exporter/ && (! command -v kubeconform || make helm-kubeconform)' origin/main` is run before opening the PR (Task 11 Step 2) and MUST exit 0 across the entire commit range.

### Contract C2 — Uniform Safe-Arg Rendering (covers R3-23, R3-14, R3-32, R3-36)

**Invariant:** Every argument the chart emits from a values-derived scalar passes through one canonical rendering path. The cacheTTL footgun (operator supplies a bare integer, Helm's `printf "%s"` produces `%!s(int=60)`) cannot exist in any other arg.

**Mechanics:**

1. **One safe-arg helper.** `_helpers.tpl` defines `hyperping-exporter.arg`:
   ```yaml
   {{- define "hyperping-exporter.arg" -}}
   {{- /* Usage: include "hyperping-exporter.arg" (list "--flag" .Values.path.to.scalar) */ -}}
   {{- $flag := index . 0 -}}
   {{- $val := index . 1 -}}
   {{- printf "%s=%v" $flag (toString $val) | toJson -}}
   {{- end -}}
   ```
   The `%v` verb + `toString` are belt-and-braces; either alone would already produce a clean string for ints/floats/bools.

2. **All conditional args migrate.** Task 4 migrates `--cache-ttl` to the helper; Task 4 ALSO migrates `--listen-address`, `--metrics-path`, `--log-level`, `--log-format`, `--web-config-file`, `--namespace`, `--exclude-name-pattern`, `--mcp-url`, and any other arg currently inline in `templates/deployment.yaml`. The inline `printf "..."` and bare `{{ .Values...}}` forms are eliminated from the deployment template; there is exactly one rendering pattern for arg lines.

3. **`validateCacheTTL` keeps the loud UX.** The helper provides defence in depth; `validateCacheTTL` provides operator-facing diagnostics. The validator uses `kindIs` (NOT `kindOf | eq "..."`) so it works on Sprig's typed kinds:
   ```yaml
   {{- define "hyperping-exporter.validateCacheTTL" -}}
   {{- if not (kindIs "string" .Values.config.cacheTTL) -}}
   {{- fail (printf "config.cacheTTL must be a quoted Go duration string (e.g. \"60s\"). Got kind %s (value %v). Quote the value in values.yaml." (kindOf .Values.config.cacheTTL) .Values.config.cacheTTL) -}}
   {{- end -}}
   {{- end -}}
   ```
   `validateCacheTTL` is included EXACTLY ONCE — at the top of `templates/deployment.yaml`, alongside `validateSecretSources` and `validateReplicaCount`. The plan does NOT instruct re-including it from any other template. Task 4 and Task 6 are both audited (Contract C7) to ensure single-inclusion.

4. **`default dict` semantics correctness.** All `secretSourceCount`-style helpers that read into a sub-block of `.Values` MUST first verify the parent is a map. Pattern:
   ```yaml
   {{- $es := .Values.externalSecret | default dict -}}
   {{- if not (kindIs "map" $es) -}}
   {{- fail "values.externalSecret must be a map; got non-map. Refer to values.yaml for the supported shape." -}}
   {{- end -}}
   ```
   Then `$es.enabled` is safe. The pattern is identical for `.Values.networkPolicy.fqdnRestriction`.

**Gate:** Task 4 Step (final) audits the deployment template with `grep -E '^[[:space:]]*-\s+"-' deploy/helm/hyperping-exporter/templates/deployment.yaml` (looks for any surviving bare-quoted arg line). Audit fails the task if any match exists outside the new helper pattern.

**Test:** new render-harness Cases — `cache-ttl-numeric` (Case 22), `cache-ttl-int-fails` (Case 23, `assert_fail`), `log-level-numeric` (Case 24, asserts a numeric log-level still renders as a quoted JSON string with no `%!s(...)` artefact), `metrics-path-with-special-chars` (Case 25, exercises path with characters needing JSON-escape).

### Contract C3 — Cilium Conversion Robustness (covers R3-2, R3-35, R3-38)

**Invariant:** The Cilium NetworkPolicy template produces a correctly-shaped CRD for ALL inputs the chart accepts, OR aborts with a clear `fail()`. No silently-malformed Cilium output reaches a cluster.

**Mechanics:**

1. **Peer-shape conversion supports both `matchLabels` AND `matchExpressions`.** The chart's `networkPolicy.ingressFrom` accepts vanilla `NetworkPolicyPeer` shape (`{podSelector: {matchLabels, matchExpressions}, namespaceSelector: {matchLabels, matchExpressions}}`). The Cilium template walks each peer and emits an `EndpointSelector` containing both `matchLabels` and `matchExpressions` from `podSelector`, AND merges in a derived label for `namespaceSelector` when one is present. The conversion logic is in Task 7 Step 3.

2. **Namespace-label encoding policy.** The Cilium label convention is `k8s:io.kubernetes.pod.namespace=<name>` for the well-known namespace label; for arbitrary namespaceSelector labels Cilium uses `k8s:io.kubernetes.pod.namespace.label.<key>=<value>`. The template only converts the well-known `kubernetes.io/metadata.name` case automatically; ANY other `namespaceSelector` label triggers a `fail()` with a message directing the operator to encode it directly as a Cilium-shaped `matchLabels` entry on the peer's `podSelector`. The Cilium label PREFIX is `k8s:` and is a project-versioned constant. We document the targeted Cilium minor: **Cilium 1.14+ (uses `k8s:io.kubernetes.pod.namespace.labels.<key>` for any label and `k8s:io.kubernetes.pod.namespace` for the well-known name).** Older Cilium (≤1.13) is not supported; the values.yaml comment states this explicitly.

3. **`ipBlock`-only peers, selector-less peers, and unconvertible mixes** abort with a specific `fail()` message naming the offending entry index and the unsupported field. No silent dropping.

4. **Cilium template existence is conditional on `ingressFrom` being non-empty for the `ingress:` block ONLY**, not on the policy itself. The `endpointSelector` and `egress:` blocks always render when `fqdnRestriction.enabled: true`.

5. **`ingress:` block is dropped, not rendered with `[]`,** when `ingressFrom` is empty, so the egress-only mode produces a clean policy.

**Gate:** `networkpolicy-cilium-defaults.values.yaml` (no ingressFrom; egress-only) and `networkpolicy-cilium-with-ingress.values.yaml` (matchLabels-only), `networkpolicy-cilium-matchexpressions.values.yaml` (matchExpressions on `podSelector`), `networkpolicy-cilium-mixed.values.yaml` (both), and three fail-path fixtures (`networkpolicy-cilium-ipblock-only`, `networkpolicy-cilium-selectorless`, `networkpolicy-cilium-foreign-namespace-label`). All seven fixtures are checked into `tests/fixtures/`.

**Test:** new render-harness Cases — `cilium-egress-only` (Case 30), `cilium-ingress-matchlabels` (Case 31), `cilium-ingress-matchexpressions` (Case 32), `cilium-ingress-mixed` (Case 33), `cilium-ipblock-only-fails` (Case 34, `assert_fail`), `cilium-selectorless-fails` (Case 35, `assert_fail`), `cilium-foreign-ns-label-fails` (Case 36, `assert_fail`).

### Contract C4 — Robust External Resolution (covers R3-5, R3-9, R3-11, R3-29, R3-30)

**Invariant:** Every external resolution (`gh api`, registry probe, schema fetch, kind/helm action tag lookup) follows a single retry+fail-loud policy implemented in one shared script. Transient network errors do not corrupt the implementation; tag-drift surfaces loudly.

**Mechanics:**

1. **One resolver script.** `tests/scripts/resolve_pins.sh` (created in Task 1 Step 6) implements:
   - **Auth precheck** — `gh auth status` MUST succeed; anonymous `gh` is rejected.
   - **Per-call retry** — 3 attempts with exponential backoff (2s, 5s, 10s) on HTTP 403/429/5xx; the script writes attempt counts to stderr so failures are diagnosable.
   - **Strict tag pinning** — the script reads `tests/pins.expected.yaml` (committed) listing expected tags for `helm/kind-action`, `kubernetes-sigs/kind` (minimum version), `actions/checkout`, `azure/setup-helm`, `actions/setup-python`, `datreeio/CRDs-catalog`. The resolver fails-loud if the resolved tag DOES NOT MATCH the expected literal (kind: minimum-version check; others: exact-match).
   - **Image existence precheck** — `tests/scripts/resolve_pins.sh` probes Docker Hub's `/v2/repositories/khaledsalhabdeveleap/hyperping-exporter/tags/<APP_VERSION>` with the retry policy. Failure means the binary isn't published yet; admission test would `ImagePullBackOff`. Surface to the user.
   - **kindest/node digest** — script greps `kind` release notes for the matching `kindest/node:v1.34.x@sha256:` line, captures BOTH version and digest. The committed `kind-pss.yaml` uses two placeholders (`__KINDEST_VERSION__` and `__KINDEST_DIGEST__`) so the substitution can't produce an inconsistent prefix/digest pair (R3-10 collapse).
   - **`helm/kind-action` vs `kind` binary version compatibility** — the resolver compares the kind binary version embedded in the chosen `helm/kind-action` release against the standalone `kind` release. If mismatched (R3-29), the resolver SURFACES the conflict and uses the `helm/kind-action`-bundled binary (the action input `version:` is OMITTED in the workflow, letting the action use its bundled binary).

2. **`tests/pins.expected.yaml`** is the SSOT for upstream pins. Plan Task 1 Step 6 instructs the executor to UPDATE this file when an upstream tag has rolled past the captured value AND the change is safe (the new tag's CI-impact has been read from release notes). Otherwise the resolver fails and the executor surfaces `USER DECISION REQUIRED:`.

**Gate:** Task 1 Step 6 exits non-zero on any resolution failure or pin mismatch. Workflow YAML and `kind-pss.yaml` are NEVER written with literal SHAs from a non-Task-1 source.

**Test:** Task 8 Step (Audit) `grep -E '<__[A-Z_]+__>|<__placeholder__>'` over the committed workflow and `kind-pss.yaml` returns NO matches; if any placeholder survives, the audit fails the task.

### Contract C5 — CI Cluster Lifecycle Hygiene (covers R3-6, R3-12, R3-19, R3-20, R3-26)

**Invariant:** The CI cluster, namespace, and release are deterministic per run and clean between fixtures. No name drift across files. No slow-target dependency for fast-iteration runs. No schema-fetch flake from a mutable upstream ref.

**Mechanics:**

1. **`tests/admission_env.sh`** is the SSOT for `KIND_CLUSTER_NAME`, `TEST_NAMESPACE`, `TEST_RELEASE_NAME`, `LIVE_BOOT_IMAGE`. Sourced by `admission_test.sh`, every `make helm-pss*` target, and every workflow shell step. The plan and workflow YAML never spell these values; they reference the env var.

2. **Per-fixture cleanup with explicit wait.** `admission_test.sh` runs between fixtures:
   ```bash
   kubectl delete --ignore-not-found -n "$NS" all -l app.kubernetes.io/instance="$REL"
   kubectl wait --for=delete --timeout=60s -n "$NS" \
     deploy,statefulset,pod,svc,sa,role,rolebinding,cm,secret,networkpolicy \
     -l app.kubernetes.io/instance="$REL" || true
   ```
   The `--for=delete` wait eliminates the SSA-on-terminating collision (R3-12). The `|| true` tolerates resources that never existed in the first place (some fixtures don't render every kind).

3. **Schema fetch pinned to a tagged ref.** `KUBECONFORM_CATALOG_REF` (anchor) is captured at Task 1 Step 6 from `datreeio/CRDs-catalog` releases and substituted into both the workflow's `SCHEMAS` variable and the `Makefile` `helm-kubeconform` target. Never `main`.

4. **Fast-iteration target.** `Makefile` exposes `helm-ci-fast` (lint + render + kubeconform) and `helm-ci` (the above plus `helm-pss`). Developers iterating on values changes pay only the fast path; CI runs `helm-ci`.

5. **Defensive cluster teardown** at workflow `if: always()` end uses `${KIND_CLUSTER_NAME}` from the env, not a literal. Same for the pre-action teardown that guards against stale clusters on self-hosted runners.

**Gate:** the workflow's `Audit pins` step (Task 8) `grep`s the committed workflow + Makefile + `kind-pss.yaml` for hard-coded values from the anchor list; any literal occurrence not sourced from `admission_env.sh` or `pins.expected.yaml` fails the workflow audit.

**Test:** `bash -c 'set -e; for f in pss-restricted networkpolicy-default; do tests/admission_test.sh $f; done'` runs both back-to-back locally with explicit between-fixture cleanup; both pass.

### Contract C6 — Validator Smoke-Test Standard (covers R3-14)

**Invariant:** Every chart-side `fail()` validator is exercised by an `assert_fail` test that checks BOTH the non-zero exit code AND the substring expected in stderr. No grep-only smoke test.

**Mechanics:**

1. **`assert_fail` helper in `render_test.py`** (added in Task 6, before any task uses it):
   ```python
   def assert_fail(case_name, fixture_path, expected_stderr_substring):
       proc = subprocess.run(
           ["helm", "template", "testrel", CHART_DIR, "-f", fixture_path],
           capture_output=True, text=True
       )
       assert proc.returncode != 0, (
           f"{case_name}: expected helm template to fail; got returncode {proc.returncode}.\n"
           f"stdout: {proc.stdout[:500]}\nstderr: {proc.stderr[:500]}"
       )
       assert expected_stderr_substring in proc.stderr, (
           f"{case_name}: expected stderr to contain {expected_stderr_substring!r}; "
           f"got stderr: {proc.stderr[:500]}"
       )
       print(f"PASS Case {case_name}: assert_fail({expected_stderr_substring!r}) returncode={proc.returncode}")
   ```

2. **Every validator gets one or more `assert_fail` cases.** `validateCacheTTL` (`cache-ttl-int-fails`), `validateSecretSources` (3 conflict fixtures + 1 missing fixture), `validateReplicaCount` (`replicas-multi`), Cilium peer-shape conversion fail paths (3 fixtures). No validator is "smoke-tested" by a grep-against-stderr pipeline.

3. **No `2>&1 | grep ... && echo PASS` patterns** anywhere in the test code or plan. Task 4's smoke test is replaced by the formal `assert_fail` Case 23 introduced in Task 6 (which runs in the same task as `validateCacheTTL` becomes the gate). Between Task 4 and Task 6, the validator is exercised by a direct `helm template ... ; test $? -ne 0` shell smoke that the plan body spells out explicitly, with `set -e` discipline.

**Gate:** Task 10 Step (final) greps the test code for any `2>&1 | grep` pattern; any match fails the task.

**Test:** all `assert_fail` cases produce both a non-zero exit AND the expected message in stderr.

### Contract C7 — Test-Artifact Storage (covers R3-37)

**Invariant:** State files the plan generates (baseline renders, captured pins, derived values) live under `tests/artifacts/` in the worktree, NOT `/tmp`. They survive shell restarts and tmpfs resets. They are `.gitignore`d (we do not commit ephemeral state).

**Mechanics:**

1. **Task 1 Step 3 captures baselines** to `deploy/helm/hyperping-exporter/tests/artifacts/render-baseline.yaml`, `tests/artifacts/go-baseline.txt`, `tests/artifacts/go-baseline-count.txt`, `tests/artifacts/tooling.txt`.

2. **Task 1 Step 6 captures pins** to `tests/artifacts/kind-action-tag.txt`, `tests/artifacts/kind-action-sha.txt`, etc. The `tests/pins.expected.yaml` file IS committed (the expected values); the `tests/artifacts/*.txt` files are RUN ARTIFACTS, not committed.

3. **`.gitignore` updates** in Task 1 Step 3:
   ```
   deploy/helm/hyperping-exporter/tests/artifacts/
   ```

4. **Task 10 Step (final-suite) re-captures** the Go test baseline if `tests/artifacts/go-baseline-count.txt` is absent; the executor never assumes a `/tmp` file from a previous task still exists.

**Gate:** the Task 10 baseline-rerun is `[ -f tests/artifacts/go-baseline-count.txt ] || (go test ./... -count=1 2>&1 | tee tests/artifacts/go-baseline.txt | grep -c '^=== RUN' > tests/artifacts/go-baseline-count.txt)`.

**Test:** `ls tests/artifacts/` after Task 1 lists exactly the captured files; after Task 10 they are all still readable.

### Contract C8 — Coverage Completeness (covers R3-16, R3-28, R3-4)

**Invariant:** Every template the chart ships is exercised by at least one rendering test that validates its structural shape (apiVersion, kind, selector), independent of whether the template renders under default values. No template is dead-code-coverage.

**Mechanics:**

1. **PDB structural test.** Because `validateReplicaCount` aborts on `replicaCount > 1`, no positive PDB render is achievable through normal fixtures. Task 7 Step (PDB test) adds a `pdb-structural.values.yaml` fixture that uses `--set` to bypass `validateReplicaCount` (`replicaCount=2` with a special-cased `--set internal.skipReplicaCheck=true` knob OR a render-only `kubeconform`-target file that calls `helm template ... --show-only templates/pdb.yaml --set replicaCount=2 --skip-validation`). Cleaner approach: the PDB template gets an explicit `internal.bypassReplicaCheck` boolean that ONLY the PDB-structural test sets, AND the `validateReplicaCount` validator skips the `replicaCount > 1` check when that bypass is set. The bypass key is `internal.` (Helm convention for not-for-production), documented as internal-only in `values.yaml` with explicit "DO NOT SET IN PRODUCTION" warning, plus a render-harness case (`pdb-structural-internal-bypass-without-pdb-fails`, Case 29) that asserts setting `internal.bypassReplicaCheck=true` without `podDisruptionBudget.enabled=true` ALSO fails (one cannot leak the bypass without enabling PDB; the bypass is gated to the PDB-test pathway only).

2. **Admission-job live-boot coverage.** The four `ADMISSION_FIXTURES` anchor entries cover: `pss-restricted` (default-config live boot — runtime read-only-rootfs proof), `networkpolicy-default` (live boot — proves the default NetworkPolicy doesn't break pod start), `external-secret` (kubeconform only — no CRDs in stock kind), `networkpolicy-cilium` (kubeconform only — same reason). The dispatch table is the SSOT (Contract C1.4).

3. **Default-render is comprehensive.** Case 1 (already extended to enumerate kinds) extends to include EVERY resource the default render emits. After Task 7's NetworkPolicy default-on, default render is `Secret, ServiceAccount, Service, Deployment, NetworkPolicy`. After Task 6's externalSecret-conditional Secret, with default `externalSecret.enabled: false`, the default still emits `Secret`. Case 1's `expected_versions` enumeration uses `set(actual.keys()) == set(expected.keys())` semantics with a per-kind diff message; future label-bearing resource additions produce a clear "new resource: <kind>" diagnostic rather than a tuple-mismatch.

**Gate:** `find deploy/helm/hyperping-exporter/templates/ -name '*.yaml' -not -name 'NOTES.txt' -not -name '_helpers.tpl'` enumerates all production templates. Task 10 Step (coverage audit) asserts every enumerated template appears in at least one positive render fixture's output (`helm template ... | grep "^# Source: ...<template-name>"`). Audit fails the task on uncovered template.

**Test:** the coverage audit is run automatically by `make helm-render` and surfaces missing coverage as a non-zero exit.

### Contract C9 — CHANGELOG Disambiguation (covers R3-7, R3-25)

**Invariant:** The CHANGELOG entry for this chart release does not collide with the v1.4.1 binary entry (same date) and identifies the release as chart-only in a parser-stable way.

**Mechanics:**

1. **Header form:** `## [1.5.0] - 2026-05-12 [Chart only — binary unchanged]`. The `[Chart only ...]` qualifier sits IN the header, after the date, so date-only sorters (release-drafter, GHA scrapers) treat it as the same date but keep semver-descending order. Keep-a-changelog grammar tolerates header suffixes after the date.

2. **Anchor stability:** the header is keyed for HTML-anchor lookup as `1.5.0--2026-05-12-chart-only-binary-unchanged`. We don't rely on this; CHANGELOG consumers should match by version, not anchor.

3. **Single CHANGELOG, dual-namespace:** the file's existing `## [1.4.1] - 2026-05-12` entry is BINARY-NAMESPACED; this PR's `## [1.5.0] - 2026-05-12 [Chart only — binary unchanged]` is CHART-NAMESPACED. The chart and binary share `1.x.y` slots because Helm chart SemVer is independent of app SemVer. We document this trade-off in CHANGELOG's header comment (which Keep-a-changelog allows).

**Gate:** Task 9 Step (CHANGELOG audit) runs `grep -c '^## \[1\.5\.0\]'` on `CHANGELOG.md`; expect exactly 1. Runs `grep -c '^## \[1\.4\.1\]'`; expect exactly 1. Both pre-existing and new entries dated `2026-05-12` are present and unique.

**Test:** CHANGELOG parses with `python3 -c "from packaging.version import Version; ..."` (no parse error).

### Contract C10 — Plan-Text Single Source (covers R3-3, R3-17, R3-18, R3-24, R3-40, R3-46)

**Invariant:** Every cross-reference in the plan body (task numbers, file line numbers, version literals, message strings) is sourced from a single anchor in this document. No two locations in the plan can disagree.

**Mechanics:**

1. **Task numbering is contiguous and unique.** This plan uses Tasks 1..11; no duplicate "Task 10". Every task has a unique heading. Executor `grep -c '^### Task [0-9]'` returns 11.

2. **Tech-stack pins source from `tests/pins.expected.yaml`.** The plan Header's Tech Stack line names the SSOT, not literal pins (with one exception: minimum versions for clarity, e.g. "kind v0.31.0 minimum"; the exact pin is the resolver's job).

3. **Cross-task references use task numbers, not line numbers.** Strategy Gate items reference "Task 7 Step 3", not `lines 998-1003`. Editing the plan does not invalidate these references.

4. **Assertion-message strings in render_test.py are anchor-derived.** Case 1's message is `f"app.kubernetes.io/version label must be {EXPECTED_VERSION} on every labelled resource"` (Python f-string), not a literal `"1.4.0 on every labelled resource"`. Task 7 Step (test update) and Task 9 Step (anchor bump) both verify Case 1's assertion message contains the f-string interpolation, not a hard-coded version.

**Gate:** Task 10 Step (plan-consistency audit) `grep -c '^### Task '` the plan file; expect 11. `grep -E '"1\.4\.0[^.]"|"1\.4\.0 on every'` the test code; expect 0.

**Test:** the audit fails the task on any hard-coded anchor literal in test code.

---

## File Structure

### Files created

- `deploy/helm/hyperping-exporter/templates/externalsecret.yaml` — renders `external-secrets.io/v1 ExternalSecret` (or `v1beta1` per `externalSecret.apiVersion` override) when `externalSecret.enabled: true`. Mutually exclusive with `templates/secret.yaml`.
- `deploy/helm/hyperping-exporter/templates/networkpolicy-cilium.yaml` — renders `cilium.io/v2 CiliumNetworkPolicy` when `networkPolicy.enabled: true` AND `networkPolicy.fqdnRestriction.enabled: true`. Mutually exclusive with `templates/networkpolicy.yaml`. Implements the C3 peer-shape conversion.
- `deploy/helm/hyperping-exporter/tests/fixtures/pss-restricted.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/external-secret.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/external-secret-defaults.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/external-secret-missing-store.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/replicas-zero.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/replicas-multi.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/pdb-enabled.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/pdb-structural.values.yaml` — uses `internal.bypassReplicaCheck: true` (C8 PDB-structural test).
- `deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-default.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-cilium-defaults.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-cilium-with-ingress.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-cilium-matchexpressions.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-cilium-mixed.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-cilium-ipblock-only.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-cilium-selectorless.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-cilium-foreign-namespace-label.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-apikey-and-existing.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-apikey-and-external.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-existing-and-external.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/secret-source-missing.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/cache-ttl-numeric.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/cache-ttl-int-fails.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/log-level-numeric.values.yaml`
- `deploy/helm/hyperping-exporter/tests/fixtures/metrics-path-with-special-chars.values.yaml`
- `deploy/helm/hyperping-exporter/tests/kind-pss.yaml` — kind cluster config with `__KINDEST_VERSION__` and `__KINDEST_DIGEST__` placeholders.
- `deploy/helm/hyperping-exporter/tests/kind-pss-config/admission-config.yaml` — PSS admission plugin configuration mounted into the kind control-plane node.
- `deploy/helm/hyperping-exporter/tests/admission_test.sh` — bash driver. Sources `admission_env.sh`; uses the dispatch table (Contract C1.4) to live-boot vs kubeconform-only validate per fixture.
- `deploy/helm/hyperping-exporter/tests/admission_env.sh` — SSOT for cluster/namespace/release/image (Contract C5.1).
- `deploy/helm/hyperping-exporter/tests/scripts/resolve_pins.sh` — Contract C4 resolver.
- `deploy/helm/hyperping-exporter/tests/pins.expected.yaml` — Contract C4 SSOT for upstream pin values.
- `deploy/helm/hyperping-exporter/tests/artifacts/.gitkeep` — establishes the directory; `.gitignore` excludes the rest.

### Files modified

- `deploy/helm/hyperping-exporter/Chart.yaml`:
  - `version: 1.1.0 → 1.5.0` (anchor `CHART_VERSION`).
  - `appVersion: "1.4.0" → "1.4.1"` (anchor `APP_VERSION`).
- `deploy/helm/hyperping-exporter/values.yaml`:
  - Comment next to `image.tag` noting chart version and app version track separately (item 9).
  - Block comment above `securityContext:` explaining the PSS-restricted contract.
  - Comment under `cacheTTL` warning that bare integers like `60` (without unit) are rejected; always supply a unit.
  - Re-tune `resources` from empirical observation (Task 2).
  - Reconcile probes against user's contract (Task 3).
  - Flip `networkPolicy.enabled` default `false → true` and update the egress rule to the unrestricted form (Task 7).
  - Add `networkPolicy.fqdnRestriction.enabled: false`, `allowedHosts: [api.hyperping.io]` (Task 7).
  - Add `externalSecret:` block (Task 6).
  - Document secret-source mutual exclusion above the secret block.
  - Document the multi-replica `fail()` and `replicaCount: 0` exemption above `replicaCount`.
  - Add `internal:` block with `bypassReplicaCheck: false` and a DO-NOT-SET-IN-PRODUCTION warning (Contract C8).
- `deploy/helm/hyperping-exporter/templates/_helpers.tpl`:
  - Add `hyperping-exporter.arg` (Contract C2.1).
  - Add `validateSecretSources` (one of three top-of-deployment validators).
  - Add `validateReplicaCount` (with `internal.bypassReplicaCheck` honoured for the PDB-structural test).
  - Add `validateCacheTTL` (Contract C2.3).
  - Add `secretSourceCount` (Contract C2.4 default-dict-with-kindIs-map guard).
  - Remove `hyperping-exporter.secretName` ONLY IF Task 6's env-block edit migrates every callsite away from it. If any callsite remains (Service, ServiceMonitor, etc.), the helper stays; Task 6's audit (Contract C1.2) decides.
- `deploy/helm/hyperping-exporter/templates/deployment.yaml`:
  - Top-of-file: replace the legacy `apiKey || existingSecret` guard with the three validator includes. Trailing `-}}` to suppress stray newlines.
  - Args block: migrate every `printf "--flag=...{{ .Values...}}` line to `{{ include "hyperping-exporter.arg" (list "--flag" .Values.path.to.scalar) }}` (Contract C2.2). Conditional args (`if .Values.config.foo`) wrap the helper call; the helper itself doesn't gate.
  - Env block (`env: HYPERPING_API_KEY`): wrap in `{{- if gt (include "hyperping-exporter.secretSourceCount" .) "0" }}` so `replicaCount: 0` Deployments without a configured secret source don't dangle a `secretKeyRef`. Use `(include ... .)` not raw `.Values...` so the env block correctness depends on the SAME helper that `validateSecretSources` uses; future weakening of one breaks both, surfacing the regression.
- `deploy/helm/hyperping-exporter/templates/secret.yaml` — extend `if` guard to suppress on `externalSecret.enabled`.
- `deploy/helm/hyperping-exporter/templates/pdb.yaml` — wrap in `{{- if and .Values.podDisruptionBudget.enabled (or (gt (int .Values.replicaCount) 1) (and .Values.internal .Values.internal.bypassReplicaCheck)) }}`. `maxUnavailable` default `1` (drain-safe).
- `deploy/helm/hyperping-exporter/templates/networkpolicy.yaml`:
  - Wrap in `{{- if and .Values.networkPolicy.enabled (not (and .Values.networkPolicy.fqdnRestriction .Values.networkPolicy.fqdnRestriction.enabled)) }}` (Cilium-suppression).
  - Replace the egress 443 rule's `- to:` block (text-matched, not line-matched; see Contract C1.3) so the rendered egress permits TCP/443 to `0.0.0.0/0` with no `except:` list. The cross-namespace scraping doc comment above `ingressFrom` is preserved verbatim.
- `deploy/helm/hyperping-exporter/tests/render_test.py`:
  - **In Task 7's commit (NetworkPolicy default-on):** extend Case 1's `expected_versions` enumeration to a set-difference idiom that produces a per-kind diff message; include `NetworkPolicy`.
  - **In Task 9's commit (Chart.yaml bump):** bump `EXPECTED_VERSION` literal `"1.4.0" → "1.4.1"`, `EXPECTED_IMAGE_DEFAULT` literal to the `LIVE_BOOT_IMAGE` anchor, and verify every assertion message uses an f-string interpolation of `EXPECTED_VERSION` rather than a literal "1.4.0".
  - **Add `EXPECTED_CHART_LABEL = "hyperping-exporter-1.5.0"` constant** and the `helm.sh/chart` label assertion (Case 1 extension), atomic with the Chart.yaml bump (Task 9).
  - **Add `assert_fail` helper** (Task 6 introduction, Contract C6).
  - **Add new cases** per task; see Task Decomposition.
- `.github/workflows/helm-ci.yml`:
  - Single job key `helm` preserved (branch-protection identifier).
  - Job body extended with named steps: `resolve-pins`, `lint-and-render`, `kubeconform`, `kind-pss-rewrite-placeholders`, `pss-admission`, `audit-pins`.
  - Branch protection: if any rule matches by `name:` instead of job-key, the rule needs updating; Task 11 Step (final) audits via `gh api`.
- `Makefile` — `helm-ci`, `helm-ci-fast`, `helm-render`, `helm-kubeconform`, `helm-pss`, `helm-pss-clean` targets. `helm-ci-fast` excludes `helm-pss` (Contract C5.4).
- `CHANGELOG.md` — add `## [1.5.0] - 2026-05-12 [Chart only — binary unchanged]` chart-section between `## [Unreleased]` and `## [1.4.1]` (Contract C9).
- `.gitignore` — add `deploy/helm/hyperping-exporter/tests/artifacts/` (Contract C7).

### Files explicitly NOT modified

- `main.go`, `internal/**` — binary unchanged.
- `.github/workflows/ci.yml` — chart still in `paths-ignore`.
- `.github/workflows/release.yml`, `.goreleaser.yml` — no release pipeline impact.
- `deploy/grafana/**`, `deploy/prometheus/**`, `deploy/k8s/**` — out of scope.
- `README.md` — chart-specific docs live in `values.yaml`.

---

## Task Decomposition

### Task 1: Pre-flight — capture baselines, resolve pins, verify tooling

**Files:** Create `tests/artifacts/.gitkeep`, `tests/scripts/resolve_pins.sh`, `tests/pins.expected.yaml`. Modify `.gitignore`. Run-only: existing render harness.

This task does NOT change any production template. It establishes:
- The render-harness, helm-lint, and Go-test baselines (Contract C7).
- The committed pin SSOT (`pins.expected.yaml`) and the resolver script (Contract C4).
- That the dev API key, Docker, helm, kubectl are all available locally.

- [ ] **Step 1: Run the existing render harness as-is**

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

- [ ] **Step 3: Capture baselines into `tests/artifacts/`** (Contract C7)

```bash
mkdir -p deploy/helm/hyperping-exporter/tests/artifacts
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/default.values.yaml \
  > deploy/helm/hyperping-exporter/tests/artifacts/render-baseline.yaml
go test ./... -count=1 2>&1 | tee deploy/helm/hyperping-exporter/tests/artifacts/go-baseline.txt | tail -5
grep -c '^=== RUN' deploy/helm/hyperping-exporter/tests/artifacts/go-baseline.txt \
  > deploy/helm/hyperping-exporter/tests/artifacts/go-baseline-count.txt
cat deploy/helm/hyperping-exporter/tests/artifacts/go-baseline-count.txt
echo "deploy/helm/hyperping-exporter/tests/artifacts/" >> .gitignore
echo "!deploy/helm/hyperping-exporter/tests/artifacts/.gitkeep" >> .gitignore
touch deploy/helm/hyperping-exporter/tests/artifacts/.gitkeep
```

- [ ] **Step 4: Verify tooling availability**

```bash
which helm kind kubectl kubeconform docker | tee deploy/helm/hyperping-exporter/tests/artifacts/tooling.txt
helm version --short
kind version 2>/dev/null || echo "kind: missing (CI provides)"
kubeconform -v 2>/dev/null || echo "kubeconform: missing (CI provides)"
docker version --format '{{.Client.Version}}'
```

- [ ] **Step 5: Verify Hyperping dev API key**

```bash
test -r /home/khaledsa/projects/develeap/terraform-provider-hyperping/.env && \
  grep -q '^HYPERPING_API_KEY=' /home/khaledsa/projects/develeap/terraform-provider-hyperping/.env && \
  echo "OK key file readable" || echo "FAIL key file missing"
```

- [ ] **Step 6: Create `tests/pins.expected.yaml` and `tests/scripts/resolve_pins.sh` (Contract C4)**

```bash
cat > deploy/helm/hyperping-exporter/tests/pins.expected.yaml <<'YAML'
# SSOT for upstream pin values. Update when a pin is intentionally rolled
# forward; never silently. The resolver fails-loud on mismatch.
helm_kind_action_min_tag: v1.14.0
kind_min_tag: v0.31.0
kindest_node_minor: v1.34
actions_checkout_tag: v6
azure_setup_helm_tag: v5.0.0
actions_setup_python_tag: v6.2.0
datreeio_crds_catalog_tag: ""     # filled by first run of resolve_pins.sh; checked back in
docker_hub_image_repo: khaledsalhabdeveleap/hyperping-exporter
docker_hub_image_tag: "1.4.1"
YAML
```

The resolver script (full body in worktree):

```bash
#!/usr/bin/env bash
set -euo pipefail
# Auth precheck
gh auth status >/dev/null 2>&1 || { echo "FATAL: gh not authenticated. Run gh auth login." >&2; exit 1; }
# Retry helper
retry() { local n=0; until "$@"; do n=$((n+1)); [ "$n" -ge 3 ] && return 1; sleep $((2*n)); done; }
# ... per-pin lookups with retry, comparison to pins.expected.yaml, write to tests/artifacts/<pin>.txt
```

Run the resolver:
```bash
bash deploy/helm/hyperping-exporter/tests/scripts/resolve_pins.sh
ls deploy/helm/hyperping-exporter/tests/artifacts/*.txt
```
Expected: every pin file populated; resolver exits 0.

If the resolver exits non-zero for any reason other than "tag rolled forward in a known-safe direction": surface the captured stderr to the user as `USER DECISION REQUIRED:`.

- [ ] **Step 7: Commit Task 1 artifacts (resolver, pins SSOT, .gitignore)**

```bash
git add deploy/helm/hyperping-exporter/tests/scripts/resolve_pins.sh \
        deploy/helm/hyperping-exporter/tests/pins.expected.yaml \
        deploy/helm/hyperping-exporter/tests/artifacts/.gitkeep \
        .gitignore
git commit -m "chore(helm-ci): add upstream-pin resolver and SSOT (Contract C4, C7)"
```

Verify Contract C1: `python3 render_test.py && helm lint deploy/helm/hyperping-exporter/` exits 0.

---

### Task 2: Empirically observe binary resource use under chart-equivalent runtime

**Files:** Modify `deploy/helm/hyperping-exporter/values.yaml` (resources block).

The user explicitly required empirical validation against the live Hyperping API. Measurement runs inside Docker with `--read-only --user 65534:65534 --tmpfs /tmp` so the runtime envelope matches the chart's deployment exactly. This task simultaneously proves the binary tolerates `readOnlyRootFilesystem: true` (item 4); if the binary aborts or hangs, this task fails-fast.

- [ ] **Step 1: Build local Docker image**

```bash
make docker-build
docker images hyperping-exporter:dev | head -2
```

- [ ] **Step 2: Launch the binary inside Docker matching chart runtime for ≥30 minutes**

```bash
export HYPERPING_API_KEY=$(grep '^HYPERPING_API_KEY=' /home/khaledsa/projects/develeap/terraform-provider-hyperping/.env | cut -d= -f2)
docker run --rm -d --name hyperping-rss-test \
  --read-only --user 65534:65534 --tmpfs /tmp \
  -e HYPERPING_API_KEY \
  -p 9312:9312 \
  hyperping-exporter:dev
sleep 1815  # 30m + 15s buffer
```

- [ ] **Step 3: Sample peak RSS and CPU every 30s**

```bash
for i in $(seq 1 60); do
  docker stats --no-stream --format '{{.MemUsage}} {{.CPUPerc}}' hyperping-rss-test
  sleep 30
done | tee deploy/helm/hyperping-exporter/tests/artifacts/rss-cpu.txt
```

- [ ] **Step 4: Derive resource defaults**

Compute peak RSS, peak CPU. Round UP for headroom (peak RSS × 1.5 → memory limit; peak RSS × 0.5 → memory request; peak CPU × 2 → cpu limit; peak CPU × 0.5 → cpu request, with floor of `10m`).

- [ ] **Step 5: Update `values.yaml` resources block** to the measured values. Commit message references the measured peaks.

```bash
docker rm -f hyperping-rss-test
git add deploy/helm/hyperping-exporter/values.yaml
git commit -m "feat(helm): tune resources from empirical 30m observation (item 1)"
```

Verify Contract C1: `python3 render_test.py && helm lint` exits 0.

---

### Task 3: Reconcile liveness/readiness probe defaults

**Files:** Modify `deploy/helm/hyperping-exporter/values.yaml`.

User contract: liveness `/healthz` initialDelaySeconds 10, periodSeconds 10, failureThreshold 3; readiness `/readyz` slightly tighter. Current chart ships liveness 5/30 and readiness 10/15 with no `failureThreshold`.

- [ ] **Step 1: Update `values.yaml` probes block** to the user contract (liveness 10/10/3; readiness 5/5/3).

- [ ] **Step 2: Run render harness and helm lint**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
helm lint deploy/helm/hyperping-exporter/
```

- [ ] **Step 3: Commit**

```bash
git commit -am "feat(helm): reconcile probes to peer-review contract (item 2)"
```

Verify Contract C1.

---

### Task 4: Uniform safe-arg rendering (Contract C2)

**Files:** Create the `hyperping-exporter.arg` helper in `_helpers.tpl`. Modify `templates/deployment.yaml` to migrate EVERY `printf`/inline arg line. Modify `values.yaml` cacheTTL comment.

- [ ] **Step 1: Add `hyperping-exporter.arg` helper to `_helpers.tpl`** (Contract C2.1).

- [ ] **Step 2: Migrate every conditional arg line in `deployment.yaml`** to `{{ include "hyperping-exporter.arg" (list "--flag" .Values.path) }}`. Specifically: `--cache-ttl`, `--listen-address`, `--metrics-path`, `--log-level`, `--log-format`, `--web-config-file`, `--namespace`, `--exclude-name-pattern`, `--mcp-url`. The `if .Values.config.foo` conditionals wrap the helper call as before.

- [ ] **Step 3: Audit deployment.yaml for any surviving inline arg pattern** (Contract C2 gate).

```bash
grep -nE '"--[a-z-]+=' deploy/helm/hyperping-exporter/templates/deployment.yaml
# Expected: zero output beyond the include lines.
```

- [ ] **Step 4: Run render harness and helm lint**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
helm lint deploy/helm/hyperping-exporter/
```

Existing 9 cases all stay green (the helper produces byte-identical output for the values they exercise).

- [ ] **Step 5: Commit**

```bash
git commit -am "refactor(helm): consolidate arg rendering through one safe helper (Contract C2)"
```

Verify Contract C1.

---

### Task 5: Document PSS-restricted securityContext (item 4 documentation)

**Files:** Modify `deploy/helm/hyperping-exporter/values.yaml`.

PSS-restricted defaults are already shipped; this task documents them so reviewers can confirm the contract.

- [ ] **Step 1: Add block comment above `securityContext:` and `podSecurityContext:`** spelling out: `runAsNonRoot`, `runAsUser/Group: 65534`, `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, `capabilities.drop: [ALL]`, `seccompProfile: RuntimeDefault`. **Document BOTH pod-scope (`podSecurityContext`) AND container-scope (`securityContext`) blocks.** (R3-31 collapse via Contract C10.)

- [ ] **Step 2: Add comment under `image.tag`** noting chart-vs-app version independence (item 9).

- [ ] **Step 3: Add comment under `cacheTTL`** with the exact wording: `# Must be a quoted Go duration string (e.g. "60s"). Bare integers (60) abort the render with a clear error pointing to validateCacheTTL.` (Contract C2.3 reference.)

- [ ] **Step 4: Render and lint**

- [ ] **Step 5: Commit**

```bash
git commit -am "docs(helm): document PSS, version-tracking, and cacheTTL contracts (items 4, 9)"
```

Verify Contract C1.

---

### Task 6: ExternalSecret template + secret-source guards + multi-replica guard + cacheTTL guard + safe-arg cacheTTL migration (single commit)

**Files:** Create `templates/externalsecret.yaml`, three secret-conflict fixtures, `secret-source-missing.values.yaml`, `replicas-multi.values.yaml`, `cache-ttl-int-fails.values.yaml`. Modify `templates/_helpers.tpl` (add `secretSourceCount`, `validateSecretSources`, `validateReplicaCount`, `validateCacheTTL`), `templates/deployment.yaml` (replace legacy guard, gate env block), `templates/secret.yaml` (suppress on `externalSecret.enabled`), `values.yaml` (externalSecret block, secret-source-mutex doc, replicaCount-zero doc, `internal.bypassReplicaCheck: false`), `tests/render_test.py` (add `assert_fail` helper + 6 new cases).

This is the largest single commit. Contract C1.1 requires the legacy two-line `apiKey || existingSecret` guard removal AND the new validator includes to land atomically, so the `external-secret.values.yaml` and `secret-source-missing.values.yaml` fixtures don't bisect-red between any two commits.

- [ ] **Step 1: Add `assert_fail` helper to `render_test.py`** (Contract C6.1).

- [ ] **Step 2: Add four helpers to `_helpers.tpl`** (Contract C2.3, C2.4):
  - `secretSourceCount` — uses `default dict` + `kindIs "map"` guard on `.Values.externalSecret`.
  - `validateSecretSources` — `fail()` on count != 1 OR count == 0 AND `gt (int .Values.replicaCount) 0`. (`replicaCount: 0` is exempt; documented inline.)
  - `validateReplicaCount` — `fail()` on `gt (int .Values.replicaCount) 1` UNLESS `internal.bypassReplicaCheck: true` (Contract C8.1).
  - `validateCacheTTL` — `fail()` if `not (kindIs "string" .Values.config.cacheTTL)`.

- [ ] **Step 3: Modify `deployment.yaml` top** — replace the legacy 2-line guard with the three new validator includes. Use the safe-arg helper for `--cache-ttl` (this is the C2.2 migration of cacheTTL specifically; other arg migrations were in Task 4).

- [ ] **Step 4: Modify `deployment.yaml` env block** — wrap in `{{- if gt (int (include "hyperping-exporter.secretSourceCount" .)) 0 }}`. (The double `int` cast is needed because `include` returns a string.)

- [ ] **Step 5: Create `templates/externalsecret.yaml`** rendering `external-secrets.io/v1` (with values-driven `externalSecret.apiVersion` override defaulting to `external-secrets.io/v1`).

- [ ] **Step 6: Modify `templates/secret.yaml`** to suppress when `externalSecret.enabled: true`.

- [ ] **Step 7: Update `values.yaml`** with the externalSecret block, secret-source mutex docs, replicaCount-zero doc, `internal:` block with `bypassReplicaCheck: false` + DO-NOT-SET-IN-PRODUCTION warning.

- [ ] **Step 8: Create fixtures**: `external-secret.values.yaml`, `external-secret-defaults.values.yaml`, `external-secret-missing-store.values.yaml`, `replicas-zero.values.yaml`, `replicas-multi.values.yaml`, `secret-conflict-{apikey-and-existing,apikey-and-external,existing-and-external}.values.yaml`, `secret-source-missing.values.yaml`, `cache-ttl-numeric.values.yaml`, `cache-ttl-int-fails.values.yaml`, `log-level-numeric.values.yaml`, `metrics-path-with-special-chars.values.yaml`.

- [ ] **Step 9: Add new render-harness cases to `render_test.py`**:
  - Case 10: `external-secret` positive render (ExternalSecret kind present; Secret kind absent).
  - Case 11: `external-secret-defaults` (no `refreshInterval` → 1h propagates).
  - Case 12: `external-secret-missing-store` (`assert_fail` on missing `secretStoreRef.name`).
  - Case 13: `replicas-zero` positive render (Deployment `replicas: 0`; env block absent because `secretSourceCount == 0`; PDB absent).
  - Case 14: `replicas-multi` (`assert_fail` on multi-replica).
  - Cases 15, 16, 17: three secret-conflict `assert_fail` cases.
  - Case 18: `secret-source-missing` (`assert_fail` with `replicaCount: 1` and no source).
  - Case 22: `cache-ttl-numeric` positive render with `cacheTTL: "30s"` (a non-default; asserts the helper emits `"--cache-ttl=30s"`).
  - Case 23: `cache-ttl-int-fails` (`assert_fail`; bare-int cacheTTL).
  - Case 24: `log-level-numeric` (a numeric `logLevel` renders as a quoted JSON string with no `%!s(...)` artefact).
  - Case 25: `metrics-path-with-special-chars` (path containing `"` and `\` round-trips through toJson).

- [ ] **Step 10: Helper hygiene audit** (Contract C1.2):

```bash
defined=$(grep -oE '^{{- define "[^"]+' deploy/helm/hyperping-exporter/templates/_helpers.tpl | sort -u)
referenced=$(grep -RhoE 'include "hyperping-exporter\.[a-zA-Z]+"' deploy/helm/hyperping-exporter/templates/ | sort -u)
# Every name in `defined` must appear in `referenced`. Surface any orphan.
```

- [ ] **Step 11: Run full harness + lint**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
helm lint deploy/helm/hyperping-exporter/
```

- [ ] **Step 12: Commit**

```bash
git commit -am "feat(helm): ExternalSecret + secret/replica/cacheTTL guards (items 3,6,7; Contracts C1,C2,C6)"
```

Verify Contract C1.

---

### Task 7: PDB guard, NetworkPolicy default-on, Cilium FQDN variant, replicaCount:0 support, Case 1 enumeration update (single commit)

**Files:** Create `templates/networkpolicy-cilium.yaml`, seven Cilium fixtures, `pdb-structural.values.yaml`, `pdb-enabled.values.yaml`, `networkpolicy-default.values.yaml`. Modify `templates/pdb.yaml`, `templates/networkpolicy.yaml`, `values.yaml` (flip `networkPolicy.enabled` to `true`, add `fqdnRestriction` block), `tests/render_test.py` (extend Case 1; add Cilium and PDB cases).

This commit lands the NetworkPolicy default flip atomically with Case 1's `expected_versions` extension (Contract C1.1) AND introduces the Cilium variant with full peer-shape coverage (Contract C3).

- [ ] **Step 1: Update `templates/pdb.yaml`** to gate on `podDisruptionBudget.enabled AND (replicaCount > 1 OR internal.bypassReplicaCheck)` (Contract C8).

- [ ] **Step 2: Targeted text-matched edit of `templates/networkpolicy.yaml`** (Contract C1.3):
  - Replace `{{- if .Values.networkPolicy.enabled }}` (top of file) with `{{- if and .Values.networkPolicy.enabled (not (and .Values.networkPolicy.fqdnRestriction .Values.networkPolicy.fqdnRestriction.enabled)) }}`.
  - Replace the egress 443 rule's `- to:` block. **The full original text to be replaced** is (the executor matches this unique substring; if absent or duplicated, the task stops):

    ```yaml
        - to:
            - ipBlock:
                cidr: 0.0.0.0/0
                except:
                  - 10.0.0.0/8
                  - 172.16.0.0/12
                  - 192.168.0.0/16
          ports:
            - protocol: TCP
              port: 443
    ```
    Replacement:
    ```yaml
        - to:
            - ipBlock:
                cidr: 0.0.0.0/0
          ports:
            - protocol: TCP
              port: 443
    ```
    The `ports:` key appears EXACTLY ONCE in the replacement; the executor confirms with `grep -c '^          ports:' templates/networkpolicy.yaml` post-edit (Contract C1.3 atomicity, R3-21 collapse).

- [ ] **Step 3: Create `templates/networkpolicy-cilium.yaml`** implementing Contract C3 conversion. Full template body (peer-shape walk with `matchLabels` + `matchExpressions`; well-known namespace label handling; `fail()` on unsupported shapes). Cilium label prefix: `k8s:io.kubernetes.pod.namespace` for the well-known name; explicit `fail()` for any other namespaceSelector label.

- [ ] **Step 4: Update `values.yaml`** — flip `networkPolicy.enabled: false → true`; add `fqdnRestriction.enabled: false`, `allowedHosts: [api.hyperping.io]`. Document mutual exclusion and Cilium 1.14+ requirement.

- [ ] **Step 5: Create Cilium fixtures (seven)** per Contract C3 gate.

- [ ] **Step 6: Create `pdb-structural.values.yaml`** with `internal.bypassReplicaCheck: true`, `podDisruptionBudget.enabled: true`. Create `pdb-enabled.values.yaml` (default `replicaCount: 1`, `podDisruptionBudget.enabled: true`; asserts PDB suppressed). Create `networkpolicy-default.values.yaml` (`apiKey: x` only; default NP rendered).

- [ ] **Step 7: Extend `render_test.py` Case 1** to use a set-difference enumeration with per-kind diff message (Contract C8.3, R3-13 collapse). Include `NetworkPolicy` in the expected set (default-on).

- [ ] **Step 8: Audit pre-existing fixtures** are still green after the NP default flip. None of the 9 pre-existing render-harness cases asserts on the rendered kinds (they assert on `deployment_args` and `assert_scalars_clean`); the new NP is silently tolerated. Confirm by running `python3 render_test.py` to a passing result.

- [ ] **Step 9: Add new render-harness cases**:
  - Case 19: `networkpolicy-default` positive render (NetworkPolicy kind present; Cilium absent).
  - Case 20: `pdb-enabled` (PDB absent — gate suppresses on replicaCount=1).
  - Case 21: `pdb-structural` (PDB rendered with internal bypass; asserts apiVersion, kind, `maxUnavailable: 1`, selector).
  - Case 29: `pdb-structural-internal-bypass-without-pdb-fails` — sets `internal.bypassReplicaCheck: true` WITHOUT `podDisruptionBudget.enabled: true` AND `replicaCount: 2`; `assert_fail` (validator still aborts because bypass only applies to the PDB gate, not to the validator's normal path; this prevents bypass leakage).
  - Case 30: `cilium-egress-only`.
  - Case 31: `cilium-ingress-matchlabels`.
  - Case 32: `cilium-ingress-matchexpressions`.
  - Case 33: `cilium-ingress-mixed`.
  - Case 34: `cilium-ipblock-only-fails` (`assert_fail`).
  - Case 35: `cilium-selectorless-fails` (`assert_fail`).
  - Case 36: `cilium-foreign-ns-label-fails` (`assert_fail`).

- [ ] **Step 10: Run full harness + lint**

- [ ] **Step 11: Commit**

```bash
git commit -am "feat(helm): NP default-on + Cilium variant + PDB + replicas:0 (items 5,7,8; Contracts C3,C8)"
```

Verify Contract C1.

---

### Task 8: kubeconform schema validation + kind PSS admission CI

**Files:** Create `tests/admission_env.sh`, `tests/admission_test.sh`, `tests/kind-pss.yaml`, `tests/kind-pss-config/admission-config.yaml`. Modify `.github/workflows/helm-ci.yml`, `Makefile`.

This task adds the CI-side admission proof and the kubeconform offline schema gate. It does NOT change any production template.

- [ ] **Step 1: Create `tests/admission_env.sh`** with KIND_CLUSTER_NAME, TEST_RELEASE_NAME, TEST_NAMESPACE, LIVE_BOOT_IMAGE exports. Sourced by every consumer (Contract C5.1).

- [ ] **Step 2: Create `tests/kind-pss.yaml`** with `__KINDEST_VERSION__` and `__KINDEST_DIGEST__` placeholders (Contract C4 substitution). Reference `__ABSPATH__` for the admission-config `hostPath`.

- [ ] **Step 3: Create `tests/admission_test.sh`** with:
  - `source admission_env.sh`.
  - Dispatch table: `pss-restricted` → live boot + rollout-status; `networkpolicy-default` → live boot + rollout-status; `external-secret` → SKIP (kubeconform validates separately); `networkpolicy-cilium` → SKIP. (Contract C1.4, R3-15 collapse.)
  - Between-fixture cleanup with `kubectl wait --for=delete` (Contract C5.2, R3-12 collapse).
  - Idempotent cluster create.

- [ ] **Step 4: Create `.github/workflows/helm-ci.yml`** with named steps:
  1. `Resolve pins` — runs `tests/scripts/resolve_pins.sh` (Contract C4).
  2. `Lint and render` — `helm lint` + `python3 render_test.py`.
  3. `Kubeconform` — runs against every passing fixture (Contract C5.3, with `KUBECONFORM_CATALOG_REF` from pins).
  4. `Rewrite kind-pss.yaml placeholders` — `sed -i` substitution for `__KINDEST_VERSION__`, `__KINDEST_DIGEST__`, `__ABSPATH__`; post-substitution audit grep aborts on any surviving placeholder.
  5. `PSS admission` — `helm/kind-action@<resolved-SHA>` (no `version:` input — uses bundled kind; R3-29 collapse) + `tests/admission_test.sh`.
  6. `Audit pins` — grep workflow + kind-pss.yaml for any literal anchor value not sourced from `admission_env.sh` or `pins.expected.yaml`. Fails on drift.

- [ ] **Step 5: Create `Makefile` targets** (Contract C5.4):
  - `helm-render` — runs `python3 render_test.py`.
  - `helm-kubeconform` — runs kubeconform against every passing fixture (using `KUBECONFORM_CATALOG_REF`).
  - `helm-pss` — runs `bash tests/admission_test.sh` against the full dispatch table.
  - `helm-pss-clean` — `kind delete cluster --name "${KIND_CLUSTER_NAME}"`.
  - `helm-ci-fast` — depends on `helm-render` + `helm-kubeconform` (no PSS).
  - `helm-ci` — depends on `helm-ci-fast` + `helm-pss`.

- [ ] **Step 6: Run `make helm-ci-fast` locally** to validate the kubeconform path. Run `make helm-pss` (requires Docker + kind) to validate the admission path.

- [ ] **Step 7: Audit committed files for placeholder leak** (Contract C4 gate):

```bash
grep -RhE '__[A-Z_]+__' .github/workflows/helm-ci.yml deploy/helm/hyperping-exporter/tests/kind-pss.yaml || echo "OK no placeholder leaks"
```

- [ ] **Step 8: Commit**

```bash
git commit -am "ci(helm): kubeconform + kind PSS admission (Contracts C1.4, C4, C5)"
```

Verify Contract C1.

---

### Task 9: Chart.yaml bump + EXPECTED constants bump + CHANGELOG entry (single commit)

**Files:** Modify `Chart.yaml`, `tests/render_test.py`, `CHANGELOG.md`.

This commit atomically bumps every anchor that depends on the chart version. Contract C1 requires these to land together.

- [ ] **Step 1: Update `Chart.yaml`**: `version: 1.1.0 → 1.5.0`, `appVersion: "1.4.0" → "1.4.1"`.

- [ ] **Step 2: Update `render_test.py`** anchor constants:
  - `EXPECTED_VERSION = "1.4.1"`
  - `EXPECTED_IMAGE_DEFAULT = "khaledsalhabdeveleap/hyperping-exporter:1.4.1"`
  - `EXPECTED_CHART_LABEL = "hyperping-exporter-1.5.0"`
  - **Audit every assertion message** for hard-coded `"1.4.0"` or `"1.1.0"` literals; convert to f-string interpolation of `EXPECTED_VERSION` / `EXPECTED_CHART_LABEL` (Contract C10.4, R3-18 collapse).

- [ ] **Step 3: Extend Case 1** to assert the `helm.sh/chart` label equals `EXPECTED_CHART_LABEL` on every labelled resource.

- [ ] **Step 4: Add `CHANGELOG.md` entry**: `## [1.5.0] - 2026-05-12 [Chart only — binary unchanged]` between `[Unreleased]` and `[1.4.1]` (Contract C9.1). Sections: Highlights, Added, Changed, Security (empty), Upgrade notes.

- [ ] **Step 5: Audit CHANGELOG**: `grep -c '^## \[1\.5\.0\]' CHANGELOG.md` returns 1; `grep -c '^## \[1\.4\.1\]'` returns 1. Both dated 2026-05-12. (Contract C9 gate.)

- [ ] **Step 6: Run full harness + lint + kubeconform**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
helm lint deploy/helm/hyperping-exporter/
make helm-kubeconform
```

- [ ] **Step 7: Commit**

```bash
git commit -am "feat(helm): bump chart to 1.5.0 / appVersion 1.4.1 (Contract C9, C10)"
```

Verify Contract C1.

---

### Task 10: Final assertion sweep, plan-consistency audit, full suite

**Files:** Modify `tests/render_test.py` (any final assertion polish only). No production-template changes.

- [ ] **Step 1: Plan-consistency audit** (Contract C10 gate):

```bash
test "$(grep -c '^### Task ' docs/plans/2026-05-12-chart-hardening-prod-defaults.md)" -eq 11 || { echo "FAIL task count"; exit 1; }
# No hard-coded version literals in test code:
grep -nE '"1\.4\.0"|"1\.4\.0 on every|"1\.1\.0[^.]"' deploy/helm/hyperping-exporter/tests/render_test.py && { echo "FAIL hardcoded version literal"; exit 1; } || echo "OK"
# No 2>&1 | grep PASS pattern:
grep -RhnE '2>&1 \| grep .* && echo PASS' deploy/helm/hyperping-exporter/tests/ && { echo "FAIL grep-only smoke"; exit 1; } || echo "OK"
```

- [ ] **Step 2: Coverage audit** (Contract C8 gate):

```bash
for tpl in $(find deploy/helm/hyperping-exporter/templates/ -name '*.yaml' -not -name 'NOTES.txt' -not -name '_helpers.tpl' -exec basename {} \;); do
  helm template testrel deploy/helm/hyperping-exporter/ -f deploy/helm/hyperping-exporter/tests/fixtures/default.values.yaml 2>/dev/null | grep -q "^# Source: hyperping-exporter/templates/$tpl" \
    || helm template testrel deploy/helm/hyperping-exporter/ -f deploy/helm/hyperping-exporter/tests/fixtures/pdb-structural.values.yaml 2>/dev/null | grep -q "^# Source: hyperping-exporter/templates/$tpl" \
    || helm template testrel deploy/helm/hyperping-exporter/ -f deploy/helm/hyperping-exporter/tests/fixtures/external-secret.values.yaml 2>/dev/null | grep -q "^# Source: hyperping-exporter/templates/$tpl" \
    || helm template testrel deploy/helm/hyperping-exporter/ -f deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-cilium-defaults.values.yaml 2>/dev/null | grep -q "^# Source: hyperping-exporter/templates/$tpl" \
    || { echo "FAIL template $tpl not covered"; exit 1; }
done
echo "OK every template covered by at least one fixture"
```

- [ ] **Step 3: Helper hygiene audit** (Contract C1.2 gate):

```bash
defined=$(grep -oE '^{{- define "[^"]+' deploy/helm/hyperping-exporter/templates/_helpers.tpl | sed 's/.*"//' | sort -u)
referenced=$(grep -RhoE 'include "[^"]+' deploy/helm/hyperping-exporter/templates/ | sed 's/.*"//' | sort -u)
for d in $defined; do
  grep -qF "$d" <<<"$referenced" || { echo "FAIL orphan helper: $d"; exit 1; }
done
echo "OK no orphan helpers"
```

- [ ] **Step 4: Bisect rebase rehearsal** (Contract C1 test):

```bash
git rebase --exec 'python3 deploy/helm/hyperping-exporter/tests/render_test.py && helm lint deploy/helm/hyperping-exporter/ && make helm-kubeconform' origin/main
```
Expected: every commit on the branch exits 0 across all three gates.

- [ ] **Step 5: Run full suite**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
helm lint deploy/helm/hyperping-exporter/
make helm-kubeconform
go test ./... -race -count=1
```

- [ ] **Step 6: Commit any polish from steps 1-3** (likely empty; if test code needed an interpolation fix the steps above caught it). If any commits land here, re-run Step 4 bisect rehearsal.

---

### Task 11: Open the PR

**Files:** None.

- [ ] **Step 1: Push the branch**

```bash
git push -u origin chore/chart-hardening-prod-defaults
```

- [ ] **Step 2: Pre-PR branch-protection sanity check**

```bash
gh api repos/develeap/hyperping-exporter/branches/main/protection --jq '.required_status_checks.contexts' 2>/dev/null || echo "no protection rules (admin or first-time)"
```
If a rule names the `helm` job context, reconcile.

- [ ] **Step 3: Open the PR via `gh pr create`** with title `chore(helm): production-readiness defaults for v1.5.0 chart` and body taken from CHANGELOG's new section plus the Strategy Gate digest.

- [ ] **Step 4: Watch CI**

```bash
gh pr checks --watch
```
Expected: the `helm` job (with `Resolve pins`, `Lint and render`, `Kubeconform`, `Rewrite kind-pss.yaml placeholders`, `PSS admission`, `Audit pins` steps) passes.

- [ ] **Step 5: Report PR URL. Do NOT merge.**

---

## Risks and Cutover Notes

- **Bisect-green contract:** the rebase rehearsal in Task 10 Step 4 is mandatory. If any commit on the branch fails any gate (`render_test.py`, `helm lint`, `kubeconform`), the offending commit MUST be amended or restructured before the PR opens. We don't ship a branch where `git bisect` would land on a broken commit.
- **`kind-pss.yaml` placeholder substitution timing:** the `Rewrite kind-pss.yaml placeholders` workflow step MUST run BEFORE `helm/kind-action`. The audit step (`grep '__'`) catches any leak. Locally, `admission_test.sh` does the equivalent substitution on a temp copy so the committed file stays clean.
- **kubeconform CRDs catalog stability:** `KUBECONFORM_CATALOG_REF` is pinned to a TAGGED RELEASE captured at Task 1 Step 6 (NOT `main`). A future schema rev for ExternalSecret or CiliumNetworkPolicy is a deliberate update, not a CI surprise.
- **kind action vs binary version compatibility:** the workflow does NOT pass `version:` to `helm/kind-action`; it uses the bundled binary (R3-29). The resolver records the bundled version in `tests/artifacts/` for traceability.
- **Docker Hub image precheck:** the resolver verifies `khaledsalhabdeveleap/hyperping-exporter:1.4.1` exists before any admission attempt (R3-11 collapse). If the binary tag isn't published yet, surface `USER DECISION REQUIRED:`.
- **PSS rejection for fields the chart doesn't set:** the chart never sets `hostNetwork`, `hostPID`, `procMount`, sysctl, AppArmor. If a future change accidentally adds `hostPort`, the PSS admission job catches it.
- **`replicaCount: 0` integer rendering:** Helm/Go YAML serialises the integer literally, producing `replicas: 0`. Verified by Case 13.
- **External Secrets Operator presence at apply time:** the chart cannot detect operator presence at render time; ExternalSecret apply fails with `no matches for kind` on clusters without ESO. Admission `external-secret` fixture is SKIPPED from the live job and validated by `kubeconform` only (which doesn't need CRDs installed). This is the C1.4 dispatch policy.
- **Cilium operator presence at apply time:** same trade-off. `networkpolicy-cilium` fixture is SKIPPED from live admission and validated by kubeconform only.
- **Branch-protection identifier:** the workflow keeps the `helm` job key, preserving any rule that matches by job-id. If a rule matches by `name:` (the human-readable `Helm chart CI`), Task 11 Step 2's audit surfaces it before merge.
- **Backwards-compat:** no existing value name renamed. New keys (`externalSecret`, `networkPolicy.fqdnRestriction`, `internal`) are additive. The new `fail()` guards intentionally reject previously-broken configurations; that's the point.
- **PSS namespace labelling:** the chart does NOT create or label the target Namespace. Operators must label `pod-security.kubernetes.io/enforce=restricted` themselves; the upgrade notes spell this out.
- **NetworkPolicy semantic correctness** (does the policy actually permit traffic to api.hyperping.io?) is NOT verified by kubeconform or PSS. The egress rule is constructed to match the documented contract; manual smoke verification post-merge is the operator's responsibility for FQDN-restricted Cilium installs.
- **Helm-to-ESO upgrade:** requires a brief window where the chart-managed Secret is deleted and ESO reconciles a new one; staged cutover guidance is in the CHANGELOG upgrade notes.

---

## Why this plan is sound

- **Ten contracts (C1-C10) collapse 38 round-3 findings into durable rules.** Findings are an audit checklist, not a patch list. New contributors enforcing the contracts will not reintroduce the original defects.
- **One PR, ten items, single cutover.** All ten production-readiness items target the same chart and share the same test harness. The single-PR scope is the right size.
- **One safe-arg rendering path.** The cacheTTL footgun cannot exist in any other arg because there is only one rendering path; the gate audit fails the task on any surviving inline form.
- **One pin resolver, one SSOT, one fail-loud policy.** Every external resolution goes through `resolve_pins.sh`; tag drift is loud; auth slips are caught at precheck; transient failures are retried with backoff.
- **One CI cluster lifecycle.** Cluster, namespace, release, image names live in `admission_env.sh`; never duplicated. Per-fixture cleanup waits for `--for=delete` so SSA-on-terminating collisions cannot occur.
- **One dispatch table for admission vs kubeconform.** Fixtures requiring CRDs are validated by kubeconform (which doesn't need CRDs installed); live-boot fixtures are minimal and prove the runtime readOnlyRootFilesystem contract.
- **One Cilium conversion specification.** Peer shapes the chart accepts are explicitly enumerated; unsupported shapes abort with a clear error. `matchExpressions` is first-class. Cilium label prefix is documented per minor version.
- **One single-source-of-truth per anchor.** Versions, dates, names, message strings all derive from one place. Hard-coded literals in test code are forbidden and gated by audit.
- **Empirical resource numbers** measured under chart-equivalent runtime (Docker `--read-only`, uid 65534) — also doubles as the runtime read-only-rootfs proof.
- **`replicaCount: 0` is a fully-supported state.** Render `replicas: 0`, suppress PDB, gate the env block on `secretSourceCount > 0`, exempt from `validateSecretSources`. Scale-up without configuring a source re-trips the guard cleanly.
- **Bisect-green is enforced, not promised.** Task 10's `git rebase --exec` rehearsal makes the contract a CI-runnable check.
- **Coverage is enforced, not promised.** Task 10's coverage audit fails the task if any production template lacks a rendering test, including PDB (the structural-bypass fixture covers it).
- **Plan-text consistency is enforced, not promised.** Task 10's plan-consistency audit fails the task on duplicate task numbers, hard-coded version literals in test code, or grep-only smoke tests.
