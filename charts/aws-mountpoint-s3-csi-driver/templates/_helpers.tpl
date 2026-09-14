{{/* vim: set filetype=mustache: */}}
{{/*
Expand the name of the chart.
*/}}
{{- define "aws-mountpoint-s3-csi-driver.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "csiDriverImageName" -}}
{{ include "renderImageName" (dict "image" .Values.image "eksImage" "/eks/aws-s3-csi-driver" "isEKSAddon" .Values.isEKSAddon ) }}
{{- end -}}

{{- define "nodeDriverRegistrarImageName" -}}
{{ include "renderImageName" (dict "image" .Values.sidecars.nodeDriverRegistrar.image "eksImage" "/eks/csi-node-driver-registrar" "isEKSAddon" .Values.isEKSAddon ) }}
{{- end -}}

{{- define "livenessProbeImageName" -}}
{{ include "renderImageName" (dict "image" .Values.sidecars.livenessProbe.image "eksImage" "/eks/livenessprobe" "isEKSAddon" .Values.isEKSAddon ) }}
{{- end -}}

{{- define "renderImageName" -}}
{{ printf "%s%s:%s" (default "" .image.containerRegistry) (ternary .image.repository .eksImage (empty .isEKSAddon)) .image.tag }}
{{- end -}}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "aws-mountpoint-s3-csi-driver.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "aws-mountpoint-s3-csi-driver.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels
*/}}
{{- define "aws-mountpoint-s3-csi-driver.labels" -}}
{{ include "aws-mountpoint-s3-csi-driver.selectorLabels" . }}
{{- if ne .Release.Name "kustomize" }}
{{- if empty .Values.isEKSAddon }}
helm.sh/chart: {{ include "aws-mountpoint-s3-csi-driver.chart" . }}
{{- end }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/component: csi-driver
app.kubernetes.io/managed-by: {{ ternary .Release.Service "EKS" (empty .Values.isEKSAddon) }}
{{- end }}
{{- if .Values.customLabels }}
{{ toYaml .Values.customLabels }}
{{- end }}
{{- end -}}

{{/*
Common selector labels
*/}}
{{- define "aws-mountpoint-s3-csi-driver.selectorLabels" -}}
app.kubernetes.io/name: {{ include "aws-mountpoint-s3-csi-driver.name" . }}
{{- if ne .Release.Name "kustomize" }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
{{- end -}}

{{/*
Convert the `--extra-tags` command line arg from a map.
*/}}
{{- define "aws-mountpoint-s3-csi-driver.extra-volume-tags" -}}
{{- $result := dict "pairs" (list) -}}
{{- range $key, $value := .Values.controller.extraVolumeTags -}}
{{- $noop := printf "%s=%v" $key $value | append $result.pairs | set $result "pairs" -}}
{{- end -}}
{{- if gt (len $result.pairs) 0 -}}
{{- printf "- \"--extra-tags=%s\"" (join "," $result.pairs) -}}
{{- end -}}
{{- end -}}

{{/*
Determine if running on OpenShift (incl. ROSA)
*/}}
{{- define "aws-mountpoint-s3-csi-driver.isOpenShift" -}}
{{- $isOpenShift := .Values.isOpenShift -}}
{{- if eq $isOpenShift nil -}}
{{- $isOpenShift = .Capabilities.APIVersions.Has "security.openshift.io/v1/SecurityContextConstraints" -}}
{{- end -}}
{{- $isOpenShift -}}
{{- end -}}

{{/*
Best-effort parse of a Kubernetes memory quantity (e.g. "2Gi", "500M", "1.5Gi") into a byte count.
Returns "" for forms it does not understand, so callers can skip rather than misjudge.
*/}}
{{- define "aws-mountpoint-s3-csi-driver.memoryQuantityBytes" -}}
{{- $value := toString . -}}
{{- if regexMatch `^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E)?$` $value -}}
{{- $suffix := regexFind `[A-Za-z]+$` $value -}}
{{- $number := trimSuffix $suffix $value | float64 -}}
{{- $multiplier := 1.0 -}}
{{- if $suffix -}}
{{- $multiplier = get (dict
      "Ki" 1024.0 "Mi" 1048576.0 "Gi" 1073741824.0
      "Ti" 1099511627776.0 "Pi" 1125899906842624.0 "Ei" 1152921504606846976.0
      "k" 1000.0 "M" 1000000.0 "G" 1000000000.0
      "T" 1000000000000.0 "P" 1000000000000000.0 "E" 1000000000000000000.0) $suffix -}}
{{- end -}}
{{- printf "%.0f" (mulf $number $multiplier) -}}
{{- end -}}
{{- end -}}
