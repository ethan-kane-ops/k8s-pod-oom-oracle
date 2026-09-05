{{/*
Chart name truncated to 63 chars (DNS label limit).
*/}}
{{- define "oom-oracle.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully-qualified app name. If the release name already contains the chart name,
collapse it; otherwise prefix the release name to the chart name.
*/}}
{{- define "oom-oracle.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "oom-oracle.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "oom-oracle.labels" -}}
helm.sh/chart: {{ include "oom-oracle.chart" . }}
{{ include "oom-oracle.selectorLabels" . }}
app.kubernetes.io/component: node-agent
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{- define "oom-oracle.selectorLabels" -}}
app.kubernetes.io/name: {{ include "oom-oracle.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "oom-oracle.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "oom-oracle.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}
