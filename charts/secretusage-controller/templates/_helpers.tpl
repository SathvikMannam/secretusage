{{/*
Expand the name of the chart.
*/}}
{{- define "secretusage-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "secretusage-controller.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "secretusage-controller.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "secretusage-controller.labels" -}}
helm.sh/chart: {{ include "secretusage-controller.chart" . }}
app.kubernetes.io/name: {{ include "secretusage-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "secretusage-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "secretusage-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "secretusage-controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "secretusage-controller.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Policy rules shared by the ClusterRole and the namespaced Role. Secrets are read as
list and watch only: the controller watches Secret metadata to answer "does this
Secret exist" and never needs to fetch a Secret body. Rendered at column zero, so
call sites indent it with nindent.
*/}}
{{- define "secretusage-controller.rules" -}}
- apiGroups:
    - usage.secretusage.io
  resources:
    - secretusages
  verbs:
    - create
    - delete
    - get
    - list
    - patch
    - update
    - watch
- apiGroups:
    - usage.secretusage.io
  resources:
    - secretusages/status
  verbs:
    - get
    - patch
    - update
- apiGroups:
    - ""
  resources:
    - secrets
  verbs:
    - list
    - watch
- apiGroups:
    - ""
  resources:
    - pods
    - replicationcontrollers
    - serviceaccounts
  verbs:
    - get
    - list
    - watch
- apiGroups:
    - apps
  resources:
    - daemonsets
    - deployments
    - replicasets
    - statefulsets
  verbs:
    - get
    - list
    - watch
- apiGroups:
    - batch
  resources:
    - cronjobs
    - jobs
  verbs:
    - get
    - list
    - watch
- apiGroups:
    - networking.k8s.io
  resources:
    - ingresses
  verbs:
    - get
    - list
    - watch
{{- range $rule := .Values.customRules }}
{{- $group := "" }}
{{- $parts := splitList "/" $rule.apiVersion }}
{{- if gt (len $parts) 1 }}{{ $group = index $parts 0 }}{{ end }}
- apiGroups:
    - {{ $group | quote }}
  resources:
    - {{ required (printf "customRules entry %s/%s must set 'resource' (the plural name) so RBAC can be generated" $rule.apiVersion $rule.kind) $rule.resource }}
  verbs:
    - get
    - list
    - watch
{{- end }}
{{- end }}
