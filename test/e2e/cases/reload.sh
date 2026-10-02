# shellcheck shell=bash
# reload: edit the mounted config file in place and send SIGHUP.
#   1. a valid edit adds listener e2e_http_reloaded (:8081) and denies the
#      client on e2e_http (:8080): the new listener serves, the ACL applies,
#      and a connection established on the unchanged e2e_echo listener (:7000)
#      before the reload keeps working after it.
#   2. an invalid edit is rejected: the running configuration stays as it was.

dir="$(case_dir reload)"
cfg="${dir}/config.yaml"

write_config() { # write_config deny_cidr extra_listener invalid
	{
		nlb_config_header
		cat <<'EOF'
  - name: e2e_echo
    listenerAddress: ":7000"
    backendPortName: tcp-echo
    namespaces: [e2e-apps]
  - name: e2e_http
    listenerAddress: ":8080"
    backendPortName: http
    namespaces: [e2e-apps]
EOF
		if [[ -n "$1" ]]; then
			printf '    access:\n      deny: ["%s"]\n' "$1"
		fi
		if [[ "$2" == "yes" ]]; then
			cat <<'EOF'
  - name: e2e_http_reloaded
    listenerAddress: ":8081"
    backendPortName: http
    namespaces: [e2e-apps]
EOF
		fi
		if [[ "$3" == "yes" ]]; then
			printf '    bogusKey: true\n'
		fi
	} >"${cfg}.new"
	# Rewrite in place (same inode), like an editor saving the mounted file.
	cat "${cfg}.new" >"${cfg}"
	rm -f "${cfg}.new"
	chmod 644 "${cfg}"
}

# reload_count RESULT
reload_count() {
	scrape "${NLB}" "${WORK}/reload.prom"
	metric "${WORK}/reload.prom" nautiluslb_config_reload_total "result=$1"
}

# await_reload RESULT N: wait until config_reload_total{result} reaches N.
await_reload() {
	local got=""
	for _ in $(seq 1 30); do
		got="$(reload_count "$1")"
		[[ "${got}" =~ ^[0-9]+$ ]] && (( got >= $2 )) && { ok "config_reload_total{result=$1} = ${got}"; return 0; }
		sleep 0.5
	done
	fail "config_reload_total{result=$1} = ${got}, want ${2}"
}

write_config "" no no
nlb_start "${NLB}" "${dir}"
nlb_wait_ready "${NLB}"
ip="$(container_ip "${NLB}")"

body="$(cexec curl -sS --max-time 5 "http://${ip}:8080/")" || fail "e2e_http does not serve before the reload"
[[ "${body}" == "bound" ]] || fail "e2e_http answered '${body}'"
if cexec curl -s --max-time 2 "http://${ip}:8081/" >/dev/null 2>&1; then
	fail ":8081 serves before the reload added it"
fi
ok "before reload: :8080 serves, :8081 closed"

# An established connection on the unchanged listener: "one" now, "two"
# after the reload, over the same TCP connection.
docker exec -d "${CLIENT}" sh -c \
	"(echo one; sleep 8; echo two; sleep 1) | socat -t2 - TCP4:${ip}:7000 >/tmp/reload-echo.out 2>&1; echo \$? >/tmp/reload-echo.rc"
sleep 1
[[ "$(cexec cat /tmp/reload-echo.out)" == "one" ]] || fail "echo connection not established: '$(cexec cat /tmp/reload-echo.out)'"

log "Valid edit + SIGHUP: add :8081, deny ${CLIENT_IP} on :8080"
write_config "${CLIENT_IP}/32" yes no
docker kill -s HUP "${NLB}" >/dev/null
await_reload applied 1

await_answer "new listener e2e_http_reloaded (:8081) serves" '^bound$' \
	cexec curl -sS --max-time 2 "http://${ip}:8081/"
if cexec curl -sS --max-time 2 "http://${ip}:8080/" >/dev/null 2>&1; then
	fail "e2e_http still serves ${CLIENT_IP} after the ACL denied it"
fi
scrape "${NLB}" "${WORK}/reload.prom"
expect_metric "${WORK}/reload.prom" ge 1 nautiluslb_connections_rejected_total listener=e2e_http reason=acl

for _ in $(seq 1 20); do
	cexec test -f /tmp/reload-echo.rc && break
	sleep 0.5
done
echo_out="$(cexec cat /tmp/reload-echo.out | tr '\n' ' ')"
[[ "${echo_out}" == "one two " ]] || fail "established echo connection across the reload: got '${echo_out}', want 'one two '"
ok "connection established before the reload on unchanged e2e_echo carried data after it"

log "Invalid edit + SIGHUP: rejected, running configuration kept"
write_config "${CLIENT_IP}/32" yes yes
docker kill -s HUP "${NLB}" >/dev/null
await_reload rejected 1
expect_log "${NLB}" "Failed to reload configuration"
nlb_running "${NLB}" || fail "NautilusLB exited after an invalid reload"
[[ "$(reload_count applied)" == "1" ]] || fail "an invalid reload was counted as applied"
body="$(cexec curl -sS --max-time 5 "http://${ip}:8081/")" || fail ":8081 stopped serving after the rejected reload"
[[ "${body}" == "bound" ]] || fail ":8081 answered '${body}' after the rejected reload"
if cexec curl -sS --max-time 2 "http://${ip}:8080/" >/dev/null 2>&1; then
	fail "the ACL on :8080 was lost by the rejected reload"
fi
code="$(cexec curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://${ip}:9090/readyz")"
[[ "${code}" == "200" ]] || fail "/readyz ${code} after the rejected reload"
ok "after the rejected reload: :8081 serves, :8080 still denies, /readyz 200"

expect_clean_log "${NLB}"
