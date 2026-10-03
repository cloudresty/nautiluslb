# shellcheck shell=bash
# udp: a UDP listener in front of a socat UDP echo (Service port udp-echo,
# protocol UDP). Each client socket is one session.

dir="$(case_dir udp)"
{
	nlb_config_header
	cat <<'EOF'
  - name: e2e_udp
    protocol: udp
    listenerAddress: ":7001"
    backendPortName: udp-echo
    namespaces: [e2e-apps]
    udp:
      sessionIdleTimeout: 10s
EOF
} >"${dir}/config.yaml"

nlb_start "${NLB}" "${dir}"
nlb_wait_ready "${NLB}"
ip="$(container_ip "${NLB}")"

for i in $(seq 1 10); do
	got="$(csh "echo ping-${i} | socat -t2 - UDP4:${ip}:7001" || true)"
	[[ "${got}" == "ping-${i}" ]] || fail "UDP round trip ${i}: got '${got}', want 'ping-${i}'"
done
ok "10/10 UDP round trips (one session each)"

# Several datagrams in one session.
got="$(csh "(echo one; sleep 0.3; echo two; sleep 0.3; echo three; sleep 1) | socat -t2 - UDP4:${ip}:7001" | tr '\n' ' ')"
[[ "${got}" == "one two three " ]] || fail "UDP session: got '${got}', want 'one two three '"
ok "3 datagrams echoed within one session"

m="${WORK}/udp.prom"
scrape "${NLB}" "${m}"
expect_metric "${m}" ge 11 nautiluslb_udp_sessions_total listener=e2e_udp event=open
expect_metric "${m}" ge 13 nautiluslb_udp_datagrams_total listener=e2e_udp direction=in
expect_metric "${m}" ge 13 nautiluslb_udp_datagrams_total listener=e2e_udp direction=out

expect_clean_log "${NLB}"
