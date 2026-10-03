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

# UDP has no retransmission: like any real UDP client (DNS, syslog), retry a
# round trip whose datagram or reply was lost. One lost datagram on a busy CI
# runner once failed a release gate (v1.0.1). Loss is tolerated, but a trip
# that never succeeds, or more than 2 retries in total, still fails the case.
retries=0
for i in $(seq 1 10); do
	got=""
	for attempt in 1 2 3; do
		got="$(csh "echo ping-${i} | socat -t2 - UDP4:${ip}:7001" || true)"
		[[ "${got}" == "ping-${i}" ]] && break
		retries=$((retries + 1))
		echo "  note: UDP round trip ${i} attempt ${attempt} got '${got}', retrying"
	done
	[[ "${got}" == "ping-${i}" ]] || fail "UDP round trip ${i}: got '${got}' after 3 attempts, want 'ping-${i}'"
done
((retries <= 2)) || fail "UDP round trips needed ${retries} retries (more than 2): datagrams are being lost systematically"
ok "10/10 UDP round trips (one session each, ${retries} retries)"

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
