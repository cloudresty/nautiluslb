#!/usr/bin/env bash
# shellcheck shell=bash
#
# Helpers for the NautilusLB performance suite (run.sh). Reuses the e2e
# harness (test/e2e/lib.sh: kind cluster, image build, SA-token kubeconfig,
# readiness and metric helpers) with its own cluster and container names, so
# it never collides with an e2e run, and never touches the caller's kube
# context: every kubectl/kind call names its kubeconfig explicitly.

PERF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export CLUSTER_NAME="${CLUSTER_NAME:-nautiluslb-perf}"
export IMAGE="${IMAGE:-nautiluslb:perf}"

# shellcheck source=test/e2e/lib.sh
source "${PERF_DIR}/../e2e/lib.sh"

# Override the e2e container names (lib.sh assigns them unconditionally).
CLIENT="nautiluslb-perf-client"
NLB_PREFIX="nautiluslb-perf"
LOAD="${NLB_PREFIX}-load"

V1_IMAGE="cloudresty/nautiluslb:v0.0.11@sha256:961e9497ee5f5a6a6dd63585c2b817448f5a1ef35b08f59222778a53bc9480cf"
HAPROXY_IMAGE="haproxy:3.2@sha256:e2b397cdbd612221cf79dec325690139d00a78b4ce4eae065530fbc81e1f6192"
IPERF_IMAGE="networkstatic/iperf3:latest@sha256:bc267517f534d9f7cced2669f325799a1fd7fe1d5953c2ca2c6ea1db61affa08"
export HOLDER_IMAGE="gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3"

# Same limits for every proxy and client container.
NET_OPTS=(--network kind --ulimit nofile=1048576:1048576
	--sysctl "net.ipv4.ip_local_port_range=1024 65535"
	--sysctl net.ipv4.tcp_tw_reuse=1
	--sysctl net.core.somaxconn=65535)

# A stable work dir so KEEP=1 runs can be resumed phase by phase.
init_perf_work() {
	WORK="${PERF_WORK:-${TMPDIR:-/tmp}/nautiluslb-perf}"
	mkdir -p "${WORK}"
	chmod 755 "${WORK}"
	ADMIN_KUBECONFIG="${WORK}/admin.kubeconfig"
	OUT="${OUT:-${WORK}/results}"
	mkdir -p "${OUT}"
	export WORK ADMIN_KUBECONFIG OUT
	touch "${WORK}/containers" "${OUT}/results.tsv"
}

# record TARGET TEST METRIC VALUE appends one result row.
record() {
	printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$4" >>"${OUT}/results.tsv"
	printf '  %-10s %-22s %-18s %s\n' "$1" "$2" "$3" "$4"
}

# with_timeout SECONDS CMD...: Docker Desktop has hung on us; never wait forever.
with_timeout() { perl -e 'alarm shift; exec @ARGV' "$@"; }

docker_alive() { with_timeout 20 docker ps >/dev/null 2>&1; }

apply_perf_fixtures() {
	log "Applying perf fixtures"
	k apply -f "${PERF_DIR}/manifests.yaml" >/dev/null
	k wait --for=condition=available deployment --all -n perf-apps --timeout=300s >/dev/null
	NP_HTTP="$(node_port perf-apps web http)"
	NP_IPERF="$(node_port perf-apps iperf iperf)"
	export NP_HTTP NP_IPERF
	await_answer "web on NodePort ${NP_HTTP}" '^64$' \
		docker exec "${NODE}" curl -s --max-time 2 -o /dev/null -w '%{size_download}' "http://${NODE_IP}:${NP_HTTP}/small"
	await_answer "iperf on NodePort ${NP_IPERF}" 'open' \
		docker exec "${NODE}" sh -c "timeout 2 bash -c '</dev/tcp/${NODE_IP}/${NP_IPERF}' && echo open"
}

# load_env restores NODE_IP and NodePorts in a resumed (KEEP=1) run.
load_env() {
	NODE_IP="$(k get node "${NODE}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"
	NP_HTTP="$(node_port perf-apps web http)"
	NP_IPERF="$(node_port perf-apps iperf iperf)"
	CLIENT_IP="$(container_ip "${CLIENT}")"
	export NODE_IP NP_HTTP NP_IPERF CLIENT_IP
}

#
# Proxies under test. Only one runs at a time so they never compete for CPU.
#

PROXIES=("${NLB_PREFIX}-v2" "${NLB_PREFIX}-v1" "${NLB_PREFIX}-haproxy" "${NLB_PREFIX}-haproxy-splice")

stop_proxies() { docker rm -f "${PROXIES[@]}" >/dev/null 2>&1 || true; }

# start_target TARGET [alog=on|off]: starts the proxy for TARGET (none for
# direct) and waits until a request through it succeeds.
start_target() {
	local t="$1" alog="${2:-on}"
	stop_proxies
	case "${t}" in
		direct) return 0 ;;
		v2) start_v2 "${alog}" ;;
		v1) start_v1 ;;
		haproxy) start_haproxy haproxy "" ;;
		haproxy-splice) start_haproxy haproxy-splice "    option splice-auto" ;;
		*) fail "unknown target ${t}" ;;
	esac
	await_answer "${t}: request through the proxy" '^64$' \
		cexec curl -s --max-time 2 -o /dev/null -w '%{size_download}' "http://$(addr "${t}" http)/small"
	# No probe of the iperf port: iperf3's server serves one client at a time,
	# and v1 does not forward a client's FIN (fixed in v1.0.1), so a probe
	# connection through v1 would leave the server busy with a dead test.
}

# addr TARGET http|iperf prints host:port.
addr() {
	local t="$1" kind="$2" np port
	if [[ "${kind}" == "http" ]]; then np="${NP_HTTP}" port=8080; else np="${NP_IPERF}" port=5201; fi
	case "${t}" in
		direct) printf '%s:%s\n' "${NODE_IP}" "${np}" ;;
		*) printf '%s:%s\n' "$(container_ip "${NLB_PREFIX}-${t}")" "${port}" ;;
	esac
}

start_v2() {
	local alog="$1" enabled=true dir
	[[ "${alog}" == "off" ]] && enabled=false
	dir="$(case_dir "v2-${alog}")"
	cat >"${dir}/config.yaml" <<EOF
apiVersion: nautiluslb.cloudresty.io/v2
kind: Config
settings:
  logLevel: info
  kubernetes:
    kubeconfig: /nautiluslb/kubeconfig
  admin:
    address: "0.0.0.0:9090"
    pprof: true
  accessLog:
    enabled: ${enabled}
configurations:
  - name: perf_http
    listenerAddress: ":8080"
    backendPortName: http
    namespaces: [perf-apps]
  - name: perf_iperf
    listenerAddress: ":5201"
    backendPortName: iperf
    namespaces: [perf-apps]
EOF
	# V2_DOCKER_ARGS: extra docker run args, e.g. "--user 0" (see README, pipes).
	# shellcheck disable=SC2086
	nlb_start "${NLB_PREFIX}-v2" "${dir}" "${NET_OPTS[@]:2}" ${V2_DOCKER_ARGS:-}
	nlb_wait_ready "${NLB_PREFIX}-v2"
}

start_v1() {
	local dir
	dir="$(case_dir v1)"
	# v1 schema: no apiVersion, settings.kubeconfigPath, one namespace per
	# configuration, read from ./config.yaml (WORKDIR /nautiluslb).
	cat >"${dir}/config.yaml" <<EOF
settings:
  kubeconfigPath: /nautiluslb/kubeconfig
configurations:
  - name: perf_http
    listenerAddress: ":8080"
    backendPortName: http
    namespace: perf-apps
  - name: perf_iperf
    listenerAddress: ":5201"
    backendPortName: iperf
    namespace: perf-apps
EOF
	chmod 644 "${dir}/config.yaml"
	printf '%s\n' "${NLB_PREFIX}-v1" >>"${WORK}/containers"
	docker run --detach --name "${NLB_PREFIX}-v1" "${NET_OPTS[@]}" \
		--volume "${dir}/config.yaml:/nautiluslb/config.yaml:ro" \
		--volume "${WORK}/kubeconfig:/nautiluslb/kubeconfig:ro" \
		"${V1_IMAGE}" >/dev/null
}

# start_haproxy NAME EXTRA_DEFAULTS_LINE
start_haproxy() {
	local name="${NLB_PREFIX}-$1" extra="$2" dir
	dir="$(case_dir "$1")"
	# mode tcp, static servers (the same NodePorts NautilusLB discovers),
	# defaults otherwise; nbthread defaults to the CPUs available.
	cat >"${dir}/haproxy.cfg" <<EOF
global
    maxconn 200000
    log stdout format raw local0 err
defaults
    mode tcp
    timeout connect 5s
    timeout client 1h
    timeout server 1h
    maxconn 200000
${extra}
frontend http
    bind :8080
    default_backend http
backend http
    server node ${NODE_IP}:${NP_HTTP} check
frontend iperf
    bind :5201
    default_backend iperf
backend iperf
    server node ${NODE_IP}:${NP_IPERF} check
EOF
	chmod 644 "${dir}/haproxy.cfg"
	printf '%s\n' "${name}" >>"${WORK}/containers"
	docker run --detach --name "${name}" "${NET_OPTS[@]}" \
		--volume "${dir}:/usr/local/etc/haproxy:ro" \
		"${HAPROXY_IMAGE}" >/dev/null
}

#
# Load generators
#

# iperf_run TARGET STREAMS SECONDS -> prints JSON
iperf_run() {
	local a
	a="$(addr "$1" iperf)"
	with_timeout $(($3 + 60)) docker run --rm "${NET_OPTS[@]}" "${IPERF_IMAGE}" \
		-c "${a%:*}" -p "${a#*:}" -t "$3" -P "$2" -J
}

# fortio_run NAME ARGS... (URL last) -> JSON in ${OUT}/fortio-NAME.json
fortio_run() {
	local name="$1"
	shift
	docker rm -f "${LOAD}" >/dev/null 2>&1 || true
	with_timeout 1200 docker run --rm --name "${LOAD}" "${NET_OPTS[@]}" "${LOAD_IMAGE}" \
		load -json - -p "50,90,99,99.9" -timeout 5s -allow-initial-errors "$@" \
		>"${OUT}/fortio-${name}.json" 2>"${OUT}/fortio-${name}.log"
}

# fortio_summary FILE prints "qps p50ms p99ms p999ms ok total errors sockets".
fortio_summary() {
	jq -r '
		def pct(p): ((.DurationHistogram.Percentiles // []) | map(select(.Percentile == p)) | .[0].Value // 0) * 1000;
		[ (.ActualQPS | floor),
		  (pct(50) | . * 1000 | round / 1000),
		  (pct(99) | . * 1000 | round / 1000),
		  (pct(99.9) | . * 1000 | round / 1000),
		  (.RetCodes["200"] // 0),
		  .DurationHistogram.Count,
		  (.DurationHistogram.Count - (.RetCodes["200"] // 0)),
		  ((.Sockets // []) | add // 0) ] | @tsv' "$1"
}

#
# Process observation
#

# metrics_of NAME > file: /metrics of a v2 container.
metrics_of() { cexec curl -sS --max-time 5 "http://$(container_ip "$1"):9090/metrics"; }

goroutines_of() {
	cexec curl -sS --max-time 10 "http://$(container_ip "$1"):9090/debug/pprof/goroutine?debug=1" |
		sed -n '1s/.*total \([0-9]*\).*/\1/p'
}

# mem_mib NAME: container memory (cgroup, as docker stats reports it), MiB.
mem_mib() {
	docker stats --no-stream --format '{{.MemUsage}}' "$1" | awk '{
		v = $1; u = v; sub(/[0-9.]+/, "", u); sub(/[A-Za-z]+$/, "", v)
		f = (u == "GiB") ? 1024 : (u == "KiB" || u == "kB") ? 1 / 1024 : (u == "B") ? 1 / 1048576 : 1
		printf "%.1f\n", v * f }'
}

# proc_stat NAME: "rss_kib fds threads" of the container's main process,
# read from the VM's /proc through a helper in the host PID namespace.
proc_stat() {
	local pid
	pid="$(docker inspect -f '{{.State.Pid}}' "$1")"
	with_timeout 60 docker run --rm --pid host --privileged "${CLIENT_IMAGE}" sh -c "
		pid=${pid}
		# haproxy's master/worker: follow to the worker if there is one.
		c=\$(cat /proc/\${pid}/task/*/children 2>/dev/null | tr ' ' '\n' | grep -v '^\$' | head -n1)
		[ -n \"\${c}\" ] && pid=\${c}
		rss=\$(awk '/VmRSS/{print \$2}' /proc/\${pid}/status)
		thr=\$(awk '/Threads/{print \$2}' /proc/\${pid}/status)
		fds=\$(ls /proc/\${pid}/fd | wc -l)
		echo \"\${rss} \${fds} \${thr}\""
}
