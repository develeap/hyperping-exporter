# Chart UX Hardening: Production-Readiness Defaults Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use trycycle-executing to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land all ten chart hardening items as one PR bumping the chart to **1.5.0** (skipping the binary's v1.2.x slot to keep the shared CHANGELOG strictly monotonic-descending and to track the just-released v1.4.1 binary), gated by an extended render harness, kubeconform schema validation, and a kind PSS-restricted admission CI job that exercises four configurations (defaults, externalSecret, replicas-zero, network-policy variants).

**Architecture:** Single-PR cutover on the existing `chore/chart-hardening-prod-defaults` worktree branch. The chart already implements partial versions of items 1, 2, 4, 5, 8; this work reconciles those surfaces against the user's contract plus adds ExternalSecret, mutual-exclusion `fail()` guards, the Cilium NP variant, the `--cache-ttl` toJson render, and a runtime-readOnlyRootFilesystem proof. Three CI surfaces gate the chart: the existing PyYAML render harness (extended fixture matrix), `kubeconform` strict offline schema validation including CRDs, and a `kind` job that admits each of four representative fixtures into a PSS-restricted namespace AND boots one of those pods to prove the binary tolerates `readOnlyRootFilesystem: true` at runtime.

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

5. **Item 5 Cilium variant:** `cilium.io/v2 CiliumNetworkPolicy` (singular, namespaced). Rendered only when `networkPolicy.fqdnRestriction.enabled: true`. Mutually exclusive with the vanilla NetworkPolicy. Be honest at apply time on non-Cilium clusters (`no matches for kind "CiliumNetworkPolicy"`) rather than silently permissive at render time.

`replicaCount: 0` is locked-in as a supported state: render `replicas: 0`, suppress PDB, **exempt from the secret-source `fail()` guard** because zero pods need no API key (operator workflow: scale to zero, then tear down secrets). Multi-replica `fail()` only fires for `replicaCount > 1`.

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
  - Flip `networkPolicy.enabled` default `false → true` and replace the egress rule with the unrestricted form (Task 8).
  - Add `networkPolicy.fqdnRestriction.enabled: false`, `allowedHosts: [api.hyperping.io]` (Task 8).
  - Add `externalSecret:` block with `apiVersion: external-secrets.io/v1`, `enabled: false`, `secretStoreRef`, `remoteRef.key`, `remoteRef.property`, `refreshInterval: "1h"` (Task 6/7).
  - Document the secret-source mutual exclusion above the secret block.
  - Document the multi-replica `fail()` and scaled-to-zero exemption above `replicaCount`.
- `deploy/helm/hyperping-exporter/templates/_helpers.tpl` — three new helpers (`secretSourceCount`, `validateSecretSources`, `validateReplicaCount`). The `secretSourceCount` helper uses the `.Values.externalSecret | default dict` idiom so removing the entire block from a consumer override doesn't NPE.
- `deploy/helm/hyperping-exporter/templates/deployment.yaml`:
  - Replace the legacy two-line apiKey/existingSecret guard (lines 1-3) with the two validator includes. Use trailing `-}}` to suppress stray newlines.
  - Render `--cache-ttl` via `toJson` (item 3).
- `deploy/helm/hyperping-exporter/templates/secret.yaml` — extend `if` guard to suppress on `externalSecret.enabled`.
- `deploy/helm/hyperping-exporter/templates/pdb.yaml` — wrap in `{{- if and .Values.podDisruptionBudget.enabled (gt (int .Values.replicaCount) 1) }}`. `maxUnavailable` default stays at `1` (drain-safe; rationale in values.yaml).
- `deploy/helm/hyperping-exporter/templates/networkpolicy.yaml`:
  - Wrap in `{{- if and .Values.networkPolicy.enabled (not (and .Values.networkPolicy.fqdnRestriction .Values.networkPolicy.fqdnRestriction.enabled)) }}` (Cilium-suppression).
  - Replace ONLY the egress 443 rule's `ipBlock` block (lines 41-47) so the existing cross-namespace scraping doc comment above `ingressFrom` is preserved. The new egress permits TCP/443 to `0.0.0.0/0` with no exceptions, matching the user's documented contract.
- `deploy/helm/hyperping-exporter/tests/render_test.py`:
  - Bump `EXPECTED_VERSION` literal to `"1.4.1"`.
  - Extend Case 1's `expected_versions` enumeration to also cover NetworkPolicy (now in default render) and the new resources surfaced by other fixtures.
  - Add `assert_fail` helper; six new `fail()` cases.
  - Add a `helm.sh/chart` label assertion (Case 1 extension): the new label flips `1.1.0 → 1.5.0` on every labelled resource; the harness now locks it.
  - 11+ new positive cases (see Test Plan).
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
# Capture current Go test count so Task 11 doesn't hard-code a stale number.
go test ./... -count=1 2>&1 | tee /tmp/go-baseline.txt | tail -5
TEST_BASELINE=$(grep -c '^=== RUN' /tmp/go-baseline.txt || true)
echo "Go test baseline RUNs: $TEST_BASELINE" > /tmp/go-baseline-count.txt
cat /tmp/go-baseline-count.txt
```
Expected: file written; baseline test count recorded for use in Task 11.

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

- [ ] **Step 6: Resolve action SHA pins (CRITICAL pre-task)**

```bash
gh api repos/helm/kind-action/git/ref/tags/v1.12.0 --jq .object.sha 2>/dev/null > /tmp/kind-action-sha.txt || \
  gh api repos/helm/kind-action/releases/latest --jq .tag_name | tee /tmp/kind-action-tag.txt
cat /tmp/kind-action-sha.txt 2>/dev/null
gh api repos/kubernetes-sigs/kind/releases/latest --jq .tag_name | tee /tmp/kind-latest-tag.txt
# Capture the digest for kindest/node:v1.34.0 from the kind release notes.
gh api repos/kubernetes-sigs/kind/releases/tags/v0.30.0 --jq .body 2>/dev/null \
  | grep -E 'kindest/node:v1\.34\.0@sha256' | head -1 | tee /tmp/kindest-node-digest.txt
```
Expected outputs: a SHA for `helm/kind-action` v1.12.0 (or `kind-action-tag.txt` containing the latest tag if v1.12.0 doesn't exist); confirmation that `kind v0.30.0` is the latest GA tag (or substitute with whatever IS); and the digest line for `kindest/node:v1.34.0@sha256:...`. **All three values feed Task 9 verbatim.** If `gh` fails or any value cannot be resolved, abort and surface the missing value to the user. Do NOT use placeholders.

- [ ] **Step 7: Commit nothing — pre-flight only.** Move directly to Task 2.

---

### Task 2: Empirically observe binary resource use under chart-equivalent runtime

**Files:** Modify `deploy/helm/hyperping-exporter/values.yaml` (resources block). Test: `deploy/helm/hyperping-exporter/tests/render_test.py` (assertion to be added in Task 11 once all values are settled).

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

Typical expected outcome (peak RSS ≤ 40Mi, p99 ≤ 5%): `requests {cpu: 50m, memory: 64Mi}`, `limits {cpu: 200m, memory: 256Mi}`. **The measured values win** regardless of expectation; record measured peak/p99 in commit and write them into both values.yaml AND the Task 11 render assertion.

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

Record the measured values in a file `/tmp/hpe-obs/computed-defaults.yaml` for use by Task 11's assertion update.

- [ ] **Step 7: Commit**

```bash
cd /home/khaledsa/projects/develeap/hyperping-exporter/.worktrees/chart-hardening-prod-defaults
git add deploy/helm/hyperping-exporter/values.yaml
git commit -m "feat(helm): set resources defaults from 30min containerized live API observation

Measured peak RSS <P>MiB, p99 CPU <X>% inside Docker with --read-only
and uid 65534 (matching chart runtime). Also proves the binary tolerates
readOnlyRootFilesystem: true; no emptyDir mount required.

requests {cpu: <CPU_REQ>, memory: <MEM_REQ>}; limits {cpu: <CPU_LIM>,
memory: <MEM_LIM>}. Render harness assertion follows in Task 11."
```
Substitute measured values. The render-time literal assertion lands in Task 11 (after all values stabilise) to avoid the TDD-discipline footgun of asserting against partial values.

---

### Task 3: Reconcile liveness/readiness probe defaults

**Files:** Modify `deploy/helm/hyperping-exporter/values.yaml`. Test: render assertion in Task 11.

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
Expected: all existing assertions still pass. Probe assertions land in Task 11.

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

### Task 4: Render `--cache-ttl` via toJson for consistency

**Files:** Modify `deploy/helm/hyperping-exporter/templates/deployment.yaml:45`. Modify `deploy/helm/hyperping-exporter/values.yaml` (cacheTTL comment).

- [ ] **Step 1: Replace the cache-ttl line**

In `deploy/helm/hyperping-exporter/templates/deployment.yaml`, change line 45 from:
```yaml
            - "--cache-ttl={{ .Values.config.cacheTTL }}"
```
to:
```yaml
            - {{ printf "--cache-ttl=%s" .Values.config.cacheTTL | toJson }}
```

- [ ] **Step 2: Add the cacheTTL numeric-warning comment to values.yaml**

Above the `cacheTTL:` line, insert:
```yaml
  # IMPORTANT: cacheTTL must be a Go duration string (e.g. "60s", "5m").
  # Supplying a bare integer like `cacheTTL: 60` is parsed by YAML as int
  # and renders as "60" without a unit; the binary's duration parser
  # rejects this at startup with "missing unit in duration". Always quote
  # the value and include a unit.
  cacheTTL: "60s"
```
(Replace the existing `cacheTTL: "60s"` line with the commented block above.)

- [ ] **Step 3: Run the harness — BASELINE_ARGS still match**

```bash
python3 deploy/helm/hyperping-exporter/tests/render_test.py
```
Expected: every existing case still passes. `toJson "60s"` produces `"60s"`; baseline `--cache-ttl=60s` literal is unaffected.

- [ ] **Step 4: Commit**

```bash
git add deploy/helm/hyperping-exporter/templates/deployment.yaml \
        deploy/helm/hyperping-exporter/values.yaml
git commit -m "refactor(helm): render --cache-ttl via toJson and document duration-string requirement

All optional-flag args (--exclude-name-pattern, --mcp-url) already go
through 'printf ... | toJson'. Bring --cache-ttl onto the same path so
the template has one canonical pattern. Inline values.yaml comment
warns against bare integers (parsed as int, rendered without unit,
rejected by Go's duration parser)."
```

---

### Task 5: Document the existing PSS-restricted securityContext

**Files:** Modify `deploy/helm/hyperping-exporter/values.yaml`. Test: assertion in Task 11.

The chart already satisfies PSS-restricted; this task adds the explanatory comment block. The actual lock-in assertion belongs in Task 11 alongside every other defaults-case assertion, because writing it here violates TDD discipline (no red state, since the chart already complies).

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
# (Task 11) locks the exact field values; the kind PSS admission CI job
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

### Task 6: Add the ExternalSecret values block, template, and a single secret fixture

**Files:** Create `deploy/helm/hyperping-exporter/templates/externalsecret.yaml`. Create `deploy/helm/hyperping-exporter/tests/fixtures/external-secret-defaults.values.yaml`. Modify `deploy/helm/hyperping-exporter/values.yaml`. Test: assertion lands in Task 11.

**Why this task comes BEFORE Task 7's guards:** committing the guards first (closing secret.yaml on `externalSecret.enabled`) without having the template that emits the ExternalSecret leaves the chart in a broken bisect state where `--set externalSecret.enabled=true` produces zero secret-source resources. Order: template + values first, then guards.

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
helper added in Task 7).

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

- [ ] **Step 4: Smoke-test the template renders for `external-secret.values.yaml`**

```bash
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/external-secret.values.yaml \
  | grep -E '^(apiVersion|kind|  name|  refreshInterval): '
```
Expected: see `kind: ExternalSecret`, `apiVersion: external-secrets.io/v1`, `name: testrel-hyperping-exporter`, `refreshInterval: 30m`. The chart's old secret guard still emits a plain Secret too at this commit; that's fine — Task 7 closes it.

- [ ] **Step 5: Commit**

```bash
git add deploy/helm/hyperping-exporter/templates/externalsecret.yaml \
        deploy/helm/hyperping-exporter/values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/external-secret.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/external-secret-defaults.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/external-secret-missing-store.values.yaml
git commit -m "feat(helm): add ExternalSecret template and values block (external-secrets.io/v1)

externalSecret.enabled: true renders an ExternalSecret that writes
into a Secret of the same name the Deployment's HYPERPING_API_KEY env
var already references. apiVersion is values-driven (default v1; set
to v1beta1 for legacy ESO ≤ 0.15). Required fields (secretStoreRef.name,
remoteRef.key) enforced via Helm's 'required' helper.

Mutual-exclusion guards land in the next commit so bisects between
the two commits stay valid for single-source configurations."
```

---

### Task 7: Multi-replica fail() + secret-source mutual-exclusion fail() + secret.yaml guard

**Files:** Modify `deploy/helm/hyperping-exporter/templates/_helpers.tpl`. Modify `deploy/helm/hyperping-exporter/templates/deployment.yaml`. Modify `deploy/helm/hyperping-exporter/templates/secret.yaml`. Create five conflict/missing fixtures. Test: assertions in Task 11.

User's policy: exactly one of `apiKey` / `existingSecret` / `externalSecret.enabled` must be set when `replicaCount ≥ 1`. `replicaCount > 1` aborts. `replicaCount == 0` is allowed and is exempt from the secret-source `fail()`.

- [ ] **Step 1: Append helpers to `_helpers.tpl`**

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

- [ ] **Step 2: Wire helpers into `deployment.yaml`**

Replace lines 1-3 of `deployment.yaml` (the current `if and (not apiKey) (not existingSecret) → fail` block) with:

```
{{- include "hyperping-exporter.validateSecretSources" . -}}
{{- include "hyperping-exporter.validateReplicaCount" . -}}
```

Trailing `-}}` on both lines so no stray blank document precedes `apiVersion:`.

- [ ] **Step 3: Extend `secret.yaml` guard**

Change line 1 of `secret.yaml` to:
```yaml
{{- if and .Values.config.apiKey (not .Values.config.existingSecret) (not (and .Values.externalSecret .Values.externalSecret.enabled)) }}
```

- [ ] **Step 4: Write five conflict / missing fixtures**

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

- [ ] **Step 5: Smoke-test that the guards fire as expected**

```bash
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-apikey-and-existing.values.yaml 2>&1 | tail -3
# Expected: stderr contains "Got multiple"
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/replicas-multi.values.yaml 2>&1 | tail -3
# Expected: stderr contains "replicaCount=2 is unsupported"
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/external-secret-missing-store.values.yaml 2>&1 | tail -3
# Expected: stderr contains "externalSecret.secretStoreRef.name is required"
```

Verify each command exits non-zero. The full assertion harness lands in Task 11.

- [ ] **Step 6: Commit**

```bash
git add deploy/helm/hyperping-exporter/templates/_helpers.tpl \
        deploy/helm/hyperping-exporter/templates/deployment.yaml \
        deploy/helm/hyperping-exporter/templates/secret.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-apikey-and-existing.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-apikey-and-external.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/secret-conflict-existing-and-external.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/secret-source-missing.values.yaml \
        deploy/helm/hyperping-exporter/tests/fixtures/replicas-multi.values.yaml
git commit -m "feat(helm): fail() guards for secret-source conflicts and replicaCount > 1

Exactly one of config.apiKey, config.existingSecret, or
externalSecret.enabled must be set; setting more than one aborts
helm template with a clear error. replicaCount > 1 also aborts because
the exporter is single-poller by design. replicaCount: 0 is allowed
and is exempt from the 'must set one' check (scaled-to-zero state).
secret.yaml suppression extended for the ExternalSecret path."
```

---

### Task 8: PDB guard, NetworkPolicy default-on, Cilium FQDN variant, replicaCount:0 support

**Files:** Modify `deploy/helm/hyperping-exporter/templates/pdb.yaml`, `templates/networkpolicy.yaml`. Create `templates/networkpolicy-cilium.yaml`. Modify `deploy/helm/hyperping-exporter/values.yaml`. Create four fixtures.

**Coupling note:** the multi-replica `fail()` from Task 7 means PDB rendering never occurs in supported configs today. The PDB template is kept correct (drain-safe defaults) so a future leader-election change can flip the multi-replica behaviour without revisiting pdb.yaml. The fixture asserts the user-visible safety property: PDB stays suppressed for `replicaCount ≤ 1` even when explicitly enabled.

- [ ] **Step 1: Update `pdb.yaml`**

```yaml
{{- /*
PDB is rendered only when both conditions hold:
  - podDisruptionBudget.enabled: true (operator opt-in)
  - replicaCount > 1 (a PDB selecting a single replica with
    maxUnavailable: 0 would block voluntary disruptions including
    kubectl drain, which is worse than no PDB at all)
With the multi-replica fail() guard in Task 7 this template effectively
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

- [ ] **Step 3: Create `templates/networkpolicy-cilium.yaml`**

```yaml
{{- if and .Values.networkPolicy.enabled .Values.networkPolicy.fqdnRestriction .Values.networkPolicy.fqdnRestriction.enabled }}
{{- /*
CiliumNetworkPolicy variant: restricts egress to specific FQDNs.
REQUIRES the Cilium CNI with cilium.io/v2 CRDs installed. On clusters
without Cilium, helm apply will fail with "no matches for kind
CiliumNetworkPolicy"; intentional honest behaviour.
The vanilla networkpolicy.yaml is suppressed when this path is active.
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

- [ ] **Step 9: Smoke-test rendering**

```bash
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/replicas-zero.values.yaml \
  | grep -E '^(kind|  replicas):' | sort -u
# Expected: kind Deployment, NetworkPolicy, Service. NO PodDisruptionBudget. replicas: 0.
helm template testrel deploy/helm/hyperping-exporter/ \
  -f deploy/helm/hyperping-exporter/tests/fixtures/networkpolicy-cilium.values.yaml \
  | grep '^kind:' | sort -u
# Expected: Deployment, Secret, Service, CiliumNetworkPolicy. NOT NetworkPolicy.
```

- [ ] **Step 10: Commit**

```bash
git add deploy/helm/hyperping-exporter/templates/pdb.yaml \
        deploy/helm/hyperping-exporter/templates/networkpolicy.yaml \
        deploy/helm/hyperping-exporter/templates/networkpolicy-cilium.yaml \
        deploy/helm/hyperping-exporter/values.yaml \
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
  exclusive with the vanilla path. Requires Cilium CNI.
- PDB is rendered only when replicaCount > 1 AND
  podDisruptionBudget.enabled: true. maxUnavailable default stays at 1
  (drain-safe). With the multi-replica fail() guard, PDB never renders
  in supported configurations today; the path stays correct for a
  future leader-election architecture.
- replicaCount: 0 supported (scaled-to-zero) and exempt from the
  must-set-one secret-source guard, so operators can tear down secrets
  cleanly.
- image.tag comment documents chart-vs-binary version independence."
```

---

### Task 9: kubeconform schema validation + kind PSS admission CI (extended job)

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
              secret-conflict-*|secret-source-missing.values.yaml|replicas-multi.values.yaml|external-secret-missing-store.values.yaml) continue ;;
            esac
            echo "::group::kubeconform $f"
            helm template testrel "$CHART" -f "$f" \
              | kubeconform -strict -summary \
                  -kubernetes-version 1.34.0 \
                  -schema-location default \
                  -schema-location "$SCHEMAS"
            echo "::endgroup::"
          done
      - name: Set up kind cluster
        uses: helm/kind-action@<KIND_ACTION_SHA>  # <KIND_ACTION_TAG>
        with:
          version: <KIND_LATEST_TAG>
          config: deploy/helm/hyperping-exporter/tests/kind-pss.yaml
          cluster_name: hyperping-pss
          wait: 120s
      - name: Admit chart into PSS-restricted namespace (four fixtures)
        run: bash deploy/helm/hyperping-exporter/tests/admission_test.sh
```

**Note on substitution:** the SHA and tag values come from Task 1 Step 6. If Step 6 surfaced that `v1.12.0` does NOT exist for `helm/kind-action`, use whatever IS the latest stable release per `gh api repos/helm/kind-action/releases/latest`. The pin must be a real SHA, not a placeholder; the workflow MUST NOT merge with `<KIND_ACTION_SHA>` literally in the file. If branch protection currently requires a specific check name and the new job name `helm` differs from the historical name, audit the repo's branch-protection rules and call out the migration in the PR body (Task 12).

- [ ] **Step 5: Add Makefile targets**

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
		case $$(basename $$f) in secret-conflict-*|secret-source-missing.values.yaml|replicas-multi.values.yaml|external-secret-missing-store.values.yaml) continue ;; esac ; \
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

- [ ] **Step 8: Commit**

```bash
git add .github/workflows/helm-ci.yml \
        deploy/helm/hyperping-exporter/tests/kind-pss.yaml \
        deploy/helm/hyperping-exporter/tests/kind-pss-config/admission-config.yaml \
        deploy/helm/hyperping-exporter/tests/admission_test.sh \
        deploy/helm/hyperping-exporter/tests/fixtures/pss-restricted.values.yaml \
        Makefile
git commit -m "ci(helm): add kubeconform schema validation and kind PSS admission to helm CI

Existing 'helm' job extended with three new steps (preserves branch
protection): kubeconform validates every passing fixture against
k8s 1.34 + datreeio CRDs (lowercase kind filenames), and a kind
cluster boots the rendered chart into a PSS-restricted namespace.

The admission test exercises four fixtures (defaults, networkpolicy,
replicas-zero, externalSecret). The defaults case applies LIVE so the
pod actually starts under readOnlyRootFilesystem: true; rollout-status
wait proves the binary tolerates the read-only root at runtime.

kind config uses kubeadm v1beta4 (required by k8s 1.34), pins
kindest/node by digest, and the admission_test.sh wrapper rewrites
the hostPath to an absolute one so kind's relative-cwd resolution
doesn't silently break PSS mount."
```

---

### Task 10: Chart.yaml bump and CHANGELOG entry

**Files:** Modify `deploy/helm/hyperping-exporter/Chart.yaml`. Modify `CHANGELOG.md`.

- [ ] **Step 1: Bump chart and appVersion**

In `deploy/helm/hyperping-exporter/Chart.yaml`:
- Change `version: 1.1.0` to `version: 1.5.0`.
- Change `appVersion: "1.4.0"` to `appVersion: "1.4.1"`.

- [ ] **Step 2: Write CHANGELOG section**

Insert immediately after `## [Unreleased]`, before `## [1.4.1] - 2026-05-12`. **Strict keep-a-changelog header** (no parenthetical suffix; the chart-only nature moves into a Highlights bullet):

```markdown
## [1.5.0] - 2026-05-12

### Highlights

- Chart-only release; binary unchanged (chart now tracks the just-released v1.4.1 image).
- Production-readiness defaults: tuned resources from a 30-minute containerised live API observation (uid 65534, read-only root), reconciled probe defaults to the peer-review contract (10/10/3 and 5/5/3), PSS-restricted securityContext locked-in by render tests AND proven at runtime via a kind admission CI job that boots the pod.
- ExternalSecret support via `external-secrets.io/v1` (default; `v1beta1` available via `externalSecret.apiVersion` override for legacy ESO ≤ 0.15). Mutually exclusive with inline `apiKey` and `existingSecret`.
- NetworkPolicy on by default; vanilla NP permits DNS to kube-system and TCP/443 to any destination (no RFC1918 except — matches the documented egress contract). Optional `cilium.io/v2 CiliumNetworkPolicy` variant for FQDN-restricted egress.
- Three render-time `fail()` guards prevent silent misconfiguration: multiple secret sources, missing secret sources (when `replicaCount ≥ 1`), and `replicaCount > 1`. `replicaCount: 0` is supported and exempt from the secret-source check (scaled-to-zero state).
- New CI gates on chart-touching PRs: `kubeconform` schema validation against k8s 1.34 + datreeio CRDs catalog, and a `kind` cluster with PSS-restricted enforced on a test namespace that exercises four representative fixtures (one boots the pod live).

### Added

- `externalSecret` values block + `templates/externalsecret.yaml`. `apiVersion` (default `external-secrets.io/v1`), `secretStoreRef`, `remoteRef.key`, `remoteRef.property`, `refreshInterval`. Required fields enforced via Helm's `required` helper.
- `networkPolicy.fqdnRestriction.enabled` + `allowedHosts` + `templates/networkpolicy-cilium.yaml` for FQDN-restricted egress on Cilium clusters.
- Multi-replica `fail()` guard and secret-source mutual-exclusion `fail()` guard in `_helpers.tpl`.
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
- `--cache-ttl` rendered via `toJson` for consistency with the other optional flags. `values.yaml` warns against bare-integer cacheTTL values.

### Upgrade notes

- **Image version flips**: `app.kubernetes.io/version` label moves `"1.4.0"` → `"1.4.1"` on every resource, and the default `image.tag` resolves to `1.4.1`. Pin `image.tag` explicitly if you must stay on the older binary.
- **Chart label churn**: `helm.sh/chart` label flips `hyperping-exporter-1.1.0` → `hyperping-exporter-1.5.0`; diffs in your GitOps tool are expected.
- **NetworkPolicy now enabled by default**: clusters that previously relied on the chart NOT applying a NetworkPolicy must explicitly set `networkPolicy.enabled: false` in their values. Audit `ingressFrom` if Prometheus lives in a different namespace.
- **`replicaCount > 1` aborts render**: installs that previously set `replicaCount: 2+` were silently misconfigured (N independent pollers hammering Hyperping). Set `replicaCount: 1` and accept the single-poller architecture. `replicaCount: 0` is supported.
- **Multiple/missing secret sources abort render** (except when `replicaCount: 0`): pick exactly one of `apiKey` / `existingSecret` / `externalSecret.enabled`.
- **ExternalSecret apiVersion default is v1**: legacy ESO installations (≤ v0.15) must set `externalSecret.apiVersion: external-secrets.io/v1beta1`.
- **PSS-restricted is the assumed namespace policy**: the chart does NOT label namespaces. Operators must label the target namespace `pod-security.kubernetes.io/enforce=restricted` themselves (or use a higher-level admission controller). The chart's securityContext defaults already satisfy the profile; overrides that weaken it will be rejected at apply time on PSS-restricted namespaces.
- **Helm-to-ESO upgrade caveat**: switching an existing release from `config.apiKey` (or `existingSecret`) to `externalSecret.enabled: true` deletes the chart-managed Secret on upgrade; ESO must reconcile the new Secret quickly enough that the Deployment doesn't restart with an unresolvable env reference. Plan the cutover during a maintenance window or stage via `helm upgrade --post-renderer` to apply the ExternalSecret first.
```

- [ ] **Step 3: Commit**

```bash
git add deploy/helm/hyperping-exporter/Chart.yaml CHANGELOG.md
git commit -m "chore(helm): bump chart to 1.5.0 and document the release in CHANGELOG

Chart 1.1.0 -> 1.5.0; appVersion 1.4.0 -> 1.4.1 to track the latest
released binary. CHANGELOG section sits between [Unreleased] and
[1.4.1], preserving strictly descending order. Highlights cover the
production-readiness defaults, ExternalSecret support, fail() guards,
and the new CI gates. Upgrade notes flag the appVersion / chart-label
flip, the NP default change, multi-replica rejection, secret-source
constraints, ESO apiVersion default, the PSS labelling responsibility,
and the ESO cutover caveat."
```

---

### Task 11: Extend render harness with all new assertions and run the full suite

**Files:** Modify `deploy/helm/hyperping-exporter/tests/render_test.py`. No other production changes.

This is where every assertion the prior tasks deferred lands. Doing it last avoids the TDD-discipline issue of asserting against partial values; every value is committed and settled by now, so the assertions are written once against the final state and exercised in one harness run.

- [ ] **Step 1: Bump `EXPECTED_VERSION` literal**

In `render_test.py`, find the line `EXPECTED_VERSION = "1.4.0"` and change to:
```python
EXPECTED_VERSION = "1.4.1"
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

- [ ] **Step 3: Extend Case 1's `expected_versions` to enumerate the post-NP-default-on resources**

The defaults render now includes a NetworkPolicy. Replace the existing `expected_versions = {...}` build with:

```python
    expected_versions: dict[str, str] = {}
    for kind in ("Secret", "Service", "Deployment", "NetworkPolicy"):
        for d in find_all(rendered, kind):
            expected_versions[f"{kind}/{d['metadata']['name']}"] = EXPECTED_VERSION
```

This is the only structural change to Case 1; it tracks every resource the common-labels helper touches in the post-Task-8 default render.

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

    # Case 22 — cacheTTL numeric round-trip (still emits a string arg the
    # binary's duration parser can handle, since values.yaml default is
    # quoted "60s"). Documents the contract: the chart NEVER emits a bare
    # integer because the template wraps it in printf "%s" via toJson.
    rendered = helm_template("cache-ttl-numeric.values.yaml")
    args = deployment_args(rendered)
    cache_arg = [a for a in args if a.startswith("--cache-ttl=")][0]
    # "60" with no unit makes it into the arg list; the admission job's
    # live boot in CI catches the runtime rejection. This case is the
    # render-time documentation; the runtime check is in admission_test.sh
    # (the defaults fixture uses the quoted default, so the live boot
    # always succeeds for the supported configuration).
    assert_eq(cache_arg, "--cache-ttl=60",
              "cache-ttl-numeric: chart emits whatever values.yaml supplies (warning in docs)")
```

Add fixture `cache-ttl-numeric.values.yaml`:
```yaml
config:
  apiKey: x
  cacheTTL: 60
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
Expected: clean working tree; 10 commits on the branch (Tasks 2/3/4/5/6/7/8/9/10 plus this one), files-changed list covers the chart, the CI workflow, the Makefile, the CHANGELOG, the docs/plans entry.

- [ ] **Step 10: Commit the harness extension**

```bash
git add deploy/helm/hyperping-exporter/tests/render_test.py \
        deploy/helm/hyperping-exporter/tests/fixtures/cache-ttl-numeric.values.yaml
git commit -m "test(helm): lock all production-readiness defaults via extended render harness

Single commit lands every assertion the prior tasks deferred so each
production change is exercised against settled values, not partial
state. EXPECTED_VERSION bumps to 1.4.1; EXPECTED_CHART_LABEL pins the
helm.sh/chart label at hyperping-exporter-1.5.0. Case 1 enumeration
now covers NetworkPolicy in the default render. 13 new cases cover
the five fail() paths, ExternalSecret defaults + override + required()
failure, vanilla NP (with the explicit 'no RFC1918 except' check
against the documented egress contract), Cilium variant, replicas-zero
(secret-exempt), PDB suppression at replicaCount=1, and cacheTTL
numeric pass-through documented at the render layer."
```

---

### Task 12: Open the PR

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
- **`--cache-ttl` renders via `toJson`**. Effect: every optional-flag arg in the chart now goes through one canonical path. `values.yaml` warns against bare-integer cacheTTL values.
- **PSS-restricted securityContext locked-in by render tests AND admitted live in CI**. The kind admission job applies the defaults fixture LIVE so the pod boots under `readOnlyRootFilesystem: true`; this is the runtime proof item 4 demanded.
- **NetworkPolicy on by default**. Vanilla NP permits DNS to kube-system and TCP/443 to 0.0.0.0/0 (no RFC1918 except — matches the documented egress contract). Optional `cilium.io/v2 CiliumNetworkPolicy` variant for FQDN-restricted egress.
- **ExternalSecret support** via `external-secrets.io/v1` (default) with `externalSecret.apiVersion` override for legacy ESO ≤ 0.15. Mutually exclusive with `apiKey` / `existingSecret`.
- **Three render-time `fail()` guards**: multi-source secret config, missing secret source at replicaCount≥1, and `replicaCount > 1`. `replicaCount: 0` is supported and secret-source-exempt.
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

- **Action SHA pinning**: Task 1 Step 6 resolves the live SHAs. The plan MUST NOT merge with `<KIND_ACTION_SHA>` or `<KINDEST_NODE_DIGEST>` literals in the workflow; the executing subagent verifies and substitutes before commit. If `helm/kind-action v1.12.0` does not exist at execution time, use the actual latest stable tag.
- **kubeconform CRDs catalog availability**: the workflow pulls schemas from `raw.githubusercontent.com/datreeio/CRDs-catalog`. Lowercase-kind filename convention is now used. If GitHub rate-limits the runner, the job will fail intermittently; document if encountered and pin a tagged catalog release.
- **kind boot time + live admission**: ~60-90 seconds on GitHub-hosted runners plus a rollout-status wait of up to 120 seconds for the defaults fixture pod. Total job time ~3-5 minutes; acceptable.
- **kind admission-config mount**: kind resolves relative `hostPath` against its own cwd, not `$GITHUB_WORKSPACE`. The `admission_test.sh` wrapper rewrites the placeholder `__ABSPATH_PLACEHOLDER__` in a temporary copy of `kind-pss.yaml` to the absolute repo root at run time, so the PSS admission plugin is always configured. helm/kind-action processes the config file before our wrapper runs; we therefore pass our own pre-rewritten config path to kind-action via the `config:` input rather than letting kind-action consume the placeholder file directly. This is done by setting `config: deploy/helm/hyperping-exporter/tests/kind-pss.yaml` and having the workflow first run a one-line `sed -i` substitution BEFORE the kind-action step — add that as a discrete workflow step ahead of the `helm/kind-action@<SHA>` step.
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
- Every edge case (replicas:0, NP-conflict, ExternalSecret-conflict, multi-replica, missing-store) is tested as a `fail()` path or admission path, not characterised into best-effort warnings.
- Real CI gates: live pod boot for the readOnlyRootFilesystem proof, kubeconform for offline schema validation including CRDs, PSS admission for restricted-namespace enforcement.
- Strict TDD discipline: production changes commit BEFORE the assertion-only commit, which lands the entire harness extension once against fully settled values rather than in TDD-incomplete fragments per task.
- Action SHAs, kindest/node digest, and ESO apiVersion are all resolved against upstream at execution time (Task 1 Step 6); no placeholders survive into the workflow.
- Version policy is explicit: chart 1.5.0 (not 1.2.0, which collides with the binary slot), appVersion 1.4.1 (tracks the latest released binary).
- Caveats the chart cannot fix (operator-side namespace labelling, NP connectivity semantics, ESO operator presence, kindest/node tag mutability between digest pins) are surfaced in the upgrade notes rather than papered over.
