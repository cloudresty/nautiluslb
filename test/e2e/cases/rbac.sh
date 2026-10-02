# shellcheck shell=bash
# rbac: the ServiceAccount NautilusLB runs as (rbac.yaml) can do what discovery
# needs and nothing more. The other cases prove it is sufficient; this proves
# it is least privilege.

sa="system:serviceaccount:nautiluslb:nautiluslb"

# can SHOULD VERB RESOURCE [kubectl args...]
can() {
	local want="$1" verb="$2" res="$3" got
	shift 3
	got="$(k auth can-i "${verb}" "${res}" --as "${sa}" "$@" 2>/dev/null || true)"
	[[ "${got}" == "${want}" ]] || fail "can-i ${verb} ${res} $*: got '${got}', want '${want}'"
	ok "can-i ${verb} ${res} $* = ${want}"
}

for verb in list watch; do
	can yes "${verb}" services -n e2e-apps
	can yes "${verb}" endpointslices.discovery.k8s.io -n e2e-apps
	can no "${verb}" services -n default
	can no "${verb}" endpointslices.discovery.k8s.io -n default
	can no "${verb}" services -n e2e-other
	can yes "${verb}" nodes
done
can no get secrets -A
can no get secrets -n e2e-apps
can no create services -n e2e-apps
can no update nodes
can no list pods -n e2e-apps
