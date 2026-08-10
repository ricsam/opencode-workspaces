{{- define "opencode-workspaces.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "opencode-workspaces.fullname" -}}
{{- if .Values.fullnameOverride }}{{ .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}{{ else }}{{ printf "%s-%s" .Release.Name (include "opencode-workspaces.name" .) | trunc 63 | trimSuffix "-" }}{{ end -}}
{{- end -}}
{{- define "opencode-workspaces.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
app.kubernetes.io/name: {{ include "opencode-workspaces.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
{{- define "opencode-workspaces.selectorLabels" -}}
app.kubernetes.io/name: {{ include "opencode-workspaces.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
{{- define "opencode-workspaces.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}{{ default (include "opencode-workspaces.fullname" .) .Values.serviceAccount.name }}{{ else }}{{ default "default" .Values.serviceAccount.name }}{{ end -}}
{{- end -}}
{{- define "opencode-workspaces.secretName" -}}{{ default (include "opencode-workspaces.fullname" .) .Values.existingSecret }}{{- end -}}
{{- define "opencode-workspaces.publicURL" -}}{{ if .Values.publicURL }}{{ .Values.publicURL }}{{ else if .Values.ingress.enabled }}https://{{ .Values.ingress.host }}{{ else }}http://{{ include "opencode-workspaces.fullname" . }}{{ end }}{{- end -}}
{{- define "opencode-workspaces.image" -}}{{ printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}{{- end -}}
{{- define "opencode-workspaces.workspaceImage" -}}{{ printf "%s:%s" .Values.workspaceImage.repository (default .Chart.AppVersion .Values.workspaceImage.tag) }}{{- end -}}
