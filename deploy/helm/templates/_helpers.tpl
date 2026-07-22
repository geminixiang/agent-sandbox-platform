{{- define "agent-sandbox-platform.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- define "agent-sandbox-platform.fullname" -}}
{{- if .Values.fullnameOverride }}{{ .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}{{ else }}{{ printf "%s-%s" .Release.Name (include "agent-sandbox-platform.name" .) | trunc 63 | trimSuffix "-" }}{{ end }}
{{- end }}
{{- define "agent-sandbox-platform.labels" -}}
app.kubernetes.io/name: {{ include "agent-sandbox-platform.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
{{- define "agent-sandbox-platform.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agent-sandbox-platform.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
{{- define "agent-sandbox-platform.preflightName" -}}
{{- printf "%s-preflight-%s" (include "agent-sandbox-platform.fullname" . | trunc 44 | trimSuffix "-") (sha256sum .Release.Namespace | trunc 8) -}}
{{- end }}
