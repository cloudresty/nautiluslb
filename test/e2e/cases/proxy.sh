# shellcheck shell=bash
# proxy: proxyProtocol.out v1 and v2. The backend is nginx with
# `listen ... proxy_protocol` answering $proxy_protocol_addr, so the body is
# the source address NautilusLB put in the header: the client container's.

dir="$(case_dir proxy)"
{
	nlb_config_header
	cat <<'EOF'
  - name: e2e_pp_v1
    listenerAddress: ":8100"
    backendPortName: pp
    namespaces: [e2e-apps]
    proxyProtocol: {out: v1}
  - name: e2e_pp_v2
    listenerAddress: ":8101"
    backendPortName: pp
    namespaces: [e2e-apps]
    proxyProtocol: {out: v2}
EOF
} >"${dir}/config.yaml"

nlb_start "${NLB}" "${dir}"
nlb_wait_ready "${NLB}"
ip="$(container_ip "${NLB}")"

for v in 1 2; do
	port=$((8099 + v))
	for i in 1 2 3; do
		body="$(cexec curl -sS --max-time 5 "http://${ip}:${port}/")" || fail "PROXY v${v} request ${i} failed"
		[[ "${body}" == "${CLIENT_IP}" ]] || fail "PROXY v${v}: backend saw source '${body}', want client ${CLIENT_IP}"
	done
	ok "PROXY v${v}: backend saw the client address ${CLIENT_IP} (3/3)"
done

expect_clean_log "${NLB}"
