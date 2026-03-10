{{/*
Expand the name of the chart.
*/}}
{{- define "locked-init.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fullname with release context.
*/}}
{{- define "locked-init.fullname" -}}
{{- $name := .Chart.Name }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "locked-init.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "locked-init.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "locked-init.selectorLabels" -}}
app.kubernetes.io/name: {{ include "locked-init.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "locked-init.serviceAccountName" -}}
{{ include "locked-init.fullname" . }}
{{- end }}

{{/*
Webhook full name used for the MutatingWebhookConfiguration and cert rotator.
*/}}
{{- define "locked-init.webhookName" -}}
{{ include "locked-init.fullname" . }}
{{- end }}

{{/*
TLS secret name.
*/}}
{{- define "locked-init.tlsSecretName" -}}
{{ include "locked-init.fullname" . }}-tls
{{- end }}

{{/*
Wrapper image reference.
*/}}
{{- define "locked-init.wrapperImage" -}}
{{ .Values.wrapper.image.repository }}:{{ .Values.wrapper.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/*
Webhook image reference.
*/}}
{{- define "locked-init.webhookImage" -}}
{{ .Values.webhook.image.repository }}:{{ .Values.webhook.image.tag | default .Chart.AppVersion }}
{{- end }}
