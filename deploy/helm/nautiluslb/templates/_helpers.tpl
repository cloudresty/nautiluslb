{{/* Chart name. */}}
{{- define "nautiluslb.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified app name. */}}
{{- define "nautiluslb.fullname" -}}
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

{{/*
Name prefix for RBAC objects (ClusterRoles and Roles in other namespaces).
Matches deploy/kubernetes (nautiluslb-nodes, nautiluslb-discovery); two
releases with the same name in different namespaces need fullnameOverride.
*/}}
{{- define "nautiluslb.clusterName" -}}
{{- include "nautiluslb.fullname" . | trunc 50 | trimSuffix "-" }}
{{- end }}

{{- define "nautiluslb.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Selector labels (immutable on Deployments/DaemonSets: keep minimal). */}}
{{- define "nautiluslb.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nautiluslb.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* Standard labels. */}}
{{- define "nautiluslb.labels" -}}
helm.sh/chart: {{ include "nautiluslb.chart" . }}
{{ include "nautiluslb.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/part-of: nautiluslb
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "nautiluslb.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "nautiluslb.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "nautiluslb.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion | toString -}}
{{- if .Values.image.digest -}}
{{- printf "%s:%s@%s" .Values.image.repository $tag .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
{{- end }}

{{/*
Validate the parts of values.config the chart depends on. NautilusLB itself
validates everything else at startup (and with --validate).
*/}}
{{- define "nautiluslb.validate" -}}
{{- $c := .Values.config | default dict -}}
{{- if ne (toString $c.apiVersion) "nautiluslb.cloudresty.io/v1" -}}
{{- fail (printf "config.apiVersion must be \"nautiluslb.cloudresty.io/v1\", got %q (see docs/upgrading-v1.md)" (toString $c.apiVersion)) -}}
{{- end -}}
{{- if ne (toString $c.kind) "Config" -}}
{{- fail (printf "config.kind must be \"Config\", got %q" (toString $c.kind)) -}}
{{- end -}}
{{- if not $c.configurations -}}
{{- fail "config.configurations must contain at least one entry" -}}
{{- end -}}
{{- range $i, $cf := $c.configurations -}}
{{- if not (or $cf.namespaces $cf.namespace) -}}
{{- fail (printf "config.configurations[%d] (%v): namespaces is required (use [\"*\"] for cluster-wide)" $i $cf.name) -}}
{{- end -}}
{{- end -}}
{{- if not (has .Values.mode (list "Deployment" "DaemonSet")) -}}
{{- fail (printf "mode must be Deployment or DaemonSet, got %q" (toString .Values.mode)) -}}
{{- end -}}
{{- if not (has .Values.privilegedPortsMode (list "sysctl" "root")) -}}
{{- fail (printf "privilegedPortsMode must be sysctl or root, got %q" (toString .Values.privilegedPortsMode)) -}}
{{- end -}}
{{- if not (has .Values.configReload (list "watchFile" "restart")) -}}
{{- fail (printf "configReload must be watchFile or restart, got %q" (toString .Values.configReload)) -}}
{{- end -}}
{{- end }}

{{/*
The rendered NautilusLB configuration: values.config with the chart-managed
settings (admin address, reload mode) applied.
*/}}
{{- define "nautiluslb.config" -}}
{{- $c := deepCopy (.Values.config | default dict) -}}
{{- $settings := get $c "settings" | default dict -}}
{{- $admin := get $settings "admin" | default dict -}}
{{- $_ := set $admin "address" (printf "%s:%d" (toString .Values.admin.host) (int .Values.admin.port)) -}}
{{- $_ = set $settings "admin" $admin -}}
{{- $reload := get $settings "reload" | default dict -}}
{{- $_ = set $reload "watchFile" (eq .Values.configReload "watchFile") -}}
{{- $_ = set $settings "reload" $reload -}}
{{- $_ = set $c "settings" $settings -}}
{{- toYaml $c -}}
{{- end }}

{{/*
Every namespace named by any configuration (namespaces + deprecated
namespace), deduplicated and sorted, as a JSON list.
*/}}
{{- define "nautiluslb.namespaces" -}}
{{- $out := list -}}
{{- range (.Values.config.configurations | default list) -}}
{{- range (.namespaces | default list) -}}
{{- $out = append $out (toString .) -}}
{{- end -}}
{{- if .namespace -}}
{{- $out = append $out (toString .namespace) -}}
{{- end -}}
{{- end -}}
{{- $out | uniq | sortAlpha | toJson -}}
{{- end }}

{{/* "true" when any configuration is cluster-wide (namespaces contains "*"). */}}
{{- define "nautiluslb.clusterWide" -}}
{{- $ns := include "nautiluslb.namespaces" . | fromJsonArray -}}
{{- if has "*" $ns }}true{{ end -}}
{{- end }}

{{/*
Parse a Go duration ("30s", "1m30s", "500ms") or a bare number of seconds
into whole seconds, rounded up.
*/}}
{{- define "nautiluslb.seconds" -}}
{{- $v := . -}}
{{- if kindIs "string" $v -}}
{{- $total := 0.0 -}}
{{- $mult := dict "h" 3600.0 "m" 60.0 "s" 1.0 "ms" 0.001 "us" 0.000001 "µs" 0.000001 "ns" 0.000000001 -}}
{{- range regexFindAll "[0-9]+(\\.[0-9]+)?(ns|us|µs|ms|s|m|h)" $v -1 -}}
{{- $num := regexFind "^[0-9]+(\\.[0-9]+)?" . -}}
{{- $unit := trimPrefix $num . -}}
{{- $total = addf $total (mulf (float64 $num) (get $mult $unit)) -}}
{{- end -}}
{{- if and (eq $total 0.0) (regexMatch "^[0-9]+$" $v) -}}
{{- $total = float64 $v -}}
{{- end -}}
{{- ceil $total | int -}}
{{- else if $v -}}
{{- ceil (float64 $v) | int -}}
{{- else -}}
0
{{- end -}}
{{- end }}

{{/*
terminationGracePeriodSeconds: drain.timeout + readinessDelay + 15, each
duration rounded up to whole seconds first (1m30s + 500ms -> 90 + 1 + 15 = 106).
The 15s covers the work after the drain deadline: waiting for force-closed
connections (up to 5s), stopping pools and the watchdog, closing the access
log (2s) and the admin server (2s).
*/}}
{{- define "nautiluslb.terminationGrace" -}}
{{- if not (kindIs "invalid" .Values.terminationGracePeriodSeconds) -}}
{{- .Values.terminationGracePeriodSeconds | int -}}
{{- else -}}
{{- $drain := ((.Values.config.settings | default dict).drain | default dict) -}}
{{- $timeout := include "nautiluslb.seconds" ($drain.timeout | default "30s") | int -}}
{{- $delay := include "nautiluslb.seconds" ($drain.readinessDelay | default "3s") | int -}}
{{- add $timeout $delay 15 -}}
{{- end -}}
{{- end }}

{{/*
Listener ports as a JSON list of {name, port, targetPort, protocol, nodePort}:
values.listeners when set, otherwise derived from
config.configurations[].listenerAddress.
*/}}
{{- define "nautiluslb.listeners" -}}
{{- $out := list -}}
{{- if .Values.listeners -}}
{{- range .Values.listeners -}}
{{- $p := int .port -}}
{{- $l := dict "name" .name "port" $p "targetPort" (int (.targetPort | default $p)) "protocol" (.protocol | default "TCP" | upper) -}}
{{- if .nodePort }}{{ $_ := set $l "nodePort" (int .nodePort) }}{{ end -}}
{{- $out = append $out $l -}}
{{- end -}}
{{- else -}}
{{- $seen := dict -}}
{{- range (.Values.config.configurations | default list) -}}
{{- $addr := toString .listenerAddress -}}
{{- $port := regexFind "[0-9]+$" $addr -}}
{{- if $port -}}
{{- $proto := ternary "UDP" "TCP" (eq (toString .protocol) "udp") -}}
{{- $key := printf "%s-%s" (lower $proto) $port -}}
{{- if not (hasKey $seen $key) -}}
{{- $_ := set $seen $key true -}}
{{- $out = append $out (dict "name" $key "port" (int $port) "targetPort" (int $port) "protocol" $proto) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $out | toJson -}}
{{- end }}

{{/* "true" when the pod runs as root to bind privileged ports. */}}
{{- define "nautiluslb.rootMode" -}}
{{- if and .Values.bindPrivilegedPorts (eq .Values.privilegedPortsMode "root") }}true{{ end -}}
{{- end }}
