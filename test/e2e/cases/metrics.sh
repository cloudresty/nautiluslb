# shellcheck shell=bash
# metrics: the admin server (0.0.0.0:9090 here) serves /healthz, /readyz and
# Prometheus metrics that count real traffic and carry the built version.

dir="$(case_dir metrics)"
{
	nlb_config_header
	cat <<'EOF'
  - name: e2e_http
    listenerAddress: ":8080"
    backendPortName: http
    namespaces: [e2e-apps]
EOF
} >"${dir}/config.yaml"

nlb_start "${NLB}" "${dir}"
nlb_wait_ready "${NLB}"
ip="$(container_ip "${NLB}")"

for path in healthz readyz health/live health/ready; do
	code="$(cexec curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://${ip}:9090/${path}")"
	[[ "${code}" == "200" ]] || fail "/${path} returned ${code}"
	ok "/${path} 200"
done

csh "for i in 1 2 3 4 5; do curl -sS --max-time 5 http://${ip}:8080/ >/dev/null || exit 1; done" ||
	fail "requests through the listener failed"

# The access-log record, and the counters, are written when a connection closes.
m="${WORK}/metrics.prom"
for _ in $(seq 1 10); do
	scrape "${NLB}" "${m}"
	[[ "$(metric "${m}" nautiluslb_connections_accepted_total listener=e2e_http)" =~ ^[0-9]+$ ]] &&
		(( $(metric "${m}" nautiluslb_connections_accepted_total listener=e2e_http) >= 5 )) && break
	sleep 1
done
expect_metric "${m}" ge 5 nautiluslb_connections_accepted_total listener=e2e_http
expect_metric "${m}" eq 1 nautiluslb_build_info "version=${E2E_VERSION}"
expect_metric "${m}" eq 1 nautiluslb_ready
expect_metric "${m}" eq 1 nautiluslb_discovery_informer_synced resource=services
expect_metric "${m}" eq 1 nautiluslb_discovery_informer_synced resource=endpointslices
expect_metric "${m}" eq 1 nautiluslb_discovery_informer_synced resource=nodes
expect_metric "${m}" ge 5 nautiluslb_backend_dial_total listener=e2e_http pool=e2e_http result=ok
expect_metric "${m}" eq 1 nautiluslb_pool_backends listener=e2e_http state=healthy
grep -q '^go_goroutines ' "${m}" || fail "Go runtime collector missing"
ok "go_* runtime metrics present"

expect_clean_log "${NLB}"
