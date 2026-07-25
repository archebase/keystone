{{/* SPDX-FileCopyrightText: 2026 ArcheBase */}}
{{/* SPDX-License-Identifier: MulanPSL-2.0 */}}

{{- define "keystone-stack.fullname" -}}
{{- default .Release.Name .Values.fullnameOverride | trunc 51 | trimSuffix "-" -}}
{{- end -}}

{{- define "keystone-stack.componentName" -}}
{{- $root := index . 0 -}}
{{- $component := index . 1 -}}
{{- printf "%s-%s" (include "keystone-stack.fullname" $root) $component | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "keystone-stack.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
app.kubernetes.io/name: {{ .Chart.Name | quote }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "keystone-stack.selectorLabels" -}}
{{- $root := index . 0 -}}
{{- $component := index . 1 -}}
app.kubernetes.io/name: {{ $root.Chart.Name | quote }}
app.kubernetes.io/instance: {{ $root.Release.Name | quote }}
app.kubernetes.io/component: {{ $component | quote }}
{{- end -}}

{{- define "keystone-stack.credentialsSecretName" -}}
{{- default (include "keystone-stack.componentName" (list . "credentials")) .Values.credentials.existingSecret -}}
{{- end -}}

{{- define "keystone-stack.serviceAccountName" -}}
{{- default (include "keystone-stack.componentName" (list . "keystone")) .Values.serviceAccount.name -}}
{{- end -}}

{{- define "keystone-stack.image" -}}
{{- $image := index . 0 -}}
{{- if $image.digest -}}
{{- printf "%s@%s" $image.repository $image.digest -}}
{{- else -}}
{{- printf "%s:%s" $image.repository $image.tag -}}
{{- end -}}
{{- end -}}

{{- define "keystone-stack.host" -}}
{{- default (printf "keystone-%s.archebase.cn" .Release.Name) .Values.ingress.host -}}
{{- end -}}

{{- define "keystone-stack.callbackPublicBaseURL" -}}
{{- if .Values.keystone.callbackPublicBaseURL -}}
{{- .Values.keystone.callbackPublicBaseURL | trimSuffix "/" -}}
{{- else if .Values.ingress.enabled -}}
{{- printf "https://%s" (include "keystone-stack.host" .) -}}
{{- else -}}
{{- printf "http://%s:%v" (include "keystone-stack.componentName" (list . "keystone")) .Values.keystone.service.ports.http -}}
{{- end -}}
{{- end -}}

{{- define "keystone-stack.secretValue" -}}
{{- $root := index . 0 -}}
{{- $key := index . 1 -}}
{{- $provided := index . 2 -}}
{{- $length := index . 3 -}}
{{- $secretName := include "keystone-stack.credentialsSecretName" $root -}}
{{- $existing := lookup "v1" "Secret" $root.Release.Namespace $secretName -}}
{{- if $provided -}}
{{- $provided -}}
{{- else if and $existing $existing.data (hasKey $existing.data $key) -}}
{{- index $existing.data $key | b64dec -}}
{{- else -}}
{{- randAlphaNum $length -}}
{{- end -}}
{{- end -}}

{{- define "keystone-stack.validateImage" -}}
{{- $name := index . 0 -}}
{{- $image := index . 1 -}}
{{- if empty $image.repository -}}{{ fail (printf "%s.image.repository is required" $name) }}{{- end -}}
{{- if and (empty $image.tag) (empty $image.digest) -}}{{ fail (printf "%s.image.tag or %s.image.digest is required" $name $name) }}{{- end -}}
{{- if and $image.digest (not (regexMatch "^sha256:[a-f0-9]{64}$" $image.digest)) -}}{{ fail (printf "%s.image.digest must be a sha256 digest" $name) }}{{- end -}}
{{- end -}}

{{- define "keystone-stack.validateValues" -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" .Release.Name) -}}{{ fail "releaseName must match ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" }}{{- end -}}
{{- if gt (len .Release.Name) 53 -}}{{ fail "releaseName must have length of 1-53 characters" }}{{- end -}}
{{- if gt (len (printf "keystone-%s" .Release.Name)) 63 -}}{{ fail "derived keystone-<releaseName> DNS label must be no longer than 63 characters" }}{{- end -}}
{{- include "keystone-stack.validateImage" (list "keystone" .Values.keystone.image) -}}
{{- include "keystone-stack.validateImage" (list "synapse" .Values.synapse.image) -}}
{{- include "keystone-stack.validateImage" (list "mysql" .Values.mysql.image) -}}
{{- if and (empty .Values.keystone.callbackPublicBaseURL) (not .Values.ingress.enabled) -}}
{{- /* The internal Service URL is a valid boot-time fallback; external recorders need an explicit public URL. */ -}}
{{- end -}}
{{- if and .Values.storage.type (eq .Values.storage.type "tos") (not .Values.serviceAccount.create) (empty .Values.serviceAccount.name) -}}{{ fail "serviceAccount.name is required when storage.type=tos and serviceAccount.create=false" }}{{- end -}}
{{- if and (eq .Values.storage.type "tos") (not .Values.serviceAccount.create) (eq .Values.serviceAccount.name "keystone") (ne .Release.Namespace "archebase-system") -}}{{ fail "production TOS defaults require --namespace archebase-system because cloud-infra manages archebase-system/keystone" }}{{- end -}}
{{- if .Values.ingress.enabled -}}
  {{- if and (ne .Values.keystone.service.type "NodePort") (ne .Values.keystone.service.type "LoadBalancer") -}}{{ fail "keystone.service.type must be NodePort or LoadBalancer when ingress.enabled=true because Volcengine ALB rejects ClusterIP backends" }}{{- end -}}
  {{- if and (ne .Values.synapse.service.type "NodePort") (ne .Values.synapse.service.type "LoadBalancer") -}}{{ fail "synapse.service.type must be NodePort or LoadBalancer when ingress.enabled=true because Volcengine ALB rejects ClusterIP backends" }}{{- end -}}
{{- end -}}
{{- if empty .Values.credentials.existingSecret -}}
  {{- if eq .Values.storage.type "s3" -}}
    {{- if empty .Values.credentials.storageAccessKey -}}{{ fail "credentials.storageAccessKey is required when storage.type=s3 and credentials.existingSecret is empty" }}{{- end -}}
    {{- if empty .Values.credentials.storageSecretKey -}}{{ fail "credentials.storageSecretKey is required when storage.type=s3 and credentials.existingSecret is empty" }}{{- end -}}
  {{- end -}}
{{- end -}}
{{- if eq .Values.storage.type "s3" -}}
  {{- if empty .Values.storage.s3.endpoint -}}{{ fail "storage.s3.endpoint is required when storage.type=s3" }}{{- end -}}
  {{- if empty .Values.storage.s3.bucket -}}{{ fail "storage.s3.bucket is required when storage.type=s3" }}{{- end -}}
{{- else if eq .Values.storage.type "tos" -}}
  {{- if empty .Values.storage.tos.endpoint -}}{{ fail "storage.tos.endpoint is required when storage.type=tos" }}{{- end -}}
  {{- if empty .Values.storage.tos.bucket -}}{{ fail "storage.tos.bucket is required when storage.type=tos" }}{{- end -}}
  {{- if empty .Values.storage.tos.region -}}{{ fail "storage.tos.region is required when storage.type=tos" }}{{- end -}}
  {{- if and (not .Values.storage.tos.mockSTS) (empty .Values.storage.tos.stsRoleTRN) -}}{{ fail "storage.tos.stsRoleTRN is required when storage.type=tos and mockSTS=false" }}{{- end -}}
  {{- if empty .Values.keystone.hilbert.baseURL -}}{{ fail "keystone.hilbert.baseURL is required when storage.type=tos" }}{{- end -}}
  {{- if empty .Values.credentials.existingSecret -}}
    {{- if empty .Values.credentials.hilbertAccessKey -}}{{ fail "credentials.hilbertAccessKey is required when storage.type=tos" }}{{- end -}}
    {{- if empty .Values.credentials.hilbertSecretKey -}}{{ fail "credentials.hilbertSecretKey is required when storage.type=tos" }}{{- end -}}
  {{- end -}}
{{- else -}}
  {{- fail "storage.type must be s3 or tos" -}}
{{- end -}}
{{- if .Values.keystone.syncEnabled -}}
  {{- if empty .Values.keystone.hilbert.baseURL -}}{{ fail "keystone.hilbert.baseURL is required when keystone.syncEnabled=true" }}{{- end -}}
  {{- if empty .Values.credentials.existingSecret -}}
    {{- if empty .Values.credentials.hilbertAccessKey -}}{{ fail "credentials.hilbertAccessKey is required when keystone.syncEnabled=true" }}{{- end -}}
    {{- if empty .Values.credentials.hilbertSecretKey -}}{{ fail "credentials.hilbertSecretKey is required when keystone.syncEnabled=true" }}{{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}
