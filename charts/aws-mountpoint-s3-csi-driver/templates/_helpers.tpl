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
Best-effort parse of a Kubernetes quantity (e.g. "2Gi", "500M", "1.5Gi") into a byte count.
Returns "" for forms it does not understand, so callers can skip rather than misjudge.
Suffixes follow the quantity spec at https://kubernetes.io/docs/reference/kubernetes-api/common-definitions/quantity/
*/}}
{{- define "aws-mountpoint-s3-csi-driver.quantityBytes" -}}
{{- $value := toString . -}}
{{- if regexMatch `^[0-9]+(\.[0-9]+)?([eE]\+?[0-9]+|Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E)?$` $value -}}
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

{{/*
Checks the daemonsetMounters entries. Each entry becomes its own mounter DaemonSet, so names must not repeat, and with
two or more entries each must say which nodes it applies to.
*/}}
{{- define "aws-mountpoint-s3-csi-driver.validateMounterEntries" -}}
{{- $entries := .Values.daemonsetMounters }}
{{- if lt (len $entries) 1 }}
{{- fail "daemonsetMounters must have at least one entry" }}
{{- end }}
{{- $names := list }}
{{- $selections := list }}
{{- $unnamed := 0 }}
{{- range $m := $entries }}
{{- if lt (int $m.maxVolumesPerNode) 0 }}
{{- fail "daemonsetMounters[].maxVolumesPerNode must be >= 0" }}
{{- end }}
{{- if $m.name }}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $m.name) }}
{{- fail (printf "daemonsetMounters: name %q must be lowercase letters, numbers and '-', and start and end with a letter or number, since it names the entry's DaemonSet" $m.name) }}
{{- end }}
{{- if has $m.name $names }}
{{- fail (printf "daemonsetMounters: two entries are named %q; each entry needs its own name" $m.name) }}
{{- end }}
{{- $names = append $names $m.name }}
{{- else }}
{{- $unnamed = add1 $unnamed }}
{{- end }}
{{- if and (gt (len $entries) 1) (not $m.nodeSelector) (not $m.matchExpressions) }}
{{- fail (printf "daemonsetMounters[%s]: nodeSelector or matchExpressions is required when there is more than one entry, or its mounter would run on every node" ($m.name | default "")) }}
{{- end }}
{{- $selection := toJson (dict "nodeSelector" $m.nodeSelector "matchExpressions" $m.matchExpressions) }}
{{- if has $selection $selections }}
{{- fail (printf "daemonsetMounters: two entries select the same nodes with %s" $selection) }}
{{- end }}
{{- $selections = append $selections $selection }}
{{- range $k, $_ := $m.nodeSelector }}
{{- if hasKey ($.Values.node.nodeSelector | default dict) $k }}
{{- fail (printf "daemonsetMounters[%s].nodeSelector and node.nodeSelector both set %q" ($m.name | default "") $k) }}
{{- end }}
{{- end }}
{{- end }}
{{- if gt $unnamed 1 }}
{{- fail "daemonsetMounters: at most one entry may leave out name" }}
{{- end }}
{{- end }}

{{/*
Returns .affinity with each required node term repeated once per list in .exprLists, with that list's expressions
added. Kubernetes ORs the terms, so the result matches a node if any list does. Other rules are kept as they are.
With no lists, .affinity is returned unchanged.
*/}}
{{- define "aws-mountpoint-s3-csi-driver.addToNodeTerms" -}}
{{- $affinity := deepCopy (.affinity | default dict) }}
{{- if .exprLists }}
{{- $nodeAffinity := $affinity.nodeAffinity | default dict }}
{{- $required := $nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution | default dict }}
{{- $nodeTerms := $required.nodeSelectorTerms | default (list (dict)) }}
{{- $terms := list }}
{{- range $n := $nodeTerms }}
{{- range $exprs := $.exprLists }}
{{- $term := dict "matchExpressions" (concat ($n.matchExpressions | default list) $exprs) }}
{{- with $n.matchFields }}{{- $_ := set $term "matchFields" . }}{{- end }}
{{- $terms = append $terms $term }}
{{- end }}
{{- end }}
{{- $_ := set $required "nodeSelectorTerms" $terms }}
{{- $_ := set $nodeAffinity "requiredDuringSchedulingIgnoredDuringExecution" $required }}
{{- $_ := set $affinity "nodeAffinity" $nodeAffinity }}
{{- end }}
{{- toYaml $affinity }}
{{- end }}
