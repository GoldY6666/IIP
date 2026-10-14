{{/*
Chart 级辅助模板。命名遵循 "iip.fullname" 惯例，避免与既有资源名冲突。
*/}}

{{- define "iip.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "iip.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "iip.labels" -}}
helm.sh/chart: {{ include "iip.chart" . }}
{{ include "iip.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "iip.selectorLabels" -}}
app.kubernetes.io/name: {{ include "iip.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
PG 连接串。内置时走 StatefulSet 的 service 名；外置时用 external.host。
密码不进模板拼接：embedded 走 Secret，external 由用户经 values 提供
（NOTES 里明确警告 external.password 会出现在 helm template 输出中）。
*/}}
{{- define "iip.pgHost" -}}
{{- if .Values.database.embedded -}}
{{- printf "%s-postgres" (include "iip.name" .) -}}
{{- else -}}
{{- required "database.embedded=false 时必须提供 database.external.host" .Values.database.external.host -}}
{{- end -}}
{{- end -}}
