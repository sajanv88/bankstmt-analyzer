{{/*
Chart name, overridable.
*/}}
{{- define "bankstmt-analyzer.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name, capped at 63 characters for label limits.
*/}}
{{- define "bankstmt-analyzer.fullname" -}}
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

{{- define "bankstmt-analyzer.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "bankstmt-analyzer.labels" -}}
helm.sh/chart: {{ include "bankstmt-analyzer.chart" . }}
{{ include "bankstmt-analyzer.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "bankstmt-analyzer.selectorLabels" -}}
app.kubernetes.io/name: {{ include "bankstmt-analyzer.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "bankstmt-analyzer.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "bankstmt-analyzer.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The Secret holding the credentials. An operator-supplied Secret is used as
given; otherwise the chart creates one from values.
*/}}
{{- define "bankstmt-analyzer.secretName" -}}
{{- if .Values.config.existingSecret }}
{{- .Values.config.existingSecret }}
{{- else }}
{{- printf "%s-config" (include "bankstmt-analyzer.fullname" .) }}
{{- end }}
{{- end }}

{{- define "bankstmt-analyzer.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}

{{/*
Non-secret environment, shared by the API, the worker and the migration
Job so all three read the same configuration. The credentials arrive
separately through envFrom, so nothing sensitive is rendered into a pod
spec where `kubectl describe` would show it.
*/}}
{{- define "bankstmt-analyzer.env" -}}
- name: ENV
  value: {{ .Values.env | quote }}
- name: LOG_LEVEL
  value: {{ .Values.logLevel | quote }}
- name: HTTP_ADDR
  value: {{ printf ":%d" (int .Values.service.targetPort) | quote }}
- name: STORAGE_BACKEND
  value: {{ .Values.storage.backend | quote }}
- name: STORAGE_DIR
  value: {{ .Values.storage.dir | quote }}
{{- if eq .Values.storage.backend "s3" }}
- name: S3_ENDPOINT
  value: {{ .Values.storage.s3.endpoint | quote }}
- name: S3_BUCKET
  value: {{ .Values.storage.s3.bucket | quote }}
- name: S3_REGION
  value: {{ .Values.storage.s3.region | quote }}
- name: S3_USE_PATH_STYLE
  value: {{ .Values.storage.s3.usePathStyle | quote }}
- name: S3_PREFIX
  value: {{ .Values.storage.s3.prefix | quote }}
{{- end }}
{{- range $key, $value := .Values.extraEnv }}
- name: {{ $key }}
  value: {{ $value | quote }}
{{- end }}
{{- end }}
