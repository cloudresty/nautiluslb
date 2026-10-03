#!/usr/bin/env bash
#
# NautilusLB end-to-end test.
#
# Creates a kind cluster with the fixtures in manifests.yaml, builds the
# NautilusLB image from this repository and runs it as a plain Docker container
# on the "kind" network, outside the cluster, the way it is deployed in
# production. NautilusLB authenticates with a token for the least-privilege
# ServiceAccount in rbac.yaml, proving the documented RBAC is sufficient.
# Each case in cases/<name>.sh starts its own NautilusLB with its own config
# and drives traffic from a client container on the same network (lib.sh).
#
# Cases (default: all but soak, in this order):
#   strict     invalid configurations (legacy v0.x file, unknown key, no namespaces) exit 1
#   rbac       the ServiceAccount can do exactly what rbac.yaml says
#   binding    hijack regression (fixed in v1.0.0): only the bound Service gets traffic
#   metrics    /metrics counters and build_info, /readyz
#   balancing  least_conn spreads by active connections, source_ip_hash is sticky
#   proxy      PROXY protocol v1 and v2 to the backend carry the client address
#   sni        TLS passthrough routed by SNI; unknown SNI is closed
#   udp        UDP echo round trips
#   reload     SIGHUP adds a listener and changes an ACL without dropping an
#              established connection; an invalid edit is rejected
#   drain      SIGTERM: /readyz 503 before the listener closes, a slow transfer
#              completes, exit 0
#   soak       (only when named) SOAK_SECONDS of load with pprof: goroutines
#              return to baseline, RSS stays bounded
#
# Requirements: docker, kind, kubectl, openssl.
#
# Environment:
#   CASE            comma-separated cases to run (default: all but soak)
#   KEEP=1          keep (and reuse, if it exists) the cluster and containers
#   FAIL_FAST=1     stop at the first failing case
#   CLUSTER_NAME    kind cluster name                  (default nautiluslb-e2e)
#   REQUESTS        binding: number of requests       (default 30)
#   SETTLE          binding: seconds after readiness  (default 15)
#   SOAK_SECONDS    soak: load duration               (default 120)
#   SOAK_QPS        soak: request rate                (default 250)
#   SOAK_MAX_RSS_MIB soak: memory bound               (default 256)
#   IMAGE           image tag to build and run        (default nautiluslb:e2e)
#   VERSION         version baked into the image      (default git describe)
#   KIND_NODE_IMAGE kind node image                   (default: kind's default)
#

set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

for cmd in docker kind kubectl openssl; do need "${cmd}"; done

if [[ -n "${CASE:-}" ]]; then
	IFS=',' read -r -a cases <<<"${CASE}"
else
	cases=("${ALL_CASES[@]}")
fi
for c in "${cases[@]}"; do
	[[ -f "${E2E_DIR}/cases/${c}.sh" ]] || fail "unknown case '${c}' (have: ${ALL_CASES[*]} soak)"
done

init_work
trap cleanup EXIT

total_start=${SECONDS}
remove_stale_containers
setup_cluster
apply_fixtures
make_sa_kubeconfig
build_image
start_client
wait_fixtures
printf '\n  setup took %ss\n' "$((SECONDS - total_start))"

results=()
failed=0
for c in "${cases[@]}"; do
	log "CASE ${c}"
	case_start=${SECONDS}
	set +e
	(
		set -euo pipefail
		CASE_NAME="${c}"
		NLB="${NLB_PREFIX}-${c}"
		export CASE_NAME NLB
		# shellcheck source=/dev/null
		source "${E2E_DIR}/cases/${c}.sh"
	)
	rc=$?
	set -e
	took=$((SECONDS - case_start))
	if [[ ${rc} -eq 0 ]]; then
		results+=("PASS  ${c} (${took}s)")
	else
		results+=("FAIL  ${c} (${took}s)")
		failed=$((failed + 1))
		dump_container_logs
	fi
	[[ "${KEEP}" == "1" ]] || remove_case_containers
	if [[ ${rc} -ne 0 && "${FAIL_FAST:-0}" == "1" ]]; then
		break
	fi
done

printf '\n==> Results (total %ss)\n' "$((SECONDS - total_start))"
printf '  %s\n' "${results[@]}"
if [[ ${failed} -ne 0 ]]; then
	printf '\nFAIL: %s case(s) failed\n' "${failed}" >&2
	dump_cluster_state
	# Logs were dumped per case already.
	: >"${WORK}/containers"
	exit 1
fi
printf '\nPASS: NautilusLB e2e\n'
