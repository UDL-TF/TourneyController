{{- define "tourney-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "tourney-controller.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "tourney-controller.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" -}}
{{- end -}}

{{- define "tourney-controller.labels" -}}
helm.sh/chart: {{ include "tourney-controller.chart" . }}
app.kubernetes.io/name: {{ include "tourney-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
{{- end -}}

{{- define "tourney-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "tourney-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "tourney-controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (printf "%s-sa" (include "tourney-controller.fullname" .)) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name must be set when serviceAccount.create is false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "tourney-controller.targetNamespace" -}}
{{- default .Release.Namespace .Values.controllerConfig.namespace -}}
{{- end -}}

{{- define "tourney-controller.matchStatuses" -}}
{{- $statuses := .Values.controllerConfig.matchStatuses | default (list) -}}
{{- if not $statuses -}}
0
{{- else -}}
{{- $sep := "" -}}
{{- range $statuses }}
{{- printf "%s%v" $sep . -}}
{{- $sep = "," -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "tourney-controller.matchCompletedStatuses" -}}
{{- $statuses := .Values.controllerConfig.matchCompletedStatuses | default (list) -}}
{{- if not $statuses -}}
3
{{- else -}}
{{- $sep := "" -}}
{{- range $statuses }}
{{- printf "%s%v" $sep . -}}
{{- $sep = "," -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "tourney-controller.dbSecretName" -}}
{{- printf "%s-db" (include "tourney-controller.fullname" .) -}}
{{- end -}}

{{- define "tourney-controller.steamSecretName" -}}
{{- printf "%s-steam" (include "tourney-controller.fullname" .) -}}
{{- end -}}

{{- define "tourney-controller.tf2ValuesConfigMap" -}}
{{- printf "%s-tf2-values" (include "tourney-controller.fullname" .) -}}
{{- end -}}
