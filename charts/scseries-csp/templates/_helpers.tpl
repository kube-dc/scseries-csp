{{- define "scseries-csp.name" -}}{{ .Chart.Name }}{{- end -}}
{{- define "scseries-csp.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}{{ .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else -}}{{ printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}{{- end -}}
{{- end -}}
{{- define "scseries-csp.labels" -}}
app.kubernetes.io/name: {{ include "scseries-csp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}
{{- define "scseries-csp.selector" -}}
app.kubernetes.io/name: {{ include "scseries-csp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
{{- define "scseries-csp.serviceName" -}}{{ include "scseries-csp.fullname" . }}{{- end -}}
{{- define "scseries-csp.backendSecret" -}}
{{- .Values.array.credentials.existingSecret | default (printf "%s-backend" (include "scseries-csp.fullname" .)) -}}
{{- end -}}
{{- define "scseries-csp.tokenSecret" -}}
{{- .Values.csp.tokenKey.existingSecret | default (printf "%s-token-key" (include "scseries-csp.fullname" .)) -}}
{{- end -}}
{{- define "scseries-csp.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}
