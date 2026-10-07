{{- define "hub.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}{{ .Release.Name | trunc 63 | trimSuffix "-" }}{{- else -}}{{ printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}{{- end -}}
{{- end -}}

{{- define "hub.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "hub.selector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "hub.secretName" -}}
{{ .Values.hub.existingSecret | default (include "hub.fullname" .) }}
{{- end -}}

{{- define "hub.databaseURL" -}}
{{- if .Values.sqld.enabled -}}
http://{{ include "hub.fullname" . }}-sqld:8080
{{- else -}}
{{- required "set externalDatabase.url or sqld.enabled=true" .Values.externalDatabase.url -}}
{{- end -}}
{{- end -}}
