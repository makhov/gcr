{{/* The release image. */}}
{{- define "gcr.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/* The operator's service account, as the gcr-migrate policy names it. */}}
{{- define "gcr.operatorUser" -}}
system:serviceaccount:{{ .Release.Namespace }}:gcr-operator
{{- end }}
