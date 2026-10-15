{{- define "kosh.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "kosh.fullname" -}}
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

{{- /*
The name of this kosh among those that serve the cluster, which kosh puts
in its groups and bindings (kosh serve --instance): the namespace and the
release, which no other release has together. A release's name and a
namespace are lower-case letters, digits, "-" and "."; cut to 63, the end
may be a run of "-" or ".", which is taken off: kosh refuses an instance
that does not begin and end with a letter or a digit.
*/}}
{{- define "kosh.instance" -}}
{{- regexReplaceAll "[-.]+$" (printf "%s-%s" .Release.Namespace .Release.Name | trunc 63) "" }}
{{- end }}

{{- define "kosh.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "kosh.labels" -}}
helm.sh/chart: {{ include "kosh.chart" . }}
{{ include "kosh.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "kosh.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kosh.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "kosh.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kosh.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* The refusals: an install that would lose its identity or has no way in. */}}
{{- define "kosh.validate" -}}
{{- if and (not .Values.state.existingSecret) (not .Values.state.persistence.enabled) }}
{{- fail "kosh needs its state: set state.existingSecret or state.persistence.enabled, or its endpoint ID and host key change at every start" }}
{{- end }}
{{- if and .Values.state.existingSecret .Values.state.persistence.enabled }}
{{- fail "kosh takes its state from one place: set either state.existingSecret or state.persistence.enabled, not both" }}
{{- end }}
{{- if and (not .Values.ssh.enabled) (not .Values.iroh.enabled) }}
{{- fail "kosh needs a way in: set ssh.enabled or iroh.enabled" }}
{{- end }}
{{- if and .Values.iroh.enabled (not .Values.iroh.relay) (not .Values.iroh.mdns) }}
{{- fail "kosh over iroh cannot be found: set iroh.relay or iroh.mdns, or switch iroh.enabled off" }}
{{- end }}
{{- end }}
