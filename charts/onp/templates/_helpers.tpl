{{/*
Expand the name of the chart.
*/}}
{{- define "onp.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name. Used as the prefix for every cluster-scoped and
namespaced resource so multiple releases never collide.
*/}}
{{- define "onp.fullname" -}}
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
Chart name and version, as used in the standard helm.sh/chart label.
*/}}
{{- define "onp.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to every object.
*/}}
{{- define "onp.labels" -}}
helm.sh/chart: {{ include "onp.chart" . }}
{{ include "onp.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels — the stable subset that must never change for a given release,
since it is baked into Deployment/DaemonSet selectors.
*/}}
{{- define "onp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "onp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Resolve a full image reference for one component, defaulting the tag to the
chart's appVersion when image.tag is empty.
Usage: include "onp.image" (dict "root" $ "name" "onp-controller")
*/}}
{{- define "onp.image" -}}
{{- $root := .root -}}
{{- $img := $root.Values.image -}}
{{- $tag := default $root.Chart.AppVersion $img.tag -}}
{{- printf "%s/%s/%s:%s" $img.registry $img.repository .name $tag -}}
{{- end }}
