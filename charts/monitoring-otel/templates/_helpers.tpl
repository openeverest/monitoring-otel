{{/*
Expand the name of the chart.
*/}}
{{- define "monitoring-otel.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name (63 chars, DNS naming spec).
*/}}
{{- define "monitoring-otel.fullname" -}}
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

{{- define "monitoring-otel.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "monitoring-otel.labels" -}}
helm.sh/chart: {{ include "monitoring-otel.chart" . }}
{{ include "monitoring-otel.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: openeverest
openeverest.io/extension-type: monitoring
{{- end }}

{{- define "monitoring-otel.selectorLabels" -}}
app.kubernetes.io/name: {{ include "monitoring-otel.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "monitoring-otel.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "monitoring-otel.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "monitoring-otel.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}

{{- define "monitoring-otel.targetAllocatorClusterRole" -}}
{{- printf "%s-target-allocator" (include "monitoring-otel.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
