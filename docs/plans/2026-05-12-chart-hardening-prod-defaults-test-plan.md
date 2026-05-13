# Test Plan — Chart UX Hardening: Production-Readiness Defaults

> Companion to `2026-05-12-chart-hardening-prod-defaults.md`. Reconciles the user-approved testing strategy (PSS kind admission in CI as a blocking job; kubeconform as a separate CI step using `--schema-location` for CRDs; `replicaCount: 0` allowed; secret-source conflicts fail loudly via `{{- fail -}}`; single PR cutover) against the final plan's 10 durable contracts (C1-C10), the 8 round-4 resolutions (R4-1..R4-8), and the per-task fixture surface.

> **WITHDRAWN — PDB cases T21 and T28 (Cases 20, 21, 29).** Per the R9 deferred decision in the implementation plan (line 15), the PDB rendering gate was unreachable in production (`validateReplicaCount` aborts on `replicaCount > 1` before the gate is ever reached), so the chart 1.5.0 release dropped the `podDisruptionBudget` block from `values.yaml` and removed `templates/pdb.yaml` entirely. The PDB-related test cases (T21 / Case 29; T28 / Cases 20, 21) and the `internal._testBypassReplicaCheck` test-only knob are therefore withdrawn from this plan. Case 13's `find_pdb is None` assertion on `replicas-zero` remains as a regression lock-in (covered by T16). See CHANGELOG.md 1.5.0 "Removed" section for the operator-facing upgrade note.

## Strategy reconciliation (no user approval required)

The final plan does not invalidate any cost-shaping decision the user already approved. Three points where the plan extends the strategy without changing its scope:

- **Live-boot fixture set.** Strategy approved "spin up kind in CI, label namespace `pod-security.kubernetes.io/enforce: restricted`, render, kubectl apply, assert no admission errors." Plan formalizes this into a dispatch table (Contract C1.4) where exactly two fixtures (`pss-restricted`, `networkpolicy-default`) boot live with `kubectl rollout status`, and two CRD-bearing fixtures (`external-secret`, `networkpolicy-cilium`) are validated by kubeconform only. Same CI surface; same blocking-job behavior; no new external dependencies the user did not approve.
- **kubeconform CRD scope.** Strategy directed `kubeconform -strict -summary -schema-location default -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/...'`. Plan pins `KUBECONFORM_CATALOG_REF` to a captured tagged release (Contract C5.3) instead of `main`, eliminating schema-fetch flake. Strictness and surface are unchanged.
- **Test-only bypass key.** R4-8 introduces `internal._testBypassReplicaCheck` as a fixture-only, undocumented knob whose sole purpose is to render PDB structurally for Case 21 without weakening `validateReplicaCount` (R4-1). The bypass is gated by a `validateNoTestKeys` `fail()` helper. This is a test-affordance, not a product surface, and adds no manual-QA step.

The harness strategy ("PyYAML render harness + `kubeconform` + kind PSS admission + go test smoke") holds. Proceeding.

---

## Harness requirements

Built across Task 1, Task 6, Task 8. Tests below depend on these in the order listed; each harness MUST exist before the cases that consume it.

### H1 — PyYAML render harness extensions (Tasks 1, 4, 6, 7, 9)

- **What it does:** `python3 deploy/helm/hyperping-exporter/tests/render_test.py` shells out to `helm template testrel <chart> -f <fixture>`, parses rendered YAML through `yaml.safe_load_all`, asserts structural properties via per-case literal expected values.
- **Exposed API (new helpers added across plan tasks):**
  - Task 6 adds `assert_fail(case_name, fixture_path, expected_stderr_substring)` (Contract C6.1) which runs `helm template` and asserts both non-zero exit AND substring in stderr.
  - Task 6 adds helpers as needed for env-block absence (`deployment_env`), ExternalSecret/Secret kind selection (`find_external_secret`, extension of existing `find_secret`).
  - Task 7 adds `find_network_policy` and `find_cilium_network_policy`, plus `find_pdb`, plus `peer_selector_walk` for Cilium peer-shape assertions.
  - Existing helpers reused: `find_deployment`, `find_secret`, `find_all`, `deployment_args`, `deployment_image`, `labels_with_version`, `assert_scalars_clean`, `assert_eq`.
- **Complexity:** moderate; ~250 LOC added across two task commits. The `assert_fail` helper is the single non-trivial new primitive; the rest are accessor functions plus per-case fixture invocations.
- **Tests depending on it:** every render-time test in this plan (T1..T26 below).

### H2 — `kubeconform` schema validation (Task 8)

- **What it does:** runs `helm template testrel <chart> -f <fixture> | kubeconform -strict -summary -schema-location default -schema-location <catalog-tag-pinned>` per fixture. Exits 0 only when every rendered manifest passes K8s core schema validation plus the relevant CRD schema (ServiceMonitor, ExternalSecret, CiliumNetworkPolicy).
- **Exposed API:** `make helm-kubeconform` Makefile target (Contract C5.4) and a workflow step (`Kubeconform`) in `.github/workflows/helm-ci.yml`. The script iterates the fixture set the render harness uses, but adds CRD-bearing fixtures (`external-secret.values.yaml`, `networkpolicy-cilium-*.values.yaml`) that the harness validates via render only.
- **Complexity:** low; ~50 LOC of bash plus a Makefile target plus a workflow step. Schema-fetch pinning to `KUBECONFORM_CATALOG_REF` (a captured tag, not `main`) eliminates upstream-drift flake.
- **Tests depending on it:** T22, T23, T24, T25, T26 (CRD-bearing schema validation), plus the cross-fixture smoke (T28) and the bisect rehearsal (T31).

### H3 — kind PSS-restricted admission driver (Task 8)

- **What it does:** `bash deploy/helm/hyperping-exporter/tests/admission_test.sh` sources `admission_env.sh` for SSOT names, creates a kind cluster from `kind-pss.yaml` (after placeholder substitution), creates the test namespace labelled `pod-security.kubernetes.io/enforce=restricted`, then for each fixture in the dispatch table either:
  - **live boot:** `helm template` → `kubectl apply -n $NS -f -` → `kubectl rollout status deploy/<name> -n $NS --timeout=120s` (proves the rendered pod is admitted under PSS-restricted AND starts under `readOnlyRootFilesystem: true`); between fixtures, `kubectl delete --ignore-not-found ... -l app.kubernetes.io/instance=$REL` followed by `kubectl wait --for=delete --timeout=60s ...`, OR
  - **kubeconform-only:** skipped from the live path; validated by H2 separately.
- **Exposed API:**
  - `make helm-pss` target — runs the full dispatch table.
  - `make helm-pss-clean` — `kind delete cluster --name "${KIND_CLUSTER_NAME}"`.
  - `.github/workflows/helm-ci.yml` `pss-admission` step — runs the same driver in CI.
- **Complexity:** moderate-high; ~150 LOC bash + ~80 LOC workflow + ~40 LOC kind config + ~20 LOC admission-config plus the resolver script (Contract C4) that pins kind binary, kindest/node digest, and helm-kind-action SHA.
- **Tests depending on it:** T27 (PSS admission live boot), T28 (NetworkPolicy default admission live boot).

### H4 — Resolver and SSOT (Task 1)

- **What it does:** `tests/scripts/resolve_pins.sh` reads `tests/pins.expected.yaml` and verifies every upstream pin (helm-kind-action tag, kind binary minimum, `actions/checkout` tag, `azure/setup-helm` tag, `actions/setup-python` tag, `datreeio/CRDs-catalog` tag, Docker Hub `khaledsalhabdeveleap/hyperping-exporter:1.4.1` existence, kindest/node v1.34.x digest). Fails-loud on tag drift or missing image; surfaces `USER DECISION REQUIRED:` to the operator.
- **Exposed API:** runs as the first workflow step (`Resolve pins`); also runs locally before `make helm-ci`.
- **Complexity:** moderate; ~80 LOC bash. Authoritative for every external reference the rest of the harness uses.
- **Tests depending on it:** T30 (pin-drift gate).

---

## Test plan

Tests are numbered in priority order. Each test maps to an `assert_*` invocation in `render_test.py` (Cases 1-36 referenced from the plan) or to a CI step (kubeconform, admission, audits). The case-number column ties each test to the implementation-plan case it implements.

### Priority 1 — Red-baseline contract gates (must turn green for plan acceptance)

These are the harness's load-bearing checks: a regression here means the chart breaks the user's contract for apply-time correctness.

#### T1 — Default render emits exactly the published image and version label on every labelled resource

- **Name:** "Operator installing the chart with no overrides receives a pod running `khaledsalhabdeveleap/hyperping-exporter:1.4.1` and every chart-labelled resource carries `app.kubernetes.io/version: 1.4.1`."
- **Type:** scenario
- **Disposition:** extend (existing Case 1 in `render_test.py`, with anchors bumped per Task 9)
- **Harness:** H1
- **Preconditions:** `default.values.yaml` fixture (existing); `EXPECTED_IMAGE_DEFAULT`, `EXPECTED_VERSION`, `EXPECTED_CHART_LABEL` constants point at the anchors `LIVE_BOOT_IMAGE`, `APP_VERSION`, `hyperping-exporter-${CHART_VERSION}` respectively.
- **Actions:** `helm template testrel <chart> -f tests/fixtures/default.values.yaml`. Parse output; extract Deployment container image; walk all docs and collect `app.kubernetes.io/version` labels keyed by `<kind>/<name>`; extract `helm.sh/chart` label from every labelled resource.
- **Expected outcome:**
  - `deployment_image(rendered) == "khaledsalhabdeveleap/hyperping-exporter:1.4.1"` (anchor `LIVE_BOOT_IMAGE`).
  - `labels_with_version(rendered) == { "Secret/...": "1.4.1", "Service/...": "1.4.1", "Deployment/...": "1.4.1", "NetworkPolicy/...": "1.4.1" }` using set-difference enumeration with a per-kind diff message (Contract C8.3); the set MUST include `NetworkPolicy` once Task 7 lands the default-on flip.
  - `helm.sh/chart` label on every labelled resource equals `EXPECTED_CHART_LABEL`.
  - Source of truth: plan anchors `APP_VERSION`, `CHART_VERSION`, `LIVE_BOOT_IMAGE`; Chart.yaml `version` and `appVersion`.
- **Interactions:** every common-labels-bearing template (Secret, Service, Deployment, NetworkPolicy). A regression in `_helpers.tpl` labels block flips this assertion before any per-resource test.

#### T2 — Default render emits exactly the baseline args list in the documented order

- **Name:** "Operator with no config overrides sees the container started with exactly `[--listen-address=:9312, --metrics-path=/metrics, --cache-ttl=60s, --log-level=info, --log-format=text]`."
- **Type:** scenario / invariant
- **Disposition:** existing (Case 1 args assertion in `render_test.py`)
- **Harness:** H1
- **Preconditions:** `default.values.yaml`; `BASELINE_ARGS` constant unchanged across Task 4's safe-arg helper migration (per R4-7 bisect-green proof).
- **Actions:** render; extract `deployment_args(rendered)`.
- **Expected outcome:** `deployment_args(rendered) == BASELINE_ARGS` exactly (list equality). Source of truth: plan File Structure `deployment.yaml` args migration table; `main.go` flag defaults.
- **Interactions:** `_helpers.tpl` `hyperping-exporter.arg` helper (Task 4). If the helper produces byte-divergent output, this test goes red; the implementer fixes the helper, not the assertion (per R4-7).

#### T3 — Cache-TTL bare integer aborts the render with the operator-actionable message

- **Name:** "An operator who supplies `config.cacheTTL: 60` (unquoted integer, missing unit) gets a render-time error naming the invalid value and the required form."
- **Type:** boundary / regression (validator gate)
- **Disposition:** new (Case 23 in `render_test.py`)
- **Harness:** H1 (`assert_fail`)
- **Preconditions:** `tests/fixtures/cache-ttl-int-fails.values.yaml` (Task 6 Step 8) sets `apiKey: x`, `config.cacheTTL: 60`; `validateCacheTTL` defined in Task 4 Step 1 and included from Task 4 Step 2.
- **Actions:** `helm template testrel <chart> -f tests/fixtures/cache-ttl-int-fails.values.yaml`.
- **Expected outcome:** exit code != 0; stderr contains `"must be a quoted Go duration"`. Source of truth: Contract C2.3 `validateCacheTTL` body; plan task 4 Step 4 inline smoke matches the same substring.
- **Interactions:** `_helpers.tpl` validators include block at top of `deployment.yaml`.

#### T4 — Cache-TTL non-default duration string renders byte-identically through the safe-arg helper

- **Name:** "An operator setting `config.cacheTTL: 30s` sees the rendered container args contain `--cache-ttl=30s` exactly (not `%!s(int=30)`, not `--cache-ttl=\"30s\"`, not `--cache-ttl=30`)."
- **Type:** boundary / regression
- **Disposition:** new (Case 22)
- **Harness:** H1
- **Preconditions:** `cache-ttl-numeric.values.yaml` fixture (note plan name `cache-ttl-numeric` actually carries a quoted-string non-default `"30s"` per Task 6 Step 9 description); Task 4's safe-arg helper live.
- **Actions:** render; extract `deployment_args(rendered)`.
- **Expected outcome:** `deployment_args(rendered) == ["--listen-address=:9312", "--metrics-path=/metrics", "--cache-ttl=30s", "--log-level=info", "--log-format=text"]`. Source of truth: Contract C2.1 helper body.
- **Interactions:** confirms the safe-arg helper is byte-equivalent to inline form for the non-default case (T2 covers the default case).

#### T5 — Numeric log-level value renders as a quoted JSON string with no Sprig formatting artefact

- **Name:** "If `config.logLevel` is provided as a number (Helm coerces to int), the args list still renders `--log-level=<n>` cleanly with no `%!s(...)` artefact bleed-through."
- **Type:** boundary / regression
- **Disposition:** new (Case 24)
- **Harness:** H1
- **Preconditions:** `log-level-numeric.values.yaml` sets a numeric logLevel value.
- **Actions:** render; extract args.
- **Expected outcome:** the corresponding arg in `deployment_args(rendered)` is a `str`, has no `%!s` substring, and equals `--log-level=<value>`. Source of truth: Contract C2.1 (`%v` + `toString` belt-and-braces).
- **Interactions:** proves the safe-arg helper handles unexpected typed inputs across every flag, not just cacheTTL.

#### T6 — Metrics-path with characters requiring JSON-escape transports byte-for-byte

- **Name:** "An operator who sets `config.metricsPath` to a value containing `\"` and `\\` sees the chart pass the same bytes into the container args."
- **Type:** boundary / regression
- **Disposition:** new (Case 25)
- **Harness:** H1
- **Preconditions:** `metrics-path-with-special-chars.values.yaml` sets `metricsPath: '/m"e\\trics'` (literal `"` and `\`).
- **Actions:** render; extract args; locate the `--metrics-path=...` entry.
- **Expected outcome:** the arg equals `--metrics-path=/m"e\trics` exactly (the YAML decoder reverses toJson's escapes); `assert_scalars_clean(rendered, "metrics-path special")` passes. Source of truth: Contract C2 invariant; existing Case 9 (mcp-url-query) precedent for `"`+`\` round-trip.
- **Interactions:** confirms the safe-arg helper covers EVERY arg, not just the originally-toJson'd `excludeNamePattern` and `mcpUrl`.

### Priority 2 — Existing characterization tests (must stay green throughout the branch)

These are the existing Cases 2-9 in `render_test.py` (already shipped on `main` post-v1.4.1). They MUST remain green at every commit; any regression flips Contract C1's bisect-green property.

#### T7 — Existing-secret path renders no chart-managed Secret and the container env references the named external Secret

- **Name:** "An operator setting `existingSecret: my-external-secret` gets no chart-managed Secret in the release; the Deployment env references `my-external-secret` for `HYPERPING_API_KEY`."
- **Type:** scenario / regression
- **Disposition:** existing (Case 2)
- **Harness:** H1
- **Preconditions:** `existing-secret.values.yaml` (existing fixture).
- **Actions:** render; check `find_secret(rendered) is None`; extract env block.
- **Expected outcome:** Secret absent; `env[0].valueFrom.secretKeyRef.name == "my-external-secret"`. Source of truth: existing chart behavior pre-v1.5.0; preserved by Task 6 Step 6 (`templates/secret.yaml` only suppresses on `externalSecret.enabled`, not on `existingSecret`).
- **Interactions:** confirms the legacy two-line guard's replacement (Task 6 Step 3) preserves the `existingSecret` path.

#### T8 — Operator-supplied ASCII regex renders as exactly one new arg appended to baseline

- **Name:** "Setting `config.excludeNamePattern: '^prod-'` adds exactly one arg `--exclude-name-pattern=^prod-` after the baseline."
- **Type:** scenario / regression
- **Disposition:** existing (Case 3)
- **Harness:** H1
- **Preconditions:** `ascii-regex.values.yaml`.
- **Actions:** render; extract args.
- **Expected outcome:** `deployment_args(rendered) == BASELINE_ARGS + ["--exclude-name-pattern=^prod-"]`. Source of truth: existing v1.4.1 contract.
- **Interactions:** Task 4's helper migration must preserve byte-equivalence for excludeNamePattern.

#### T9 — README regex (backslash, brackets, pipe) round-trips through `toJson` byte-for-byte

- **Name:** "The README's `'\[DRILL|\[TEST'` example reaches the container args exactly as documented."
- **Type:** scenario / regression
- **Disposition:** existing (Case 4)
- **Harness:** H1
- **Preconditions:** `readme-regex.values.yaml`.
- **Actions:** render; extract args; compare to `BASELINE_ARGS + [f"--exclude-name-pattern={pattern}"]` where `pattern = r"\[DRILL|\[TEST"`.
- **Expected outcome:** exact byte equality. Source of truth: README example; existing harness contract.

#### T10 — Single-quote-bearing regex transports through `toJson` byte-for-byte

- **Name:** "A regex containing `'` (e.g. `^test'name`) reaches the container args with the literal apostrophe."
- **Type:** boundary / regression
- **Disposition:** existing (Case 5)
- **Harness:** H1
- **Preconditions:** `single-quote-regex.values.yaml`.
- **Actions:** render; extract args.
- **Expected outcome:** `deployment_args(rendered) == BASELINE_ARGS + ["--exclude-name-pattern=^test'name"]`.

#### T11 — `mcpUrl` alone renders as one new arg

- **Name:** "Setting `config.mcpUrl: https://mcp.example.com/v1/mcp` appends `--mcp-url=https://mcp.example.com/v1/mcp` to the args list."
- **Type:** scenario / regression
- **Disposition:** existing (Case 6)
- **Harness:** H1
- **Preconditions:** `mcp-url.values.yaml`.
- **Actions:** render; extract args.
- **Expected outcome:** literal equality.

#### T12 — Both `excludeNamePattern` and `mcpUrl` render in template order

- **Name:** "Setting both flags appends them to the args list in the order the template renders them (`excludeNamePattern` first, then `mcpUrl`)."
- **Type:** scenario / regression
- **Disposition:** existing (Case 7)
- **Harness:** H1
- **Preconditions:** `both-flags.values.yaml`.
- **Actions:** render; extract args.
- **Expected outcome:** literal equality.

#### T13 — Regex with literal double quotes round-trips through `toJson`

- **Name:** "Setting `excludeNamePattern: '\"foo\"'` reaches the container args with embedded double quotes intact."
- **Type:** boundary / regression
- **Disposition:** existing (Case 8)
- **Harness:** H1
- **Preconditions:** `quote-regex.values.yaml`.
- **Actions:** render; extract args.
- **Expected outcome:** `deployment_args(rendered) == BASELINE_ARGS + ['--exclude-name-pattern="foo"']`.

#### T14 — `mcpUrl` carrying `?token="ab\cd"` round-trips byte-for-byte (load-bearing mutation case)

- **Name:** "An mcpUrl whose query value embeds `\"` and `\\` reaches the container args byte-for-byte; mutation test for the `toJson` rendering."
- **Type:** boundary / regression / differential (mutates template to prove load-bearing)
- **Disposition:** existing (Case 9)
- **Harness:** H1
- **Preconditions:** `mcp-url-query.values.yaml`.
- **Actions:** render; extract args.
- **Expected outcome:** `deployment_args(rendered) == BASELINE_ARGS + ['--mcp-url=https://mcp.example.com/v1/mcp?token="ab\\cd"']`. Source of truth: PR52 review's load-bearing mutation verification.

### Priority 3 — Validator and mutual-exclusion gates (new in v1.5.0)

These prove the new `fail()` validators (`validateSecretSources`, `validateReplicaCount`, `validateNoTestKeys`) abort the render with operator-actionable diagnostics, AND that the env-block gate suppresses correctly per R4-2 + R4-6.

#### T15 — Multi-replica (`replicaCount > 1`) always aborts the render with the validator message

- **Name:** "An operator who sets `replicaCount: 2` sees the chart abort the render with a message explaining the single-replica design."
- **Type:** boundary / regression
- **Disposition:** new (Case 14, `replicas-multi`)
- **Harness:** H1 (`assert_fail`)
- **Preconditions:** `replicas-multi.values.yaml` sets `apiKey: x`, `replicaCount: 2`.
- **Actions:** `helm template`.
- **Expected outcome:** exit != 0; stderr substring identifies `replicaCount` and the single-replica constraint. Source of truth: Contract C1.1 commit-atomicity for `validateReplicaCount`; R4-1 (no bypass).
- **Interactions:** Task 6's validator include block in `deployment.yaml`.

#### T16 — `replicaCount: 0` renders successfully with no secret source and produces `replicas: 0`, no env block, no PDB

- **Name:** "An operator setting `replicaCount: 0` (scaled-to-zero) with no apiKey/existingSecret/externalSecret renders cleanly: Deployment has `replicas: 0`, container env block omitted, PDB absent."
- **Type:** scenario / boundary (the R4-6 exemption)
- **Disposition:** new (Case 13, `replicas-zero`)
- **Harness:** H1
- **Preconditions:** `replicas-zero.values.yaml` sets `replicaCount: 0` and provides NO secret source.
- **Actions:** render; check Deployment `replicas`; check container env list; check PDB absence.
- **Expected outcome:**
  - `find_deployment(rendered)["spec"]["replicas"] == 0`.
  - `find_deployment(rendered)["spec"]["template"]["spec"]["containers"][0].get("env", [])` does NOT contain `HYPERPING_API_KEY` (env block suppressed by the `gt (int (include "hyperping-exporter.secretSourceCount" .)) 0` wrap per R4-2).
  - `find_pdb(rendered) is None`.
  - Source of truth: R4-6 boolean tree; R4-2 int-vs-int gate.
- **Interactions:** confirms the env-block gate uses the SAME helper as `validateSecretSources` (so future weakening of one breaks both).

#### T17 — Setting both `apiKey` and `existingSecret` aborts the render with a conflict message naming the offending pair

- **Name:** "Operator misconfiguring two secret sources (`apiKey` + `existingSecret`) sees a render-time abort that names which pair conflict."
- **Type:** boundary / regression
- **Disposition:** new (Case 15)
- **Harness:** H1 (`assert_fail`)
- **Preconditions:** `secret-conflict-apikey-and-existing.values.yaml`.
- **Actions:** render.
- **Expected outcome:** exit != 0; stderr contains `apiKey` AND `existingSecret`. Source of truth: R4-6 step 2.

#### T18 — Setting both `apiKey` and `externalSecret.enabled` aborts with a conflict message

- **Name:** Same as T17 but for the apiKey + externalSecret conflict.
- **Type:** boundary / regression
- **Disposition:** new (Case 16)
- **Harness:** H1 (`assert_fail`)
- **Preconditions:** `secret-conflict-apikey-and-external.values.yaml`.
- **Actions:** render.
- **Expected outcome:** exit != 0; stderr names the conflict pair.

#### T19 — Setting both `existingSecret` and `externalSecret.enabled` aborts with a conflict message

- **Name:** Same shape; existingSecret + externalSecret conflict.
- **Type:** boundary / regression
- **Disposition:** new (Case 17)
- **Harness:** H1 (`assert_fail`)
- **Preconditions:** `secret-conflict-existing-and-external.values.yaml`.
- **Actions:** render.
- **Expected outcome:** exit != 0; stderr names the conflict pair.

#### T20 — Missing all three sources with `replicaCount: 1` aborts the render

- **Name:** "Operator with `replicaCount: 1` and no secret source set sees the render abort with a missing-source error."
- **Type:** boundary / regression
- **Disposition:** new (Case 18, `secret-source-missing`)
- **Harness:** H1 (`assert_fail`)
- **Preconditions:** `secret-source-missing.values.yaml` sets `replicaCount: 1`, no source.
- **Actions:** render.
- **Expected outcome:** exit != 0; stderr describes the missing-source condition and the three documented options. Source of truth: R4-6 step 3.

#### T21 — WITHDRAWN (R9): Test-only bypass + `replicaCount: 2` + `podDisruptionBudget.enabled: true` ALWAYS aborts, AND no PDB appears in any captured stdout

> **WITHDRAWN per R9 deferred decision (implementation plan line 15).** The PDB template was removed in chart 1.5.0; `podDisruptionBudget.enabled` is no longer a chart value and `internal._testBypassReplicaCheck` is no longer referenced. The leakage-proof rationale no longer applies because there is no longer a gate to leak past. The historical specification below is preserved for review trace; do NOT implement.

- **Name:** "Even when the test-only `internal._testBypassReplicaCheck` key is supplied, `replicaCount > 1` aborts the render BEFORE the PDB template renders; the failed render contains no `kind: PodDisruptionBudget`."
- **Type:** invariant / regression (leakage proof per R4-8)
- **Disposition:** new (Case 29, `pdb-structural-internal-bypass-multi-replica-fails`)
- **Harness:** H1 (`assert_fail` extended to capture stdout and grep for forbidden substrings)
- **Preconditions:** Case 29's bespoke fixture sets `internal._testBypassReplicaCheck: true`, `replicaCount: 2`, `podDisruptionBudget.enabled: true`, plus a valid secret source.
- **Actions:** render; capture stdout AND stderr; assert returncode != 0 AND `validateReplicaCount` substring in stderr AND `kind: PodDisruptionBudget` absent from captured stdout.
- **Expected outcome:** per R4-1 the validator always aborts on `replicaCount > 1`; per R4-8 the bypass cannot smuggle a PDB into a render the validator rejected. Source of truth: R4-1, R4-8.
- **Interactions:** proves the bypass is purely additive to the PDB gate, never weakens the validator.

### Priority 4 — Coverage tests for new templates (ExternalSecret, Cilium, PDB)

These prove every new template is structurally correct and exercised by at least one positive render.

#### T22 — ExternalSecret enabled renders an `external-secrets.io/v1beta1 ExternalSecret` (deferred decision; see plan tech-stack note) and suppresses the chart-managed Secret

- **Name:** "Operator enabling `externalSecret.enabled: true` with a valid `secretStoreRef` sees an `ExternalSecret` resource rendered targeting the chart's Secret name; no chart-managed `Secret` in the release."
- **Type:** scenario / coverage
- **Disposition:** new (Case 10, `external-secret`)
- **Harness:** H1 + H2 (schema)
- **Preconditions:** `external-secret.values.yaml` sets `externalSecret.enabled: true`, `externalSecret.secretStoreRef.name: my-store`, `externalSecret.secretStoreRef.kind: SecretStore` (or `ClusterSecretStore`), refreshInterval explicit, plus apiKey/existingSecret cleared.
- **Actions:** render; assert one ExternalSecret resource present; assert chart-managed Secret absent; assert ExternalSecret's `target.name` equals the chart's resolved secret name; assert `target.template` or `data` mapping populates `api-key`.
- **Expected outcome:** structural match per CRD; mutual exclusion with chart Secret. Source of truth: external-secrets.io v1 CRD; Task 6 Step 5 template body.
- **Interactions:** H2 (kubeconform with the CRD catalog) validates schema correctness independently.

#### T23 — ExternalSecret with defaults (no `refreshInterval` override) renders the documented default

- **Name:** "Operator enabling ExternalSecret without setting `refreshInterval` sees `refreshInterval: 1h` (the chart default) in the rendered ExternalSecret."
- **Type:** scenario / regression
- **Disposition:** new (Case 11, `external-secret-defaults`)
- **Harness:** H1 + H2
- **Preconditions:** `external-secret-defaults.values.yaml`.
- **Actions:** render; extract ExternalSecret's `spec.refreshInterval`.
- **Expected outcome:** equals `1h` (or whichever default the plan implementer settles on; the test plan's expected literal updates atomically with `values.yaml`).
- **Interactions:** documents the chart's default ESO reconcile cadence.

#### T24 — ExternalSecret missing `secretStoreRef.name` aborts the render

- **Name:** "Operator enabling ExternalSecret without setting `secretStoreRef.name` sees a render-time abort naming the missing field."
- **Type:** boundary
- **Disposition:** new (Case 12, `external-secret-missing-store`)
- **Harness:** H1 (`assert_fail`)
- **Preconditions:** `external-secret-missing-store.values.yaml` sets `externalSecret.enabled: true` but omits `secretStoreRef.name`.
- **Actions:** render.
- **Expected outcome:** exit != 0; stderr names `secretStoreRef.name` as the missing field.

#### T25 — Cilium variant with no `ingressFrom` renders egress-only `CiliumNetworkPolicy` and suppresses the vanilla NetworkPolicy

- **Name:** "Operator enabling `networkPolicy.fqdnRestriction.enabled: true` with no ingress configured sees a `cilium.io/v2 CiliumNetworkPolicy` with `egress:` populated and no `ingress:` key; the vanilla `NetworkPolicy` template is absent."
- **Type:** scenario / coverage
- **Disposition:** new (Case 30, `cilium-egress-only`)
- **Harness:** H1 + H2
- **Preconditions:** `networkpolicy-cilium-defaults.values.yaml`.
- **Actions:** render; find Cilium NP; check `egress` populated, `ingress` not a key (Contract C3.5: dropped, not `[]`); confirm vanilla NetworkPolicy absent.
- **Expected outcome:** Cilium resource present with correct apiVersion `cilium.io/v2` and kind `CiliumNetworkPolicy`; ingress key absent; egress block has `toFQDNs` for `api.hyperping.io`. Source of truth: Contract C3.4-C3.5; Task 7 Step 3 template body; H2 schema validation independent.

#### T26 — Cilium variant with `matchExpressions` peer renders a flattened `EndpointSelector`

- **Name:** "Operator providing `ingressFrom: [{podSelector: {matchExpressions: [...]}}]` with `fqdnRestriction.enabled: true` sees the Cilium peer's `EndpointSelector` flatten `matchExpressions` correctly."
- **Type:** scenario / coverage
- **Disposition:** new (Case 32, `cilium-ingress-matchexpressions`)
- **Harness:** H1 + H2
- **Preconditions:** `networkpolicy-cilium-matchexpressions.values.yaml`.
- **Actions:** render; find the Cilium policy's ingress[].fromEndpoints[].matchExpressions; compare to fixture.
- **Expected outcome:** matchExpressions present and structurally correct per CiliumNetworkPolicy schema. Source of truth: Contract C3.1.

#### T27 — Cilium variant aborts on `ipBlock`-only peers and selector-less peers and foreign namespace labels

- **Name:** Three cases (34, 35, 36): "Operator providing unsupported peer shapes sees a render abort naming the offending entry and the supported shape."
- **Type:** boundary
- **Disposition:** new (Cases 34, 35, 36; each is a separate `assert_fail`)
- **Harness:** H1 (`assert_fail`)
- **Preconditions:** `networkpolicy-cilium-ipblock-only.values.yaml`, `networkpolicy-cilium-selectorless.values.yaml`, `networkpolicy-cilium-foreign-namespace-label.values.yaml`.
- **Actions:** render each.
- **Expected outcome:** each exits != 0; stderr identifies the peer index and the unsupported field; the foreign-label case names the canonical `k8s:io.kubernetes.pod.namespace.labels.<key>` form (per R4-5 plural-labels canonicalization).

#### T28 — WITHDRAWN (R9): PDB-enabled at `replicaCount: 1` (no bypass) renders no PDB; the structural fixture with bypass renders a correct PDB

> **WITHDRAWN per R9 deferred decision (implementation plan line 15).** The PDB template and the `podDisruptionBudget.enabled` value were removed in chart 1.5.0; no PDB renders for any input. The `find_pdb is None` regression lock-in is preserved under T16 (Case 13, `replicas-zero`). The historical specification below is preserved for review trace; do NOT implement.

- **Name:** Two cases (20, 21):
  - Case 20: "Operator setting `podDisruptionBudget.enabled: true` at the default `replicaCount: 1` (no test bypass) sees no PDB rendered (drain-safe)."
  - Case 21: "The test-only bypass fixture renders a PDB with `apiVersion: policy/v1`, `kind: PodDisruptionBudget`, `maxUnavailable: 1`, and the chart's standard selector labels (structural coverage for the PDB template)."
- **Type:** scenario + coverage
- **Disposition:** new (Cases 20, 21)
- **Harness:** H1
- **Preconditions:** `pdb-enabled.values.yaml` (no bypass); `pdb-structural.values.yaml` (the sole carrier of `internal._testBypassReplicaCheck: true`).
- **Actions:** render each; check PDB presence and shape.
- **Expected outcome:**
  - Case 20: `find_pdb(rendered) is None`.
  - Case 21: PDB present; `apiVersion == "policy/v1"`; `kind == "PodDisruptionBudget"`; `spec.maxUnavailable == 1`; `spec.selector.matchLabels` includes the chart's standard `app.kubernetes.io/name` and `app.kubernetes.io/instance` labels.
  - Source of truth: R4-1 (validator aborts on replicaCount > 1), R4-8 (bypass key restricted to PDB gate).

#### T29 — Default-on NetworkPolicy renders with deny-all egress except DNS and `TCP/443 → 0.0.0.0/0` with no `except:` list

- **Name:** "Operator installing with defaults sees a NetworkPolicy whose egress block permits DNS to kube-dns/coredns and `TCP/443` to the open internet (the documented vanilla-NP equivalent of 'talk to api.hyperping.io over HTTPS')."
- **Type:** scenario / coverage
- **Disposition:** new (Case 19, `networkpolicy-default`)
- **Harness:** H1 + H2
- **Preconditions:** `networkpolicy-default.values.yaml` (apiKey: x only).
- **Actions:** render; find NetworkPolicy; walk egress rules; confirm DNS rule present and `TCP/443 → 0.0.0.0/0` rule contains NO `except:` list (per Task 7 Step 2 targeted text-matched edit).
- **Expected outcome:** vanilla NP present; CiliumNetworkPolicy absent; egress rules structurally match the documented contract. Source of truth: Task 7 Step 2 replacement block; user-approved decision #1 (vanilla NP TCP/443 default).

### Priority 5 — Schema validation (kubeconform)

Operates over the same fixture set H1 covers, but exits 0 only when every rendered resource passes K8s core schema AND the relevant CRD schema. The render harness asserts shape; kubeconform asserts schema legality independently.

#### T30 — Every fixture's rendered manifests pass `kubeconform -strict` with CRD schemas pinned to `KUBECONFORM_CATALOG_REF`

- **Name:** "Every fixture in `tests/fixtures/` that the render harness accepts produces resources that pass `kubeconform -strict` with K8s core schemas and the pinned CRD catalog (ServiceMonitor, ExternalSecret, CiliumNetworkPolicy)."
- **Type:** invariant
- **Disposition:** new (`make helm-kubeconform`)
- **Harness:** H2
- **Preconditions:** Task 8 Step 5 `helm-kubeconform` target; Task 1 Step 6 catalog tag pinned in `pins.expected.yaml`; pin resolver passes.
- **Actions:** for each fixture, `helm template ... | kubeconform -strict -summary -schema-location default -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/${KUBECONFORM_CATALOG_REF}/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' -`.
- **Expected outcome:** every invocation exits 0; the workflow's `Kubeconform` step is green. Source of truth: K8s core schemas; datreeio CRD catalog at the pinned ref.
- **Interactions:** independent of H1 — if a fixture renders cleanly per H1 but its output violates schema, this test catches it.

### Priority 6 — Live admission (kind PSS-restricted)

Empirically proves the chart's default config is admission-clean under PSS-restricted enforcement AND tolerates `readOnlyRootFilesystem: true` at pod start.

#### T31 — Default `pss-restricted` fixture admits and rolls out under PSS-restricted with `readOnlyRootFilesystem: true`

- **Name:** "An operator deploying the chart into a namespace labelled `pod-security.kubernetes.io/enforce=restricted` sees the Pod admitted (no admission errors), scheduled, and `Available=True` within 120s without writing to the root filesystem."
- **Type:** scenario / integration
- **Disposition:** new (`pss-restricted` dispatch entry in `admission_test.sh`)
- **Harness:** H3
- **Preconditions:** kind cluster from `kind-pss.yaml` (after placeholder substitution); namespace `${TEST_NAMESPACE}` created with the PSS enforce label; `pss-restricted.values.yaml` (apiKey: x + image override to `${LIVE_BOOT_IMAGE}` if needed); `${LIVE_BOOT_IMAGE}` published on Docker Hub (resolver verifies).
- **Actions:** `helm template testrel <chart> -f tests/fixtures/pss-restricted.values.yaml` | `kubectl apply -n "${TEST_NAMESPACE}" -f -` ; `kubectl rollout status deploy/<name> -n "${TEST_NAMESPACE}" --timeout=120s`.
- **Expected outcome:** `kubectl apply` exits 0 (no admission denials referencing `runAsNonRoot`, `readOnlyRootFilesystem`, capabilities, or seccompProfile); rollout status exits 0; pod transitions `Ready`. Source of truth: K8s PSS restricted profile; user-approved decision #1 (PSS admission as blocking CI job).
- **Interactions:** Cleanup between fixtures uses `kubectl wait --for=delete --timeout=60s` (Contract C5.2). Failure mode: image pull failure (resolver catches); admission denial (PSS config drift); pod crash-loop under read-only rootfs (chart writes somewhere it shouldn't — would require an `emptyDir` mount for `/tmp`).

#### T32 — Default `networkpolicy-default` fixture admits and rolls out with the new NetworkPolicy in place

- **Name:** "An operator deploying the chart with the default (Task 7) `networkPolicy.enabled: true` sees the Pod start and stay healthy under the vanilla NetworkPolicy."
- **Type:** scenario / integration
- **Disposition:** new (`networkpolicy-default` dispatch entry)
- **Harness:** H3
- **Preconditions:** same as T31, but the fixture relies on the Task 7 default-on flip; cluster created without a CNI that would block egress beyond NetworkPolicy default-allow.
- **Actions:** apply rendered manifest; `kubectl rollout status`.
- **Expected outcome:** rollout exits 0 (the chart's NetworkPolicy permits DNS + TCP/443 egress, which is sufficient for the readiness probe to succeed because the probe is a Service-targeted HTTP on the pod's own port, not an external call).
- **Interactions:** confirms the NetworkPolicy default-on flip does not break pod start in the most common deployment shape.

### Priority 7 — Audits and gates (no new test cases; runtime guards)

#### T33 — Bisect rebase rehearsal exits 0 across every commit on the branch

- **Name:** "Every commit on `chore/chart-hardening-prod-defaults` satisfies the three-gate bisect contract: `python3 render_test.py && helm lint && make helm-kubeconform` all exit 0."
- **Type:** invariant / regression
- **Disposition:** new (Contract C1 test; Task 10 Step 4)
- **Harness:** H1 + H2 + git rebase --exec
- **Actions:** `git -C <worktree> rebase --exec 'python3 deploy/helm/hyperping-exporter/tests/render_test.py && helm lint deploy/helm/hyperping-exporter/ && (! command -v kubeconform || make helm-kubeconform)' origin/main`.
- **Expected outcome:** every commit exits 0 across all three gates; `git bisect` on the branch lands only on intentionally-failing commits (there should be none). Source of truth: Contract C1 invariant.

#### T34 — Helper-hygiene audit reports no orphan helpers and no orphan callers

- **Name:** "Every `{{- define ... -}}` in `_helpers.tpl` has at least one `include` callsite; every `include "hyperping-exporter.X"` references a defined helper."
- **Type:** invariant
- **Disposition:** new (Task 10 Step 3; Contract C1.2)
- **Actions:** `grep -oE '^{{- define "[^"]+'` vs `grep -RhoE 'include "hyperping-exporter\.[a-zA-Z]+"'`.
- **Expected outcome:** the two sets match.

#### T35 — Coverage audit reports every production template rendered by at least one fixture

- **Name:** "`find deploy/helm/hyperping-exporter/templates/ -name '*.yaml' -not -name 'NOTES.txt' -not -name '_helpers.tpl'` enumerates all production templates; each appears in at least one positive render fixture's output."
- **Type:** invariant
- **Disposition:** new (Task 10 Step 2; Contract C8)
- **Actions:** the audit shell loop in Task 10 Step 2.
- **Expected outcome:** every template covered; no template is dead-code.

#### T36 — Plan-consistency audit confirms no hard-coded anchor literals in test code, no `2>&1 | grep ... && echo PASS` patterns, and exactly 11 task headings

- **Name:** "Test code uses f-string interpolation of `EXPECTED_VERSION` / `EXPECTED_CHART_LABEL`, not literal `1.4.0` / `1.1.0` strings; no grep-only smoke pattern exists; plan has 11 unique task headings."
- **Type:** invariant
- **Disposition:** new (Task 10 Step 1; Contract C10)
- **Actions:** the audit grep commands in Task 10 Step 1.
- **Expected outcome:** zero matches for forbidden patterns; task-heading count equals 11.

#### T37 — Pin resolver exits 0 with every committed pin satisfied

- **Name:** "`tests/scripts/resolve_pins.sh` exits 0; every pin in `pins.expected.yaml` matches upstream; `khaledsalhabdeveleap/hyperping-exporter:1.4.1` exists on Docker Hub."
- **Type:** invariant
- **Disposition:** new (Task 1 Step 6; Contract C4)
- **Harness:** H4
- **Actions:** run the resolver; check `tests/artifacts/*.txt` populated.
- **Expected outcome:** resolver exits 0; if any pin drifts, the resolver fails-loud and the operator handles `USER DECISION REQUIRED:` rather than the chart silently shipping against a drifted upstream.

### Priority 8 — Go test smoke (regression guard)

#### T38 — `go test ./... -race -count=1` reports 133 tests pass

- **Name:** "Every Go test on `main` continues to pass; the Helm chart change does not regress any Go-side test."
- **Type:** regression / smoke
- **Disposition:** existing
- **Harness:** native `go test`
- **Actions:** `go test ./... -race -count=1`.
- **Expected outcome:** `PASS`; test count unchanged at 133. Source of truth: `tests/artifacts/go-baseline-count.txt` captured in Task 1 Step 3.
- **Interactions:** the chart change touches no Go code; this test catches accidental Go-side changes that would otherwise sneak through.

---

## Coverage summary

### Covered action space

- **Operator configuration knobs (every value in `values.yaml`):**
  - `apiKey`, `existingSecret`, `externalSecret.enabled` — T7, T17, T18, T19, T20, T22, T23, T24; Cases 12a (`externalsecret-bad-apiversion`) and 12b (`externalsecret-bad-storekind`) cover the Group B apiVersion / storeKind validators.
  - `cacheTTL`, `metricsPath`, `logLevel`, `logFormat`, `namespace`, `excludeNamePattern`, `mcpUrl` — T2, T3, T4, T5, T6, T8, T9, T10, T11, T12, T13, T14.
  - `webConfigFile` — Case 38 (`web-config-file-fails`); see Validators inventory for `validateWebConfigFile`.
  - `replicaCount` (including 0, 1, 2+) — T15, T16. (T21, T28 WITHDRAWN per R9.)
  - `podDisruptionBudget.enabled` — WITHDRAWN per R9 (value removed from chart 1.5.0; no PDB template).
  - `networkPolicy.enabled` (default flip) — T1 (version label includes NP), T29.
  - `networkPolicy.fqdnRestriction.enabled` (Cilium variant) — T25, T26, T27.
  - `image.repository`, `image.tag` — T1.
  - `internal._testBypassReplicaCheck` (test-only) — WITHDRAWN per R9 (test-only knob removed alongside the PDB template).
- **Validators (every `fail()` in the chart):**
  - `validateCacheTTL` — T3.
  - `validateSecretSources` (every branch of the R4-6 tree) — T16, T17, T18, T19, T20.
  - `validateReplicaCount` — T15. (T21 WITHDRAWN per R9.)
  - `validateNoTestKeys` — Case 39 (`internal-bogus-key-fails`) covers the non-`_test`-prefixed rejection path directly.
  - `validateWebConfigFile` — Case 38 (`web-config-file-fails`) covers the gating abort pending probe/ServiceMonitor TLS wiring.
- **Templates (every file in `templates/`):**
  - `deployment.yaml` — T1, T2, T4..T14, T16.
  - `secret.yaml` — T1, T7, T22.
  - `service.yaml` — T1.
  - `servicemonitor.yaml` — Case 37 (`servicemonitor-enabled`) covers positive render; H2 covers schema validation.
  - `networkpolicy.yaml` — T29.
  - `networkpolicy-cilium.yaml` (new) — T25, T26, T27.
  - `pdb.yaml` — WITHDRAWN per R9 (template deleted in chart 1.5.0).
  - `externalsecret.yaml` (new) — T22, T23, T24.
- **Late-added cases (fix-rounds, not enumerated under T1..T36):**
  - Case 37 (`servicemonitor-enabled`) — positive render of `servicemonitor.yaml`.
  - Case 38 (`web-config-file-fails`) — `validateWebConfigFile` aborts when `config.webConfigFile` is set (R10).
  - Case 39 (`internal-bogus-key-fails`) — `validateNoTestKeys` aborts on any `internal.*` key not prefixed `_test`; Case 39a covers the `_test`-prefixed pass path.
  - Cases 12a/12b — `validateExternalSecretApiVersion` and `validateExternalSecretStoreKind` (Group B follow-up).
- **CI surfaces:**
  - `helm-ci.yml` job `helm` — every step exercised end-to-end by T30 + T31 + T32 + the audits.
  - `Makefile` targets — `helm-ci-fast` covers T30 + audits; `helm-ci` adds T31 + T32.
- **Runtime invariants:**
  - PSS-restricted admission of the default render (T31).
  - `readOnlyRootFilesystem: true` tolerated by the binary at pod start (T31 — the rollout-status wait is the empirical proof).
  - Default NetworkPolicy does not block pod readiness (T32).

### Explicitly excluded per agreed strategy

- **No end-to-end Hyperping API smoke from the kind cluster.** The kind pod has no Hyperping API key and no network connectivity to api.hyperping.io. T31's success criterion is `rollout status` (pod becomes Ready), not "scrape succeeds." Risk: a binary regression that breaks `/readyz` after the cache-TTL change would surface in `internal/collector/` unit tests, not here.
- **No Cilium-CNI admission test.** Strategy approved kubeconform-only validation for the Cilium variant; kind is configured with kindnet, not Cilium. T25, T26, T27 prove structural correctness; field-level Cilium policy enforcement is operator territory at apply-time on a real Cilium cluster.
- **No External Secrets Operator deployment in kind.** Strategy approved kubeconform-only validation for ExternalSecret; T22, T23, T24 use H1 + H2 to prove structural correctness without standing up ESO.
- **No load testing of the binary's poll cadence under the new resources defaults.** Task 2's empirical observation (30-min docker run, peak RSS/CPU sampling) sets the defaults; there is no follow-up benchmark gate in CI. Risk: long-tail behavior beyond 30 minutes may push the binary past the limits; mitigated by ~30% headroom in the `peak × 1.5` derivation.
- **No CHANGELOG-text rendering test.** T36's `grep -c '^## \[1\.5\.0\]'` audit confirms structural presence; the prose content of the entry is human-reviewed at PR time.
- **No manual QA steps.** All checks are reproducible from a single Makefile target or workflow step.

### Risks the exclusions carry

- The kind cluster runs Kubernetes 1.34 (per `kindest_node_minor`). Operators on K8s 1.30-1.33 may see PSS-enforcement differences in edge cases. Plan accepts this risk per Task 1 Step 6 pin policy.
- The kubeconform CRD catalog is pinned to a tagged ref (Contract C5.3); a critical CRD-schema bugfix landing post-pin will not propagate to CI until the pin is rolled. Pin-roll cadence is operator-driven via Task 1 Step 6.
- Cilium peer-shape conversion (Contract C3) is tested via render plus schema, not via Cilium's own policy-import semantics. A subtle Cilium-version-specific rejection would surface only at apply time on a real Cilium cluster.
- The empirical resources observation (Task 2) is a one-shot 30-minute sample on a single Hyperping account. Long-tail or larger-tenant deployments may exceed the defaults; `values.yaml` comments steer operators to override.

---

## Test count summary

- **Existing checks reused (T2, T7-T14):** 9 (Cases 1-9 in `render_test.py`; T1 extends Case 1).
- **New render-harness cases (T1 extension, T3-T6, T15-T29):** 24 (Cases 10-25 and 29-36, plus T1's Case 1 extensions for version/chart label/NP).
- **New CI gates (T30-T32):** 3 (kubeconform, PSS admission live boot ×2).
- **New audits (T33-T37):** 5 (bisect rebase, helper hygiene, coverage, plan-consistency, pin resolver).
- **Smoke (T38):** 1 (Go test regression).

Total: ~38 test invocations covering ~30 distinct test cases in the render harness plus 3 CI gates plus 5 audits plus 1 smoke. The new render-harness work (24 cases) is the bulk of new test artifacts; the CI gates and audits are workflow-level checks the harness depends on.
