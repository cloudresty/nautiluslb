# shellcheck shell=bash
# strict: invalid configurations stop startup with exit 1 and the exact log
# message "Failed to load configuration" (docs/configuration.md#loading-rules).
# A v1 file additionally carries the upgrade hint.

dir="$(case_dir strict)"

# expect_rejected DESCRIPTION [EXTRA_LOG_TEXT]: config.yaml in ${dir} must be
# refused at startup.
expect_rejected() {
	local what="$1" hint="${2:-}" state="" code="" start=${SECONDS}
	nlb_start "${NLB}" "${dir}"
	while (( SECONDS - start < 20 )); do
		state="$(docker inspect -f '{{.State.Running}}' "${NLB}")"
		[[ "${state}" == "false" ]] && break
		sleep 0.5
	done
	[[ "${state}" == "false" ]] || fail "config with ${what} was accepted (container still running after 20s)"
	code="$(docker inspect -f '{{.State.ExitCode}}' "${NLB}")"
	[[ "${code}" == "1" ]] || fail "config with ${what} exited ${code}, want 1"
	docker logs "${NLB}" 2>&1 | grep "Failed to load configuration" >/dev/null ||
		fail "config with ${what} exited ${code}, but not because the configuration was refused"
	if [[ -n "${hint}" ]]; then
		docker logs "${NLB}" 2>&1 | grep -F -- "${hint}" >/dev/null || fail "config with ${what}: log lacks '${hint}'"
	fi
	ok "config with ${what} rejected (exit ${code})"
	docker rm -f "${NLB}" >/dev/null
}

# A v1.0.1 file: no apiVersion/kind.
cat >"${dir}/config.yaml" <<'EOF'
settings:
  kubeconfigPath: /nautiluslb/kubeconfig
configurations:
  - name: e2e_http
    listenerAddress: ":8080"
    backendPortName: http
    namespaces: [e2e-apps]
EOF
expect_rejected "a v1 file" "this looks like a v1 file (no apiVersion)"

{
	nlb_config_header
	cat <<'EOF'
  - name: e2e_http
    listenerAddress: ":8080"
    backendPortName: http
    namespaces: [e2e-apps]
    namespcae: e2e-other
EOF
} >"${dir}/config.yaml"
expect_rejected "an unknown key" "namespcae"

{
	nlb_config_header
	cat <<'EOF'
  - name: e2e_http
    listenerAddress: ":8080"
    backendPortName: http
EOF
} >"${dir}/config.yaml"
expect_rejected "no namespaces" "no namespaces configured"
