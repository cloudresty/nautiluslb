# shellcheck shell=bash
# binding: the hijack regression fixed in v1.0.0. Configuration e2e_http allowlists
# e2e-apps; of the three Services with a port named "http" only "bound" may
# answer: "intruder" names another configuration, "outsider" lives in a
# namespace that is not allowlisted.
#
# Every fixture answered on its NodePort before NautilusLB started
# (wait_fixtures), and the requests go out only after SETTLE (>= 15s) seconds,
# so a wrongly bound backend cannot hide behind a failed first dial.

requests="${REQUESTS:-30}"
dir="$(case_dir binding)"
{
	nlb_config_header
	cat <<'EOF'
  - name: e2e_http
    listenerAddress: ":8080"
    backendPortName: http
    namespaces:
      - e2e-apps
EOF
} >"${dir}/config.yaml"

nlb_start "${NLB}" "${dir}"
nlb_wait_ready "${NLB}"
ip="$(container_ip "${NLB}")"
url="http://${ip}:8080/"

deadline=$((SECONDS + 60))
body=""
until [[ "${body}" == "bound" ]]; do
	nlb_running "${NLB}" || fail "NautilusLB exited"
	(( SECONDS < deadline )) || fail "bound Service not served within 60s (last body '${body}')"
	body="$(cexec curl -s --max-time 2 "${url}" || true)"
	case "${body}" in
		intruder | outsider) fail "first response came from '${body}'" ;;
	esac
	[[ "${body}" == "bound" ]] || sleep 1
done
ok "first response from 'bound'"

if (( SETTLE < 15 )); then
	printf '  note: SETTLE=%s is below the 15s the hijack check needs\n' "${SETTLE}"
fi
log "Settling ${SETTLE}s, then sending ${requests} requests"
sleep "${SETTLE}"
# One client process, one connection per request, so each request is balanced.
bodies="$(csh "for i in \$(seq 1 ${requests}); do curl -sS --max-time 5 ${url} || echo FAILED; done")"
n=0
while read -r body; do
	n=$((n + 1))
	[[ "${body}" == "bound" ]] || fail "request ${n} answered by '${body}', expected 'bound'"
done <<<"${bodies}"
(( n == requests )) || fail "got ${n} responses, want ${requests}"
ok "${n}/${requests} responses from the bound Service, none from intruder or outsider"

expect_clean_log "${NLB}"
