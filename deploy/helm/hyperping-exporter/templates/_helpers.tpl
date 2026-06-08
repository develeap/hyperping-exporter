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
validateProjectsCacheBlocks. Per-project tier override safety. Same floor
shape and same fail() error message style as validateTierTTLs, applied to
each entry's optional cache: block. Without this check a chart user can
slip cache.hotTTL: "1s" past the chart's safety net (validateTierTTLs only
inspects the global config.hotTTL/warmTTL/coldTTL) and saturate the SDK
rate limit at boot. Also rejects hotEnabled: false at render time so the
operator sees a clear template error instead of a CrashLoop pod when the
binary refuses the same value at startup.

Skipped entirely when cacheMode is not "tiered" (cache: blocks have no
runtime effect in legacy mode, so render-time validation would only
generate noise). Also skipped when config.projects is empty.
*/}}
{{- define "hyperping-exporter.validateProjectsCacheBlocks" -}}
{{- $mode := .Values.config.cacheMode | default "legacy" -}}
{{- if and (eq (lower $mode) "tiered") .Values.config.projects -}}
{{- range $i, $p := .Values.config.projects -}}
{{- if $p.cache -}}
{{- $id := $p.id | default (printf "(index %d)" $i) -}}
{{- /* hotEnabled: false is fatal in the binary; reject at render time. */ -}}
{{- if hasKey $p.cache "hotEnabled" -}}
{{- if not $p.cache.hotEnabled -}}
{{- fail (printf "project %q at projects[%d]: cache.hotEnabled: false is rejected. HOT carries every per-monitor up/down series and is the readiness gate; a project with HOT disabled produces a Pod that boots, never readies, and emits no scrape. Remove the field or set hotEnabled: true." $id $i) -}}
{{- end -}}
{{- end -}}
{{- /* Per-tier TTL floors mirror validateTierTTLs (hot >= 30s, warm >= 60s, cold >= 300s). */ -}}
{{- if hasKey $p.cache "hotTTL" -}}
{{- $v := $p.cache.hotTTL -}}
{{- if or (not (kindIs "string" $v)) (eq $v "") -}}
{{- fail (printf "project %q at projects[%d]: cache.hotTTL must be a non-empty quoted Go duration string (e.g. \"60s\"); got kind %s (value %v)." $id $i (kindOf $v) $v) -}}
{{- end -}}
{{- $n := int (regexReplaceAll "[^0-9]" $v "") -}}
{{- $sec := 0 -}}
{{- if hasSuffix "h" $v }}{{- $sec = mul $n 3600 -}}
{{- else if hasSuffix "m" $v }}{{- $sec = mul $n 60 -}}
{{- else if hasSuffix "s" $v }}{{- $sec = $n -}}
{{- else }}{{- fail (printf "project %q at projects[%d]: cache.hotTTL %q must end in s/m/h (e.g. \"60s\"); the binary's time.ParseDuration rejects unit-less values." $id $i $v) -}}{{- end -}}
{{- if lt $sec 30 -}}
{{- fail (printf "project %q at projects[%d]: cache.hotTTL %q is below the 30s floor (same floor as the global config.hotTTL; see validateTierTTLs)." $id $i $v) -}}
{{- end -}}
{{- end -}}
{{- if hasKey $p.cache "warmTTL" -}}
{{- $v := $p.cache.warmTTL -}}
{{- if or (not (kindIs "string" $v)) (eq $v "") -}}
{{- fail (printf "project %q at projects[%d]: cache.warmTTL must be a non-empty quoted Go duration string (e.g. \"5m\"); got kind %s (value %v)." $id $i (kindOf $v) $v) -}}
{{- end -}}
{{- $n := int (regexReplaceAll "[^0-9]" $v "") -}}
{{- $sec := 0 -}}
{{- if hasSuffix "h" $v }}{{- $sec = mul $n 3600 -}}
{{- else if hasSuffix "m" $v }}{{- $sec = mul $n 60 -}}
{{- else if hasSuffix "s" $v }}{{- $sec = $n -}}
{{- else }}{{- fail (printf "project %q at projects[%d]: cache.warmTTL %q must end in s/m/h (e.g. \"5m\")." $id $i $v) -}}{{- end -}}
{{- if lt $sec 60 -}}
{{- fail (printf "project %q at projects[%d]: cache.warmTTL %q is below the 60s floor (same floor as the global config.warmTTL; see validateTierTTLs)." $id $i $v) -}}
{{- end -}}
{{- end -}}
{{- if hasKey $p.cache "coldTTL" -}}
{{- $v := $p.cache.coldTTL -}}
{{- if or (not (kindIs "string" $v)) (eq $v "") -}}
{{- fail (printf "project %q at projects[%d]: cache.coldTTL must be a non-empty quoted Go duration string (e.g. \"15m\"); got kind %s (value %v)." $id $i (kindOf $v) $v) -}}
{{- end -}}
{{- $n := int (regexReplaceAll "[^0-9]" $v "") -}}
{{- $sec := 0 -}}
{{- if hasSuffix "h" $v }}{{- $sec = mul $n 3600 -}}
{{- else if hasSuffix "m" $v }}{{- $sec = mul $n 60 -}}
{{- else if hasSuffix "s" $v }}{{- $sec = $n -}}
{{- else }}{{- fail (printf "project %q at projects[%d]: cache.coldTTL %q must end in s/m/h (e.g. \"15m\")." $id $i $v) -}}{{- end -}}
{{- if lt $sec 300 -}}
{{- fail (printf "project %q at projects[%d]: cache.coldTTL %q is below the 300s floor (same floor as the global config.coldTTL; see validateTierTTLs)." $id $i $v) -}}
{{- end -}}
{{- end -}}
{{- end -}}
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
  2. Else if `config.projects` is non-empty -> SKIP this validator and
     delegate to validateProjects, which enforces per-project source
     uniqueness. Letting both run would double-fail when projects mode
     intentionally moves every secret source into the per-project list.
  3. Else if `secretSourceCount > 1` -> fail with conflict naming the pair.
  4. Else if `secretSourceCount == 0` -> fail with missing-source message.
  5. Else -> pass.
The conflict message enumerates every set pair so the operator does not
have to guess which two values to reconcile.
*/}}
{{- define "hyperping-exporter.validateSecretSources" -}}
{{- if eq (int .Values.replicaCount) 0 -}}
{{- /* skip */ -}}
{{- else if .Values.config.projects -}}
{{- /* multi-project mode: per-project validation owns this; see validateProjects */ -}}
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
validateProjects (multi-project mode, Contract C2.4+). Enforces the
per-project secret-source contract when config.projects is non-empty:
  - Each project id must match [a-zA-Z0-9._-]{1,64} (same alphabet as
    the tenant regex used elsewhere; matches reProjectID in main.go).
  - Project ids are globally unique within the list.
  - Each project sets exactly ONE secret source: inline apiKey, or
    existingSecret, or (when top-level externalSecret.enabled is true) a
    per-project externalSecret.remoteRef block.
  - Top-level secret sources (config.apiKey / config.existingSecret /
    top-level externalSecret.remoteRef) must be empty: multi-project
    mode moves every key into the per-project list.

Skipped entirely when replicaCount == 0 or when config.projects is
empty (the legacy single-key path stays as in chart 1.5.x).
*/}}
{{- define "hyperping-exporter.validateProjects" -}}
{{- if and (ne (int .Values.replicaCount) 0) .Values.config.projects -}}
{{- $es := .Values.externalSecret | default dict -}}
{{- $esEnabled := $es.enabled -}}
{{- /* Top-level mutex with the projects list. */ -}}
{{- if .Values.config.apiKey -}}
{{- fail "config.apiKey conflicts with config.projects: multi-project mode moves every secret source into the per-project list. Move the inline key to projects[*].apiKey or clear config.apiKey." -}}
{{- end -}}
{{- if .Values.config.existingSecret -}}
{{- fail "config.existingSecret conflicts with config.projects: multi-project mode moves every secret source into the per-project list. Move the existingSecret name to projects[*].existingSecret or clear config.existingSecret." -}}
{{- end -}}
{{- if and $esEnabled $es.remoteRef $es.remoteRef.key -}}
{{- fail "externalSecret.remoteRef.key conflicts with config.projects: multi-project mode requires one remoteRef per project under projects[*].externalSecret.remoteRef. Move the remoteRef into each project entry." -}}
{{- end -}}
{{- /* Per-project alphabet, uniqueness, secret-source uniqueness. */ -}}
{{- $idAlphabet := `^[a-zA-Z0-9._-]{1,64}$` -}}
{{- $seen := dict -}}
{{- range $i, $p := .Values.config.projects -}}
{{- $id := $p.id | default "" -}}
{{- if not (regexMatch $idAlphabet $id) -}}
{{- fail (printf "project id %q at projects[%d] is invalid: project id must match %s (same alphabet as the tenant regex; ensures the Prometheus constLabel cannot inject characters that downstream label matchers cannot escape)." $id $i $idAlphabet) -}}
{{- end -}}
{{- if hasKey $seen $id -}}
{{- fail (printf "duplicate project id %q at projects[%d]: every project id must be unique within the list (the id becomes the value of the `project` constLabel; duplicates would collapse two projects onto one Desc)." $id $i) -}}
{{- end -}}
{{- $seen = set $seen $id true -}}
{{- /*
ESO mode is incompatible with per-project existingSecret: the
deployment.yaml projected-volume template skips the existingSecret
branch entirely when externalSecret.enabled is true (only the ESO
target Secret is projected), so an operator who sets existingSecret
under ESO mode would silently lose the on-disk api-key file. The
externalsecret.yaml `required` filter then explodes with a
confusing "externalSecret.remoteRef.key is required" message for
the same project the operator deliberately moved to existingSecret.
Catch the misconfiguration here with a message that names both
options.
*/ -}}
{{- if and $esEnabled $p.existingSecret -}}
{{- fail (printf "project %q at projects[%d]: existingSecret is incompatible with externalSecret.enabled in projects mode. Either set externalSecret.enabled: false (chart-managed or pre-existing Secrets per project) or move this project to externalSecret.remoteRef and drop existingSecret." $id $i) -}}
{{- end -}}
{{- $count := 0 -}}
{{- if $p.apiKey -}}{{- $count = add $count 1 -}}{{- end -}}
{{- if $p.existingSecret -}}{{- $count = add $count 1 -}}{{- end -}}
{{- $pEs := $p.externalSecret | default dict -}}
{{- if and $esEnabled $pEs.remoteRef $pEs.remoteRef.key -}}{{- $count = add $count 1 -}}{{- end -}}
{{- if gt $count 1 -}}
{{- fail (printf "project %q at projects[%d]: exactly one of apiKey, existingSecret, externalSecret.remoteRef may be set per project; got %d. Pick a single secret source per project." $id $i $count) -}}
{{- end -}}
{{- if eq $count 0 -}}
{{- fail (printf "project %q at projects[%d]: missing secret source. Set exactly one of apiKey (dev-only), existingSecret (recommended), or externalSecret.remoteRef (requires top-level externalSecret.enabled: true)." $id $i) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
projectsYaml (multi-project mode). Renders the YAML document mounted at
/etc/hyperping/projects.yaml. Every project entry resolves to an
apiKeyFile reference under /etc/hyperping/api-key-<id>; the
deployment.yaml template projects per-project Secrets at those paths so
the binary can read each key off disk without env-var leakage. Inline
apiKey entries are also indirected through the same path so the
projects.yaml document never carries plaintext secrets.
*/}}
{{- define "hyperping-exporter.projectsYaml" -}}
{{- range $i, $p := .Values.config.projects -}}
- id: {{ $p.id | quote }}
  apiKeyFile: /etc/hyperping/api-key-{{ $p.id }}
{{- if $p.mcpUrl }}
  mcpUrl: {{ $p.mcpUrl | quote }}
{{- end }}
{{- if $p.excludeNamePattern }}
  excludeNamePattern: {{ $p.excludeNamePattern | quote }}
{{- end }}
{{- /*
Per-project cache tier overrides (feat/per-project-tier-cache). The
`cache:` block is OPTIONAL and pass-through: every key is rendered
verbatim only when its operator-supplied value is non-empty. Absent
keys do NOT emit a `null` or empty-map placeholder, so a project
without an override produces byte-identical YAML to chart 1.6.x.
The binary parses every field as a Go duration (TTLs) or bool
(Enabled flags); see main.projectCacheOverride for the canonical
field semantics. Quote durations so the chart never coerces a bare
integer; bools render with toJson so true/false round-trip cleanly.
*/}}
{{- if $p.cache }}
  cache:
  {{- if hasKey $p.cache "hotTTL" }}
    hotTTL: {{ $p.cache.hotTTL | quote }}
  {{- end }}
  {{- if hasKey $p.cache "warmTTL" }}
    warmTTL: {{ $p.cache.warmTTL | quote }}
  {{- end }}
  {{- if hasKey $p.cache "coldTTL" }}
    coldTTL: {{ $p.cache.coldTTL | quote }}
  {{- end }}
  {{- if hasKey $p.cache "hotEnabled" }}
    hotEnabled: {{ $p.cache.hotEnabled | toJson }}
  {{- end }}
  {{- if hasKey $p.cache "warmEnabled" }}
    warmEnabled: {{ $p.cache.warmEnabled | toJson }}
  {{- end }}
  {{- if hasKey $p.cache "coldEnabled" }}
    coldEnabled: {{ $p.cache.coldEnabled | toJson }}
  {{- end }}
{{- end }}
{{- /*
Per-project periods list (feat/multi-period). Optional; when present
renders verbatim into projects.yaml so the binary's resolvePeriods
sees the same slice the operator wrote. Each token is quoted to
match the chart's general policy of never letting the YAML serializer
guess a type for a user-supplied scalar. Absent / empty list produces
no `periods:` line and the binary's loader defaults to ["24h"], which
keeps a pre-1.8 values.yaml byte-identical on the wire.
*/}}
{{- if $p.periods }}
  periods:
  {{- range $period := $p.periods }}
    - {{ $period | quote }}
  {{- end }}
{{- end }}
{{ end -}}
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
