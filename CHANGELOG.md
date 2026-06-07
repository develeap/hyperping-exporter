# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added

- `--projects-file` flag (env `HYPERPING_PROJECTS_FILE`) on the exporter
  binary. The flag points at a YAML list of `{id, apiKey|apiKeyFile,
  mcpUrl?, excludeNamePattern?}` entries; the exporter fans out one
  `hyperping.Client`, one `ObservedTransport`, and one
  `*collector.Collector` per project. Each Collector runs its own
  refresh loop (legacy ticker or `tieredRefresher`) so a rate-limit
  pause on one project does not stall the others.
- Helm chart `config.projects` values knob. When the list is non-empty,
  the chart renders `--projects-file=/etc/hyperping/projects.yaml` and
  mounts a projected volume that composes the chart-managed Secret
  (`projects.yaml` + inline-mode `api-key-<id>` entries) with each
  per-project external Secret (existingSecret or the ESO target Secret
  re-keyed to `api-key-<id>`).
- `validateProjects` Helm helper enforcing per-project id alphabet
  (`[a-zA-Z0-9._-]{1,64}`, same as the binary's tenant regex), id
  uniqueness, and exactly-one-secret-source per project. The top-level
  `config.apiKey` / `config.existingSecret` / `externalSecret.remoteRef`
  surface is mutually exclusive with the projects list.
- Multi-key ExternalSecret rendering: in projects mode the chart's
  `externalsecret.yaml` renders one `data` entry per project
  (`secretKey: api-key-<id>`) targeting a single `<fullname>-projects`
  Secret. The chart-managed Secret retains ownership of `projects.yaml`.

### Changed

- BREAKING (downstream dashboards/alerts): every `hyperping_*`,
  `hyperping_client_*`, and `hyperping_mcp_*` metric now carries a
  `project` constLabel. The new project label defaults to
  `project="default"` on single-project deployments so series remain
  joinable, but Grafana panels and Prometheus alerts that hardcode
  label sets without a `project=...` matcher must add one during this
  rollout. Recording rules that aggregate across the exporter should
  add a `by (project, ...)` or `without (project)` clause as
  appropriate.
- Helm chart version `1.5.4 -> 1.6.0`; appVersion `1.5.1 -> 1.7.0`.

### Migration

- Existing single-project deployments need no values changes. The
  legacy `--api-key` / `--api-key-file` / `HYPERPING_API_KEY` path is
  preserved verbatim and synthesises a single project with id
  `default`; the chart's `config.apiKey` / `config.existingSecret`
  / `externalSecret` flow stays as in chart 1.5.x.
- Operators adopting multi-project mode populate `config.projects`
  in the chart values; the top-level secret-source knobs MUST be
  cleared in the same change (validateProjects fails the render
  otherwise).
- Dashboard and alert migration tracks alongside this release in
  hyperping-automation; transitional alert selectors should use
  `project=~"hyp_core|"` so the rule ships before the exporter is
  cut over without breaking the pre-upgrade single-project series.
- CUTOVER REQUIREMENT: when deploying 1.7.0, cut over to
  `config.projects` mode, NOT the legacy single-key path. A 1.7.0
  binary on the legacy path emits `project="default"`, which the
  transitional `project=~"hyp_core|"` selectors do NOT match (they
  match `hyp_core` or the pre-1.7.0 unlabeled series only), silently
  hiding the whole fleet from those alerts. If a deployment must stay
  on the legacy path during transition, widen the selectors to
  include `default` (`project=~"hyp_core|default|"`).
- CUTOVER REQUIREMENT: set `config.cacheMode: tiered` if the
  downstream per-tier `HyperpingDataStale` alerts are expected to
  fire. The chart default is `legacy`, which emits only
  `tier="hot"`; the warm/cold staleness rules have no series to
  evaluate under legacy mode and stay permanently inert.
- Dashboard `$project` variable defaults should be set to an
  explicit project id (e.g. `hyp_core`), NOT `All` or `.*`; the
  latter silently includes drill or staging series in headline
  numbers and is a common silent-regression source.

### Fixed

- Bump `github.com/develeap/hyperping-go` v0.6.2 -> v0.6.3. v0.6.2 had a critical HTTP/2 ALPN regression that broke all HTTPS API calls to non-localhost servers (manifested as `Unsolicited response received on idle HTTP channel` errors with `\x00\x00\x12\x04` byte patterns, HTTP/2 SETTINGS frames being misread by Go's HTTP/1 parser). Affects both REST and MCP paths. Upstream fix: https://github.com/develeap/hyperping-go/pull/37.

### Security

- Bump Go toolchain `1.26.3` -> `1.26.4`. Resolves two newly-disclosed stdlib CVEs: `GO-2026-5039` (arbitrary inputs unescaped in `net/textproto` errors; reached via the MCP transport's MIME header parsing) and `GO-2026-5037` (`CVE-2026-42504`, inefficient candidate hostname parsing in `crypto/x509`; reached via TLS hostname verification). Both fixed in stdlib 1.26.4.

## [1.6.0] - 2026-05-31

### Security

- **Go toolchain bumped to `1.26.3`** (from `1.26.2`); release workflow now hard-gates on `govulncheck v1.3.0` so a vulnerable transitive dependency cannot ship to operators.
- **Label-bomb mitigation**: all metric-label values (monitor names, healthcheck names) are truncated to 256 bytes at emit time via a shared `capLabel` helper. A compromised tenant or operator with rename rights can no longer force the Prometheus side to ingest multi-kilobyte label values multiplied across every per-monitor series. Truncation is UTF-8 safe (never splits mid-rune).
- **Tenant label validation**: `extractTenant` now rejects any character outside `[a-zA-Z0-9._-]` and bounds the resulting tag at 64 bytes. Tenant tags with colons, slashes, unicode, or any non-token character previously flowed verbatim into the `tenant` label; they now collapse to the empty string. See "Changed" for the operator-visible side-effect.
- **`/metrics` startup warning** when binding any-interface (`:port`, `0.0.0.0:port`, `[::]:port`) without `--web.config.file`. The default bind is unchanged; this is a hint, not a hard fail, and the word "unauthenticated" appears in the log line so it is grep-able during incident response.
- **SLA series name label**: the per-period SLA metrics now derive their `name` label from the live monitor record (`mon.Name`) instead of the report's stale `Name` field. A mid-window rename previously fragmented the `hyperping_monitor_sla_ratio` series for the same `uuid` against the base monitor series, silently splitting dashboards keyed on `name`.
- **`--api-key` argv scrub** tightened so only the value directly attached to `--api-key` (either the next positional arg or the `=value` suffix) is overwritten. Unrelated args whose value happens to contain the secret bytes as a substring (listen address, log path) are no longer mangled. Scrub is still best-effort: it touches only the in-process `os.Args` slice, not `/proc/<pid>/cmdline`.

### Added

- **`--api-key-file <path>`** flag: read the Hyperping API key from a file rather than from argv or the environment. Recommended source for container deployments; the file can be owned by a dedicated user and is not visible via `ps` / `/proc/<pid>/cmdline`. Any trailing combination of CR/LF is stripped (Unix LF, Windows CRLF, classic-Mac CR, and multi-newline tails all yield the same key).
- **HTTP server hardening**: `IdleTimeout` (120s) and `MaxHeaderBytes` (1 MiB) are now set explicitly on the metrics `http.Server` to guard against keep-alive idle-connection DoS and header-bomb DoS respectively. `ReadHeaderTimeout` (10s), `ReadTimeout` (30s), and `WriteTimeout` (30s) were already present and are now pinned to exact equality in tests so a silent regression trips CI.

### Changed

- **Tenant label rejects non-token characters** (see Security). Operators whose existing monitor naming convention used tags with colons, slashes, or unicode characters will see those monitors fall out of the `tenant` aggregate (collapsing to `tenant=""`). Migrate naming to `[a-zA-Z0-9._-]` to recover the previous tag.
- **Metric-label values truncated at 256 bytes** (see Security). Dashboards and alert routes that previously matched on very long monitor names will see the truncated form. Operators with sub-256-byte monitor names are unaffected.
- **`/metrics` emits a startup warning** when binding any-interface without `--web.config.file` (see Security). Loopback (`127.0.0.1:9312`, `[::1]:9312`) and any specific IP bind keep the warning silent.
- **`hyperping-go` bumped from `v0.6.0` to `v0.6.2`**. v0.6.1 / v0.6.2 reject userinfo (`user:pass@host`) embedded in URLs passed to `WithBaseURL` and `NewMcpTransport`, and rewrap caller-supplied transports in `WithMCPHTTPClient` so the redaction policy cannot be bypassed by injecting a custom `http.Client`. The exporter never accepted userinfo on `--mcp-url` (the scheme check rejects anything that is not bare `https://` or `http://localhost`), so this is a defence-in-depth bump rather than a user-visible behaviour change for supported configurations.

### Deprecated

- **`--api-key` CLI flag**. The flag still works for one deprecation cycle to avoid breaking existing deployments mid-upgrade, but it now emits a stderr warning at startup. Prefer `HYPERPING_API_KEY` (env var) or `--api-key-file` instead; the CLI form leaks the secret into `/proc/<pid>/cmdline` and any process listing.

### Upgrade notes

- **Prometheus alert / dashboard migration (label-bomb mitigation).** Metric-label values are now capped at 256 bytes. Operators whose monitor names or healthcheck names exceeded 256 bytes will see the truncated form on every series carrying the `name` label (`hyperping_monitor_up`, `hyperping_monitor_response_time_seconds`, `hyperping_monitor_sla_ratio`, `hyperping_healthcheck_*`). Audit alert routes and dashboard variables that match on `name=~"..."` or `name="..."`: any exact-match selector for a name longer than 256 bytes will silently stop firing because the emitted series no longer carries the full value. Either shorten the upstream monitor name to fit, or migrate the selector to a prefix regex against the truncated form. Truncation is UTF-8 safe so the cap never lands mid-rune.
- **Prometheus tenant aggregate migration (tenant validation).** The `tenant` label now rejects any character outside `[a-zA-Z0-9._-]` and is bounded at 64 bytes. Monitors whose tag previously carried colons, slashes, spaces, or unicode characters now emit `tenant=""` rather than the previous verbatim tag. PromQL like `sum by (tenant) (hyperping_monitor_up)` will bucket those monitors under the empty-tenant series, and `hyperping_tenant_health_score{tenant="ops:edge"}` no longer matches. Rename the upstream tag to use only `[a-zA-Z0-9._-]` to recover the previous behaviour.
- **SLA name-label stabilisation.** `hyperping_monitor_sla_ratio` now derives its `name` label from the live monitor record rather than the per-report snapshot. Dashboards that joined the SLA series against the base monitor series on `(uuid, name)` no longer fragment across a mid-window rename. Operators who relied on the prior stale `name` (e.g. to keep an alert pinned to a historical label) should switch the join to `uuid` only.
- **API key sourcing.** `--api-key` still works for this release. New deployments should source the key from `HYPERPING_API_KEY` or `--api-key-file`; the CLI form is logged by the kernel into `/proc/<pid>/cmdline` and surfaces in any `ps` listing.

## [1.5.4] - 2026-05-24 [Chart bump to ship binary 1.5.1 (security patch)]

### Security

- `Chart.yaml` `version` `1.5.3` -> `1.5.4`; `appVersion` `"1.5.0"` -> `"1.5.1"`. The new image (`khaledsalhabdeveleap/hyperping-exporter:1.5.1`) is built against `golang.org/x/crypto v0.52.0`, clearing 10 advisories that affected v0.51.0 (7 critical / 2 high / 1 medium across CVE-2026-46595, CVE-2026-46597, CVE-2026-42508, and the CVE-2026-3983x family).

### Upgrade notes

- **Pure security upgrade.** No behaviour or values.yaml changes vs chart 1.5.3. Operators on 1.5.3 should bump immediately. The Phase 2 (tiered cache opt-in) and Phase 3 (chart 1.6.0 default-flip) roadmap is unchanged.

## [1.5.1] - 2026-05-24 [Binary release, security patch]

### Security

- Bump indirect dependency `golang.org/x/crypto` from `v0.51.0` to `v0.52.0`. Clears the following advisories that affected the released v1.5.0 image:
  - CVE-2026-46595 (critical, CVSS 10.0)
  - CVE-2026-39834, CVE-2026-39832, CVE-2026-39833, CVE-2026-39831, CVE-2026-42508, CVE-2026-39830 (all critical, CVSS 9.1)
  - CVE-2026-46597, CVE-2026-39829 (high, CVSS 7.5)
  - CVE-2026-39827 (medium, CVSS 6.5)
- No code-level changes; only `go.mod` / `go.sum` were modified. All existing tests (172) continue to pass under `go test -race`.

## [1.5.3] - 2026-05-24 [Chart bump to ship binary 1.5.0 with tiered cache values]

### Changed

- `Chart.yaml` `version` `1.5.2` -> `1.5.3`; `appVersion` `"1.4.2"` -> `"1.5.0"`. The new image (`khaledsalhabdeveleap/hyperping-exporter:1.5.0`) carries the tiered cache refactor and the `WithStatus("ongoing")` HOT-tier optimisation.

### Added

- **Chart values for the tiered cache**: `config.cacheMode` ("legacy" / "tiered"; default "legacy"), `config.hotTTL` ("60s"), `config.warmTTL` ("5m"), `config.coldTTL` ("15m"). New chart helpers `validateCacheMode` and `validateTierTTLs` fail() the render with a clear, named-value error on invalid mode or sub-floor TTLs (hot >= 30s, warm >= 60s, cold >= 300s), same shape as the existing `validateCacheTTL`. Deployment template renders the per-tier TTL flags only when `cacheMode != legacy`.

### Upgrade notes

- **Pure default-preserving upgrade.** Operators on chart 1.5.2 can bump to 1.5.3 with no values.yaml changes; the rendered Deployment is identical until `config.cacheMode: tiered` is explicitly set. Phase 2 (per-environment opt-in) and Phase 3 (default flip in chart 1.6.0) are tracked separately. See `docs/tiered-cache-design.md` for the full rollout plan.

## [1.5.0] - 2026-05-24 [Binary release]

### Added

- **Tiered cache mode** behind the new `--cache-mode=tiered` flag. The legacy single-`cacheTTL` ticker is replaced by three independent HOT / WARM / COLD goroutines each owning its own subset of endpoints and atomic snapshot pointer. HOT (60s default) carries monitors, healthchecks, incidents, ongoing outages (via `ListOutages(ctx, hyperping.WithStatus("ongoing"))`), and ongoing maintenance. WARM (5m default) carries the MCP per-monitor metric pool, the 24h SLA report, the full maintenance list, and the global alert count. COLD (15m default) carries the 7d / 30d SLA reports. Per-tier failure isolation: a tier whose refresh aborts before `Store(...)` leaves its previous snapshot pointer untouched; readers stitch three atomic `Load()` calls in `buildCollectorSnapshot`. See `docs/tiered-cache-design.md` for the full design (tier assignments, concurrency model, migration plan). Default remains `--cache-mode=legacy` so existing deployments see no behaviour change.
- **CLI flags**: `--cache-mode`, `--hot-ttl`, `--warm-ttl`, `--cold-ttl`.

### Changed

- **`HyperpingAPI` interface `ListOutages` signature** widened to accept variadic `hyperping.OutageListOption` values so the HOT tier can pass `hyperping.WithStatus("ongoing")` while the legacy `Refresh()` continues to call it with no options. `github.com/develeap/hyperping-go` bumped to `v0.6.0` which ships that option.
- **`hyperping_data_age_seconds` now carries a `tier` label.** Legacy mode emits a single series with `tier="hot"` (matches the prior single-ticker semantics most closely). Tiered mode emits up to three series, one per populated tier. PromQL like `hyperping_data_age_seconds > 300` continues to fire when any tier stalls; PromQL that previously matched the unlabelled series will need an explicit tier selector (e.g. `hyperping_data_age_seconds{tier="hot"}` or `max(hyperping_data_age_seconds)`).

### Note

- **Cold-start gap in tiered mode**: `hyperping_tenant_health_score` requires the 30d SLA window which is owned by the COLD tier. Until COLD's first tick (default 15 min after pod boot), the metric is absent. Documented per design doc Q3.
- **`hyperping_mcp_partial_refresh_total` will gain a `tier` label** in a follow-up release once the chart default flips to tiered (planned for 1.6.0). Until then, the counter is emitted without a `tier` label and existing dashboards / alerts keep working. Consumer-side migration of queries in the downstream `hyperping-automation` Grafana dashboards and recording rules is tracked as BACKLOG item **T2**. This is intentionally NOT done in this release.

### Migration

- **Phase 1 (this release)**: ships behind a flag, default `legacy`. No existing deployment changes behaviour.
- **Phase 2 (separate change)**: operators opt in per-environment via `--cache-mode=tiered` / `config.cacheMode: tiered`.
- **Phase 3 (chart 1.6.0)**: flip default to tiered, add the `tier` label to `partial_refresh_total`, do the consumer-side query migration.
- **Phase 4 (later)**: remove the legacy `Refresh()`. Out of scope.

## [1.5.2] - 2026-05-21 [Chart bump to ship binary 1.4.2]

### Changed

- `Chart.yaml` `version` `1.5.1` -> `1.5.2`; `appVersion` `"1.4.1"` -> `"1.4.2"`. The new image (`khaledsalhabdeveleap/hyperping-exporter:1.4.2`) is what carries the MCP session-id fix; deploying chart 1.5.2 is the path operators take to clear the WARN logs from issue #60.

### Upgrade notes

- **Pull in chart 1.5.2 to fix the MCP rate-limit WARN class.** Chart 1.5.0/1.5.1 ships image `1.4.1`, which was built against `hyperping-go v0.4.0` and sends sessionless `tools/call` requests. Hyperping's server rejects them with `-32000` rate-limit-on-initialize errors and the exporter emits one WARN per refresh. Bump to chart 1.5.2 (image `1.4.2`) and the counters `hyperping_mcp_call_rate_limited_total{*}` flatline at 0 without any values.yaml change.

## [1.4.2] - 2026-05-21 [Binary release]

### Fixed

- **MCP rate-limit failure class (#60).** Bumped `github.com/develeap/hyperping-go` from `v0.4.0` to `v0.5.0`, which captures `Mcp-Session-Id` from the `initialize` response and echoes it on every subsequent JSON-RPC request per the MCP 2025-03-26 Streamable HTTP spec. The previous SDK sent sessionless `tools/call` requests, which Hyperping's server bucketed against the `initialize` rate limit and rejected with WARN-logged `-32000` errors on every refresh against any tenant with more than a handful of monitors. v0.5.0 also adds one-shot session-loss recovery (single re-`initialize` + retry under the existing init mutex) and exports an `ErrSessionLost` sentinel for callers to match via `errors.Is`.
- **Nil dereference in `fetchMcpData`.** Latent bug surfaced by the new regression test: when the high-level `MCPClient.ListRecentAlerts` returned `(nil, nil)` (legitimate "empty content" response shape from upstream), the exporter unconditionally dereferenced `alerts.Total` and would have crashed the scrape. Added a nil guard that retains the previous cached `totalAlerts` value.

### Added

- **MCP observability counters** under the `hyperping_mcp_*` subsystem:
  - `hyperping_mcp_initialize_total` — counts handshakes observed through `ObservedTransport`. main.go now eagerly initializes the MCP session at process startup, so steady state is `1`. Transparent in-SDK session-loss recoveries are NOT counted here (the SDK calls its own `Initialize` receiver method directly, bypassing the `MCPTransport` interface). `0` means the eager init failed and the SDK is doing lazy init on first tool call.
  - `hyperping_mcp_session_lost_total` — counts tool calls that returned `hyperping.ErrSessionLost` after the SDK's one-shot recovery (re-`initialize` + retry) failed. Steady state is `0`. Non-zero indicates two consecutive session expirations on the same call or the server rejecting newly-issued session ids.
  - `hyperping_mcp_call_rate_limited_total{method}` — counts tool calls rejected with a rate-limit error. Steady state is `0`; non-zero indicates either a tool-call budget burst or (regression) a return of the sessionless-request bug from #60.
  - `hyperping_mcp_partial_refresh_total` — counts cache refreshes where one or more per-monitor MCP fetches failed and the cached values were retained as graceful degradation.
- New `collector.ObservedTransport` wraps the raw `hyperping.MCPTransport` and feeds the counters. Wired into `main.go`; `MCPMetrics` is also threaded into `NewCollector` via the new `WithMCPMetrics` option for partial-refresh accounting.
- Eager MCP `Initialize` at process startup (`main.go`). Surfaces MCP connectivity errors at boot rather than mid-first-scrape, and is the only point at which the wrapper's `Initialize` method is invoked through the `MCPTransport` interface (the SDK's lazy-init path bypasses the interface).
- `internal/collector/mcp_session_test.go` — fakes Hyperping's session-id behavior with `httptest.NewServer` and locks the contract end-to-end (exactly one `initialize` per process; every `tools/call` carries the session header).

### Changed

- `cache refreshed` log line gains an `mcp_partial=<bool>` field. The pre-existing `mcp_metrics=<bool>` field reflects only the top-level fetchMcpData outcome (it never returned an error in practice); `mcp_partial` is the load-bearing signal for "we kept stale cached MCP values for one or more monitors this refresh".

## [1.5.1] - 2026-05-13 [Chart only, binary unchanged]

### Fixed

- `values.yaml` `securityContext.runAsUser` / `runAsGroup` and `podSecurityContext.runAsUser` / `runAsGroup` / `fsGroup` flipped from `65534` to `65532`. The chart ships with `gcr.io/distroless/static:nonroot` (uid 65532, `/home/nonroot` WORKDIR owned by 65532). With the previous 65534 default, the kubelet's `chdir` into the image's WORKDIR failed with `permission denied` (Exit Code 126, `OCI runtime create failed ... chdir to cwd ("/home/nonroot") set in config.json failed: permission denied`), crashlooping the pod immediately under any cluster that did not override these in an overlay. The same numeric mismatch is fixed in the raw-k8s example at `deploy/k8s/deployment.yaml`. Operators who previously worked around the issue by setting these to `65532` in their overlay can now drop the override.

### Changed

- `values.yaml` `externalSecret.apiVersion` default `external-secrets.io/v1beta1` -> `external-secrets.io/v1`. ESO promoted the `v1` CRD to GA in 0.10; chart 1.5.0 had to keep `v1beta1` because the kubeconform CRDs-catalog tag at the time did not ship the `v1` schema. 1.5.1 rolls the catalog pin forward (now a main-branch commit SHA, see below) so the new default is fully validated by `kubeconform -strict`. The validator still accepts `external-secrets.io/v1beta1` for operators pinned to ESO < 0.10.
- `tests/pins.expected.yaml` `datreeio_crds_catalog_tag` renamed to `datreeio_crds_catalog_ref` and the value moved from `v0.0.12` to a specific commit SHA on the catalog's main branch. The upstream repo has not cut a tag since 2023-07; the schemas the chart relies on (`external-secrets.io/v1`, `cilium.io/v2` `enableDefaultDeny`) only exist on main. The resolver verifies the pinned SHA still resolves at `api.github.com` so a typo or upstream rebase fails loud rather than silently skipping schema validation.
- `Makefile` `helm-kubeconform` drops `-skip CiliumNetworkPolicy`. The catalog SHA pin now ships the `cilium.io/v2` schema with `spec.enableDefaultDeny`, so CiliumNetworkPolicy fixtures get full kubeconform coverage end-to-end.

### Added

- `values.yaml` `tmpfs` block (default `enabled: false`). When enabled, the Deployment mounts an `emptyDir` at `/tmp` so the binary can write scratch files even with `readOnlyRootFilesystem: true`. The current binary does not need it (verified by the 30-minute Docker observation that grounded the resources defaults), so default-off preserves the prior behavior. Operators running a custom build, a sidecar, or an init container that needs `/tmp` enable it. `tmpfs.medium` (e.g. `"Memory"`) and `tmpfs.sizeLimit` (e.g. `"16Mi"`) pass through to the `emptyDir` spec when set.
- `tests/fixtures/external-secret-v1beta1.values.yaml` + render-test case locking the backwards-compat path.
- `tests/fixtures/tmpfs-enabled.values.yaml` + render-test case locking the `emptyDir` + `volumeMount` shape under `tmpfs.enabled: true`. The defaults case asserts neither block renders under chart defaults.

### Upgrade notes

- **`externalSecret.apiVersion` default flipped to `external-secrets.io/v1`.** Operators on ESO 0.10+ get the modern CRD automatically. Operators on ESO < 0.10 (where the `v1` CRD does not exist on the cluster) MUST set `externalSecret.apiVersion: external-secrets.io/v1beta1` in their overlay before upgrading, or the next `helm upgrade` will produce a manifest the cluster rejects at apply time.
- **`tmpfs.enabled`** stays `false` by default. No-op upgrade for anyone who does not opt in.
- **`runAsUser` / `runAsGroup` / `fsGroup` defaults flipped from 65534 to 65532.** Default deployments unbreak (the pod was crashlooping on `chdir` with the previous defaults). Operators who pinned `65534` in their own overlay to override the chart default should drop the override — keeping `65534` still produces the same crash. Operators using a different (custom-built) image whose nonroot user is `65534` MUST keep the override.

## [1.5.0] - 2026-05-13 [Chart only, binary unchanged]

### Highlights

- **Production-readiness hardening for the Helm chart.** Ten peer-review items landed as one cutover: empirically-grounded `resources` defaults, reconciled probe periods, PSS-restricted documentation, a uniform safe-arg rendering helper, ExternalSecret support with mutual-exclusion guards, `replicaCount: 0` as a fully-supported scaled-to-zero state, a `validateReplicaCount` singleton guard, an opt-in `NetworkPolicy` (operator-configurable DNS and egress) with a `cilium.io/v2 CiliumNetworkPolicy` FQDN-restriction variant, and a `validateCacheTTL` guard that aborts the render on bare-integer cacheTTL.
- **New CI gates.** `kubeconform -strict` schema validation against a pinned `datreeio/CRDs-catalog` tag plus a `kind` PSS-restricted live admission job; every commit on the branch keeps `render_test.py`, `helm lint`, and `make helm-kubeconform` green (bisect-safe).

### Added

- `templates/externalsecret.yaml` rendering `external-secrets.io/v1beta1 ExternalSecret` (default; override via `externalSecret.apiVersion` to `external-secrets.io/v1` on ESO 0.10+). The plan originally targeted `external-secrets.io/v1` as the default, but the pinned `datreeio/CRDs-catalog` tag does not ship the `v1` schema, which would have left `kubeconform -strict` unable to validate the fixture. The default was therefore rolled back to `v1beta1` for chart 1.5.0 with the operator opt-in preserved (see Upgrade notes).
- `templates/networkpolicy-cilium.yaml` rendering `cilium.io/v2 CiliumNetworkPolicy` when `networkPolicy.fqdnRestriction.enabled: true`; mutually exclusive with the vanilla `templates/networkpolicy.yaml`.
- `_helpers.tpl` helpers: `hyperping-exporter.arg` (safe-arg rendering), `secretSourceCount`, `validateSecretSources`, `validateReplicaCount`, `validateCacheTTL`, `validateNoTestKeys`.
- `tests/admission_env.sh`, `tests/admission_test.sh`, `tests/kind-pss.yaml`, `tests/kind-pss-config/admission-config.yaml`, `tests/scripts/resolve_pins.sh`, `tests/pins.expected.yaml`: admission-test harness and pin resolver.
- 28 new render-harness cases covering external-secret positive/defaults/missing-store, replicas-zero / replicas-multi, three secret-conflict variants, missing-source abort, cache-ttl numeric and abort, log-level numeric (no `%!s` artefact), metrics-path special chars, networkpolicy-default, pdb-enabled/structural with multi-replica leakage proof, and seven Cilium variants (egress-only, matchLabels, matchExpressions, mixed, plus three fail paths).
- `Makefile` targets `helm-render`, `helm-kubeconform`, `helm-pss`, `helm-pss-clean`, `helm-ci-fast`, `helm-ci`.

### Changed

- Helm chart `Chart.yaml` `version` `1.1.0` → `1.5.0`.
- Helm chart `appVersion` `"1.4.0"` → `"1.4.1"` to track the published binary.
- `values.yaml` `resources` block re-tuned from a 30-minute Docker observation (peak RSS 11.47 MiB, peak CPU 6.33%): requests `cpu: 50m, memory: 64Mi`; limits `cpu: 200m, memory: 256Mi` (~30% request headroom; ~4x limit headroom).
- `values.yaml` `livenessProbe` `initialDelaySeconds: 5 → 10`, `periodSeconds: 30 → 10`, `failureThreshold` introduced at `3`; `readinessProbe` `initialDelaySeconds: 10 → 5`, `periodSeconds: 15 → 5`, `failureThreshold: 3`.
- `values.yaml` `networkPolicy` block reshaped. `enabled` stays `false` (cluster-coupled decision: the chart cannot know your CNI capability, DNS topology, or whether your platform team reserves NetworkPolicy authority). New `networkPolicy.dns` (namespace, podLabels) and `networkPolicy.egress` (cidr, except, port) knobs let operators retarget the DNS rule and reshape egress without forking the chart. `egress.except` defaults to the RFC1918 ranges, blocking intra-cluster lateral movement on TCP/443; operators routing api.hyperping.io through an in-cluster egress gateway set `egress.except: []`. The DNS rule's default selector (`k8s-app: kube-dns`) is applied via a template fallback rather than a deep-merging values default, so a non-empty operator `dns.podLabels` REPLACES the selector wholesale (avoids the Helm map-merge footgun that would leak `k8s-app: kube-dns` into clusters with different DNS labels).
- `values.yaml` `serviceMonitor` documents the cluster-coupled assumption (Prometheus Operator CRDs must be installed) that motivates the existing `enabled: false` default.
- `values.yaml` `resources` comment no longer references a gitignored measurement-artifact path and explicitly tells operators to measure their own workload before pinning these defaults in production.
- `templates/deployment.yaml` arg lines migrated to the safe-arg helper; the legacy 2-line `apiKey || existingSecret` guard replaced by validator includes; the env block now gates on `secretSourceCount > 0` so `replicaCount: 0` deployments without a secret source render cleanly.
- `templates/secret.yaml` now suppresses when `externalSecret.enabled: true`.

### Removed

- The `podDisruptionBudget` block has been removed from `values.yaml` and the `templates/pdb.yaml` template has been deleted. The exporter is a singleton (`validateReplicaCount` aborts the render at `replicaCount > 1`), which means a PodDisruptionBudget rendering gate is unreachable for any production caller: you cannot run a maxUnavailable budget across a single replica without blocking node drains. The previous knob was therefore dead config and is no longer documented. Operators who had previously set `podDisruptionBudget.enabled: true` should remove the entry from their values overlays; Helm ignores unknown values keys, so the chart will still render cleanly but the value has no effect. If you need to gate voluntary disruptions for the singleton pod, the supported pattern is a cluster-level node-drain orchestration policy (e.g. PriorityClass + scheduling rules), not a PDB.

### Upgrade notes

- **PSS namespace labelling.** The chart does not create or label the target Namespace. Operators MUST label the target namespace `pod-security.kubernetes.io/enforce=restricted` to enforce; the chart's container and pod SecurityContext defaults already satisfy that profile.
- **Helm-to-ESO migration.** Switching an existing release from `apiKey` / `existingSecret` to `externalSecret.enabled: true` requires a brief window where the chart-managed Secret is replaced by an ESO-reconciled Secret of the same name. Stage: (1) deploy ESO and the `(Cluster)SecretStore`; (2) bump the chart with `externalSecret.enabled: true` AND clear `apiKey` / `existingSecret` in the SAME release; (3) verify the ExternalSecret reaches `SecretSynced` before scaling the Deployment.
- **`replicaCount: 2` (or higher) now aborts the render** with a `validateReplicaCount` error. The exporter is a singleton; scale horizontally by sharding monitor namespaces across separate releases.
- **`cacheTTL` MUST be a quoted Go duration string** (e.g. `"60s"`). Bare integers abort the render via `validateCacheTTL`.
- **NetworkPolicy is opt-in.** The default remains `networkPolicy.enabled: false` (the chart cannot know your cluster's CNI, DNS topology, or NP-authority story). Operators who want default-deny egress with a Hyperping-API allow rule set `enabled: true` and review `networkPolicy.dns` and `networkPolicy.egress` against their cluster: the DNS rule's default selector matches vanilla kube-dns / CoreDNS (`k8s-app: kube-dns` in `kube-system`); clusters running NodeLocal DNSCache or non-default CoreDNS labels override `networkPolicy.dns.podLabels` (a non-empty value replaces the default selector wholesale to avoid Helm map-merge leakage). The egress rule defaults to TCP/443 to `0.0.0.0/0` with RFC1918 excluded; clear `networkPolicy.egress.except` to permit an in-cluster egress gateway.
- **`config.webConfigFile` is currently unsupported and aborts the render** via the new `validateWebConfigFile` helper. The binary's `--web.config.file` flag puts it into TLS mode, but the chart's probes (`livenessProbe.httpGet.scheme`, `readinessProbe.httpGet.scheme`) and the ServiceMonitor template's `endpoints[].scheme` hardcode HTTP. Enabling TLS in isolation would leave the pod permanently NotReady and silently break Prometheus scrapes; failing the render loud is the safer default. Operators who need TLS today should terminate at a sidecar or at the Service edge. A future chart release will wire `httpGet.scheme` + ServiceMonitor `scheme` + `tlsConfig` together so the knob becomes load-bearing.
- **ExternalSecret apiVersion default is `external-secrets.io/v1beta1`.** Operators running ESO 0.10+ (where the `v1` CRD is GA and `v1beta1` is deprecated) MUST set `externalSecret.apiVersion: external-secrets.io/v1` explicitly. The chart's `kubeconform` job pins a CRDs-catalog tag that does not yet ship the `v1` schema; the chart will roll the default to `v1` in a subsequent chart bump once the catalog tag does.

## [1.4.1] - 2026-05-12

### Added

- **`config.excludeNamePattern`** (RE2 regex) and **`config.mcpUrl`** Helm chart values, rendered into the Deployment container args via `toJson` so backslash-bearing regexes round-trip byte-for-byte through Kubernetes' YAML decoder.
- **Render-test harness** at `deploy/helm/hyperping-exporter/tests/render_test.py` driven by PyYAML, with fixture files exercising defaults, existingSecret-only, plain-ASCII regex, the README example, a single-quote-containing regex, `mcpUrl` alone, and both flags together.
- **Helm CI workflow** (`.github/workflows/helm-ci.yml`) that runs `helm lint` and the render harness on chart-touching PRs.

### Changed

- Helm chart `Chart.yaml` `version` `1.0.0` → `1.1.0`.
- Helm chart `appVersion` `"1.0.3"` → `"1.4.0"` to match the released binary that understands `--exclude-name-pattern` and `--mcp-url`. Side-effect: the `app.kubernetes.io/version` label on Secret, Service, and Deployment flips from `"1.0.3"` to `"1.4.0"`.
- Helm chart `image.repository` default `develeap/hyperping-exporter` → `khaledsalhabdeveleap/hyperping-exporter` to match the published Docker Hub repo.
- `.github/workflows/ci.yml` `paths-ignore` lists extended with `.github/workflows/helm-ci.yml` so future chart-only changes skip the seven Go jobs.
- **`goreleaser/goreleaser-action`** `7.1.0` → `7.2.1` (dependabot).
- **`aquasecurity/trivy-action`** `0.35.0` → `0.36.0` (dependabot).

### Security

- **`golang.org/x/net`** `v0.51.0` → `v0.54.0` (GO-2026-4918: infinite loop in HTTP/2 transport on bad `SETTINGS_MAX_FRAME_SIZE`, reachable via `hyperping.Client.ListIncidents`). Companion bumps: `golang.org/x/crypto` `v0.49.0` → `v0.51.0`, `golang.org/x/sys` `v0.42.0` → `v0.44.0`, `golang.org/x/text` `v0.35.0` → `v0.37.0`.

## [1.4.0] - 2026-04-26

### Added

- **Maintenance-window suppression** for `HyperpingMonitorDown`, `HyperpingMonitorActiveOutage`, and `HyperpingCoreMonitorDown`. Each alert now has `unless on(uuid) hyperping_monitor_in_maintenance == 1` appended, so monitors covered by an active maintenance window do not page during the planned downtime.
- **`HyperpingOpenIncidents`** alert: fires when there are unresolved Hyperping incidents for more than 30 minutes.
- **`HyperpingMonitorRegionalOutage`** alert: fires when a monitor is down in some regions but up in others (partial regional infrastructure failure rather than complete outage). Requires monitors configured with multiple regions.
- **`HyperpingMonitorAnomalyHigh`** alert: fires when MCP-derived anomaly score is sustained above 0.8 for 15 minutes. Requires `--mcp-url`.
- **`HyperpingMonitorMTTAHigh`** alert: fires when MCP-derived MTTA exceeds 10 minutes for over an hour. Requires `--mcp-url`.
- **`hyperping:fleet:anomalies_high`** recording rule: count of monitors currently above the 0.8 anomaly threshold; useful for fleet-wide trend panels.
- **promtool unit tests** for bundled alerts (`deploy/prometheus/tests/alerts.test.yml`). Verifies maintenance suppression, regional outage detection, anomaly threshold behaviour, and recording-rule output. New CI job runs `promtool test rules` on every PR.
- **Sanity assertion in `TestPrometheusRulesReferenceOnlyEmittedMetrics`**: asserts at least 30 metric names were extracted, catching the case where the underlying `Desc.String()` regex silently breaks in a future client_golang upgrade.

### Changed

- **BREAKING (small impact): `hyperping_monitors_excluded` renamed to `hyperping_excluded_monitors`.** The metric was introduced in v1.3.0 (one day before v1.3.1) and the new name reads more naturally ("excluded monitors" parses cleanly as adjective + noun). Anyone who already started using the v1.3.0 name needs to update dashboards/alerts. No backwards-compatibility shim is shipped.
- **`avgSLAForPeriod` now returns `(float64, bool)`.** When no report's monitor is in the index (e.g., `--exclude-name-pattern` removes every monitor that has reports), the function returns `(0, false)` and the caller skips emitting `hyperping_tenant_health_score` rather than emitting a misleadingly low value. Internal API change; not user-facing.
- **`HyperpingMonitorAnomalyHigh` annotation** uses `printf "%.2f"` instead of `humanize` for the score. Prevents the score 0.95 from rendering as "950m" (SI prefix).
- CI's `prom/prometheus` Docker image is now pinned by digest (`@sha256:378f4e...`) for supply-chain reproducibility, matching the rest of the workflow's habit of pinning actions by SHA.
- CI's `promtool` job no longer waits on `verify`; it can run in parallel with the Go module check.
- `deploy/k8s/deployment.yaml` example image tag changed from `:<VERSION>` to `:REPLACE_ME` for unambiguous "fill this in" semantics.
- Makefile `test` target now enforces the same 90% coverage threshold as CI, so locally-run tests fail in the same conditions as CI.

## [1.3.1] - 2026-04-26

### Fixed

- **Tenant SLA aggregates ignored `--exclude-name-pattern`.** `hyperping_tenant_avg_sla_ratio` summed only visible monitors' SLA but divided by `len(reports)` (which still counted excluded monitors), and `hyperping_tenant_health_score`'s `avgSLAForPeriod` summed reports for every monitor unconditionally. Both metrics were systematically pulled down by the visible/total ratio. `hyperping_tenant_active_outages` and `hyperping_tenant_monitors_up_ratio` were unaffected.
- **Bundled alerts that never fired.** `HyperpingMultipleActiveOutages` and `HyperpingNoMonitors` referenced `hyperping_monitors_total`, but the exporter emits `hyperping_monitors` (no `_total` suffix). Dividing by an absent series produced no result, so the alerts were silent in v1.3.0. Fixed by using the correct metric name. Users who deployed v1.3.0 alerts must redeploy `alerts.yml`.
- Removed unreachable `if coreErr != nil` branch after the lock in `Refresh()`; the early return at the top of the function already handles this case.
- `fetchMcpData` MTTA branch logged "failed to fetch" debug lines even when the API legitimately returned `(nil, nil)` with no error. Mirrored the Response Time branch's `else if err != nil` to silence noise on empty results.

### Added

- `filterReportsByMonitorUUID` (private) applied in `Refresh()` alongside the existing outage filter, using the same `includedUUIDs` set. Root-cause fix for the SLA aggregate bug above.
- Defensive `slaCount` counter in `emitReportMetrics` so the tenant SLA average can never again be divided by a count larger than what was actually summed.
- `TestPrometheusRulesReferenceOnlyEmittedMetrics`: walks `deploy/prometheus/alerts.yml` and `recording-rules.yml`, extracts every `hyperping_*` / `hyperping:*` identifier from each `expr:` field, and asserts each one is either an emitted Prometheus descriptor or a recording-rule output. Catches the typo class of bug that `promtool check rules` cannot.
- New `promtool` CI job that validates rule syntax on every PR.

### Changed

- `avgSLAForPeriod` now requires the `monitorIndex` and skips reports whose UUID is absent. The function is correct independently of upstream filtering, so a future caller cannot reintroduce the bug by passing unfiltered reports.
- `fetchMcpData` short-circuits with an explicit early return when `len(monitors) == 0`, instead of relying on `numWorkers=0` to coincidentally produce the same outcome.
- `hyperping_monitors_excluded` help text now states the relationship with `hyperping_monitors` explicitly: "...hyperping_monitors counts the visible remainder."
- `validateNamespace` now documents why the 64-character cap exists (downstream Kubernetes label and storage backend limits, not a Prometheus protocol requirement).
- Per-tier `hyperping:tier:monitors_up_ratio` recording rule now documents that the `* 0` zero-guard cannot synthesise an empty tier — if every monitor for a tier is removed, no series is emitted (which is correct).
- CI no longer ignores `deploy/prometheus/**` paths. Changes to alert and recording rules now trigger the full CI matrix, including the new metric-reference and promtool checks.
- `deploy/k8s/deployment.yaml` example image tag changed from `:latest` to a `:<VERSION>` placeholder so an unmodified `kubectl apply -f` fails with a clear `ImagePullBackOff` instead of silently following whatever tag is current.

## [1.3.0] - 2026-04-25

### Added

- **`--exclude-name-pattern` flag**: RE2 regex that filters monitors by name before any metric computation. Excluded monitors are dropped from all `hyperping_monitor_*` metrics and from tenant aggregates (`up_ratio`, `active_outages`, `health_score`). Outages belonging to excluded monitors are filtered out too, so synthetic drill monitors no longer inflate fleet health dashboards. Typical use: `--exclude-name-pattern='\[DRILL|\[TEST'`.
- **`hyperping_cache_ttl_seconds`** constant gauge exposing the configured `--cache-ttl` value. Enables the bundled `HyperpingDataStale` alert to self-configure its threshold (`2 × cache_ttl`) without manual updates when the flag changes.
- **`hyperping_monitors_excluded`** gauge: count of monitors filtered out by `--exclude-name-pattern` on the last refresh; always emitted (value 0 when no pattern is configured) so exclusions are observable in Prometheus without log scraping.
- New `HyperpingNoMonitors` alert: fires when scrape succeeds but the API returns zero monitors (deleted monitors, API key mismatch, or an over-aggressive `--exclude-name-pattern`).
- Alertmanager inhibition rule examples added as comments in `alerts.yml`, including a multi-cluster `equal: [cluster]` note.

### Changed

- **`HyperpingMultipleActiveOutages`**: replaced absolute `> 3` threshold with a fleet-size-aware ratio (`> 10% of monitors`) plus a division-by-zero guard. (Note: this alert and `HyperpingNoMonitors` referenced an incorrect metric name in v1.3.0 and were silent in production. Fixed in v1.3.1.)
- **`HyperpingDataStale`**: threshold is now `2 * hyperping_cache_ttl_seconds` instead of a hardcoded value, adapting automatically when `--cache-ttl` is changed.
- **Recording rules** (`deploy/prometheus/recording-rules.yml`): fleet counts (`monitors_up`, `monitors_down`, `monitors_paused`) and SLA breach counts now use `or vector(0)` so they return 0 instead of disappearing when all monitors match the filter condition. Per-tier `monitors_up_ratio` numerator uses a label-preserving `* 0` pattern so the ratio evaluates to 0 (not absent) when all monitors in a tier are down.
- **Grafana dashboards** (`fleet-overview`, `shared-infrastructure`, `tenant-health`): can now be imported directly via the Grafana UI ("Dashboards → Import → Upload JSON file"). Added `__inputs` block and replaced all 43 bare datasource references with `${DS_PROMETHEUS}`.
- Provisioning datasource pinned to a stable `uid: hyperping-prometheus` so dashboards can reference it by ID across redeploys.

### Fixed

- **Nil pointer panic in `fetchMcpData` (intermittent).** `GetMonitorResponseTime` and `GetMonitorMtta` could return `(nil, nil)`; accessing fields on the nil pointer crashed the scrape. The v1.2.1 release claimed this fix but the guard was never actually added. Now correctly guarded with `report != nil` alongside the existing `err == nil` check.

## [1.2.1] - 2026-04-25

### Fixed

- Nil pointer panic in MCP worker pool: `GetMonitorResponseTime` and `GetMonitorMtta` return `(nil, nil)` when the MCP server returns an empty result. Accessing `report.Avg` / `report.AvgWait` on a nil pointer caused a crash during scrape. Added `report != nil` guard alongside the existing `err == nil` check.
- Removed stale `hyperping-go v0.3.0` hashes from `go.sum` (left behind after upgrading to v0.4.0).

## [1.2.0] - 2026-04-25

### Added

- **MCP-sourced metrics** via the Hyperping MCP server (requires `--mcp-url`):
  - `hyperping_monitor_response_time_seconds{uuid,name}` - average response time per monitor.
  - `hyperping_monitor_mtta_seconds{uuid,name}` - mean time to acknowledge per monitor.
  - `hyperping_monitor_anomaly_count{uuid,name}` - number of detected anomalies.
  - `hyperping_monitor_anomaly_score{uuid,name}` - highest anomaly score.
  - `hyperping_alerts{uuid,name}` - recent alert snapshot count (gauge).
- MCP metrics are fetched in parallel with REST metrics. If MCP is unavailable or unconfigured, all existing REST metrics continue to work (graceful degradation).
- Per-operation 8-second timeout on MCP worker pool calls to prevent scrape blocking.

### Changed

- `--mcp-url` flag now validates the URL at startup: must start with `https://` (or `http://localhost` for local dev). Invalid URLs cause a non-zero exit instead of silently misbehaving.
- Upgraded `github.com/develeap/hyperping-go` from v0.3.0 to v0.4.0.

### Fixed

- MCP graceful degradation: REST metrics are now covered by a dedicated test (`TestRefresh_McpErrorIsNonFatal`) confirming they remain available when all MCP calls return errors.

### Breaking: metric renames

Dashboards referencing the old names must be updated:

| Old name | New name | Reason |
|----------|----------|--------|
| `hyperping_alerts_total` | `hyperping_alerts` | Was typed as Counter but is a snapshot gauge; `_total` is reserved for counters |
| `hyperping_monitor_response_time_avg_seconds` | `hyperping_monitor_response_time_seconds` | `_avg` is not a Prometheus unit suffix |

## [1.1.0] - 2026-04-09

### Added

- **`tenant` and `tier` labels** on all per-monitor metrics (`hyperping_monitor_up`,
  `hyperping_monitor_sla`, etc.). `tenant` is the tenant ID; `tier` is `core`,
  `noncore`, or `unknown` derived from escalation policy name (EXP-01).
- **`hyperping_monitor_in_maintenance`** gauge - 1 when monitor has an active
  maintenance window, 0 otherwise. Prevents false-positive alerts during planned
  downtime (EXP-02).
- **`hyperping_monitor_up_by_region{uuid,name,region}`** gauge - per-region up/down
  status derived from active outage `detectedLocation`/`confirmedLocations`. Additive
  alongside the existing `hyperping_monitor_up` family (EXP-03).
- **`hyperping_incident_active`** and **`hyperping_maintenance_active`** event gauges
  with `tenant`, `tier`, `severity` labels (EXP-04).
- **`WatchdogStalled`** alert rule - fires when the watchdog has not updated its
  state file within the expected interval (AUTO-03).
- **`WatchdogNeverRan`** alert rule - fires when no watchdog state file exists
  at all (AUTO-03).
- Dependabot group for `hyperping-go` module auto-updates.

### Changed

- Migrated from vendored `internal/client/` to the shared
  `github.com/develeap/hyperping-go` module. No API or metric changes - internal
  only.
- Alert and recording rules updated to include `tenant` and `tier` label selectors
  matching the new EXP-01 schema. **Existing dashboards must add `tenant` and `tier`
  to their variable filters** - see `deploy/grafana/` for updated dashboard JSONs (OPS-40).

### Fixed

- Release workflow: GoReleaser archive name corrected.
- Collector: minor field mapping corrections from phase 0 audit.

## [1.0.3] - 2026-04-05

### Added
- Helm chart committed to VCS (previously excluded by `.gitignore` binary pattern)
- Kubernetes `NetworkPolicy` for raw manifests and Helm chart (DNS scoped to kube-system, HTTPS egress blocks RFC1918)
- Cosign keyless image signing in release pipeline (Sigstore/Fulcio)
- Trivy container image scanning in CI (fails on CRITICAL/HIGH CVEs)
- GHCR mirror (`ghcr.io/develeap/hyperping-exporter`) alongside Docker Hub
- License compliance check in CI (rejects copyleft: GPL-3, AGPL, SSPL, EUPL)
- VCR cassette sanitization hooks: PII, infrastructure headers, and sensitive request header values are automatically redacted on recording
- Outage pagination truncation warning when page cap is reached
- `StatusPageService.ID` null/absent tests for group-header edge case
- `automountServiceAccountToken: false` in Helm deployment template

### Changed
- Outage pagination limit reduced from 100 to 10 pages to prevent linear degradation on long-lived accounts
- `StatusPageService.ID` type changed from `interface{}` to `*FlexibleString` for type-safe string/number handling
- All GitHub Actions pinned to commit SHAs (supply chain hardening)
- Helm `securityContext` defaults now include `seccompProfile: RuntimeDefault` and `runAsGroup: 65534`
- Helm PDB changed from `minAvailable` to `maxUnavailable` (safe with single replica)
- Docker Compose hardened: `read_only`, `cap_drop: ALL`, `no-new-privileges`, resource limits on all services
- K8s deployment pod-level `securityContext` expanded to full Restricted PSS compliance
- Grafana admin username configurable via `GRAFANA_ADMIN_USER` env var
- Exporter image version in Compose configurable via `EXPORTER_VERSION` env var
- SBOM output format explicitly pinned to `spdx-json`
- `govulncheck` pinned to `@v1.1.4` in Makefile (matches CI)
- `HyperpingDataStale` alert threshold documented with formula: `threshold = 2 x cache-ttl`
- Helm `apiKey` value marked as development-only; `existingSecret` recommended for production
- Dependabot expanded to cover Helm chart directory

### Fixed
- `.gitignore` binary pattern `hyperping-exporter` scoped to root (`/hyperping-exporter`) to stop excluding `deploy/helm/hyperping-exporter/`
- VCR `api_key=` URL sanitization now replaces the full key=value pair (was only replacing the prefix)
- Scrubbed PII (emails, profile URLs), infrastructure headers (Cf-Ray, Nel, Ratelimit-Policy), and token values from 27 VCR cassettes
- Removed mutable `latest` Docker tag from GoReleaser release pipeline
- Cosign signs images by digest (not tag) to prevent TOCTOU race

### Removed
- `latest` Docker tag from release pipeline (use versioned tags instead)

## [1.0.2] - 2026-03-31

### Added
- Docker Hub description auto-sync on release via `peter-evans/dockerhub-description`

### Changed
- Migrated from `goreleaser dockers` to `dockers_v2` for native multi-arch Docker builds (linux/amd64, linux/arm64)
- SBOM and provenance attestations attached to Docker images

### Fixed
- `--push` flag added to buildx to support SBOM and provenance attestations

## [1.0.1] - 2026-03-31

### Changed
- Container registry switched from GHCR to Docker Hub (`khaledsalhabdeveleap/hyperping-exporter`)

### Fixed
- Reduced cyclomatic complexity in status page test

## [1.0.0] - 2026-03-31

### Added
- `deploy/.env.example` — credential template for Docker Compose (`HYPERPING_API_KEY`, `GRAFANA_ADMIN_PASSWORD`); `deploy/.env` is gitignored
- Binary releases now include Windows (386, amd64), Linux 386, and Linux arm (v6, v7) targets
- Cross-compile CI job validates all 5 new target platforms on every PR
- `--namespace` flag (env: `HYPERPING_EXPORTER_NAMESPACE`, default: `hyperping`) to customise the Prometheus metric prefix; explicit flag always beats env var
- Client observability metrics: `hyperping_client_api_call_duration_seconds`, `hyperping_client_retry_total`, `hyperping_client_circuit_breaker_state` expose API call latency, retry counts, and circuit breaker state
- `WithCircuitBreakerSettings(gobreaker.Settings)` and `WithNoCircuitBreaker()` client options
- `deploy/k8s/secret.yaml.example` — Secret manifest template for Kubernetes deployments
- PodDisruptionBudget template in Helm chart (`podDisruptionBudget.enabled`)
- SBOM generation (`${artifact}.sbom.json`) for every release archive
- `govulncheck` dedicated CI job (pinned to v1.1.4)
- HTTP server `ReadTimeout` and `WriteTimeout` (30s each)

### Changed
- Relicensed from MPL-2.0 to MIT
- README quick-start restructured: API key note, verification step, and 30-second framing added
- Exporter-only Docker Compose snippet added for users with existing Prometheus
- Configuration table clarifies which options are flag-only vs env-var
- `IsReady()` readiness probe now latches on first successful scrape — pod stays ready through transient API outages
- `make build` now uses `CGO_ENABLED=0` and `-trimpath` to produce a static binary compatible with distroless containers
- `make compose-up` now pre-builds the Go binary before running `docker compose up --build`
- Docker Compose port bindings changed to `127.0.0.1:PORT:PORT` for local dev safety
- `GRAFANA_ADMIN_PASSWORD` is now required in Docker Compose (no fallback default)
- Removed redundant `--cache-ttl=60s` from docker-compose.yml (equals compiled default)
- Kubernetes `deployment.yaml` no longer embeds a Secret object; use `secret.yaml.example`
- Kubernetes Deployment hardened: `automountServiceAccountToken: false`, `seccompProfile: RuntimeDefault`
- Helm chart validates that either `apiKey` or `existingSecret` is set
- `Collect()` snapshots cache under a brief read lock then releases before metric emission
- URL labels strip query parameters before use
- `golangci-lint` pinned to v2.11.4 in CI
- `goreleaser` pinned to `~> v2` in release workflow

### Fixed
- `make build` produced a dynamically-linked binary that crashed in `distroless/static`; fixed with `CGO_ENABLED=0`
- `--namespace` env var override incorrectly beat an explicit flag value; env var now only applies when flag is unset
- `validateBaseURL()` enforces https (or localhost) scheme in `WithBaseURL()`
- `HyperpingCoreMonitorDown` alert expression corrected to match actual metric labels
- Prometheus `--web.enable-lifecycle` removed from Docker Compose args

## [0.1.0] - 2026-03-30

### Added

- Initial release — standalone Prometheus exporter for Hyperping monitoring service
- Full metric coverage: monitors, healthchecks, outages, SLA ratios, health scores, and escalation tiers
- Background cache refresh with configurable TTL (default 60s)
- Circuit breaker and retry logic in the Hyperping API client
- 3 Grafana dashboards: Fleet Overview, Shared Infrastructure, Tenant Health
- Prometheus alert rules and recording rules
- Docker Compose stack (exporter + Prometheus + Grafana)
- Kubernetes manifests: Deployment, Service, ServiceMonitor
- Multi-arch Dockerfile (linux/amd64, linux/arm64) based on distroless/static
- GoReleaser configuration for multi-arch releases
- GitHub Actions CI and release pipelines
