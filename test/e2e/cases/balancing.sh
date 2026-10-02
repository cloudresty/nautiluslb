# shellcheck shell=bash
# balancing: three backends (Services lb-0, lb-1, lb-2, each answering with its
# pod name).
#   least_conn:     a held connection's backend is skipped while the other two
#                   are idle; three held connections land on three backends.
#   source_ip_hash: one client always reaches the same backend.

dir="$(case_dir balancing)"
{
	nlb_config_header
	cat <<'EOF'
  - name: e2e_lc
    listenerAddress: ":8200"
    backendPortName: http
    namespaces: [e2e-apps]
    balancer: {algorithm: least_conn}
  - name: e2e_hash
    listenerAddress: ":8201"
    backendPortName: http
    namespaces: [e2e-apps]
    balancer: {algorithm: source_ip_hash}
EOF
} >"${dir}/config.yaml"

nlb_start "${NLB}" "${dir}"
nlb_wait_ready "${NLB}"
ip="$(container_ip "${NLB}")"
m="${WORK}/balancing.prom"

# NodePort -> Service name, to read the backend label of the metrics.
port_map=""
for s in lb-0 lb-1 lb-2; do port_map+="$(node_port e2e-apps "${s}" http)=${s} "; done

for _ in $(seq 1 30); do
	scrape "${NLB}" "${m}"
	[[ "$(metric "${m}" nautiluslb_pool_backends listener=e2e_lc state=healthy)" == "3" &&
		"$(metric "${m}" nautiluslb_pool_backends listener=e2e_hash state=healthy)" == "3" ]] && break
	sleep 1
done
expect_metric "${m}" eq 3 nautiluslb_pool_backends listener=e2e_lc state=healthy
expect_metric "${m}" eq 3 nautiluslb_pool_backends listener=e2e_hash state=healthy

# held_backends: "<service>=<active>" for every e2e_lc backend with active > 0.
held_backends() {
	scrape "${NLB}" "${m}"
	awk '/^nautiluslb_backend_connections_active\{/ && /listener="e2e_lc"/ && $2 > 0 {
		match($0, /backend="[^"]*"/); b = substr($0, RSTART + 9, RLENGTH - 10)
		sub(/.*:/, "", b); print b "=" $2 }' "${m}" |
		while IFS='=' read -r port active; do
			svc="port${port}"
			for pair in ${port_map}; do [[ "${pair%%=*}" == "${port}" ]] && svc="${pair#*=}"; done
			printf '%s=%s\n' "${svc}" "${active}"
		done | sort
}

# hold N: open a TCP connection that sends nothing and stays open for 60s.
# NautilusLB dials the backend at accept, so it counts as active at once.
hold() {
	docker exec -d "${CLIENT}" sh -c "sleep 60 | nc -4 ${ip} 8200 >/dev/null 2>&1"
}

all_seen=""
for _ in 1 2 3 4 5 6; do
	all_seen+="$(cexec curl -sS --max-time 5 "http://${ip}:8200/")"$'\n'
done
for s in lb-0 lb-1 lb-2; do
	grep -qx "${s}" <<<"${all_seen}" || fail "least_conn never reached ${s} in 6 idle requests: $(tr '\n' ' ' <<<"${all_seen}")"
done
ok "least_conn reaches all three backends when idle"

hold
held=""
for _ in $(seq 1 20); do
	held="$(held_backends)"
	[[ -n "${held}" ]] && break
	sleep 0.5
done
[[ "$(wc -l <<<"${held}" | tr -d ' ')" == "1" && "${held}" == *=1 ]] || fail "expected one held connection, metrics say: ${held}"
held_svc="${held%=*}"
ok "held connection is on ${held_svc}"

for i in 1 2 3 4 5 6; do
	body="$(cexec curl -sS --max-time 5 "http://${ip}:8200/")"
	[[ "${body}" != "${held_svc}" ]] || fail "least_conn sent request ${i} to ${held_svc}, which holds a connection while two backends are idle"
	sleep 0.3
done
ok "6 requests avoided the busy backend ${held_svc}"

hold
sleep 0.5
hold
for _ in $(seq 1 20); do
	held="$(held_backends)"
	[[ "$(wc -l <<<"${held}" | tr -d ' ')" == "3" ]] && break
	sleep 0.5
done
[[ "${held}" == $'lb-0=1\nlb-1=1\nlb-2=1' ]] || fail "three held connections are not one per backend: $(tr '\n' ' ' <<<"${held}")"
ok "three held connections, one per backend"
cexec pkill -x nc || true

hashed="$(csh "for i in \$(seq 1 20); do curl -sS --max-time 5 http://${ip}:8201/ || echo FAILED; done" | sort | uniq -c | sed 's/^ *//')"
[[ "$(wc -l <<<"${hashed}" | tr -d ' ')" == "1" && "${hashed}" == "20 lb-"* ]] ||
	fail "source_ip_hash spread one client over several backends: $(tr '\n' ' ' <<<"${hashed}")"
ok "source_ip_hash: 20/20 requests from ${CLIENT_IP} to ${hashed#20 }"

expect_clean_log "${NLB}"
