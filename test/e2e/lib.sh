#!/usr/bin/env bash
# shellcheck shell=bash
#
# Shared helpers for the NautilusLB end-to-end test. Sourced by run.sh; every
# case in cases/*.sh runs in a subshell with these functions available.
#
# Topology: one kind cluster with the fixtures from manifests.yaml (all
# NodePort Services), NautilusLB as a plain Docker container on the "kind"
# network (outside the cluster, the way it is deployed in production) using a
# token for the least-privilege ServiceAccount from rbac.yaml, and one long-lived
# client container (netshoot: curl, openssl, socat, nc) on the same network that
# every case drives traffic from. All traffic uses IPv4 addresses so the client
# address NautilusLB sees is predictable.

# Pipelines into grep never use -q: with pipefail, grep -q exiting at the
# first match SIGPIPEs the writer and the pipeline reports failure.
#
# Never touch the caller's kube context: every kubectl/kind call below names
# its kubeconfig explicitly.

ROOT="${NLB_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
E2E_DIR="${E2E_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"

CLUSTER="${CLUSTER_NAME:-nautiluslb-e2e}"
KEEP="${KEEP:-0}"
IMAGE="${IMAGE:-nautiluslb:e2e}"
SETTLE="${SETTLE:-15}"
NODE="${CLUSTER}-control-plane"
CLIENT="nautiluslb-e2e-client"
NLB_PREFIX="nautiluslb-e2e"

CLIENT_IMAGE="nicolaka/netshoot:v0.16@sha256:b09d9b21381f47a79b3cbcb30da25266dc17186ea00ae65e99fdc51396f48e70"
export LOAD_IMAGE="fortio/fortio:1.75.3@sha256:942c86131c24ee9bd4a7754610be417ac1b9fe2f11d1860620a9f6932ceb712f"

# The version baked into the image; the metrics case asserts build_info carries it.
E2E_VERSION="${VERSION:-$(git -C "${ROOT}" describe --tags --always --dirty 2>/dev/null || echo dev)}"
E2E_COMMIT="${COMMIT:-$(git -C "${ROOT}" rev-parse --short HEAD 2>/dev/null || echo unknown)}"

# Every case name; the containers they start are named ${NLB_PREFIX}-<case>.
ALL_CASES=(strict rbac binding metrics balancing proxy sni udp reload drain)

log()  { printf '\n==> %s\n' "$*"; }
ok()   { printf '  ok: %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"; }

k() { kubectl --kubeconfig "${ADMIN_KUBECONFIG}" --context "kind-${CLUSTER}" "$@"; }

# cexec CMD... runs a command in the client container.
cexec() { docker exec "${CLIENT}" "$@"; }
# csh SCRIPT runs a shell snippet in the client container.
csh() { docker exec "${CLIENT}" sh -c "$1"; }

# init_work creates the work directory (mounted into containers, so readable
# by the image's UID 65532).
init_work() {
	WORK="$(mktemp -d "${TMPDIR:-/tmp}/nautiluslb-e2e.XXXXXX")"
	chmod 755 "${WORK}"
	ADMIN_KUBECONFIG="${WORK}/admin.kubeconfig"
	export WORK ADMIN_KUBECONFIG
	: >"${WORK}/containers"
}

# container_ip NAME prints the container's IPv4 address on the kind network.
container_ip() {
	docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' "$1"
}

remove_stale_containers() {
	local names=("${CLIENT}" "${NLB_PREFIX}" "${NLB_PREFIX}-strict" "${NLB_PREFIX}-soak" "${NLB_PREFIX}-load")
	local c
	for c in "${ALL_CASES[@]}"; do names+=("${NLB_PREFIX}-${c}"); done
	docker rm -f "${names[@]}" >/dev/null 2>&1 || true
}

#
# Cluster, fixtures, image
#

setup_cluster() {
	if kind get clusters 2>/dev/null | grep -x "${CLUSTER}" >/dev/null; then
		if [[ "${KEEP}" == "1" ]]; then
			log "Reusing kind cluster ${CLUSTER}"
		else
			log "Deleting stale kind cluster ${CLUSTER}"
			kind delete cluster --name "${CLUSTER}" --kubeconfig "${ADMIN_KUBECONFIG}"
		fi
	fi
	if ! kind get clusters 2>/dev/null | grep -x "${CLUSTER}" >/dev/null; then
		log "Creating kind cluster ${CLUSTER}"
		local node_image_args=()
		if [[ -n "${KIND_NODE_IMAGE:-}" ]]; then
			node_image_args=(--image "${KIND_NODE_IMAGE}")
		fi
		kind create cluster --name "${CLUSTER}" --kubeconfig "${ADMIN_KUBECONFIG}" \
			--wait 120s ${node_image_args[@]+"${node_image_args[@]}"}
	fi
	kind get kubeconfig --name "${CLUSTER}" >"${ADMIN_KUBECONFIG}"
	kind get kubeconfig --name "${CLUSTER}" --internal >"${WORK}/admin-internal.kubeconfig"
	NODE_IP="$(k get node "${NODE}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"
	export NODE_IP
}

# make_tls_secrets creates the self-signed certificates of the sni fixtures.
make_tls_secrets() {
	local host
	mkdir -p "${WORK}/tls"
	for host in a b; do
		openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 \
			-subj "/CN=${host}.e2e.test" -addext "subjectAltName=DNS:${host}.e2e.test" \
			-keyout "${WORK}/tls/${host}.key" -out "${WORK}/tls/${host}.crt" 2>/dev/null
		k create secret tls "tls-${host}" -n e2e-apps \
			--cert "${WORK}/tls/${host}.crt" --key "${WORK}/tls/${host}.key" \
			--dry-run=client -o yaml | k apply -f - >/dev/null
	done
}

apply_fixtures() {
	log "Applying RBAC and fixtures"
	k apply -f "${E2E_DIR}/rbac.yaml" >/dev/null
	k apply -f "${E2E_DIR}/manifests.yaml" >/dev/null
	make_tls_secrets
}

# make_sa_kubeconfig writes ${WORK}/kubeconfig: a token for the nautiluslb
# ServiceAccount, never the admin credentials.
make_sa_kubeconfig() {
	local server ca token
	server="$(kubectl --kubeconfig "${WORK}/admin-internal.kubeconfig" config view --raw -o jsonpath='{.clusters[0].cluster.server}')"
	ca="$(kubectl --kubeconfig "${WORK}/admin-internal.kubeconfig" config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
	token="$(k create token nautiluslb -n nautiluslb --duration=2h)"
	cat >"${WORK}/kubeconfig" <<EOF
apiVersion: v1
kind: Config
clusters:
  - name: e2e
    cluster:
      server: ${server}
      certificate-authority-data: ${ca}
users:
  - name: nautiluslb
    user:
      token: ${token}
contexts:
  - name: nautiluslb@e2e
    context:
      cluster: e2e
      user: nautiluslb
current-context: nautiluslb@e2e
EOF
	chmod 644 "${WORK}/kubeconfig"
}

build_image() {
	log "Building ${IMAGE} (version ${E2E_VERSION})"
	docker build --quiet \
		--build-arg "VERSION=${E2E_VERSION}" \
		--build-arg "COMMIT=${E2E_COMMIT}" \
		--build-arg "BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
		-f "${ROOT}/build/Dockerfile" -t "${IMAGE}" "${ROOT}" >/dev/null
}

start_client() {
	docker rm -f "${CLIENT}" >/dev/null 2>&1 || true
	docker image inspect "${CLIENT_IMAGE}" >/dev/null 2>&1 || docker pull --quiet "${CLIENT_IMAGE}" >/dev/null
	docker run --detach --name "${CLIENT}" --network kind "${CLIENT_IMAGE}" sleep infinity >/dev/null
	CLIENT_IP="$(container_ip "${CLIENT}")"
	[[ -n "${CLIENT_IP}" ]] || fail "client container has no IPv4 address on the kind network"
	export CLIENT_IP
}

# node_port NS SVC PORTNAME
node_port() {
	k get svc "$2" -n "$1" -o jsonpath="{.spec.ports[?(@.name==\"$3\")].nodePort}"
}

# await_answer DESCRIPTION EXPECTED_REGEX CMD...: retry CMD (60s) until its
# output matches.
await_answer() {
	local what="$1" want="$2" got=""
	shift 2
	for _ in $(seq 1 30); do
		got="$("$@" 2>/dev/null || true)"
		[[ "${got}" =~ ${want} ]] && { ok "${what} (${got//$'\n'/ })"; return 0; }
		sleep 2
	done
	fail "${what}: no answer matching '${want}' (got '${got}')"
}

# wait_fixtures waits for every workload, then for every fixture to answer on
# NodeIP:NodePort.
#
# TRAP (found by mutation testing in v1.0.1): if NautilusLB starts while a
# wrongly bound backend's NodePort is not programmed yet, that backend fails its
# first dial, is ejected, and a hijack goes unnoticed for the few seconds the
# requests take. So no NautilusLB starts before every fixture answers here.
wait_fixtures() {
	log "Waiting for workloads"
	k wait --for=condition=available deployment --all -n e2e-apps --timeout=240s >/dev/null
	k wait --for=condition=available deployment --all -n e2e-other --timeout=240s >/dev/null
	k rollout status statefulset/lb -n e2e-apps --timeout=240s >/dev/null

	log "Checking every fixture answers on its NodePort (${NODE_IP})"
	local f ns name np
	# Plain HTTP fixtures, from inside the kind node.
	for f in e2e-apps/bound e2e-apps/intruder e2e-other/outsider e2e-apps/lb-0 e2e-apps/lb-1 e2e-apps/lb-2; do
		ns="${f%/*}" name="${f#*/}"
		np="$(node_port "${ns}" "${name}" http)"
		await_answer "${f} on NodePort ${np}" "^${name}\$" \
			docker exec "${NODE}" curl -s --max-time 2 "http://${NODE_IP}:${np}/"
	done
	np="$(node_port e2e-apps blob http)"
	await_answer "e2e-apps/blob on NodePort ${np}" '^6000000$' \
		docker exec "${NODE}" curl -s --max-time 5 -o /dev/null -w '%{size_download}' "http://${NODE_IP}:${np}/blob"
	# PROXY, TLS, TCP and UDP fixtures, from the client container.
	np="$(node_port e2e-apps ppecho pp)"
	await_answer "e2e-apps/ppecho on NodePort ${np}" "^${CLIENT_IP}\$" \
		cexec curl -s --max-time 2 --haproxy-protocol "http://${NODE_IP}:${np}/"
	for name in a b; do
		np="$(node_port e2e-apps "tls-${name}" https)"
		await_answer "e2e-apps/tls-${name} on NodePort ${np}" "^${name}\$" \
			cexec curl -sk --max-time 2 "https://${NODE_IP}:${np}/"
	done
	np="$(node_port e2e-apps echo tcp-echo)"
	await_answer "e2e-apps/echo tcp on NodePort ${np}" '^ping$' \
		csh "echo ping | socat -t2 - TCP4:${NODE_IP}:${np}"
	np="$(node_port e2e-apps echo udp-echo)"
	await_answer "e2e-apps/echo udp on NodePort ${np}" '^ping$' \
		csh "echo ping | socat -t2 - UDP4:${NODE_IP}:${np}"
}

#
# NautilusLB
#

# nlb_config_header [pprof] prints the common v2 header. Cases append their
# `configurations:` entries.
nlb_config_header() {
	local pprof="false"
	[[ "${1:-}" == "pprof" ]] && pprof="true"
	cat <<EOF
apiVersion: nautiluslb.cloudresty.io/v2
kind: Config
settings:
  logLevel: info
  kubernetes:
    kubeconfig: /nautiluslb/kubeconfig
  admin:
    address: "0.0.0.0:9090"
    pprof: ${pprof}
configurations:
EOF
}

# case_dir NAME creates and prints a config directory for one NautilusLB. The
# directory (not the file) is mounted, so in-place edits for reload are seen.
case_dir() {
	local d="${WORK}/$1"
	mkdir -p "${d}"
	chmod 755 "${d}"
	printf '%s\n' "${d}"
}

# nlb_start NAME CONFIG_DIR [docker run args...] starts NautilusLB with
# CONFIG_DIR/config.yaml. The admin port is published on a random loopback
# port (`docker port NAME 9090`) for debugging with KEEP=1.
nlb_start() {
	local name="$1" dir="$2"
	shift 2
	chmod 644 "${dir}"/*.yaml
	docker rm -f "${name}" >/dev/null 2>&1 || true
	printf '%s\n' "${name}" >>"${WORK}/containers"
	docker run --detach \
		--name "${name}" \
		--network kind \
		--publish "127.0.0.1::9090" \
		--volume "${dir}:/nautiluslb/conf:ro" \
		--volume "${WORK}/kubeconfig:/nautiluslb/kubeconfig:ro" \
		"$@" \
		"${IMAGE}" --config /nautiluslb/conf/config.yaml >/dev/null
}

nlb_running() { [[ "$(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null)" == "true" ]]; }

# nlb_wait_ready NAME [TIMEOUT]: wait for /readyz 200 (discovery synced).
nlb_wait_ready() {
	local name="$1" timeout="${2:-90}" ip code="" start=${SECONDS}
	ip="$(container_ip "${name}")"
	while (( SECONDS - start < timeout )); do
		nlb_running "${name}" || fail "${name} exited before becoming ready"
		code="$(cexec curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://${ip}:9090/readyz" || true)"
		if [[ "${code}" == "200" ]]; then
			ok "${name} ready after $((SECONDS - start))s"
			return 0
		fi
		sleep 1
	done
	fail "${name} not ready within ${timeout}s (last /readyz ${code})"
}

# scrape NAME FILE: save /metrics.
scrape() {
	cexec curl -sS --max-time 5 "http://$(container_ip "$1"):9090/metrics" >"$2"
}

# metric FILE NAME [label=value...]: sum of the samples of NAME whose labels
# include every label=value given; "absent" when none matches.
metric() {
	local file="$1" name="$2"
	shift 2
	awk -v name="${name}" -v want="$*" '
		BEGIN { n = split(want, w, " ") }
		/^#/ { next }
		{
			series = $1; val = $2
			i = index(series, "{")
			mname = i ? substr(series, 1, i - 1) : series
			if (mname != name) next
			for (j = 1; j <= n; j++) {
				eq = index(w[j], "=")
				pat = substr(w[j], 1, eq - 1) "=\"" substr(w[j], eq + 1) "\""
				if (index(series, "{" pat) == 0 && index(series, "," pat) == 0) next
			}
			sum += val; found = 1
		}
		END { if (found) printf "%.0f\n", sum; else print "absent" }
	' "${file}"
}

# expect_metric FILE OP WANT NAME [label=value...]; OP is eq or ge.
expect_metric() {
	local file="$1" op="$2" want="$3" name="$4" got
	shift 4
	got="$(metric "${file}" "${name}" "$@")"
	local desc="${name}{$*}"
	[[ "${got}" != "absent" ]] || fail "${desc} absent from /metrics"
	case "${op}" in
		eq) (( got == want )) || fail "${desc} = ${got}, want ${want}" ;;
		ge) (( got >= want )) || fail "${desc} = ${got}, want >= ${want}" ;;
		*) fail "expect_metric: bad op ${op}" ;;
	esac
	ok "${desc} = ${got}"
}

# expect_log NAME TEXT: the container logged TEXT (fixed string).
expect_log() {
	docker logs "$1" 2>&1 | grep -F -- "$2" >/dev/null || fail "$1 did not log '$2'"
	ok "$1 logged '$2'"
}

# expect_clean_log NAME: no panic or RBAC denial in the container's log.
expect_clean_log() {
	if docker logs "$1" 2>&1 | grep -iE 'panic|forbidden' >/dev/null; then
		docker logs "$1" 2>&1 | grep -iE 'panic|forbidden' | tail -n 5 >&2
		fail "$1 logged a panic or an RBAC denial"
	fi
}

#
# Diagnostics and cleanup
#

dump_container_logs() {
	local name
	[[ -s "${WORK}/containers" ]] || return 0
	sort -u "${WORK}/containers" | while read -r name; do
		docker inspect "${name}" >/dev/null 2>&1 || continue
		printf '\n==> logs of %s (last 150 lines)\n' "${name}" >&2
		docker logs "${name}" 2>&1 | tail -n 150 >&2
	done
}

dump_cluster_state() {
	[[ -s "${ADMIN_KUBECONFIG:-}" ]] || return 0
	printf '\n==> Cluster state\n' >&2
	k get nodes -o wide >&2 || true
	k get svc,pods -n e2e-apps -o wide >&2 || true
	k get svc,pods -n e2e-other -o wide >&2 || true
}

# remove_case_containers removes the NautilusLB containers started so far.
remove_case_containers() {
	local name
	[[ -s "${WORK}/containers" ]] || return 0
	sort -u "${WORK}/containers" | while read -r name; do
		docker rm -f "${name}" >/dev/null 2>&1 || true
	done
	: >"${WORK}/containers"
}

cleanup() {
	local rc=$?
	set +e
	if [[ ${rc} -ne 0 ]]; then
		dump_container_logs
		dump_cluster_state
	fi
	if [[ "${KEEP}" == "1" ]]; then
		printf '\nKEEP=1: kept cluster "%s", containers and work dir %s\n' "${CLUSTER}" "${WORK}"
		printf '  kubectl --kubeconfig %s get svc -A\n' "${ADMIN_KUBECONFIG}"
	else
		remove_case_containers
		docker rm -f "${CLIENT}" "${NLB_PREFIX}-load" >/dev/null 2>&1
		kind delete cluster --name "${CLUSTER}" --kubeconfig "${ADMIN_KUBECONFIG}" >/dev/null 2>&1
		rm -rf "${WORK}"
	fi
	exit "${rc}"
}
