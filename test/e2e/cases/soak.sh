# shellcheck shell=bash
# soak (make soak; not part of the default run): SOAK_SECONDS of HTTP load,
# one new connection per request, through NautilusLB with pprof enabled.
# Asserts that goroutines return to within 10% of the pre-load baseline once
# the load stops, and that memory stays under SOAK_MAX_RSS_MIB.

soak_seconds="${SOAK_SECONDS:-120}"
qps="${SOAK_QPS:-250}"
max_rss_mib="${SOAK_MAX_RSS_MIB:-256}"
load="${NLB_PREFIX}-load"

dir="$(case_dir soak)"
{
	nlb_config_header pprof
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

goroutines() {
	cexec curl -sS --max-time 5 "http://${ip}:9090/debug/pprof/goroutine?debug=1" | sed -n '1s/.*total \([0-9]*\).*/\1/p'
}
# mem_mib: container memory as docker stats reports it, in MiB.
mem_mib() {
	docker stats --no-stream --format '{{.MemUsage}}' "${NLB}" | awk '{
		v = $1; u = v; sub(/[0-9.]+/, "", u); sub(/[A-Za-z]+$/, "", v)
		f = (u == "GiB") ? 1024 : (u == "KiB" || u == "kB") ? 1 / 1024 : (u == "B") ? 1 / 1048576 : 1
		printf "%.1f\n", v * f }'
}

# Warm up (first connections, health checks, informer resync) before the baseline.
csh "for i in \$(seq 1 20); do curl -s --max-time 5 http://${ip}:8080/ >/dev/null; done"
sleep 5
base_g="$(goroutines)"
base_m="$(mem_mib)"
[[ "${base_g}" =~ ^[0-9]+$ ]] || fail "cannot read the goroutine count from /debug/pprof (pprof enabled?)"
ok "baseline: ${base_g} goroutines, ${base_m} MiB"

log "Load: ${qps} req/s, new connection per request, for ${soak_seconds}s"
docker rm -f "${load}" >/dev/null 2>&1 || true
docker image inspect "${LOAD_IMAGE}" >/dev/null 2>&1 || docker pull --quiet "${LOAD_IMAGE}" >/dev/null
docker run --detach --name "${load}" --network kind "${LOAD_IMAGE}" \
	load -qps "${qps}" -c 32 -t "${soak_seconds}s" -keepalive=false -timeout 5s -json /tmp/soak.json \
	"http://${ip}:8080/" >/dev/null

peak_g=0 peak_m=0
soak_start=${SECONDS}
while [[ "$(docker inspect -f '{{.State.Running}}' "${load}")" == "true" ]]; do
	nlb_running "${NLB}" || fail "NautilusLB died under load"
	g="$(goroutines)" || g=0
	m="$(mem_mib)"
	(( g > peak_g )) && peak_g=${g}
	awk -v a="${m}" -v b="${peak_m}" 'BEGIN { exit !(a > b) }' && peak_m=${m}
	printf '  t+%ss: %s goroutines, %s MiB\n' "$((SECONDS - soak_start))" "${g}" "${m}"
	sleep 10
done
summary="$(docker logs "${load}" 2>&1 | grep -E '^(Code |Sockets used|All done|Aggregated Function Time)' || true)"
printf '%s\n' "${summary}" | sed 's/^/  fortio: /'
total="$(docker logs "${load}" 2>&1 | sed -n 's/^All done \([0-9]*\) calls.*/\1/p')"
ok200="$(docker logs "${load}" 2>&1 | sed -n 's/^Code 200 : \([0-9]*\) .*/\1/p')"
[[ -n "${total}" && -n "${ok200}" ]] || fail "could not read the fortio result"
errors=$((total - ok200))
# Allow 0.1% transport errors (kube-proxy NodePort churn); anything more fails.
(( errors * 1000 <= total )) || fail "${errors}/${total} requests failed under load"
ok "${ok200}/${total} requests 200"

# After the load: connections close, goroutines must return to baseline.
limit=$(( (base_g * 110 + 99) / 100 ))
end_g=""
for _ in $(seq 1 30); do
	end_g="$(goroutines)"
	(( end_g <= limit )) && break
	sleep 1
done
end_m="$(mem_mib)"
m="${WORK}/soak.prom"
scrape "${NLB}" "${m}"
accepted="$(metric "${m}" nautiluslb_connections_accepted_total listener=e2e_http)"
active="$(metric "${m}" nautiluslb_connections_active listener=e2e_http)"

printf '\n  SOAK RESULT: %ss at %s req/s: %s requests (%s errors), accepted %s, active after %s\n' \
	"${soak_seconds}" "${qps}" "${total}" "${errors}" "${accepted}" "${active}"
printf '  SOAK RESULT: goroutines baseline %s, peak %s, end %s (limit %s)\n' "${base_g}" "${peak_g}" "${end_g}" "${limit}"
printf '  SOAK RESULT: memory baseline %s MiB, peak %s MiB, end %s MiB (limit %s MiB)\n' "${base_m}" "${peak_m}" "${end_m}" "${max_rss_mib}"

(( end_g <= limit )) || fail "goroutines did not return to baseline: ${base_g} -> ${end_g} (limit ${limit})"
awk -v a="${peak_m}" -v b="${max_rss_mib}" 'BEGIN { exit !(a < b) }' || fail "memory peaked at ${peak_m} MiB, limit ${max_rss_mib} MiB"
awk -v a="${end_m}" -v b="${max_rss_mib}" 'BEGIN { exit !(a < b) }' || fail "memory ended at ${end_m} MiB, limit ${max_rss_mib} MiB"
[[ "${active}" == "0" ]] || fail "${active} connections still active after the load"
ok "goroutines back to baseline, memory bounded, no connection left active"

docker rm -f "${load}" >/dev/null 2>&1 || true
expect_clean_log "${NLB}"
