{{/*
Expand the name of the chart.
*/}}
{{- define "hyperping-exporter.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "hyperping-exporter.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "hyperping-exporter.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "hyperping-exporter.labels" -}}
helm.sh/chart: {{ include "hyperping-exporter.chart" . }}
{{ include "hyperping-exporter.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "hyperping-exporter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "hyperping-exporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Determine the secret name for the API key.
Uses existingSecret if set, otherwise falls back to the chart's fullname.
*/}}
{{- define "hyperping-exporter.secretName" -}}
{{- if .Values.config.existingSecret }}
{{- .Values.config.existingSecret }}
{{- else }}
{{- include "hyperping-exporter.fullname" . }}
{{- end }}
{{- end }}

{{/*
Safe-arg helper (Contract C2.1). Renders a single container arg line as a
JSON-escaped string so any bytes the value carries (quotes, backslashes,
special whitespace) round-trip through Helm/Go YAML's text serializer
unchanged. `%v` + `toString` is belt-and-braces: either alone already
produces a clean string for ints/floats/bools, but together they tolerate
typed inputs that would otherwise leak `%!s(int=N)` artefacts.

Usage:
  {{ include "hyperping-exporter.arg" (list "--flag" .Values.path.to.scalar) }}
*/}}
{{- define "hyperping-exporter.arg" -}}
{{- $flag := index . 0 -}}
{{- $val := index . 1 -}}
{{- printf "%s=%v" $flag (toString $val) | toJson -}}
{{- end -}}

{{/*
validateCacheMode. Accepts "legacy" or "tiered" (case-insensitive). Any
other value abort()s the render. Default is "legacy" so omitted values
are accepted unchanged; an unset cacheMode is taken as "legacy".
*/}}
{{- define "hyperping-exporter.validateCacheMode" -}}
{{- $mode := .Values.config.cacheMode | default "legacy" -}}
{{- if not (kindIs "string" $mode) -}}
{{- fail (printf "config.cacheMode must be a string (\"legacy\" or \"tiered\"); got kind %s." (kindOf .Values.config.cacheMode)) -}}
{{- end -}}
{{- $lc := lower $mode -}}
{{- if and (ne $lc "legacy") (ne $lc "tiered") -}}
{{- fail (printf "config.cacheMode %q is not supported. Allowed values: \"legacy\", \"tiered\" (case-insensitive). See docs/tiered-cache-design.md for the tiered mode semantics." $mode) -}}
{{- end -}}
{{- end -}}

{{/*
validateTierTTLs. Enforces per-tier floor durations when cacheMode is
"tiered": hotTTL >= 30s, warmTTL >= 60s, coldTTL >= 300s. Same error
shape as validateCacheTTL: fail() with a clear message naming the
specific value the operator must change. Floors are conservative,
chosen so the SDK rate-limit budget (300 req/min/key) is comfortably
respected even with 100+ monitors.

A bare integer for any of the three TTLs is rejected with the same
shape as validateCacheTTL (Go duration parsing requires a unit).
*/}}
{{- define "hyperping-exporter.validateTierTTLs" -}}
{{- $mode := .Values.config.cacheMode | default "legacy" -}}
{{- if eq (lower $mode) "tiered" -}}
{{- $hot := .Values.config.hotTTL -}}
{{- $warm := .Values.config.warmTTL -}}
{{- $cold := .Values.config.coldTTL -}}
{{- if or (not (kindIs "string" $hot)) (eq $hot "") -}}
{{- fail (printf "config.hotTTL must be a non-empty quoted Go duration string (e.g. \"60s\"); empty or non-string values would render `--hot-ttl=` which the binary's flag.Duration parser rejects at startup. Got kind %s (value %v). Quote the value in values.yaml." (kindOf .Values.config.hotTTL) .Values.config.hotTTL) -}}
{{- end -}}
{{- if or (not (kindIs "string" $warm)) (eq $warm "") -}}
{{- fail (printf "config.warmTTL must be a non-empty quoted Go duration string (e.g. \"5m\"); got kind %s (value %v)." (kindOf .Values.config.warmTTL) .Values.config.warmTTL) -}}
{{- end -}}
{{- if or (not (kindIs "string" $cold)) (eq $cold "") -}}
{{- fail (printf "config.coldTTL must be a non-empty quoted Go duration string (e.g. \"15m\"); got kind %s (value %v)." (kindOf .Values.config.coldTTL) .Values.config.coldTTL) -}}
{{- end -}}
{{- $hotS := int (regexReplaceAll "[^0-9]" $hot "") -}}
{{- $hotIsSeconds := hasSuffix "s" $hot -}}
{{- $hotIsMinutes := hasSuffix "m" $hot -}}
{{- $hotIsHours   := hasSuffix "h" $hot -}}
{{- $hotSeconds := 0 -}}
{{- if $hotIsHours }}{{- $hotSeconds = mul $hotS 3600 -}}
{{- else if $hotIsMinutes }}{{- $hotSeconds = mul $hotS 60 -}}
{{- else if $hotIsSeconds }}{{- $hotSeconds = $hotS -}}
{{- else }}{{- fail (printf "config.hotTTL %q must end in s/m/h (e.g. \"60s\"); the binary's flag.Duration parser rejects unit-less values." $hot) -}}{{- end -}}
{{- if lt $hotSeconds 30 -}}
{{- fail (printf "config.hotTTL %q is below the 30s floor. The HOT tier carries every per-monitor up/down/outage gauge in tiered mode; refreshing more often than 30s wastes the SDK's rate-limit budget without measurable observability gain. Same enforcement shape as validateCacheTTL." $hot) -}}
{{- end -}}

{{- $warmS := int (regexReplaceAll "[^0-9]" $warm "") -}}
{{- $warmSeconds := 0 -}}
{{- if hasSuffix "h" $warm }}{{- $warmSeconds = mul $warmS 3600 -}}
{{- else if hasSuffix "m" $warm }}{{- $warmSeconds = mul $warmS 60 -}}
{{- else if hasSuffix "s" $warm }}{{- $warmSeconds = $warmS -}}
{{- else }}{{- fail (printf "config.warmTTL %q must end in s/m/h (e.g. \"5m\")." $warm) -}}{{- end -}}
{{- if lt $warmSeconds 60 -}}
{{- fail (printf "config.warmTTL %q is below the 60s floor. The WARM tier carries the MCP per-monitor metric pool; refreshing more often than 60s can saturate the SDK rate-limit budget." $warm) -}}
{{- end -}}

{{- $coldS := int (regexReplaceAll "[^0-9]" $cold "") -}}
{{- $coldSeconds := 0 -}}
{{- if hasSuffix "h" $cold }}{{- $coldSeconds = mul $coldS 3600 -}}
{{- else if hasSuffix "m" $cold }}{{- $coldSeconds = mul $coldS 60 -}}
{{- else if hasSuffix "s" $cold }}{{- $coldSeconds = $coldS -}}
{{- else }}{{- fail (printf "config.coldTTL %q must end in s/m/h (e.g. \"15m\")." $cold) -}}{{- end -}}
{{- if lt $coldSeconds 300 -}}
{{- fail (printf "config.coldTTL %q is below the 300s floor. The COLD tier carries 7d/30d SLA reports which are heavy server-side; the 300s floor is conservative and matches the design doc." $cold) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
validateCacheTTL (Contract C2.3). `fail()`s if `config.cacheTTL` is not a
non-empty string. Operators must quote bare integers (e.g. `"60s"` not
`60`); the binary expects a Go duration and an empty value renders
`--cache-ttl=` which `flag.Duration` rejects at startup. `kindIs` works
on Sprig's typed kinds. cache-ttl is pre-rejected here; the safe-arg helper's typed-input path is only load-bearing for logLevel/logFormat/metricsPath/namespace/excludeNamePattern/mcpUrl.
*/}}
{{- define "hyperping-exporter.validateCacheTTL" -}}
{{- if or (not (kindIs "string" .Values.config.cacheTTL)) (eq .Values.config.cacheTTL "") -}}
{{- fail (printf "config.cacheTTL must be a non-empty quoted Go duration string (e.g. \"60s\"); empty string is rejected because it would render `--cache-ttl=` which the binary's flag.Duration parser rejects at startup. Got kind %s (value %v). Quote the value in values.yaml." (kindOf .Values.config.cacheTTL) .Values.config.cacheTTL) -}}
{{- end -}}
{{- end -}}

{{/*
secretSourceCount (Contract C2.4). Returns the count (as a string, since
Helm `include` always returns a string; callers wrap with `int`) of secret
sources the operator has set. Sources are mutually exclusive:
  - `config.apiKey` (a non-empty string)
  - `config.existingSecret` (a non-empty string)
  - `externalSecret.enabled` (a truthy boolean)
Guards `.Values.externalSecret` is a map before reading `.enabled` to keep
operator error messages on shape mismatches honest.
*/}}
{{- define "hyperping-exporter.secretSourceCount" -}}
{{- $count := 0 -}}
{{- if .Values.config.apiKey -}}{{- $count = add $count 1 -}}{{- end -}}
{{- if .Values.config.existingSecret -}}{{- $count = add $count 1 -}}{{- end -}}
{{- $es := .Values.externalSecret | default dict -}}
{{- if not (kindIs "map" $es) -}}
{{- fail (printf "values.externalSecret must be a map; got kind %s. Refer to values.yaml for the supported shape." (kindOf .Values.externalSecret)) -}}
{{- end -}}
{{- if $es.enabled -}}{{- $count = add $count 1 -}}{{- end -}}
{{- $count -}}
{{- end -}}

{{/*
validateSecretSources (R4-6 boolean tree, Contract C2.4).
  1. If `replicaCount == 0` -> SKIP all checks (no Pods to authenticate).
  2. Else if `secretSourceCount > 1` -> fail with conflict naming the pair.
  3. Else if `secretSourceCount == 0` -> fail with missing-source message.
  4. Else -> pass.
The conflict message enumerates every set pair so the operator does not
have to guess which two values to reconcile.
*/}}
{{- define "hyperping-exporter.validateSecretSources" -}}
{{- if eq (int .Values.replicaCount) 0 -}}
{{- /* skip */ -}}
{{- else -}}
{{- $count := int (include "hyperping-exporter.secretSourceCount" .) -}}
{{- if gt $count 1 -}}
{{- $set := list -}}
{{- if .Values.config.apiKey -}}{{- $set = append $set "config.apiKey" -}}{{- end -}}
{{- if .Values.config.existingSecret -}}{{- $set = append $set "config.existingSecret" -}}{{- end -}}
{{- $es := .Values.externalSecret | default dict -}}
{{- if $es.enabled -}}{{- $set = append $set "externalSecret.enabled" -}}{{- end -}}
{{- fail (printf "secret-source conflict: %s are all set; set exactly one. (config.apiKey is dev-only; config.existingSecret consumes an externally-managed Secret; externalSecret.enabled lets External Secrets Operator manage the Secret.)" (join ", " $set)) -}}
{{- else if eq $count 0 -}}
{{- fail "secret-source missing: set exactly one of config.apiKey (dev-only), config.existingSecret (recommended for production; references an externally-managed Secret with key 'api-key'), or externalSecret.enabled: true (lets External Secrets Operator manage the Secret)." -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
validateReplicaCount (R4-1, Contract C1.1). The exporter is a singleton:
its cache is in-memory and per-process, and tenant-aggregate metrics
would double-count under HA. ALWAYS aborts on replicaCount > 1. The
chart currently has NO consumer of `internal._test*` keys (the prior
PDB rendering gate that honored `_testBypassReplicaCheck` was removed
in 57cbbb2); the `_test`-prefix carve-out is reserved for future
test-only knobs.
*/}}
{{- define "hyperping-exporter.validateReplicaCount" -}}
{{- if gt (int .Values.replicaCount) 1 -}}
{{- fail (printf "replicaCount must be 0 or 1; got %d. The hyperping-exporter binary is a singleton (in-memory cache, tenant-aggregate metrics would double-count under HA). Scale horizontally by sharding monitor namespaces across deployments, not by raising replicaCount." (int .Values.replicaCount)) -}}
{{- end -}}
{{- end -}}

{{/*
validateWebConfigFile (R10). The `config.webConfigFile` knob puts the
binary into TLS mode (Prometheus client toolkit `--web.config.file`),
but the chart's probes default to `httpGet.scheme: HTTP` and the
ServiceMonitor template hardcodes `scheme: http`. Setting the flag in
isolation produces a permanently-NotReady pod: kubelet probes fail
against the HTTPS endpoint and Prometheus scrapes also fail. Until the
chart wires both surfaces to switch to HTTPS, fail the render with a
clear message rather than silently break the install.
*/}}
{{- define "hyperping-exporter.validateWebConfigFile" -}}
{{- if .Values.config.webConfigFile -}}
{{- fail "config.webConfigFile is not supported by this chart. Setting it would enable TLS in the binary, but the chart's livenessProbe / readinessProbe httpGet.scheme and the ServiceMonitor endpoint scheme are hardcoded to HTTP. Either run the binary without --web.config.file (recommended; let a sidecar / Service-level TLS handle ingress encryption) or wait for a future chart release that wires httpGet.scheme + ServiceMonitor scheme + tlsConfig together." -}}
{{- end -}}
{{- end -}}

{{/*
validateExternalSecretApiVersion (C-3). When externalSecret.enabled, the
chart pins the rendered apiVersion to a known-good External Secrets
Operator CRD revision. Without this guard a typo like
`external-secrets.io/v1beata1` renders verbatim and CI passes, then
GitOps surfaces `no matches for kind ExternalSecret in version ...` at
apply time. Skipped entirely when externalSecret is disabled because the
externalsecret.yaml template is not rendered in that case.
*/}}
{{- define "hyperping-exporter.validateExternalSecretApiVersion" -}}
{{- $es := .Values.externalSecret | default dict -}}
{{- if $es.enabled -}}
{{- $allowed := list "external-secrets.io/v1beta1" "external-secrets.io/v1" -}}
{{- $av := $es.apiVersion | default "external-secrets.io/v1beta1" -}}
{{- if not (has $av $allowed) -}}
{{- fail (printf "externalSecret.apiVersion %q is not supported. Allowed values: %s. A typo here renders the ExternalSecret with an unknown apiVersion and surfaces only at apply time as `no matches for kind ExternalSecret`." $av (join ", " $allowed)) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
validateExternalSecretStoreKind (C-3). Same rationale as
validateExternalSecretApiVersion: pin secretStoreRef.kind to the two
values External Secrets Operator accepts so a typo fails at render time
rather than at apply time.
*/}}
{{- define "hyperping-exporter.validateExternalSecretStoreKind" -}}
{{- $es := .Values.externalSecret | default dict -}}
{{- if $es.enabled -}}
{{- $allowed := list "SecretStore" "ClusterSecretStore" -}}
{{- $ref := $es.secretStoreRef | default dict -}}
{{- $kind := $ref.kind | default "SecretStore" -}}
{{- if not (has $kind $allowed) -}}
{{- fail (printf "externalSecret.secretStoreRef.kind %q is not supported. Allowed values: %s." $kind (join ", " $allowed)) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
validateNoTestKeys (R4-8, Contract C8.1). The chart currently has NO
consumer of `internal._test*` keys (the prior PDB rendering gate that
honored `internal._testBypassReplicaCheck` was removed in 57cbbb2). The
`_test`-prefix carve-out is reserved for future test-only knobs; any
other key under `.Values.internal` is a footgun and aborts the render
here so production callers cannot stumble into an undocumented key.
*/}}
{{- define "hyperping-exporter.validateNoTestKeys" -}}
{{- $internal := .Values.internal | default dict -}}
{{- if not (kindIs "map" $internal) -}}
{{- fail (printf "values.internal must be a map (or absent); got kind %s." (kindOf .Values.internal)) -}}
{{- end -}}
{{- range $k, $v := $internal -}}
{{- if not (hasPrefix "_test" $k) -}}
{{- fail (printf "values.internal.%s is not a documented chart key. The `internal` block is reserved for test-only knobs prefixed `_test`; production callers should not set anything here." $k) -}}
{{- end -}}
{{- end -}}
{{- end -}}
