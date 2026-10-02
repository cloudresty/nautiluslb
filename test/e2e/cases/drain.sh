# shellcheck shell=bash
# drain: `docker stop` (SIGTERM) while a slow download is in flight.
#   - /readyz turns 503 while the listener still accepts (readinessDelay 3s);
#   - then the listener closes (new connections fail);
#   - the in-flight 6MB transfer completes intact;
#   - the process exits 0, well before docker's 40s SIGKILL.

dir="$(case_dir drain)"
{
	nlb_config_header
	cat <<'EOF'
  - name: e2e_blob
    listenerAddress: ":8080"
    backendPortName: http
    namespaces: [e2e-apps]
EOF
} >"${dir}/config.yaml"

nlb_start "${NLB}" "${dir}"
nlb_wait_ready "${NLB}"
ip="$(container_ip "${NLB}")"

csh 'rm -f /tmp/drain.*'
want_sum="$(csh 'yes nautiluslb | head -c 6000000 | sha256sum | cut -d" " -f1')"

# ~6s transfer at 1MB/s.
docker exec -d "${CLIENT}" sh -c \
	"curl -sS --limit-rate 1M -o /tmp/drain.blob http://${ip}:8080/blob 2>/tmp/drain.err; echo \$? >/tmp/drain.rc"
# Poll readiness and the listener every 200ms; one line per sample:
#   <ms since start> <readyz code> <listener code>
docker exec -d "${CLIENT}" sh -c "
	ms() { awk '{ printf \"%d\", \$1 * 1000 }' /proc/uptime; }
	t0=\$(ms)
	i=0
	while [ \$i -lt 100 ]; do
		r=\$(curl -s -o /dev/null -w '%{http_code}' --max-time 1 http://${ip}:9090/readyz)
		l=\$(curl -s -o /dev/null -w '%{http_code}' --max-time 1 http://${ip}:8080/)
		echo \"\$(( \$(ms) - t0 )) \$r \$l\" >>/tmp/drain.poll
		[ \"\$r\" = 000 ] && [ \"\$l\" = 000 ] && break
		i=\$((i + 1))
		sleep 0.2
	done
	touch /tmp/drain.polldone"

sleep 1.5
cexec test -s /tmp/drain.blob || fail "download did not start"
log "docker stop -t 40 ${NLB} with a transfer in flight"
stop_start=${SECONDS}
docker stop -t 40 "${NLB}" >/dev/null
stop_took=$((SECONDS - stop_start))
code="$(docker inspect -f '{{.State.ExitCode}}' "${NLB}")"
[[ "${code}" == "0" ]] || fail "NautilusLB exited ${code} after SIGTERM, want 0"
(( stop_took < 40 )) || fail "docker stop took ${stop_took}s: NautilusLB was SIGKILLed"
ok "exit 0 after SIGTERM, stop took ${stop_took}s"

for _ in $(seq 1 30); do
	cexec test -f /tmp/drain.rc && cexec test -f /tmp/drain.polldone && break
	sleep 0.5
done
rc="$(cexec cat /tmp/drain.rc 2>/dev/null || echo missing)"
[[ "${rc}" == "0" ]] || fail "in-flight download failed (curl exit ${rc}: $(cexec cat /tmp/drain.err 2>/dev/null))"
got_sum="$(csh 'sha256sum /tmp/drain.blob | cut -d" " -f1')"
size="$(csh 'wc -c </tmp/drain.blob' | tr -d ' ')"
[[ "${got_sum}" == "${want_sum}" ]] || fail "in-flight download corrupted: ${size} bytes, sha256 ${got_sum}"
ok "in-flight download completed intact (${size} bytes, sha256 matches)"

poll="$(cexec cat /tmp/drain.poll)"
# Order of events in the samples:
#   first_503  first sample with /readyz 503
#   ready_open a sample with /readyz 503 while the listener still answered 200
#   closed     first sample where the listener refused (000)
first_503="" ready_open="" closed=""
while read -r t r l; do
	if [[ -z "${first_503}" && "${r}" == "503" ]]; then first_503="${t}"; fi
	if [[ "${r}" == "503" && "${l}" == "200" && -z "${ready_open}" ]]; then ready_open="${t}"; fi
	if [[ -z "${closed}" && "${l}" != "200" ]]; then
		closed="${t}"
		[[ -n "${first_503}" ]] || { printf '%s\n' "${poll}" >&2; fail "listener stopped answering at ${t}ms before /readyz turned 503"; }
	fi
done <<<"${poll}"
[[ -n "${first_503}" ]] || { printf '%s\n' "${poll}" >&2; fail "/readyz never returned 503 during the drain"; }
[[ -n "${ready_open}" ]] || { printf '%s\n' "${poll}" >&2; fail "no sample with /readyz 503 while the listener still served"; }
[[ -n "${closed}" ]] || { printf '%s\n' "${poll}" >&2; fail "the listener never closed during the drain"; }
ok "/readyz 503 at ${first_503}ms, listener still serving at ${ready_open}ms, listener closed at ${closed}ms"

expect_log "${NLB}" "Shutting down gracefully"
expect_clean_log "${NLB}"
