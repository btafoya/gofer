{{/* Chart name, truncated to 63 chars for DNS labels. */}}
{{- define "gofer.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "gofer.fullname" -}}
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

{{- define "gofer.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "gofer.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gofer.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "gofer.labels" -}}
helm.sh/chart: {{ include "gofer.chart" . }}
{{ include "gofer.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "gofer.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "gofer.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "gofer.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) }}
{{- end }}

{{- define "gofer.secretName" -}}
{{- if .Values.secret.existingSecret }}
{{- .Values.secret.existingSecret }}
{{- else }}
{{- include "gofer.fullname" . }}
{{- end }}
{{- end }}

{{- define "gofer.pvcName" -}}
{{- if .Values.persistence.existingClaim }}
{{- .Values.persistence.existingClaim }}
{{- else }}
{{- include "gofer.fullname" . }}
{{- end }}
{{- end }}

{{/* Probe handler: tcpSocket by default, HTTP path when probes.type is http. Expects dict root, path. */}}
{{- define "gofer.probeHandler" -}}
{{- if eq .root.Values.probes.type "http" }}
httpGet:
  path: {{ .path }}
  port: http
{{- else }}
tcpSocket:
  port: http
{{- end }}
{{- end }}
