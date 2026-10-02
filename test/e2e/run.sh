#!/usr/bin/env bash
#
# NautilusLB end-to-end test.
#
# Creates a kind cluster, deploys three annotated NodePort Services (see
# manifests.yaml), builds the NautilusLB image from this repository and runs it
# as a plain Docker container on the "kind" network, outside the cluster, the
# way it is deployed in production. NautilusLB authenticates with a token for
# the least-privilege ServiceAccount in rbac.yaml, proving the documented RBAC
# is sufficient.
#
# Asserts:
#   1. every response on the listener comes from the bound Service only
#      (no hijack by a Service naming another configuration, none from a
#      namespace outside the allowlist);
#   2. configurations that are invalid (unknown key, no namespaces) make the
#      container exit non-zero.
#
# Requirements: docker, kind, kubectl, curl.
#
# Environment:
#   KEEP=1          keep (and reuse, if it exists) the cluster and container
#   CLUSTER_NAME    kind cluster name           (default nautiluslb-e2e)
#   HOST_PORT       host port for the listener  (default 8080)
#   REQUESTS        number of requests to check (default 30)
#   SETTLE          seconds to wait after readiness (default 15)
#   IMAGE           image tag to build and run  (default nautiluslb:e2e)
#   KIND_NODE_IMAGE kind node image             (default: kind's default)
#

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
E2E_DIR="${ROOT}/test/e2e"

CLUSTER="${CLUSTER_NAME:-nautiluslb-e2e}"
KEEP="${KEEP:-0}"
IMAGE="${IMAGE:-nautiluslb:e2e}"
HOST_PORT="${HOST_PORT:-8080}"
REQUESTS="${REQUESTS:-30}"
CONTAINER="nautiluslb-e2e"
STRICT_CONTAINER="nautiluslb-e2e-strict"
READY_TIMEOUT=120
SETTLE="${SETTLE:-15}"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/nautiluslb-e2e.XXXXXX")"
# The container runs as UID 65532 and must be able to read the mounted files.
chmod 755 "${WORK}"
ADMIN_KUBECONFIG="${WORK}/admin.kubeconfig"

log()  { printf '\n==> %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"; }

k() { kubectl --kubeconfig "${ADMIN_KUBECONFIG}" "$@"; }

cleanup() {
	local rc=$?
	set +e
	if [[ ${rc} -ne 0 ]]; then
		printf '\n==> NautilusLB logs (%s)\n' "${CONTAINER}" >&2
		docker logs "${CONTAINER}" 2>&1 | tail -n 200 >&2
		if [[ -s "${ADMIN_KUBECONFIG}" ]]; then
			printf '\n==> Cluster state\n' >&2
			k get nodes -o wide >&2
			k get svc,pods -n e2e-apps -o wide >&2
			k get svc,pods -n e2e-other -o wide >&2
		fi
	fi
	docker rm -f "${STRICT_CONTAINER}" >/dev/null 2>&1
	if [[ "${KEEP}" == "1" ]]; then
		printf '\nKEEP=1: kept cluster "%s", container "%s" and work dir %s\n' "${CLUSTER}" "${CONTAINER}" "${WORK}"
		printf 'kubectl --kubeconfig %s get svc -A\n' "${ADMIN_KUBECONFIG}"
	else
		docker rm -f "${CONTAINER}" >/dev/null 2>&1
		kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1
		rm -rf "${WORK}"
	fi
	if [[ ${rc} -eq 0 ]]; then
		printf '\nPASS: NautilusLB e2e\n'
	fi
	exit "${rc}"
}
trap cleanup EXIT

for cmd in docker kind kubectl curl; do need "${cmd}"; done

#
# Cluster
#

docker rm -f "${CONTAINER}" "${STRICT_CONTAINER}" >/dev/null 2>&1 || true

if kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
	if [[ "${KEEP}" == "1" ]]; then
		log "Reusing kind cluster ${CLUSTER}"
	else
		log "Deleting stale kind cluster ${CLUSTER}"
		kind delete cluster --name "${CLUSTER}"
	fi
fi

if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
	log "Creating kind cluster ${CLUSTER}"
	node_image_args=()
	if [[ -n "${KIND_NODE_IMAGE:-}" ]]; then
		node_image_args=(--image "${KIND_NODE_IMAGE}")
	fi
	# A separate kubeconfig keeps the caller's ~/.kube/config untouched.
	kind create cluster --name "${CLUSTER}" --kubeconfig "${ADMIN_KUBECONFIG}" \
		--wait 120s ${node_image_args[@]+"${node_image_args[@]}"}
fi

kind get kubeconfig --name "${CLUSTER}" >"${ADMIN_KUBECONFIG}"
kind get kubeconfig --name "${CLUSTER}" --internal >"${WORK}/admin-internal.kubeconfig"

log "Applying RBAC and workloads"
k apply -f "${E2E_DIR}/rbac.yaml"
k apply -f "${E2E_DIR}/manifests.yaml"

# The documented RBAC must be enough, and must not be more than it says.
sa="system:serviceaccount:nautiluslb:nautiluslb"
[[ "$(k auth can-i list nodes --as "${sa}")" == "yes" ]] || fail "ServiceAccount cannot list nodes"
[[ "$(k auth can-i list services -n e2e-apps --as "${sa}")" == "yes" ]] || fail "ServiceAccount cannot list services"
[[ "$(k auth can-i get secrets -A --as "${sa}" || true)" == "no" ]] || fail "ServiceAccount can read secrets"

#
# Least-privilege kubeconfig for NautilusLB (ServiceAccount token, not admin)
#

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

#
# Image
#

log "Building ${IMAGE}"
docker build -f "${ROOT}/build/Dockerfile" -t "${IMAGE}" "${ROOT}"

# run_nlb NAME CONFIG_FILE [extra docker args...]
run_nlb() {
	local name="$1" cfg="$2"
	shift 2
	chmod 644 "${cfg}"
	docker run --detach \
		--name "${name}" \
		--network kind \
		--volume "${cfg}:/nautiluslb/config.yaml:ro" \
		--volume "${WORK}/kubeconfig:/nautiluslb/kubeconfig:ro" \
		"$@" \
		"${IMAGE}" >/dev/null
}

#
# Strict configuration: invalid configs must exit non-zero
#

# expect_rejected DESCRIPTION CONFIG_FILE
expect_rejected() {
	local what="$1" cfg="$2" state="" code=""
	docker rm -f "${STRICT_CONTAINER}" >/dev/null 2>&1 || true
	run_nlb "${STRICT_CONTAINER}" "${cfg}"
	for _ in $(seq 1 20); do
		state="$(docker inspect -f '{{.State.Running}}' "${STRICT_CONTAINER}")"
		[[ "${state}" == "false" ]] && break
		sleep 1
	done
	if [[ "${state}" != "false" ]]; then
		docker logs "${STRICT_CONTAINER}" 2>&1 | tail -n 50 >&2
		fail "config with ${what} was accepted (container still running after 20s)"
	fi
	code="$(docker inspect -f '{{.State.ExitCode}}' "${STRICT_CONTAINER}")"
	if [[ "${code}" == "0" ]]; then
		docker logs "${STRICT_CONTAINER}" 2>&1 | tail -n 50 >&2
		fail "config with ${what} exited 0"
	fi
	if ! docker logs "${STRICT_CONTAINER}" 2>&1 | grep -q "Failed to load configuration"; then
		docker logs "${STRICT_CONTAINER}" 2>&1 | tail -n 50 >&2
		fail "config with ${what} exited ${code}, but not because the configuration was refused"
	fi
	printf '  ok: config with %s rejected (exit %s)\n' "${what}" "${code}"
	docker rm -f "${STRICT_CONTAINER}" >/dev/null 2>&1 || true
}

log "Checking strict configuration parsing"

cat >"${WORK}/config-unknown-key.yaml" <<'EOF'
settings:
  kubeconfigPath: /nautiluslb/kubeconfig
configurations:
  - name: e2e_http
    listenerAddress: ":8080"
    backendPortName: http
    namespaces: [e2e-apps]
    namespcae: e2e-other
EOF
expect_rejected "an unknown key" "${WORK}/config-unknown-key.yaml"

cat >"${WORK}/config-no-namespaces.yaml" <<'EOF'
settings:
  kubeconfigPath: /nautiluslb/kubeconfig
configurations:
  - name: e2e_http
    listenerAddress: ":8080"
    backendPortName: http
EOF
expect_rejected "no namespaces" "${WORK}/config-no-namespaces.yaml"

#
# Service binding
#

log "Waiting for workloads"
k wait --for=condition=available deployment --all -n e2e-apps --timeout=180s
k wait --for=condition=available deployment --all -n e2e-other --timeout=180s

# Every fixture must answer on its NodePort before NautilusLB starts. Otherwise
# an intruder whose NodePort is not programmed yet would fail its first health
# check, sit out of the rotation, and a hijack would go unnoticed.
log "Checking every fixture answers on its NodePort"
node="${CLUSTER}-control-plane"
node_ip="$(k get node "${node}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"
for fixture in e2e-apps/bound e2e-apps/intruder e2e-other/outsider; do
	ns="${fixture%/*}" name="${fixture#*/}"
	node_port="$(k get svc "${name}" -n "${ns}" -o jsonpath='{.spec.ports[?(@.name=="http")].nodePort}')"
	answer=""
	for _ in $(seq 1 30); do
		answer="$(docker exec "${node}" curl -s --max-time 2 "http://${node_ip}:${node_port}/" || true)"
		[[ "${answer}" == "${name}" ]] && break
		sleep 2
	done
	[[ "${answer}" == "${name}" ]] || fail "fixture ${fixture} does not answer on ${node_ip}:${node_port} (got '${answer}')"
	printf '  ok: %s answers on NodePort %s\n' "${fixture}" "${node_port}"
done

cat >"${WORK}/config.yaml" <<'EOF'
settings:
  kubeconfigPath: /nautiluslb/kubeconfig
configurations:
  - name: e2e_http
    listenerAddress: ":8080"
    backendPortName: http
    namespaces:
      - e2e-apps
EOF

log "Starting NautilusLB (listener 127.0.0.1:${HOST_PORT})"
run_nlb "${CONTAINER}" "${WORK}/config.yaml" --publish "127.0.0.1:${HOST_PORT}:8080"

url="http://127.0.0.1:${HOST_PORT}/"
deadline=$((SECONDS + READY_TIMEOUT))
body=""
until [[ "${body}" == "bound" ]]; do
	if [[ "$(docker inspect -f '{{.State.Running}}' "${CONTAINER}")" != "true" ]]; then
		fail "NautilusLB exited before becoming ready"
	fi
	if (( SECONDS >= deadline )); then
		fail "NautilusLB did not serve the bound Service within ${READY_TIMEOUT}s (last body: '${body}')"
	fi
	body="$(curl -s --max-time 2 "${url}" || true)"
	case "${body}" in
		intruder | outsider) fail "first response came from '${body}'" ;;
	esac
	[[ "${body}" == "bound" ]] || sleep 2
done
printf '  ready after %ss\n' "$((READY_TIMEOUT - (deadline - SECONDS)))"

# Let at least one full health-check round (10s) pass, so a wrongly bound
# backend that missed its first check is back in the rotation by now.
log "Settling for ${SETTLE}s, then sending ${REQUESTS} requests"
sleep "${SETTLE}"
bound=0
for i in $(seq 1 "${REQUESTS}"); do
	body="$(curl -sS --max-time 5 "${url}")" || fail "request ${i} failed"
	[[ "${body}" == "bound" ]] || fail "request ${i} answered by '${body}', expected 'bound'"
	bound=$((bound + 1))
done
printf '  %s/%s responses from the bound Service, none from intruder or outsider\n' "${bound}" "${REQUESTS}"

if docker logs "${CONTAINER}" 2>&1 | grep -qiE 'panic|forbidden'; then
	fail "NautilusLB logged a panic or an RBAC denial"
fi
