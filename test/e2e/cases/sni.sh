# shellcheck shell=bash
# sni: TLS passthrough. Routes a (a.e2e.test) and b (b.e2e.test) are served by
# two nginx fixtures with distinct self-signed certificates (Services annotated
# e2e_tls/a and e2e_tls/b). The certificate the client receives proves the TLS
# session reached the right backend untouched; an unknown SNI is closed.

dir="$(case_dir sni)"
{
	nlb_config_header
	cat <<'EOF'
  - name: e2e_tls
    protocol: tls
    listenerAddress: ":8443"
    backendPortName: https
    namespaces: [e2e-apps]
    tls:
      routes:
        - name: a
          hosts: ["a.e2e.test"]
        - name: b
          hosts: ["b.e2e.test"]
EOF
} >"${dir}/config.yaml"

nlb_start "${NLB}" "${dir}"
nlb_wait_ready "${NLB}"
ip="$(container_ip "${NLB}")"

for r in a b; do
	host="${r}.e2e.test"
	for i in 1 2 3; do
		body="$(cexec curl -sS -k --max-time 5 --connect-to "${host}:443:${ip}:8443" "https://${host}/")" ||
			fail "SNI ${host} request ${i} failed"
		[[ "${body}" == "${r}" ]] || fail "SNI ${host} answered by '${body}', want '${r}'"
	done
	subject="$(csh "openssl s_client -connect ${ip}:8443 -servername ${host} </dev/null 2>/dev/null | openssl x509 -noout -subject")"
	[[ "${subject}" == *"CN=${host}"* || "${subject}" == *"CN = ${host}"* ]] ||
		fail "SNI ${host}: served certificate '${subject}', want CN=${host}"
	ok "SNI ${host} -> backend ${r} (3/3), certificate ${subject}"
done

# Unknown name, no defaultRoute: NautilusLB closes the connection without a
# backend, so no certificate is ever received.
if csh "openssl s_client -connect ${ip}:8443 -servername c.e2e.test </dev/null 2>&1" | grep 'BEGIN CERTIFICATE\|^subject=' >/dev/null; then
	fail "unknown SNI c.e2e.test received a certificate"
fi
if cexec curl -sS -k --max-time 5 --connect-to "c.e2e.test:443:${ip}:8443" "https://c.e2e.test/" >/dev/null 2>&1; then
	fail "unknown SNI c.e2e.test was served"
fi
ok "unknown SNI c.e2e.test closed without a certificate"

m="${WORK}/sni.prom"
scrape "${NLB}" "${m}"
expect_metric "${m}" ge 2 nautiluslb_connections_rejected_total listener=e2e_tls reason=sni_no_route
expect_metric "${m}" eq 1 nautiluslb_pool_backends listener=e2e_tls pool=e2e_tls/a state=healthy
expect_metric "${m}" eq 1 nautiluslb_pool_backends listener=e2e_tls pool=e2e_tls/b state=healthy

expect_clean_log "${NLB}"
