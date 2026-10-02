#!/usr/bin/env bash
# Drift check: the raw manifests in deploy/kubernetes must mirror the Helm
# chart defaults in deploy/helm/nautiluslb. Run from anywhere:
#
#   bash deploy/check-drift.sh
#
# It renders
#   helm template nautiluslb deploy/helm/nautiluslb --namespace nautiluslb
#   kubectl kustomize deploy/kubernetes
# and compares only the semantically relevant parts:
#   - the set of (kind, name) objects
#   - RBAC: ClusterRole/Role rules, binding roleRef and subjects
#   - Deployment: serviceAccountName, terminationGracePeriodSeconds, pod and
#     container securityContext, liveness/readiness/startup probes
#   - PodDisruptionBudget spec
#
# Deliberately NOT compared (documented exclusions):
#   - labels and annotations (helm.sh/chart, app.kubernetes.io/managed-by,
#     chart version, checksum/config annotations);
#   - Namespace/nautiluslb: Helm never creates its release namespace;
#   - Service/nautiluslb-metrics: rendered by the chart only with
#     metricsService.enabled or serviceMonitor.enabled;
#   - the namespace of Role/RoleBinding and the ConfigMap contents, Service
#     ports and container ports: they follow the example configuration, which
#     differs on purpose (the chart's default listens on :80 in "default");
#   - image, resources, env and volumes.
#
# Needs helm, kubectl and python3. YAML is converted to JSON with yq (v4) when
# it is on PATH, otherwise with python3's PyYAML. CI wiring is added
# separately; the script exits non-zero on drift and prints a unified diff.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

helm template nautiluslb "$root/deploy/helm/nautiluslb" --namespace nautiluslb >"$tmp/helm.yaml"
kubectl kustomize "$root/deploy/kubernetes" >"$tmp/raw.yaml"

to_json() {
  if command -v yq >/dev/null 2>&1 && yq --version 2>&1 | grep -q 'v4'; then
    yq -o=json -I=0 'select(. != null)' "$1" | python3 -c 'import json,sys; print(json.dumps([json.loads(l) for l in sys.stdin if l.strip()]))'
  else
    python3 -c 'import json,sys,yaml; print(json.dumps([d for d in yaml.safe_load_all(open(sys.argv[1])) if d]))' "$1"
  fi
}
to_json "$tmp/helm.yaml" >"$tmp/helm.json"
to_json "$tmp/raw.yaml" >"$tmp/raw.json"

normalise='
import json, sys

EXCLUDE = {("Namespace", "nautiluslb"), ("Service", "nautiluslb-metrics")}

def canon(v):
    """Sort lists of scalars and lists of dicts so ordering never counts as drift."""
    if isinstance(v, dict):
        return {k: canon(v[k]) for k in sorted(v)}
    if isinstance(v, list):
        items = [canon(i) for i in v]
        return sorted(items, key=lambda i: json.dumps(i, sort_keys=True))
    return v

docs = json.load(open(sys.argv[1]))
out = {}
for d in docs:
    kind, name = d["kind"], d["metadata"]["name"]
    if (kind, name) in EXCLUDE:
        continue
    key = f"{kind}/{name}"
    if kind in ("ClusterRole", "Role"):
        out[key] = {"rules": canon(d.get("rules", []))}
    elif kind in ("ClusterRoleBinding", "RoleBinding"):
        out[key] = {"roleRef": canon(d["roleRef"]), "subjects": canon(d.get("subjects", []))}
    elif kind == "Deployment":
        pod = d["spec"]["template"]["spec"]
        out[key] = {
            "serviceAccountName": pod.get("serviceAccountName"),
            "terminationGracePeriodSeconds": pod.get("terminationGracePeriodSeconds"),
            "podSecurityContext": canon(pod.get("securityContext")),
            "containers": {
                c["name"]: {
                    "securityContext": canon(c.get("securityContext")),
                    "livenessProbe": canon(c.get("livenessProbe")),
                    "readinessProbe": canon(c.get("readinessProbe")),
                    "startupProbe": canon(c.get("startupProbe")),
                }
                for c in pod.get("containers", [])
            },
        }
    elif kind == "PodDisruptionBudget":
        out[key] = {"spec": canon(d["spec"])}
    else:
        out[key] = {}
print(json.dumps(out, indent=2, sort_keys=True))
'

python3 -c "$normalise" "$tmp/helm.json" >"$tmp/helm.norm"
python3 -c "$normalise" "$tmp/raw.json" >"$tmp/raw.norm"

if diff -u --label "helm (deploy/helm/nautiluslb)" --label "raw (deploy/kubernetes)" "$tmp/helm.norm" "$tmp/raw.norm"; then
  echo "check-drift: deploy/kubernetes matches the Helm chart defaults"
else
  echo "check-drift: deploy/kubernetes drifted from the Helm chart defaults (see diff above)" >&2
  exit 1
fi
