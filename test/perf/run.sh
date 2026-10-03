#!/usr/bin/env bash
#
# NautilusLB performance suite. See README.md.
#
# Compares four paths to the same kind NodePort backends:
#   direct   client -> NodeIP:NodePort
#   nlb      client -> NautilusLB v1.0.0 (image built from this repository)
#   v0       client -> NautilusLB v0.0.11 (published image, legacy v0.x config)
#   haproxy  client -> HAProxy 3.2 (mode tcp, static servers)
#
# Phases (PHASES, comma-separated; default all but soak, in this order):
#   setup       kind cluster, fixtures, image, client container
#   throughput  iperf3 1 and 8 streams; nlb splice counter
#   rate        new connection per request (fortio keepalive=false), -c 64/256
#   ratestd     the same at -c 64 with fortio -stdclient (see phase_ratestd)
#   latency     reused connections, fortio -c 64 -qps 5000
#   bigfile     1 MiB GETs on reused connections, -c 16
#   alog        nlb rate -c 64 with the access log on vs off
#   idle        10k idle connections: RSS, goroutines, fds at 0 / 10k / after close
#   pipes       nlb splice pipe sizes and iperf3 -P 1 with 1000 idle connections held
#   profile     nlb CPU profiles under rate -c 64 and iperf3 -P 8
#   soak        SOAK_SECONDS (900) of -c 256 churn through nlb, sampled every 60s
#   cleanup     remove containers and the cluster
#
# Environment: PHASES, TARGETS (default "direct nlb v0 haproxy haproxy-splice"), KEEP=1
# (do not clean up at the end), PERF_WORK (work dir), OUT (results dir),
# DURATION (fortio seconds, default 30), IPERF_SECONDS (20), IDLE_CONNS (10000),
# SOAK_SECONDS (900).

set -euo pipefail

# shellcheck source=test/perf/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

for cmd in docker kind kubectl openssl jq go perl; do need "${cmd}"; done

PHASES="${PHASES:-setup,throughput,rate,ratestd,latency,bigfile,alog,idle,pipes,profile}"
TARGETS="${TARGETS:-direct nlb v0 haproxy haproxy-splice}"
DURATION="${DURATION:-30}"
IPERF_SECONDS="${IPERF_SECONDS:-20}"
IDLE_CONNS="${IDLE_CONNS:-10000}"
SOAK_SECONDS="${SOAK_SECONDS:-900}"

init_perf_work
docker_alive || fail "docker does not answer within 20s"

has_phase() { [[ ",${PHASES}," == *",$1,"* ]]; }

phase_setup() {
	remove_stale_containers
	docker rm -f "${CLIENT}" "${LOAD}" "${PROXIES[@]}" "${NLB_PREFIX}-holder" >/dev/null 2>&1 || true
	{
		docker info --format 'docker: {{.ServerVersion}} {{.OperatingSystem}} kernel {{.KernelVersion}} {{.Architecture}} CPUs={{.NCPU}} mem={{.MemTotal}}'
		printf 'nautiluslb: %s (%s)\n' "${E2E_VERSION}" "${E2E_COMMIT}"
		date -u +'date: %Y-%m-%dT%H:%M:%SZ'
	} | tee "${OUT}/environment.txt"
	KEEP=1 setup_cluster
	apply_perf_fixtures
	make_sa_kubeconfig
	build_image
	start_client
	# idle-connection holder, a static linux binary run in a distroless image.
	CGO_ENABLED=0 GOOS=linux GOARCH="$(docker info --format '{{.Architecture}}' | sed 's/aarch64/arm64/;s/x86_64/amd64/')" \
		go build -o "${WORK}/idleconn" "${PERF_DIR}/idleconn/main.go"
	chmod 755 "${WORK}/idleconn"
	docker exec "${NODE}" sh -c 'echo "kind node: nf_conntrack_max=$(cat /proc/sys/net/netfilter/nf_conntrack_max) somaxconn=$(cat /proc/sys/net/core/somaxconn)"' |
		tee -a "${OUT}/environment.txt"
	k get cm kube-proxy -n kube-system -o jsonpath='{.data.config\.conf}' | grep -E '^mode:' | sed 's/^/kube-proxy /' | tee -a "${OUT}/environment.txt" || true
}

phase_throughput() {
	local t p j before after
	for t in ${TARGETS}; do
		log "throughput: ${t}"
		start_target "${t}"
		before=""
		[[ "${t}" == nlb ]] && before="$(metrics_of "${NLB_PREFIX}-nlb" | awk '/^nautiluslb_pipe_mode_total/{print}')"
		for p in 1 8; do
			j="${OUT}/iperf-${t}-P${p}.json"
			iperf_run "${t}" "${p}" "${IPERF_SECONDS}" >"${j}" || true
			record "${t}" "iperf_P${p}" Gbit/s "$(jq -r '(.end.sum_received.bits_per_second // 0) / 1e9 | . * 100 | round / 100' "${j}")"
			record "${t}" "iperf_P${p}" retransmits "$(jq -r '.end.sum_sent.retransmits // "n/a"' "${j}")"
			sleep 2
		done
		if [[ "${t}" == nlb ]]; then
			after="$(metrics_of "${NLB_PREFIX}-nlb" | awk '/^nautiluslb_pipe_mode_total/{print}')"
			printf 'before:\n%s\nafter:\n%s\n' "${before}" "${after}" | tee "${OUT}/nlb-pipe-mode.txt"
			record nlb iperf splice_total "$(awk '/mode="splice"/{print $2}' <<<"${after}")"
			record nlb iperf generic_total "$(awk '/mode="generic"/{print $2}' <<<"${after}")"
			docker logs "${NLB_PREFIX}-nlb" 2>&1 | grep -E '"mode"' | grep -v perf_http | tail -n 3 >"${OUT}/nlb-accesslog-iperf.txt" || true
		fi
	done
}

# rate_run TARGET CONC NAME [LABEL]: LABEL (default TARGET) names the result rows.
rate_run() {
	local t="$1" c="$2" name="$3" label="${4:-$1}" s
	fortio_run "${name}" -qps 0 -c "${c}" -t "${DURATION}s" -keepalive=false "http://$(addr "${t}" http)/small" || true
	s="$(fortio_summary "${OUT}/fortio-${name}.json")"
	read -r qps p50 p99 p999 ok total errors sockets <<<"${s}"
	record "${label}" "rate_c${c}" conns_per_s "${qps}"
	record "${label}" "rate_c${c}" p50_ms "${p50}"
	record "${label}" "rate_c${c}" p99_ms "${p99}"
	record "${label}" "rate_c${c}" p99.9_ms "${p999}"
	record "${label}" "rate_c${c}" errors "${errors}/${total}"
	: "${ok}" "${sockets}"
}

phase_rate() {
	local t c
	for t in ${TARGETS}; do
		log "connection rate: ${t}"
		start_target "${t}"
		for c in 64 256; do
			rate_run "${t}" "${c}" "rate-${t}-c${c}"
			sleep 5
		done
		if [[ "${t}" == nlb ]]; then
			metrics_of "${NLB_PREFIX}-nlb" >"${OUT}/nlb-metrics-after-rate.prom"
		fi
	done
}

# ratestd: the rate test with fortio's Go net/http client (-stdclient), which
# stops reading at Content-Length instead of waiting for the server's close.
# v0 never forwards the backend's FIN to the client, so with fortio's default
# client every v0 request waits for the 5s timeout; this gives v0 a number.
phase_ratestd() {
	local t s name
	for t in ${TARGETS}; do
		log "connection rate, Go net/http client: ${t}"
		start_target "${t}"
		name="ratestd-${t}-c64"
		fortio_run "${name}" -stdclient -qps 0 -c 64 -t "${DURATION}s" -keepalive=false "http://$(addr "${t}" http)/small" || true
		s="$(fortio_summary "${OUT}/fortio-${name}.json")"
		read -r qps p50 p99 p999 ok total errors sockets <<<"${s}"
		record "${t}" "ratestd_c64" conns_per_s "${qps}"
		record "${t}" "ratestd_c64" p50_ms "${p50}"
		record "${t}" "ratestd_c64" p99_ms "${p99}"
		record "${t}" "ratestd_c64" p99.9_ms "${p999}"
		record "${t}" "ratestd_c64" errors "${errors}/${total}"
		: "${ok}" "${sockets}"
		sleep 5
	done
}

phase_latency() {
	local t s name
	for t in ${TARGETS}; do
		log "latency on reused connections: ${t}"
		start_target "${t}"
		name="latency-${t}"
		fortio_run "${name}" -qps 5000 -c 64 -t "${DURATION}s" "http://$(addr "${t}" http)/small" || true
		s="$(fortio_summary "${OUT}/fortio-${name}.json")"
		read -r qps p50 p99 p999 ok total errors sockets <<<"${s}"
		record "${t}" "latency_c64_q5000" qps "${qps}"
		record "${t}" "latency_c64_q5000" p50_ms "${p50}"
		record "${t}" "latency_c64_q5000" p99_ms "${p99}"
		record "${t}" "latency_c64_q5000" p99.9_ms "${p999}"
		record "${t}" "latency_c64_q5000" errors "${errors}/${total}"
		: "${ok}" "${sockets}"
	done
}

phase_bigfile() {
	local t s name
	for t in ${TARGETS}; do
		log "1 MiB GETs: ${t}"
		start_target "${t}"
		name="bigfile-${t}"
		fortio_run "${name}" -httpbufferkb 2048 -qps 0 -c 16 -t 20s "http://$(addr "${t}" http)/1m" || true
		s="$(fortio_summary "${OUT}/fortio-${name}.json")"
		read -r qps p50 p99 p999 ok total errors sockets <<<"${s}"
		record "${t}" "1MiB_c16" MiB_per_s "${qps}"
		record "${t}" "1MiB_c16" p99_ms "${p99}"
		record "${t}" "1MiB_c16" errors "${errors}/${total}"
		: "${p50}" "${p999}" "${ok}" "${sockets}"
	done
}

phase_alog() {
	local a i
	# Alternate on/off ALOG_ROUNDS times: run-to-run noise on one VM is ~10%.
	for i in $(seq 1 "${ALOG_ROUNDS:-3}"); do
		for a in on off; do
			log "nlb access log ${a} (round ${i})"
			start_target nlb "${a}"
			rate_run nlb 64 "alog-${a}-c64-r${i}" "nlb-alog-${a}"
			metrics_of "${NLB_PREFIX}-nlb" | grep -E '^nautiluslb_accesslog_dropped_total' | tee "${OUT}/alog-${a}-r${i}-dropped.txt" || true
		done
	done
}

# idle_sample TARGET LABEL
idle_sample() {
	local t="$1" label="$2" name="${NLB_PREFIX}-$1" ps rss fds thr g="n/a" m
	ps="$(proc_stat "${name}")"
	read -r rss fds thr <<<"${ps}"
	m="$(mem_mib "${name}")"
	if [[ "${t}" == nlb ]]; then
		g="$(goroutines_of "${name}")"
		metrics_of "${name}" | grep -E '^(nautiluslb_connections_active|process_open_fds|process_resident_memory_bytes|go_memstats_heap_inuse_bytes|go_memstats_stack_inuse_bytes|go_memstats_sys_bytes)' \
			>"${OUT}/idle-${t}-${label}.prom" || true
	fi
	record "${t}" "idle_${label}" "rss_kib/fds/thr/goroutines/cgroup_mib" "${rss}/${fds}/${thr}/${g}/${m}"
	printf '%s %s %s %s %s %s\n' "${label}" "${rss}" "${fds}" "${thr}" "${g}" "${m}" >>"${OUT}/idle-${t}.txt"
}

phase_idle() {
	local t holder="${NLB_PREFIX}-holder" a out
	for t in ${TARGETS}; do
		[[ "${t}" == direct || "${t}" == haproxy-splice ]] && continue
		log "idle connections: ${t}"
		start_target "${t}"
		: >"${OUT}/idle-${t}.txt"
		sleep 10
		idle_sample "${t}" 0
		a="$(addr "${t}" http)"
		docker rm -f "${holder}" >/dev/null 2>&1 || true
		docker run --detach --name "${holder}" "${NET_OPTS[@]}" \
			--volume "${WORK}/idleconn:/idleconn:ro" --entrypoint /idleconn \
			"${HOLDER_IMAGE}" -addr "${a}" -n "${IDLE_CONNS}" -workers 64 >/dev/null
		for _ in $(seq 1 120); do
			out="$(docker logs "${holder}" 2>&1 | grep '^established' || true)"
			[[ -n "${out}" ]] && break
			sleep 2
		done
		printf '  holder: %s\n' "${out}"
		record "${t}" idle_open holder "${out// /_}"
		sleep 15 # let the proxy finish dialling upstream for every connection
		idle_sample "${t}" "${IDLE_CONNS}"
		if [[ "${t}" == nlb ]]; then
			cexec curl -sS --max-time 30 "http://$(container_ip "${NLB_PREFIX}-nlb"):9090/debug/pprof/heap" >"${OUT}/nlb-heap-idle.pb.gz"
			cexec curl -sS --max-time 30 "http://$(container_ip "${NLB_PREFIX}-nlb"):9090/debug/pprof/goroutine?debug=1" >"${OUT}/nlb-goroutines-idle.txt"
		fi
		docker stop -t 30 "${holder}" >/dev/null
		docker logs "${holder}" 2>&1 | tail -n 3 | sed 's/^/  holder: /'
		docker rm -f "${holder}" >/dev/null 2>&1 || true
		sleep 20
		idle_sample "${t}" after_close
		if [[ "${t}" == nlb ]]; then
			# Go caches splice pipes in a sync.Pool, released only after two
			# GCs; an idle process GCs every 2 minutes (forcegcperiod).
			sleep 250
			idle_sample "${t}" after_close_270s
		fi
	done
}

# pipes: does the unprivileged pipe budget (fs.pipe-user-pages-soft) shrink
# the splice pipes once many connections are open, and does it cost
# throughput? iperf3 -P 1 through nlb alone, then with PIPE_IDLE idle
# connections held open.
# PIPES_TARGET (default nlb) runs the same throughput pair through another
# proxy as a control (pipe sizes are only read for nlb).
phase_pipes() {
	local t="${PIPES_TARGET:-nlb}"
	local name="${NLB_PREFIX}-${t}" holder="${NLB_PREFIX}-holder" pid j
	log "${t}: iperf3 with and without idle connections"
	start_target "${t}"
	pid="$(docker inspect -f '{{.State.Pid}}' "${name}")"
	docker run --rm --privileged --pid host "${CLIENT_IMAGE}" cat /proc/sys/fs/pipe-user-pages-soft |
		sed 's/^/fs.pipe-user-pages-soft = /' | tee "${OUT}/pipes.txt"
	j="${OUT}/iperf-pipes-alone.json"
	iperf_run "${t}" 1 "${IPERF_SECONDS}" >"${j}" || true
	record "${t}" pipes_alone iperf_P1_Gbit/s "$(jq -r '(.end.sum_received.bits_per_second // 0) / 1e9 | . * 100 | round / 100' "${j}")"
	docker rm -f "${holder}" >/dev/null 2>&1 || true
	docker run --detach --name "${holder}" "${NET_OPTS[@]}" \
		--volume "${WORK}/idleconn:/idleconn:ro" --entrypoint /idleconn \
		"${HOLDER_IMAGE}" -addr "$(addr "${t}" http)" -n "${PIPE_IDLE:-1000}" -workers 64 >/dev/null
	sleep 15
	if [[ "${t}" == nlb ]]; then
		docker run --rm --privileged --pid host -v "${PERF_DIR}:/p:ro" "${CLIENT_IMAGE}" python3 /p/pipesizes.py "${pid}" |
			sed "s/^/with ${PIPE_IDLE:-1000} idle: /" | tee -a "${OUT}/pipes.txt"
	fi
	j="${OUT}/iperf-pipes-idle.json"
	iperf_run "${t}" 1 "${IPERF_SECONDS}" >"${j}" || true
	record "${t}" "pipes_${PIPE_IDLE:-1000}_idle" iperf_P1_Gbit/s "$(jq -r '(.end.sum_received.bits_per_second // 0) / 1e9 | . * 100 | round / 100' "${j}")"
	docker rm -f "${holder}" >/dev/null 2>&1 || true
}

phase_profile() {
	local ip load_pid
	log "nlb CPU profiles"
	start_target nlb
	ip="$(container_ip "${NLB_PREFIX}-nlb")"
	docker create --name "${NLB_PREFIX}-extract" "${IMAGE}" >/dev/null
	docker cp "${NLB_PREFIX}-extract:/nautiluslb/nautiluslb" "${OUT}/nautiluslb-bin" >/dev/null
	docker rm -f "${NLB_PREFIX}-extract" >/dev/null

	# Under connection churn.
	fortio_run profile-rate -qps 0 -c 64 -t 30s -keepalive=false "http://${ip}:8080/small" &
	load_pid=$!
	sleep 5
	cexec curl -sS --max-time 40 "http://${ip}:9090/debug/pprof/profile?seconds=20" >"${OUT}/nlb-cpu-rate.pb.gz"
	wait "${load_pid}" || true
	go tool pprof -top -nodecount=25 "${OUT}/nautiluslb-bin" "${OUT}/nlb-cpu-rate.pb.gz" >"${OUT}/nlb-cpu-rate-top.txt" 2>&1 || true
	go tool pprof -top -cum -nodecount=40 "${OUT}/nautiluslb-bin" "${OUT}/nlb-cpu-rate.pb.gz" >"${OUT}/nlb-cpu-rate-cum.txt" 2>&1 || true
	cexec curl -sS --max-time 30 "http://${ip}:9090/debug/pprof/allocs" >"${OUT}/nlb-allocs-rate.pb.gz"

	# Under bulk throughput.
	sleep 5
	iperf_run nlb 8 30 >"${OUT}/iperf-profile.json" &
	load_pid=$!
	sleep 5
	cexec curl -sS --max-time 40 "http://${ip}:9090/debug/pprof/profile?seconds=20" >"${OUT}/nlb-cpu-iperf.pb.gz"
	wait "${load_pid}" || true
	go tool pprof -top -nodecount=25 "${OUT}/nautiluslb-bin" "${OUT}/nlb-cpu-iperf.pb.gz" >"${OUT}/nlb-cpu-iperf-top.txt" 2>&1 || true
	ok "profiles in ${OUT}/nlb-cpu-*.pb.gz"
}

phase_soak() {
	local ip name="${NLB_PREFIX}-nlb" t0 m g
	log "soak: ${SOAK_SECONDS}s of -c 256 connection churn through nlb"
	start_target nlb
	ip="$(container_ip "${name}")"
	sleep 5
	printf 't_s\tgoroutines\trss_mib\topen_fds\theap_inuse_mib\tactive_conns\taccepted_total\tcgroup_mib\n' >"${OUT}/soak.tsv"
	fortio_run soak -qps 0 -c 256 -t "${SOAK_SECONDS}s" -keepalive=false "http://${ip}:8080/small" &
	local load_pid=$!
	t0=${SECONDS}
	while kill -0 "${load_pid}" 2>/dev/null || (( SECONDS - t0 < 5 )); do
		nlb_running "${name}" || fail "NautilusLB died during the soak"
		m="$(metrics_of "${name}")"
		g="$(goroutines_of "${name}")"
		printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$((SECONDS - t0))" "${g}" \
			"$(awk '/^process_resident_memory_bytes /{printf "%.1f", $2/1048576}' <<<"${m}")" \
			"$(awk '/^process_open_fds /{print $2}' <<<"${m}")" \
			"$(awk '/^go_memstats_heap_inuse_bytes /{printf "%.1f", $2/1048576}' <<<"${m}")" \
			"$(awk '/^nautiluslb_connections_active\{listener="perf_http"\}/{print $2}' <<<"${m}")" \
			"$(awk '/^nautiluslb_connections_accepted_total\{listener="perf_http"\}/{print $2}' <<<"${m}")" \
			"$(mem_mib "${name}")" | tee -a "${OUT}/soak.tsv"
		for _ in $(seq 1 60); do kill -0 "${load_pid}" 2>/dev/null || break; sleep 1; done
	done
	wait "${load_pid}" || true
	sleep 10
	m="$(metrics_of "${name}")"
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "end+10" "$(goroutines_of "${name}")" \
		"$(awk '/^process_resident_memory_bytes /{printf "%.1f", $2/1048576}' <<<"${m}")" \
		"$(awk '/^process_open_fds /{print $2}' <<<"${m}")" \
		"$(awk '/^go_memstats_heap_inuse_bytes /{printf "%.1f", $2/1048576}' <<<"${m}")" \
		"$(awk '/^nautiluslb_connections_active\{listener="perf_http"\}/{print $2}' <<<"${m}")" \
		"$(awk '/^nautiluslb_connections_accepted_total\{listener="perf_http"\}/{print $2}' <<<"${m}")" \
		"$(mem_mib "${name}")" | tee -a "${OUT}/soak.tsv"
	read -r qps p50 p99 p999 ok total errors sockets <<<"$(fortio_summary "${OUT}/fortio-soak.json")"
	record nlb soak conns_per_s "${qps}"
	record nlb soak p99_ms "${p99}"
	record nlb soak errors "${errors}/${total}"
	: "${p50}" "${p999}" "${ok}" "${sockets}"
	printf '%s\n' "${m}" >"${OUT}/nlb-metrics-after-soak.prom"
	docker logs "${name}" 2>&1 | grep -iE 'panic|"level":"error"' | head -n 20 >"${OUT}/nlb-soak-errors.txt" || true
}

phase_cleanup() {
	log "Cleanup"
	docker rm -f "${CLIENT}" "${LOAD}" "${PROXIES[@]}" "${NLB_PREFIX}-holder" "${NLB_PREFIX}-extract" >/dev/null 2>&1 || true
	kind delete cluster --name "${CLUSTER}" --kubeconfig "${ADMIN_KUBECONFIG}" >/dev/null 2>&1 || true
	docker image rm "${IMAGE}" >/dev/null 2>&1 || true
}

if ! has_phase setup; then
	load_env
	make_sa_kubeconfig # fresh 2h token for this invocation
fi
for ph in setup throughput rate ratestd latency bigfile alog idle pipes profile soak; do
	if has_phase "${ph}"; then
		"phase_${ph}"
		if [[ "${ph}" == setup ]]; then load_env; fi
	fi
done
stop_proxies
if has_phase cleanup || [[ "${KEEP:-0}" != "1" ]]; then
	phase_cleanup
fi
printf '\nResults: %s/results.tsv\n' "${OUT}"
