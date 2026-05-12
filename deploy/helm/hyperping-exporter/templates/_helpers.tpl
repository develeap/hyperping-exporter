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
validateCacheTTL (Contract C2.3). `fail()`s if `config.cacheTTL` is not a
string. Operators must quote bare integers (e.g. `"60s"` not `60`); the
binary expects a Go duration. `kindIs` works on Sprig's typed kinds.
*/}}
{{- define "hyperping-exporter.validateCacheTTL" -}}
{{- if not (kindIs "string" .Values.config.cacheTTL) -}}
{{- fail (printf "config.cacheTTL must be a quoted Go duration string (e.g. \"60s\"). Got kind %s (value %v). Quote the value in values.yaml." (kindOf .Values.config.cacheTTL) .Values.config.cacheTTL) -}}
{{- end -}}
{{- end -}}
