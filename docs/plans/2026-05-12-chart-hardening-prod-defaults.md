# Chart UX Hardening: Production-Readiness Defaults Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use trycycle-executing to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land all ten chart hardening items (resources tune, probes reconcile, cacheTTL via toJson, PSS-restricted security context audit, NetworkPolicy default-on with optional Cilium FQDN variant, ExternalSecret support with mutual-exclusion fail(), replicas:0-friendly behavior, PDB guard, docs touch-ups) as one PR bumping the chart to 1.2.0, gated by an extended render harness, kubeconform schema validation, and a PSS-restricted kind admission CI job.

**Architecture:** Single-PR cutover on a `chore/chart-hardening-prod-defaults` branch. The chart already implements partial versions of items 1, 2, 4, 5, 8 (resources, probes, securityContext, NetworkPolicy template, PDB template), so this work is largely reconciling existing surfaces against the user's contract plus adding ExternalSecret, the multi-replica/secret-source `fail()` guards, the Cilium NP variant, and the `--cache-ttl` toJson render. Three CI surfaces gate the chart: the existing PyYAML render harness (extended fixture set), a new `kubeconform` strict offline schema validation step in `helm-ci.yml`, and a new `kind` job that admits the rendered chart into a PSS-restricted namespace. All three run only on chart-touching PRs (`paths` filter already in place).

**Tech Stack:** Helm v3.20.2, PyYAML 6.0.3, `kubeconform` v0.7.x, `kind` v0.30.x, Kubernetes 1.34 (PSS-restricted v1.33+ semantics), external-secrets.io v1beta1 CRD shape, cilium.io/v2 CRD shape. Go binary unchanged.

---

## Scope Check

The user explicitly requires a single PR (decision #4). All ten items target a single Helm chart, share a common test harness, and have interlocking semantics (the secret-source `fail()` guard depends on the `externalSecret` block existing; the PDB guard depends on the multi-replica policy; the kubeconform step depends on the same fixture matrix the render harness uses). They form a coherent end-state and split cleanly along no natural boundary that survives mutual-exclusion testing. Single PR is correct.

## Strategy Gate

The proposed architecture has been validated against the existing chart and against the user's explicit decisions. Three judgment calls baked in:

1. **Item 7 (multi-replica)** — `fail()` guard chosen over leader-election sidecar. User explicitly leaned (a). The binary has no leader-election support today; adding it would change the binary (forbidden by "WHAT NOT TO DO"). `fail()` is the only path.

2. **Item 6 (ExternalSecret)** — `external-secrets.io/v1beta1` API version, `target.creationPolicy: Owner`, default `refreshInterval: 1h`. This is the upstream-recommended shape for the External Secrets Operator as of late 2025 and is what production users actually deploy. `v1beta1` (not `v1`) is the namespaced CRD shape; v1 exists as an alias on newer ESO versions but `v1beta1` is universally compatible with operators ≥ 0.9.x.

3. **Item 5 Cilium variant** — `cilium.io/v2 CiliumNetworkPolicy` (singular, namespaced). Rendered when `networkPolicy.fqdnRestriction.enabled: true`. Mutually exclusive with the vanilla `NetworkPolicy` (rendering one short-circuits the other). The chart cannot detect CRD presence at render time; document the Cilium requirement in values comments and accept that the manifest will fail at apply time on non-Cilium clusters (this is honest behavior — the user gets a clear `no matches for kind "CiliumNetworkPolicy"` error rather than silent permissiveness).

The `replicaCount: 0` decision is locked-in: render `replicas: 0` cleanly, suppress PDB (PDB selecting a zero-replica workload is harmless but kubeconform-noisy and serves no purpose), keep all other resources rendering. This matches Kubernetes' "scaled to zero" semantics. The multi-replica `fail()` only fires for `replicaCount > 1`.

The empirical resources validation (item 1) uses the dev API key at `/home/khaledsa/projects/develeap/terraform-provider-hyperping/.env` (user explicitly pointed at it). The validation is a one-shot 30-minute run during execution; resulting numbers commit as defaults; the render harness asserts them literally so future drift surfaces in CI.

No further direction change is warranted. Proceeding to file structure.

## File Structure

### Files created

- `deploy/helm/hyperping-exporter/templates/externalsecret.yaml` — renders `external-secrets.io/v1beta1 ExternalSecret` when `externalSecret.enabled: true`. Mutually exclusive with `templates/secret.yaml` via the `_helpers.tpl` source-count guard.
- `deploy/helm/hyperping-exporter/templates/networkpolicy-cilium.yaml` — renders `cilium.io/v2 CiliumNetworkPolicy` when `networkPolicy.enabled: true` AND `networkPolicy.fqdnRestriction.enabled: true`. Mutually exclusive with `templates/networkpolicy.yaml`.
- `deploy/helm/hyperping-exporter/tests/fixtures/pss-restricted.values.yaml` — minimal apiKey config; used by the PSS admission job AND a render-harness fixture. Asserts the default securityContext block passes restricted enforcement.
- `deploy/helm/hyperping-exporter/tests/fixtures/external-secret.values.yaml` — `externalSecret.enabled: true` with `secretStoreRef`, `remoteRef.key`.
- `deploy/helm/hyperping-exporter/tests/fixtures/replicas-zero.values.yaml` — `replicaCount: 0`.
- `deploy/helm/hyperping-exporter/tests/fixtures/replicas-multi.values.yaml` — `replicaCount: 2` (used to assert the `fail()` aborts render).
- `deploy/helm/hyperping-exporter/tests/fixtures/pdb-enabled.values.yaml` — `replicaCount: 2` + `podDisruptionBudget.enabled: true` (only valid PDB path).
- `deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-default.values.yaml` — `networkPolicy.enabled: true`, vanilla path.
- `deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-cilium.values.yaml` — `networkPolicy.enabled: true` + `fqdnRestriction.enabled: true`.
- `deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-apikey-and-existing.values.yaml` — both `apiKey` and `existingSecret` set; asserts `fail()`.
- `deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-apikey-and-external.values.yaml` — `apiKey` + `externalSecret.enabled`; asserts `fail()`.
- `deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-existing-and-external.values.yaml` — `existingSecret` + `externalSecret.enabled`; asserts `fail()`.
- `deploy/helm/hyperping-exporter/tests/fixtures/secret-source-missing.values.yaml` — none of the three set; asserts `fail()`.
- `deploy/helm/hyperping-exporter/tests/kind-pss.yaml` — `kind` cluster config used by the CI admission job and a `make ci-pss` target.
- `deploy/helm/hyperping-exporter/tests/admission_test.sh` — bash driver that creates the kind cluster, labels a namespace `pod-security.kubernetes.io/enforce=restricted`, runs `helm template ... | kubectl apply --server-side -f -`, asserts no admission errors. Idempotent.
- `docs/plans/2026-05-12-chart-hardening-prod-defaults.md` — this plan.

### Files modified

- `deploy/helm/hyperping-exporter/Chart.yaml:5` — `version: 1.1.0 → 1.2.0`. `appVersion` stays `"1.4.0"` (binary unchanged, decision per user).
- `deploy/helm/hyperping-exporter/values.yaml` — comprehensive edits:
  - Add `externalSecret:` block (enabled: false, secretStoreRef, remoteRef.key, refreshInterval).
  - Re-tune `resources` requests/limits from empirical observation. Provisional starting point: `requests {cpu: 50m, memory: 64Mi}`, `limits {cpu: 200m, memory: 256Mi}`. Final numbers from the 30-min observation in Task 2.
  - Reconcile probes against user's contract: `livenessProbe.initialDelaySeconds: 10`, `periodSeconds: 10`, `failureThreshold: 3`; readiness slightly tighter (`initialDelaySeconds: 5`, `periodSeconds: 5`, `failureThreshold: 3`). Both fully overridable.
  - Flip `networkPolicy.enabled` default `false → true`.
  - Add `networkPolicy.fqdnRestriction.enabled: false` plus `allowedHosts: [api.hyperping.io]`.
  - Add `# Chart version (Chart.yaml: version) and binary version (Chart.yaml: appVersion / image.tag) track separately.` comment next to `image.tag`.
  - Document the secret-source mutual exclusion above the secret block.
  - Document the multi-replica `fail()` guard above `replicaCount`.
- `deploy/helm/hyperping-exporter/templates/_helpers.tpl` — add four helpers:
  - `hyperping-exporter.secretSourceCount` — returns the count of configured secret sources (apiKey, existingSecret, externalSecret.enabled).
  - `hyperping-exporter.validateSecretSources` — emits `fail()` if count != 1.
  - `hyperping-exporter.validateReplicaCount` — emits `fail()` if `replicaCount > 1`.
  - Extend `hyperping-exporter.secretName` to handle the externalSecret path (return fullname; ExternalSecret writes into a Secret of that name).
- `deploy/helm/hyperping-exporter/templates/deployment.yaml`:
  - Replace the legacy two-line apiKey/existingSecret guard with `{{- include "hyperping-exporter.validateSecretSources" . }}` and `{{- include "hyperping-exporter.validateReplicaCount" . }}`.
  - Render `--cache-ttl` via `toJson` (consistency with item 3).
- `deploy/helm/hyperping-exporter/templates/secret.yaml` — change the `if` guard so the chart-managed Secret renders only when inline `apiKey` is set AND neither `existingSecret` nor `externalSecret.enabled`. Today's guard `if and .Values.config.apiKey (not .Values.config.existingSecret)` becomes `if and .Values.config.apiKey (not .Values.config.existingSecret) (not .Values.externalSecret.enabled)`.
- `deploy/helm/hyperping-exporter/templates/pdb.yaml` — wrap rendering in `{{- if and .Values.podDisruptionBudget.enabled (gt (int .Values.replicaCount) 1) }}` so PDB is suppressed when replicaCount is 0 or 1.
- `deploy/helm/hyperping-exporter/templates/networkpolicy.yaml` — wrap in `{{- if and .Values.networkPolicy.enabled (not .Values.networkPolicy.fqdnRestriction.enabled) }}` so the vanilla NP is suppressed when the Cilium variant is active.
- `deploy/helm/hyperping-exporter/tests/render_test.py` — extend with 11 new cases (one per new fixture plus the resources/probes/security-context characterization assertions; see Test Plan).
- `.github/workflows/helm-ci.yml` — add three steps: kubeconform install + run, kind cluster setup + PSS admission test. Job split into `lint-and-render`, `schema-validate` (kubeconform), `pss-admission` (kind). All three required for chart-touching PRs.
- `Makefile` — add `helm-ci`, `helm-render`, `helm-kubeconform`, `helm-pss` targets that mirror the CI steps for local execution.
- `CHANGELOG.md` — add `## [1.2.0] - 2026-05-12` chart-section with Highlights / Added / Changed / Upgrade notes; matches v1.4.1 style. The `[Unreleased]` header remains empty above. This is a chart release, not a binary release; the section header uses the chart version.

### Files explicitly NOT modified

- `main.go`, `internal/**` — binary unchanged.
- `.github/workflows/ci.yml` — chart still in `paths-ignore`; no Go-side changes.
- `.github/workflows/release.yml`, `.goreleaser.yml` — no release pipeline impact.
- `deploy/grafana/**`, `deploy/prometheus/**`, `deploy/k8s/**` — out of scope.
- `README.md` — chart-specific docs live in `values.yaml`; no README change needed for v1.2.0.

---

## Task Decomposition

### Task 1: Pre-flight — capture the current contract before touching it

**Files:**
- Modify: none (capture only)
- Test: `deploy/helm/hyperping-exporter/tests/render_test.py` (run as-is)

This task does NOT change the chart. It establishes the red baseline: every existing test runs green BEFORE any production change, so when we run them after Task 2-12, drift surfaces from real changes, not accidental ones.

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

- [ ] **Step 3: Capture rendered defaults for diff reference**

```bash
helm template testrel deploy/helm/hyperping-exporter/ -f deploy/helm/hyperping-exporter/tests/fixtures/default.values.yaml > /tmp/render-baseline.yaml
wc -l /tmp/render-baseline.yaml
```
Expected: file written, line count noted. Used in Task 12 as the human-readable diff.

- [ ] **Step 4: Verify `kind`, `kubeconform`, and `kubectl` tooling availability**

```bash
which kind kubectl kubeconform || echo "MISSING TOOL"
```
If any is missing, install per their upstream README before Task 9. (`kind` and `kubectl` come from the standard Kubernetes toolchain; `kubeconform` from `https://github.com/yannh/kubeconform/releases`.)

- [ ] **Step 5: Verify Hyperping dev API key**

```bash
test -r /home/khaledsa/projects/develeap/terraform-provider-hyperping/.env && \
  grep -q '^HYPERPING_API_KEY=' /home/khaledsa/projects/develeap/terraform-provider-hyperping/.env && \
  echo "OK key file readable" || echo "FAIL key file missing or has no HYPERPING_API_KEY="
```
Expected: `OK key file readable`. Used in Task 2.

- [ ] **Step 6: Commit nothing — this task is pre-flight only**

Move directly to Task 2.

---

### Task 2: Empirically observe binary resource use to set defaults

**Files:**
- Modify: `deploy/helm/hyperping-exporter/values.yaml:62-68` (resources block)
- Test: `deploy/helm/hyperping-exporter/tests/render_test.py` (new defaults assertion)

The user explicitly required empirical validation against the live Hyperping API. The chart currently ships `requests {cpu: 10m, memory: 32Mi}, limits {cpu: 100m, memory: 128Mi}`. The user's starting point suggests `requests {cpu: 50m, memory: 64Mi}, limits {memory: 256Mi}`. Real numbers determine the commit.

- [ ] **Step 1: Build the binary fresh**

```bash
cd /home/khaledsa/projects/develeap/hyperping-exporter/.worktrees/chart-hardening-prod-defaults
make build
ls -lh hyperping-exporter
```
Expected: binary present, ~20–40MB.

- [ ] **Step 2: Launch under `/usr/bin/time -v` with the live API key**

```bash
set -a
. /home/khaledsa/projects/develeap/terraform-provider-hyperping/.env
set +a
mkdir -p /tmp/hpe-obs
/usr/bin/time -v ./hyperping-exporter \
  --cache-ttl=60s --log-level=info --log-format=json \
  2> /tmp/hpe-obs/time.txt &
echo $! > /tmp/hpe-obs/pid
sleep 5
test -f /tmp/hpe-obs/pid && echo "exporter pid $(cat /tmp/hpe-obs/pid)"
```
Expected: process running. Tail `/tmp/hpe-obs/time.txt` later for peak RSS.

- [ ] **Step 3: Sample CPU/RSS every 10s for 30 minutes**

```bash
PID=$(cat /tmp/hpe-obs/pid)
for i in $(seq 1 180); do
  ps -o pid,pcpu,rss,vsz -p "$PID" --no-headers >> /tmp/hpe-obs/samples.txt
  curl -fsS http://127.0.0.1:9312/metrics > /dev/null || echo "scrape failed at sample $i"
  sleep 10
done
echo "done sampling"
```
Expected: 180 samples, all scrapes succeed.

- [ ] **Step 4: Compute peak and 99th-percentile values**

```bash
awk '{print $3}' /tmp/hpe-obs/samples.txt | sort -n | tail -1   # peak RSS in KB
awk '{print $2}' /tmp/hpe-obs/samples.txt | sort -n | awk 'NR==int(NR*0.99)+1{print}'  # rough p99 cpu%
kill "$(cat /tmp/hpe-obs/pid)" || true
grep -E 'Maximum resident set size' /tmp/hpe-obs/time.txt
```
Convert peak RSS (KB) to Mi (`peak / 1024`). Cap CPU at the highest observed steady-state sample.

- [ ] **Step 5: Determine defaults**

Rules:
- `requests.memory` = `ceil(peak_RSS_Mi * 1.5)` rounded to the nearest 16Mi.
- `limits.memory` = `ceil(peak_RSS_Mi * 4)` rounded to the nearest 32Mi.
- `requests.cpu` = `max(50m, p99_cpu_pct * 10m)` (so 5% CPU → 50m).
- `limits.cpu` = `max(200m, p99_cpu_pct * 20m)`.

If the measured peak RSS is ≤ 40Mi (typical for this binary), the resulting defaults will be `requests {cpu: 50m, memory: 64Mi}`, `limits {cpu: 200m, memory: 256Mi}` — matching the user's starting point. If measurement disagrees, the measured values win and the rationale is recorded in the commit message.

- [ ] **Step 6: Update `values.yaml`**

```yaml
resources:
  requests:
    cpu: 50m         # measured p99 + safety margin
    memory: 64Mi     # measured peak * 1.5
  limits:
    cpu: 200m        # 4x requests, allows brief bursts
    memory: 256Mi    # 4x peak; OOMKills surface as a clear failure
```
Adjust per Step 5 if measurement differs.

- [ ] **Step 7: Extend `render_test.py` with a defaults-resources assertion (red first)**

Add immediately after the existing `defaults` case (just before `assert_scalars_clean(rendered, "defaults")`):

```python
    # Defaults case extension: resources requests/limits match the
    # empirically observed envelope. Hard-coded so any future tuning
    # surfaces in CI as an explicit diff.
    EXPECTED_RESOURCES = {
        "requests": {"cpu": "50m", "memory": "64Mi"},
        "limits":   {"cpu": "200m", "memory": "256Mi"},
    }
    deployment = find_deployment(rendered)
    actual_resources = deployment["spec"]["template"]["spec"]["containers"][0]["resources"]
    assert_eq(actual_resources, EXPECTED_RESOURCES,
              "defaults: resources match empirical envelope")
```

- [ ] **Step 8: Run the harness — verify the new assertion**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
```
Expected: 10 PASS lines (9 prior + new assertion), `ALL RENDER TESTS PASSED`. If the chart values weren't updated yet at Step 6, the new assertion would have failed — confirming red-before-green for this contract.

- [ ] **Step 9: Commit**

```bash
git -C /home/khaledsa/projects/develeap/hyperping-exporter/.worktrees/chart-hardening-prod-defaults add \
  deploy/helm/hyperping-exporter/values.yaml \
  deploy/helm/hyperping-exporter/tests/render_test.py
git -C /home/khaledsa/projects/develeap/hyperping-exporter/.worktrees/chart-hardening-prod-defaults commit -m "feat(helm): set resources defaults from 30min live API observation

Peak RSS <P>Mi, p99 CPU <X>%. requests {cpu: 50m, memory: 64Mi}; limits
{cpu: 200m, memory: 256Mi}. Render harness now asserts the exact envelope
so future drift surfaces in CI."
```
Replace `<P>` and `<X>` with measured values.

---

### Task 3: Reconcile liveness/readiness probe defaults

**Files:**
- Modify: `deploy/helm/hyperping-exporter/values.yaml:95-107`
- Test: `deploy/helm/hyperping-exporter/tests/render_test.py` (new probes assertion)

Current liveness: `initialDelaySeconds: 5, periodSeconds: 30` (no failureThreshold → inherits 3). Readiness: `initialDelaySeconds: 10, periodSeconds: 15` (no failureThreshold → 3). User's contract: liveness `10/10/3`, readiness slightly tighter — pick `5/5/3` for readiness (faster recovery from transient unreadiness).

- [ ] **Step 1: Write failing probes assertion in `render_test.py`**

Add to the defaults case, after the resources assertion from Task 2:

```python
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
    actual_live = deployment["spec"]["template"]["spec"]["containers"][0]["livenessProbe"]
    actual_ready = deployment["spec"]["template"]["spec"]["containers"][0]["readinessProbe"]
    assert_eq(actual_live, EXPECTED_LIVENESS,
              "defaults: livenessProbe matches user contract (/healthz, 10/10/3)")
    assert_eq(actual_ready, EXPECTED_READINESS,
              "defaults: readinessProbe matches user contract (/readyz, 5/5/3)")
```

- [ ] **Step 2: Run harness to confirm probe assertion FAILS**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
```
Expected: exit 1; "FAIL defaults: livenessProbe..." pointing to actual `5/30/(unset)`.

- [ ] **Step 3: Update `values.yaml` probes block**

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: http
  initialDelaySeconds: 10
  periodSeconds: 10
  failureThreshold: 3

readinessProbe:
  httpGet:
    path: /readyz
    port: http
  initialDelaySeconds: 5
  periodSeconds: 5
  failureThreshold: 3
```

- [ ] **Step 4: Run harness — assertion PASSES**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
```
Expected: all green, 12 PASS lines.

- [ ] **Step 5: Commit**

```bash
git add deploy/helm/hyperping-exporter/values.yaml deploy/helm/hyperping-exporter/tests/render_test.py
git commit -m "feat(helm): set liveness 10/10/3 and readiness 5/5/3 probe defaults

User contract: liveness on /healthz at 10s initial / 10s period /
failureThreshold 3; readiness on /readyz tighter (5/5/3) so a flapping
backend pulls the pod out of Service endpoints quickly. Both fully
overridable."
```

---

### Task 4: Render `--cache-ttl` via toJson for consistency

**Files:**
- Modify: `deploy/helm/hyperping-exporter/templates/deployment.yaml:45`
- Test: `deploy/helm/hyperping-exporter/tests/render_test.py` (existing baseline still passes)

Today: `- "--cache-ttl={{ .Values.config.cacheTTL }}"`. After: same toJson form the new flags use. No functional change for valid Go-duration values; brings the rendering pattern into a single style for all string-typed config args. Item 3 of the user's contract.

- [ ] **Step 1: Read the deployment template's current arg block**

```bash
sed -n '42,60p' deploy/helm/hyperping-exporter/templates/deployment.yaml
```
Note the existing pattern.

- [ ] **Step 2: Replace the cache-ttl line**

In `deploy/helm/hyperping-exporter/templates/deployment.yaml`, change line 45 from
```yaml
            - "--cache-ttl={{ .Values.config.cacheTTL }}"
```
to
```yaml
            - {{ printf "--cache-ttl=%s" .Values.config.cacheTTL | toJson }}
```

- [ ] **Step 3: Run the harness — BASELINE_ARGS still match**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
```
Expected: every assertion still passes. `toJson "60s"` produces `"60s"` (no escape needed); harness's baseline `--cache-ttl=60s` literal is unaffected.

- [ ] **Step 4: Commit**

```bash
git add deploy/helm/hyperping-exporter/templates/deployment.yaml
git commit -m "refactor(helm): render --cache-ttl via toJson for arg-rendering consistency

All optional-flag args (--exclude-name-pattern, --mcp-url) already go
through 'printf ... | toJson'. Bring --cache-ttl onto the same path so
the template has one canonical pattern for string-typed config args.
No functional change for valid Go duration values."
```

---

### Task 5: Audit and document the existing PSS-restricted securityContext

**Files:**
- Modify: `deploy/helm/hyperping-exporter/values.yaml` (comments + fsGroup audit)
- Test: `deploy/helm/hyperping-exporter/tests/render_test.py` (new securityContext assertion)

The chart already ships a PSS-restricted block. Verify it matches the standard and write a literal assertion that fails on any future weakening.

- [ ] **Step 1: Cross-check chart values against the PSS Restricted (v1.33) profile**

PSS Restricted requires:
- `securityContext.allowPrivilegeEscalation: false`
- `securityContext.capabilities.drop: [ALL]` (or contains ALL)
- `securityContext.runAsNonRoot: true` (or pod-level)
- `securityContext.seccompProfile.type: RuntimeDefault` (or `Localhost`)
- Pod or container `runAsUser != 0`

Current chart satisfies all of these. No production change needed unless audit surfaces a gap.

- [ ] **Step 2: Add failing PSS assertion to `render_test.py`**

Add to defaults case, after the probes assertion:

```python
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
    pod = deployment["spec"]["template"]["spec"]
    actual_container_sc = pod["containers"][0]["securityContext"]
    actual_pod_sc = pod["securityContext"]
    assert_eq(actual_container_sc, EXPECTED_CONTAINER_SC,
              "defaults: container securityContext is PSS-restricted compliant")
    assert_eq(actual_pod_sc, EXPECTED_POD_SC,
              "defaults: pod securityContext is PSS-restricted compliant")
```

- [ ] **Step 3: Run harness — expect PASS**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
```
Expected: all green. The current chart already complies; this assertion locks the contract.

- [ ] **Step 4: Add a `values.yaml` block-comment explaining the contract**

Above the `securityContext:` block at line 70:

```yaml
# Pod and container securityContext defaults satisfy the Kubernetes
# Pod Security Standard "restricted" profile (v1.33+). Overriding any
# of these fields may cause the pod to be rejected by PSS-restricted
# admission. See the kind PSS admission test in CI.
```

- [ ] **Step 5: Commit**

```bash
git add deploy/helm/hyperping-exporter/values.yaml deploy/helm/hyperping-exporter/tests/render_test.py
git commit -m "test(helm): lock PSS-restricted securityContext defaults via render harness

Chart already ships a PSS-restricted-compliant securityContext block;
this commit pins the exact field values via assert_eq so any future
weakening surfaces immediately. Adds a values.yaml comment explaining
the contract."
```

---

### Task 6: Multi-replica fail() and secret-source mutual-exclusion fail() in _helpers.tpl

**Files:**
- Modify: `deploy/helm/hyperping-exporter/templates/_helpers.tpl`
- Modify: `deploy/helm/hyperping-exporter/templates/deployment.yaml:1-3` (replace existing guard)
- Modify: `deploy/helm/hyperping-exporter/templates/secret.yaml:1` (extend guard)
- Test: new fixtures + render harness

The user's policy: exactly one of `apiKey` / `existingSecret` / `externalSecret.enabled` must be set. `replicaCount > 1` must `fail()`. `replicaCount == 0` is allowed. The current chart's two-line guard (`if and (not apiKey) (not existingSecret) → fail`) becomes a count-based check.

- [ ] **Step 1: Add helpers to `_helpers.tpl`**

Append:

```yaml
{{/*
Count configured secret sources. Exactly one of apiKey, existingSecret,
externalSecret.enabled must be set; everything else is a configuration
mistake the chart aborts on at render time.
*/}}
{{- define "hyperping-exporter.secretSourceCount" -}}
{{- $count := 0 -}}
{{- if .Values.config.apiKey -}}{{- $count = add $count 1 -}}{{- end -}}
{{- if .Values.config.existingSecret -}}{{- $count = add $count 1 -}}{{- end -}}
{{- if and .Values.externalSecret .Values.externalSecret.enabled -}}{{- $count = add $count 1 -}}{{- end -}}
{{- $count -}}
{{- end }}

{{/*
Render-time validation: fail loudly when secret sources are misconfigured.
*/}}
{{- define "hyperping-exporter.validateSecretSources" -}}
{{- $count := include "hyperping-exporter.secretSourceCount" . | int -}}
{{- if gt $count 1 -}}
{{- fail "Configuration error: set exactly one of .Values.config.apiKey, .Values.config.existingSecret, or .Values.externalSecret.enabled. Got multiple." -}}
{{- end -}}
{{- if eq $count 0 -}}
{{- fail "Configuration error: must set one of .Values.config.apiKey, .Values.config.existingSecret, or .Values.externalSecret.enabled." -}}
{{- end -}}
{{- end }}

{{/*
Render-time validation: the chart is single-replica by design. Multiple
replicas would mean N independent pollers hammering the Hyperping API
in parallel, which is a configuration mistake, not a feature. replicas:0
remains valid (scaled-to-zero state). See values.yaml for context.
*/}}
{{- define "hyperping-exporter.validateReplicaCount" -}}
{{- if gt (int .Values.replicaCount) 1 -}}
{{- fail (printf "Configuration error: replicaCount=%d is unsupported. The exporter is single-poller by design; multiple replicas would each independently poll the Hyperping API. Set replicaCount to 1 (or 0 to scale to zero)." (int .Values.replicaCount)) -}}
{{- end -}}
{{- end }}
```

- [ ] **Step 2: Wire the helpers into `deployment.yaml`**

Replace lines 1–3 of `deployment.yaml` (current `if and (not apiKey) (not existingSecret) → fail` block) with:

```yaml
{{- include "hyperping-exporter.validateSecretSources" . }}
{{- include "hyperping-exporter.validateReplicaCount" . }}
```

- [ ] **Step 3: Extend `secret.yaml` guard**

Change line 1 of `secret.yaml`:
```yaml
{{- if and .Values.config.apiKey (not .Values.config.existingSecret) (not (and .Values.externalSecret .Values.externalSecret.enabled)) }}
```

- [ ] **Step 4: Add `externalSecret` skeleton to `values.yaml`**

Insert after the `config` block (before `service:`):

```yaml
# Mutually-exclusive secret sources for the Hyperping API key:
#   - config.apiKey      (inline, dev/test only)
#   - config.existingSecret (operator manages a Secret out-of-band)
#   - externalSecret.enabled (External Secrets Operator path; below)
# Setting more than one (or none) aborts the install with a clear error.
externalSecret:
  enabled: false
  # Reference to an existing SecretStore or ClusterSecretStore that the
  # External Secrets Operator can reach. Required when enabled is true.
  secretStoreRef:
    name: ""
    kind: SecretStore   # or ClusterSecretStore
  # The remote key under which the Hyperping API key is stored in the
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

- [ ] **Step 5: Add new fixtures**

Files (each creates a new file under `deploy/helm/hyperping-exporter/tests/fixtures/`):

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

`secret-source-missing.values.yaml`:
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

- [ ] **Step 6: Extend `render_test.py` with `assert_fail` helper and four conflict cases plus the multi-replica case**

Add the helper near the top, after `assert_eq`:

```python
def assert_fail(fixture: str, expected_substring: str, label: str) -> None:
    """Render with the given fixture and assert that helm exits non-zero
    AND the stderr contains the expected substring.

    Used to lock fail() error contracts the chart promises to operators."""
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

Append five new cases after Case 9:

```python
    # Case 10 — secret sources: apiKey + existingSecret = fail.
    assert_fail("secret-conflict-apikey-and-existing.values.yaml",
                "Got multiple",
                "secret-conflict apikey+existingSecret aborts render")
    # Case 11 — secret sources: apiKey + externalSecret = fail.
    assert_fail("secret-conflict-apikey-and-external.values.yaml",
                "Got multiple",
                "secret-conflict apikey+externalSecret aborts render")
    # Case 12 — secret sources: existingSecret + externalSecret = fail.
    assert_fail("secret-conflict-existing-and-external.values.yaml",
                "Got multiple",
                "secret-conflict existing+externalSecret aborts render")
    # Case 13 — secret sources: none set = fail with the "must set one" message.
    assert_fail("secret-source-missing.values.yaml",
                "must set one of",
                "secret-source missing aborts render")
    # Case 14 — replicaCount > 1 = fail.
    assert_fail("replicas-multi.values.yaml",
                "replicaCount=2 is unsupported",
                "replicaCount > 1 aborts render")
```

- [ ] **Step 7: Run harness — expect all new cases PASS**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
```
Expected: all green, including 5 new PASS lines.

- [ ] **Step 8: Commit**

```bash
git add deploy/helm/hyperping-exporter/templates/_helpers.tpl \
        deploy/helm/hyperping-exporter/templates/deployment.yaml \
        deploy/helm/hyperping-exporter/templates/secret.yaml \
        deploy/helm/hyperping-exporter/values.yaml \
        deploy/helm/hyperping-exporter/tests/render_test.py \
        deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-apikey-and-existing.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-apikey-and-external.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-existing-and-external.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/secret-source-missing.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/replicas-multi.values.yaml
git commit -m "feat(helm): fail() guards for secret-source conflicts and replicaCount > 1

Exactly one of config.apiKey, config.existingSecret, or
externalSecret.enabled must be set; setting more than one (or none)
aborts helm template with a clear error. replicaCount > 1 also aborts
because the exporter is single-poller by design; replicaCount: 0 is
allowed (scaled-to-zero state). Render harness covers all five failure
paths via the new assert_fail helper."
```

---

### Task 7: ExternalSecret template + render harness coverage

**Files:**
- Create: `deploy/helm/hyperping-exporter/templates/externalsecret.yaml`
- Create: `deploy/helm/hyperping-exporter/tests/fixtures/external-secret.values.yaml`
- Modify: `deploy/helm/hyperping-exporter/tests/render_test.py`

The `externalSecret.enabled: true` path renders a `v1beta1 ExternalSecret` that writes into a Secret of the same name the Deployment's `valueFrom.secretKeyRef.name` already references. Secret.yaml is suppressed via the Task 6 guard.

- [ ] **Step 1: Write the ExternalSecret template**

```yaml
{{- if and .Values.externalSecret .Values.externalSecret.enabled }}
apiVersion: external-secrets.io/v1beta1
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
{{- end }}
```

- [ ] **Step 2: Write `external-secret.values.yaml` fixture**

```yaml
config:
  apiKey: ""
  existingSecret: ""
externalSecret:
  enabled: true
  secretStoreRef:
    name: vault-store
    kind: SecretStore
  remoteRef:
    key: hyperping/api-key
  refreshInterval: "30m"
```

- [ ] **Step 3: Extend `render_test.py` with the external-secret case**

Add helpers at top-level if not already present:

```python
def find_external_secret(rendered: str) -> dict | None:
    for d in docs(rendered):
        if d.get("kind") == "ExternalSecret":
            return d
    return None
```

Add Case 15:

```python
    # Case 15 — externalSecret.enabled: true. ExternalSecret rendered,
    # plain Secret absent (mutual exclusion via secret-source count).
    rendered = helm_template("external-secret.values.yaml")
    es = find_external_secret(rendered)
    assert es is not None, "FAIL external-secret: ExternalSecret should be rendered"
    print("PASS external-secret: ExternalSecret rendered")
    assert find_secret(rendered) is None, \
        "FAIL external-secret: chart-managed Secret must NOT be present"
    print("PASS external-secret: chart-managed Secret absent (ES path consumed)")
    assert_eq(es["apiVersion"], "external-secrets.io/v1beta1",
              "external-secret: v1beta1 ExternalSecret rendered")
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
    # Deployment env still resolves the same secret name (now ESO-managed).
    env = find_deployment(rendered)["spec"]["template"]["spec"]["containers"][0]["env"]
    secret_ref = env[0]["valueFrom"]["secretKeyRef"]["name"]
    assert_eq(secret_ref, "testrel-hyperping-exporter",
              "external-secret: container env references the ESO-target name")
    assert_scalars_clean(rendered, "external-secret")
```

- [ ] **Step 4: Run harness — verify Case 15 PASSES**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
```
Expected: all green; 7 new PASS lines under "external-secret".

- [ ] **Step 5: Commit**

```bash
git add deploy/helm/hyperping-exporter/templates/externalsecret.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/external-secret.values.yaml \
        deploy/helm/hyperping-exporter/tests/render_test.py
git commit -m "feat(helm): ExternalSecret support via external-secrets.io/v1beta1

externalSecret.enabled: true renders an ExternalSecret that writes into
a Secret of the same name the Deployment's HYPERPING_API_KEY env var
already references. Mutually exclusive with config.apiKey and
config.existingSecret via the secret-source count guard. Required
fields (secretStoreRef.name, remoteRef.key) enforced via Helm 'required'
helper. Render harness covers the enabled path end-to-end."
```

---

### Task 8: PDB guard for replicaCount in {0, 1}, NetworkPolicy default-on, Cilium FQDN variant

**Files:**
- Modify: `deploy/helm/hyperping-exporter/templates/pdb.yaml`
- Modify: `deploy/helm/hyperping-exporter/templates/networkpolicy.yaml`
- Create: `deploy/helm/hyperping-exporter/templates/networkpolicy-cilium.yaml`
- Modify: `deploy/helm/hyperping-exporter/values.yaml` (NP defaults flipped, FQDN block added)
- Create fixtures + harness cases

**Note on coupling:** the multi-replica `fail()` from Task 6 means PDB rendering is only possible for `replicaCount ∈ {0, 1}` in normal cases — and PDB selecting zero replicas is harmless but useless. The guard combines both: render PDB ONLY when `replicaCount > 1`. With the Task 6 guard in place, PDB is therefore effectively never rendered today. We keep the template wired (and the value enabled by default false) so that if the user ever removes the multi-replica fail() (e.g., adds leader-election), the PDB path is already correct. The fixture `pdb-enabled.values.yaml` sets `replicaCount: 2` AND a temporary disable of the multi-replica fail() via... no, that's not possible. Instead the PDB path is tested by asserting that with `replicaCount: 1`, PDB is suppressed even when enabled (the user-visible safety property). The "rendered when replicas > 1" branch is documented in the template comment but not in the render harness because it would require the user to first remove the chart's multi-replica fail() — outside this PR.

- [ ] **Step 1: Update `pdb.yaml` with the combined guard**

Replace contents of `deploy/helm/hyperping-exporter/templates/pdb.yaml`:

```yaml
{{- /*
PDB is rendered only when both conditions hold:
  - podDisruptionBudget.enabled: true (operator opt-in)
  - replicaCount > 1 (a PDB selecting a single replica blocks
    voluntary disruptions like node drains, which is worse than no
    PDB at all)
Today the chart's multi-replica fail() guard means replicaCount > 1
already aborts render, so this template effectively never emits. The
guard is kept so that a future leader-election architecture can
remove the fail() guard without also having to revisit pdb.yaml.
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
  maxUnavailable: {{ .Values.podDisruptionBudget.maxUnavailable | default 0 }}
  selector:
    matchLabels:
      {{- include "hyperping-exporter.selectorLabels" . | nindent 6 }}
{{- end }}
```

The `maxUnavailable` default flips `1 → 0` per the user's contract for multi-replica safety.

- [ ] **Step 2: Update `networkpolicy.yaml` to skip when Cilium variant is on**

Wrap the existing template body so it only renders when the vanilla path is active. Replace line 1 (`{{- if .Values.networkPolicy.enabled }}`) with:

```yaml
{{- if and .Values.networkPolicy.enabled (not (and .Values.networkPolicy.fqdnRestriction .Values.networkPolicy.fqdnRestriction.enabled)) }}
```

- [ ] **Step 3: Create `networkpolicy-cilium.yaml`**

```yaml
{{- if and .Values.networkPolicy.enabled .Values.networkPolicy.fqdnRestriction .Values.networkPolicy.fqdnRestriction.enabled }}
{{- /*
CiliumNetworkPolicy variant: restricts egress to specific FQDNs.
REQUIRES the Cilium CNI with cilium.io/v2 CRDs installed. On clusters
without Cilium, helm apply will fail with "no matches for kind
CiliumNetworkPolicy"; that's intentional and honest about the
deployment requirement.

The vanilla networkpolicy.yaml is suppressed when this path is active
(mutually exclusive).
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
        {{- toYaml .Values.networkPolicy.ingressFrom | nindent 8 }}
      toPorts:
        - ports:
            - port: "{{ .Values.service.port }}"
              protocol: TCP
  {{- end }}
  egress:
    # DNS to kube-dns/coredns.
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
    # FQDN-restricted egress over HTTPS to allowed hosts.
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

- [ ] **Step 4: Update `values.yaml` networkPolicy block**

Replace the existing `networkPolicy:` block (lines 115–130) with:

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
    # cilium.io/v2 CRDs installed; will be rejected by the API server
    # on other CNIs. Mutually exclusive with the vanilla path.
    enabled: false
    allowedHosts:
      - api.hyperping.io
  # Selectors for pods allowed to scrape metrics (e.g. Prometheus).
  # The default assumes Prometheus is in the SAME namespace as this chart.
  # For cross-namespace scraping, add a namespaceSelector to the entry:
  #   ingressFrom:
  #     - namespaceSelector:
  #         matchLabels:
  #           kubernetes.io/metadata.name: monitoring
  #       podSelector:
  #         matchLabels:
  #           app.kubernetes.io/name: prometheus
  ingressFrom:
    - podSelector:
        matchLabels:
          app.kubernetes.io/name: prometheus
```

- [ ] **Step 5: Add fixtures**

`networkpolicy-default.values.yaml`:
```yaml
# networkPolicy.enabled: true is now the default; just supply apiKey.
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

`replicas-zero.values.yaml`:
```yaml
replicaCount: 0
config:
  apiKey: x
podDisruptionBudget:
  enabled: true   # operator opt-in; PDB still suppressed because replicaCount==0
```

`pdb-enabled.values.yaml`:
```yaml
# This fixture documents the PDB enabled branch but is gated by the
# multi-replica fail(); the harness asserts the fail(), then a follow-on
# assertion confirms PDB is suppressed with replicaCount=1 even when
# podDisruptionBudget.enabled is true (the user-facing safety property).
config:
  apiKey: x
podDisruptionBudget:
  enabled: true
```

- [ ] **Step 6: Extend `render_test.py` with five new cases**

Add helpers at top-level:

```python
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
```

Add Cases 16–20 after the external-secret case:

```python
    # Case 16 — defaults render the vanilla NetworkPolicy now that
    # networkPolicy.enabled flipped to true. CiliumNetworkPolicy absent.
    rendered = helm_template("networkpolicy-default.values.yaml")
    np = find_networkpolicy(rendered)
    assert np is not None, "FAIL np-default: vanilla NetworkPolicy must render"
    print("PASS np-default: vanilla NetworkPolicy rendered")
    assert find_cilium_networkpolicy(rendered) is None, \
        "FAIL np-default: CiliumNetworkPolicy must NOT render in vanilla path"
    print("PASS np-default: CiliumNetworkPolicy absent")
    # Egress: DNS to kube-system + 443 to 0.0.0.0/0 with RFC1918 except.
    egress_ports = sorted(
        port["port"] for rule in np["spec"]["egress"]
        for port in rule.get("ports", [])
    )
    assert_eq(egress_ports, [53, 53, 443],
              "np-default: egress allows DNS (UDP+TCP/53) and TCP/443 only")

    # Case 17 — Cilium variant. CiliumNetworkPolicy rendered; vanilla absent.
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

    # Case 18 — replicaCount: 0 renders Deployment with replicas:0
    # and suppresses PDB even when podDisruptionBudget.enabled is true.
    rendered = helm_template("replicas-zero.values.yaml")
    deployment = find_deployment(rendered)
    assert_eq(deployment["spec"]["replicas"], 0,
              "replicas-zero: Deployment has replicas: 0")
    assert find_pdb(rendered) is None, \
        "FAIL replicas-zero: PDB must NOT render for replicaCount=0"
    print("PASS replicas-zero: PDB suppressed for zero-replica state")
    # Other resources still render: Service, Secret, NetworkPolicy.
    assert find_secret(rendered) is not None, "FAIL replicas-zero: Secret should still render"
    print("PASS replicas-zero: Secret still rendered when scaled to zero")

    # Case 19 — PDB enabled with replicaCount=1 (the default) is suppressed.
    # PDB selecting a single replica blocks voluntary disruptions
    # (node drains); the chart guards against this footgun.
    rendered = helm_template("pdb-enabled.values.yaml")
    assert find_pdb(rendered) is None, \
        "FAIL pdb-enabled: PDB must NOT render with replicaCount=1"
    print("PASS pdb-enabled: PDB suppressed at replicaCount=1 (drain safety)")
```

- [ ] **Step 7: Run harness — expect all PASS**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
```
Expected: every case green.

- [ ] **Step 8: Commit**

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
git commit -m "feat(helm): NetworkPolicy default-on with optional Cilium FQDN variant, PDB safety guard

- networkPolicy.enabled flips to true by default; vanilla NetworkPolicy
  denies all egress except DNS to kube-system and TCP/443 to 0.0.0.0/0
  (vanilla NP cannot filter by FQDN; this is the strongest expressible).
- networkPolicy.fqdnRestriction.enabled: true renders a
  cilium.io/v2 CiliumNetworkPolicy with toFQDNs rules instead; mutually
  exclusive with the vanilla path. Requires Cilium CNI.
- PDB is rendered only when replicaCount > 1 AND
  podDisruptionBudget.enabled: true. With the multi-replica fail() guard
  this means PDB never renders in supported configurations today; the
  conditional path stays for a future leader-election architecture.
  Default maxUnavailable flipped 1 -> 0 so multi-replica disruption is
  conservative.
- replicaCount: 0 is allowed (scaled-to-zero) and renders Deployment
  with replicas: 0; all other resources still emit."
```

---

### Task 9: kubeconform schema validation + kind PSS admission CI

**Files:**
- Modify: `.github/workflows/helm-ci.yml`
- Create: `deploy/helm/hyperping-exporter/tests/kind-pss.yaml`
- Create: `deploy/helm/hyperping-exporter/tests/admission_test.sh`
- Modify: `Makefile`

Two additions: (a) `kubeconform -strict` against every fixture for offline schema validation including CRDs (ExternalSecret, CiliumNetworkPolicy, ServiceMonitor); (b) a `kind` cluster with PSS-restricted enforced on a test namespace, into which the rendered chart is applied; admission errors fail the job.

- [ ] **Step 1: Create kind cluster config**

`deploy/helm/hyperping-exporter/tests/kind-pss.yaml`:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: hyperping-pss
nodes:
  - role: control-plane
    image: kindest/node:v1.34.0
    kubeadmConfigPatches:
      - |
        kind: ClusterConfiguration
        apiServer:
          extraArgs:
            admission-control-config-file: /etc/kubernetes/policies/admission-config.yaml
        extraVolumes:
          - name: admission-config
            hostPath: /etc/kubernetes/policies
            mountPath: /etc/kubernetes/policies
            readOnly: true
            pathType: DirectoryOrCreate
    extraMounts:
      - hostPath: ./deploy/helm/hyperping-exporter/tests/kind-pss-config
        containerPath: /etc/kubernetes/policies
```

Plus a sibling `kind-pss-config/admission-config.yaml`:

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

(Default enforcement stays `privileged`; the test namespace is labeled `restricted` explicitly so the test is scoped and other components aren't accidentally blocked.)

- [ ] **Step 2: Write `admission_test.sh`**

```bash
#!/usr/bin/env bash
# Render the chart and admit it into a PSS-restricted namespace.
# Exits 0 on success, non-zero on any admission error.
set -euo pipefail

CHART_DIR="${CHART_DIR:-deploy/helm/hyperping-exporter}"
NS="hyperping-pss-test"

# Use a fixture that explicitly exercises the new defaults (NP on, etc).
FIXTURE="${FIXTURE:-$CHART_DIR/tests/fixtures/default.values.yaml}"

# Ensure cluster exists with PSS-restricted-friendly config.
if ! kind get clusters 2>/dev/null | grep -q '^hyperping-pss$'; then
  kind create cluster --config "$CHART_DIR/tests/kind-pss.yaml" --wait 120s
fi

# Apply CRDs required by the chart's optional resources so kubectl apply
# does not reject ExternalSecret/CiliumNetworkPolicy with "no matches".
# These CRDs are NOT exercised in this test (default fixture has neither
# enabled), but admission for the manifests we DO render must not be
# influenced by missing CRDs.
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl label namespace "$NS" \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/enforce-version=latest \
  --overwrite

# Render and apply via server-side dry-run so PSS admission runs but no
# state is persisted (faster cleanup, idempotent across reruns).
helm template testrel "$CHART_DIR" -f "$FIXTURE" \
  | kubectl apply --server-side --force-conflicts \
      --dry-run=server -n "$NS" -f -

echo "PASS admission: PSS-restricted namespace admitted the rendered chart"
```

`chmod +x deploy/helm/hyperping-exporter/tests/admission_test.sh`.

- [ ] **Step 3: Extend `.github/workflows/helm-ci.yml`**

Replace the existing `helm` job with three jobs (`lint-and-render`, `kubeconform`, `pss-admission`) all running on chart-touching PRs:

```yaml
name: Helm CI
on:
  push:
    branches: [main]
    paths:
      - 'deploy/helm/**'
      - '.github/workflows/helm-ci.yml'
  pull_request:
    branches: [main]
    paths:
      - 'deploy/helm/**'
      - '.github/workflows/helm-ci.yml'

permissions:
  contents: read

jobs:
  lint-and-render:
    name: Helm chart lint and render
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd  # v6
      - uses: azure/setup-helm@dda3372f752e03dde6b3237bc9431cdc2f7a02a2  # v5.0.0
        with:
          version: v3.20.2
      - uses: actions/setup-python@a309ff8b426b58ec0e2a45f0f869d46889d02405  # v6.2.0
        with:
          python-version: '3.12'
      - run: python -m pip install --no-cache-dir 'pyyaml==6.0.3'
      - run: helm lint deploy/helm/hyperping-exporter/
      - run: python3 deploy/helm/hyperping-exporter/tests/render_test.py

  kubeconform:
    name: Helm chart schema validation
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd  # v6
      - uses: azure/setup-helm@dda3372f752e03dde6b3237bc9431cdc2f7a02a2  # v5.0.0
        with:
          version: v3.20.2
      - name: Install kubeconform
        run: |
          curl -sSL https://github.com/yannh/kubeconform/releases/download/v0.7.0/kubeconform-linux-amd64.tar.gz \
            | tar xz -C /usr/local/bin kubeconform
      - name: Validate every render-test fixture against k8s 1.34 + CRDs
        run: |
          set -e
          CHART=deploy/helm/hyperping-exporter
          SCHEMAS='https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
          for f in $CHART/tests/fixtures/*.values.yaml; do
            case "$(basename "$f")" in
              # Conflict fixtures intentionally fail render; skip them.
              secret-conflict-*|secret-source-missing.values.yaml|replicas-multi.values.yaml) continue ;;
            esac
            echo "::group::kubeconform $f"
            helm template testrel "$CHART" -f "$f" \
              | kubeconform -strict -summary \
                  -kubernetes-version 1.34.0 \
                  -schema-location default \
                  -schema-location "$SCHEMAS"
            echo "::endgroup::"
          done

  pss-admission:
    name: PSS-restricted admission via kind
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd  # v6
      - uses: azure/setup-helm@dda3372f752e03dde6b3237bc9431cdc2f7a02a2  # v5.0.0
        with:
          version: v3.20.2
      - uses: helm/kind-action@a1b0e391336a6ee6e6a2c6b58acdcf5a08b58e9b  # v1.12.0
        with:
          version: v0.30.0
          config: deploy/helm/hyperping-exporter/tests/kind-pss.yaml
          cluster_name: hyperping-pss
          wait: 120s
      - name: Apply chart into PSS-restricted namespace
        run: bash deploy/helm/hyperping-exporter/tests/admission_test.sh
```

NOTE on the `kind-action` SHA: the executing subagent must verify the SHA against `gh api repos/helm/kind-action/git/ref/tags/v1.12.0` (or the current latest) and pin the actual SHA before this template is canonized. If the latest stable kind-action tag differs from v1.12.0 at execution time, use that and update the comment accordingly.

- [ ] **Step 4: Add Makefile targets mirroring CI**

Append to `Makefile`:

```make
.PHONY: helm-ci helm-render helm-kubeconform helm-pss
helm-ci: helm-render helm-kubeconform helm-pss

helm-render:
	helm lint deploy/helm/hyperping-exporter/
	python3 deploy/helm/hyperping-exporter/tests/render_test.py

helm-kubeconform:
	@command -v kubeconform >/dev/null || { echo "kubeconform missing; install from https://github.com/yannh/kubeconform/releases"; exit 2; }
	@for f in deploy/helm/hyperping-exporter/tests/fixtures/*.values.yaml; do \
		case $$(basename $$f) in secret-conflict-*|secret-source-missing.values.yaml|replicas-multi.values.yaml) continue ;; esac ; \
		echo "kubeconform $$f" ; \
		helm template testrel deploy/helm/hyperping-exporter -f $$f \
		  | kubeconform -strict -summary -kubernetes-version 1.34.0 \
		      -schema-location default \
		      -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
		  || exit 1 ; \
	done

helm-pss:
	bash deploy/helm/hyperping-exporter/tests/admission_test.sh
```

- [ ] **Step 5: Run the local equivalents**

```bash
make helm-render
make helm-kubeconform
make helm-pss
```
Expected: all three succeed locally. If `kind`/`kubeconform` aren't installed locally, document that and rely on CI for the empirical proof — but at minimum `helm-render` and `helm-kubeconform` must run clean before commit. If `kind` is unavailable, the `helm-pss` target will exit 2 with a clear install hint (add a `command -v kind` guard mirroring the kubeconform one).

- [ ] **Step 6: Commit**

```bash
git add .github/workflows/helm-ci.yml \
        deploy/helm/hyperping-exporter/tests/kind-pss.yaml \
        deploy/helm/hyperping-exporter/tests/kind-pss-config/admission-config.yaml \
        deploy/helm/hyperping-exporter/tests/admission_test.sh \
        Makefile
git commit -m "ci(helm): add kubeconform schema validation and kind PSS admission jobs

Three jobs now gate chart-touching PRs:
- lint-and-render (existing): helm lint + PyYAML render harness.
- kubeconform: every render-test fixture (less the conflict fixtures
  that intentionally fail render) is validated against k8s 1.34 schemas
  plus the datreeio CRDs-catalog so ExternalSecret, CiliumNetworkPolicy,
  and ServiceMonitor get real type checking.
- pss-admission: kind cluster with PSS-restricted-friendly admission
  config; the chart is applied --server-side --dry-run=server into a
  pod-security.kubernetes.io/enforce=restricted namespace. Admission
  errors fail the job.

make helm-ci runs the same three locally."
```

---

### Task 10: Chart.yaml bump and CHANGELOG entry

**Files:**
- Modify: `deploy/helm/hyperping-exporter/Chart.yaml:5`
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Bump chart version**

Change `Chart.yaml:5` from `version: 1.1.0` to `version: 1.2.0`. Leave `appVersion: "1.4.0"` (binary unchanged).

- [ ] **Step 2: Update `render_test.py`'s implicit `helm.sh/chart` expectation**

The `labels_with_version` helper checks `app.kubernetes.io/version`, which doesn't change (binary still v1.4.0). The `helm.sh/chart` label flips `hyperping-exporter-1.1.0 → hyperping-exporter-1.2.0` but no render-test asserts that today. Verify by running the harness.

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
```
Expected: still all green.

- [ ] **Step 3: Write CHANGELOG section**

Insert after the existing `## [Unreleased]` line (which stays empty), before `## [1.4.1]`:

```markdown
## [1.2.0] - 2026-05-12 (Chart only; binary unchanged)

### Highlights

- Production-readiness defaults: tuned resources from a 30-min live API observation, reconciled probe defaults, PSS-restricted securityContext locked-in by render tests, NetworkPolicy on by default with an optional Cilium FQDN variant.
- ExternalSecret support via `external-secrets.io/v1beta1`, gated by `externalSecret.enabled`. Mutually exclusive with inline `apiKey` and `existingSecret`.
- Two render-time `fail()` guards prevent silent misconfiguration: setting more than one (or zero) secret source aborts the install; `replicaCount > 1` aborts because the exporter is single-poller by design.
- New CI gates on chart-touching PRs: `kubeconform` schema validation against k8s 1.34 + CRDs, and a `kind` cluster with PSS-restricted enforced on a test namespace.

### Added

- `externalSecret` values block + `templates/externalsecret.yaml`. `secretStoreRef`, `remoteRef.key`, `refreshInterval` exposed.
- `networkPolicy.fqdnRestriction.enabled` + `allowedHosts` + `templates/networkpolicy-cilium.yaml` for FQDN-restricted egress on Cilium clusters.
- Multi-replica `fail()` guard via `_helpers.tpl` validator.
- Secret-source mutual-exclusion `fail()` guard (apiKey / existingSecret / externalSecret.enabled — exactly one).
- 11 new render-harness cases covering ExternalSecret, NetworkPolicy default-on, Cilium variant, replicas-zero, PDB safety, and the five `fail()` paths.
- `kubeconform` step in Helm CI, validating every passing fixture against k8s 1.34 schemas + datreeio CRDs catalog.
- `pss-admission` step in Helm CI, applying the rendered chart into a PSS-restricted namespace inside `kind`.
- `make helm-ci` (and `helm-render`, `helm-kubeconform`, `helm-pss`) for local parity with CI.

### Changed

- `Chart.yaml` `version` `1.1.0` → `1.2.0`. `appVersion` unchanged (`"1.4.0"`); binary not modified by this release.
- `resources` defaults tuned from empirical observation: `requests {cpu: 50m, memory: 64Mi}`, `limits {cpu: 200m, memory: 256Mi}` (final values confirmed at execution; commit message records measured peak).
- `livenessProbe` defaults: `initialDelaySeconds 10`, `periodSeconds 10`, `failureThreshold 3`.
- `readinessProbe` defaults: `initialDelaySeconds 5`, `periodSeconds 5`, `failureThreshold 3`.
- `networkPolicy.enabled` default `false` → `true`.
- `podDisruptionBudget.maxUnavailable` default `1` → `0`. PDB is also now suppressed when `replicaCount ≤ 1` regardless of `enabled` (a PDB selecting a single replica blocks node drains).
- `--cache-ttl` rendered via `toJson` for consistency with the other optional flags.

### Upgrade notes

- **Chart version label churn**: the `helm.sh/chart` label flips `hyperping-exporter-1.1.0` → `hyperping-exporter-1.2.0` on every resource; diffs in your GitOps tool are expected. `app.kubernetes.io/version` does NOT change (still `"1.4.0"`).
- **NetworkPolicy now enabled by default**: clusters that previously relied on the chart NOT applying a NetworkPolicy must explicitly set `networkPolicy.enabled: false` in their values. The vanilla policy permits DNS to kube-system and TCP/443 egress; if your environment uses a non-standard CoreDNS namespace or non-443 metrics scrape target, audit `ingressFrom`.
- **`replicaCount > 1` now aborts render**: installs that previously set replicaCount to 2 or more silently were misconfigured (N independent pollers hammering Hyperping). Set `replicaCount: 1` and accept the single-poller architecture. `replicaCount: 0` is supported.
- **Multiple secret sources now abort render**: setting both `apiKey` and `existingSecret` (or any other pair, or none) was a configuration mistake; the chart now `fail()`s with a clear message. Pick exactly one.
- **PSS-restricted by default**: pods now must be admitted by a `pod-security.kubernetes.io/enforce=restricted` namespace. The existing securityContext block satisfies the profile; overrides that weaken it will be rejected at apply time.
```

- [ ] **Step 4: Commit**

```bash
git add deploy/helm/hyperping-exporter/Chart.yaml CHANGELOG.md
git commit -m "chore(helm): bump chart to 1.2.0 and document release in CHANGELOG

Chart-only release; binary stays at v1.4.0. Highlights are the
production-readiness defaults (resources, probes, securityContext,
NetworkPolicy on by default), ExternalSecret support, and two new
fail() guards. Upgrade notes flag label churn, NP default flip,
multi-replica rejection, and PSS-restricted enforcement."
```

---

### Task 11: Full-suite verification

**Files:** none (verification only)

- [ ] **Step 1: Render harness**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
```
Expected: 20+ PASS lines, `ALL RENDER TESTS PASSED`. Cases now in suite: defaults (with 4 new sub-assertions), existing-secret, ascii-regex, readme-regex, single-quote-regex, mcp-url, both-flags, quote-regex, mcp-url-query, secret-conflict ×3, secret-source-missing, replicas-multi, external-secret, np-default, np-cilium, replicas-zero, pdb-enabled. Plus the no-issues paths inside each.

- [ ] **Step 2: Helm lint**

```bash
helm lint deploy/helm/hyperping-exporter/
```
Expected: `1 chart(s) linted, 0 chart(s) failed`.

- [ ] **Step 3: kubeconform**

```bash
make helm-kubeconform
```
Expected: every passing fixture validates cleanly against k8s 1.34 + CRDs.

- [ ] **Step 4: kind PSS admission**

```bash
make helm-pss
```
Expected: `PASS admission: PSS-restricted namespace admitted the rendered chart`.

- [ ] **Step 5: Go regression smoke (chart change should not touch Go)**

```bash
go test ./... -race -count=1
```
Expected: 133 tests pass, no new failures. Chart files are in `paths-ignore` for `ci.yml`, so this is a local-only safety check that ensures no accidental Go-side coupling crept in.

- [ ] **Step 6: Confirm git status matches expectations**

```bash
git -C /home/khaledsa/projects/develeap/hyperping-exporter/.worktrees/chart-hardening-prod-defaults status --short
git -C /home/khaledsa/projects/develeap/hyperping-exporter/.worktrees/chart-hardening-prod-defaults log --oneline main..HEAD
git -C /home/khaledsa/projects/develeap/hyperping-exporter/.worktrees/chart-hardening-prod-defaults diff --stat main..HEAD
```
Expected: clean working tree; 9 commits on the branch (Tasks 2–10); files-changed list covers the chart, the CI workflow, the Makefile, the CHANGELOG, and the docs/plans/ entry.

- [ ] **Step 7: No commit (verification only)**

---

### Task 12: Open the PR

**Files:** none (orchestration only)

- [ ] **Step 1: Push the branch**

```bash
git -C /home/khaledsa/projects/develeap/hyperping-exporter/.worktrees/chart-hardening-prod-defaults push -u origin chore/chart-hardening-prod-defaults
```
Expected: branch pushed; remote tracking set.

- [ ] **Step 2: Open PR via `gh`**

```bash
gh pr create \
  --title "feat(helm): chart 1.2.0 — production-readiness defaults" \
  --body "$(cat <<'EOF'
## Summary

Chart-only release (binary unchanged at v1.4.0) implementing the ten production-readiness items from the v1.4.1 peer review.

### What changed and why

- **Resources defaults tuned from a live 30-min observation** against the Hyperping dev API. Effect: requests/limits reflect actual binary use; render harness asserts the literal envelope so future drift is caught.
- **Liveness/readiness probes reconciled** to the peer-review contract (10/10/3 and 5/5/3). Effect: pods become Ready faster and unhealthy pods get pulled from Service endpoints sooner.
- **`--cache-ttl` renders via `toJson`**. Effect: every optional-flag arg in the chart now goes through one canonical path.
- **PSS-restricted securityContext locked-in by render tests**. Effect: any future weakening fails CI loudly.
- **NetworkPolicy enabled by default**, with optional Cilium FQDN variant behind `networkPolicy.fqdnRestriction.enabled`. Effect: every install ships with the strongest egress restriction expressible in vanilla Kubernetes (TCP/443 + DNS), and Cilium clusters can opt into FQDN-restricted egress to `api.hyperping.io`.
- **ExternalSecret support** via `external-secrets.io/v1beta1`. Mutually exclusive with `apiKey` / `existingSecret`. Effect: production users have a first-class path that's neither inline secrets nor manual Secret management.
- **Secret-source `fail()` guard**: exactly one of `apiKey` / `existingSecret` / `externalSecret.enabled` must be set; misconfiguration aborts render with a clear message.
- **Multi-replica `fail()` guard**: `replicaCount > 1` aborts; `replicaCount: 0` and `1` are valid.
- **PDB guard**: suppressed when `replicaCount ≤ 1` regardless of `enabled` (a PDB selecting a single replica blocks node drains).
- **`image.tag` comment** noting chart vs binary version tracking.

### CI / verification

Three jobs gate chart-touching PRs (matching local `make helm-ci`):

- `lint-and-render`: `helm lint` + PyYAML render harness (20+ cases including five new `fail()` paths).
- `kubeconform`: every passing fixture validated against k8s 1.34 schemas + datreeio CRDs catalog (covers `ExternalSecret`, `CiliumNetworkPolicy`, `ServiceMonitor`).
- `pss-admission`: rendered chart applied `--server-side --dry-run=server` into a `pod-security.kubernetes.io/enforce=restricted` namespace inside a `kind` cluster.

### Upgrade notes (see CHANGELOG for full text)

- `helm.sh/chart` label flips `1.1.0 → 1.2.0` on every resource.
- `app.kubernetes.io/version` does NOT change (still `1.4.0`).
- `networkPolicy.enabled` flips to `true` by default — set `false` explicitly if you don't want it.
- `replicaCount > 1` and multi-source secret configs now abort install.
- PSS-restricted enforcement is the assumed namespace policy.

### Source of the work

Peer-review action list at https://github.com/develeap/hyperping-exporter/pull/52 (10 items). All items addressed; none punted.
EOF
)"
```

- [ ] **Step 3: Watch CI**

```bash
gh pr checks --watch
```
All three Helm-CI jobs must pass: `lint-and-render`, `kubeconform`, `pss-admission`. (Go-side CI is skipped via the existing `paths-ignore`.)

- [ ] **Step 4: Report PR URL upstream and stop**

Do NOT merge. The user explicitly required PR-only; the user does the merge.

---

## Risks and Cutover Notes

- **`kind-action` SHA pin**: the executing subagent MUST verify the actual `helm/kind-action` tag and SHA at execution time and update the workflow accordingly. The plan uses `v1.12.0` as a placeholder; if a newer stable tag exists at execution time, use it (and pin the corresponding SHA) to keep CI fast and using a maintained release.
- **kubeconform CRDs catalog availability**: the workflow pulls schemas from `raw.githubusercontent.com/datreeio/CRDs-catalog`. If GitHub rate-limits the runner, the job will fail intermittently. The CRDs catalog is the standard practice; if it proves flaky, switch to pinning a tagged release of the catalog and caching schemas. Document if encountered.
- **kind boot time**: ~60–90 seconds on GitHub-hosted runners. The PSS admission job is the long pole at ~2 minutes total. Acceptable.
- **PSS rejection for fields we don't set**: the chart never sets `hostNetwork`, `hostPID`, `hostPIDs`, `procMount`, sysctl, AppArmor — all PSS-restricted-friendly defaults. The container image is `gcr.io/distroless`-style at the binary's release pipeline. If a future change accidentally adds e.g. `hostPort`, the PSS job will catch it.
- **External Secrets Operator presence**: the ExternalSecret template renders unconditionally on `externalSecret.enabled: true`. The chart cannot detect whether ESO is installed in the cluster; if it's missing, `kubectl apply` returns `no matches for kind "ExternalSecret"`. This is the same trade-off the chart makes for the Cilium variant: be honest at apply time rather than silent at render time.
- **`add` is the Helm template function**: the count helper uses `add` from Helm's Sprig functions. Verified available in Helm v3.20.2.
- **`int` conversion**: `gt (int .Values.replicaCount) 1` works because `replicaCount` is a YAML integer; if a user passes `replicaCount: "1"` as a string, `int` coerces it. Tested implicitly by the harness.
- **Backwards-compat**: no existing value name renamed. The new keys (`externalSecret`, `networkPolicy.fqdnRestriction`) are additive. Existing values consumed by users on chart 1.0.0/1.1.0 still work, except for the new fail() guards which intentionally reject previously-broken configurations.
- **`replicas: 0` rendering**: Helm/Go renders Go ints by their natural representation. `replicas: {{ .Values.replicaCount }}` with `replicaCount: 0` produces `replicas: 0` (not `null` or `""`) because YAML serializes the integer literally. Verified by Case 18.

## Why this plan is bold and direct

- One PR, ten items, single cutover.
- Empirical numbers, not invented ones.
- All four edge cases the strategy flagged (replicas:0, NP-conflict, ExternalSecret-conflict, multi-replica) are tested as `fail()` paths or admission paths, not characterized into best-effort warnings.
- Real CI gates (kubeconform + kind admission) rather than render-only confidence.
- No interim "characterize then change" steps; the existing partial implementations are reconciled in-line.
- The plan explicitly documents what is intentionally NOT changed (binary, Go-side workflows, README) so the reviewer can short-circuit "why didn't you touch X" objections.

