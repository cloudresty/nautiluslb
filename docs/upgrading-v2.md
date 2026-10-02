# Upgrading from v1.x to v2.0.0

v2.0.0 changes the configuration format, the RBAC it needs, the logs it writes and the ports it opens. The Service annotations and the binding rules introduced in v1.0.1 are unchanged. If you are on v1.0.0 or earlier, first apply the v1.0.1 changes: name configurations in `nautiluslb.cloudresty.io/configurations` and set `namespaces` on every configuration. They are described in the repository README, under "Upgrading to v1.0.1".

## Breaking changes

| # | Area | v1.x | v2.0.0 |
| --- | --- | --- | --- |
| 1 | Config header | none | `apiVersion: nautiluslb.cloudresty.io/v2` and `kind: Config` are **required**. A file without them fails with `this looks like a v1 file (no apiVersion)`. |
| 2 | Kubeconfig key | `settings.kubeconfigPath` | `settings.kubernetes.kubeconfig` (old key accepted, deprecated) |
| 3 | Connect timeout | `requestTimeout: 5` (integer seconds) | `dialTimeout: 5s` (duration; old key accepted, deprecated; still capped at 10s) |
| 4 | Namespace key | `namespace: x` | `namespaces: [x]` (old key accepted and merged, deprecated) |
| 5 | RBAC | `list` on `nodes` and `services` | `list` + `watch` on `nodes`, `services` and `discovery.k8s.io/endpointslices` (informers replace 30s polling) |
| 6 | Admin port | none | HTTP server on `127.0.0.1:9090` by default: `/metrics`, `/healthz`, `/readyz`. Set `settings.admin.address: ""` to disable it. |
| 7 | Logs | one `Info` line per connection | one JSON **access-log** record per connection, on stdout by default. Per-connection application logs are `Debug` only. |
| 8 | Source entry point | `app/main.go` | `app/cmd/nautiluslb` (`go build ./cmd/nautiluslb` from `app/`). Only matters if you build from source. |
| 9 | Durations | integers | strings (`"30s"`). Bare integers are still read as seconds, with a deprecation warning. |

Unchanged:

- Service annotations `nautiluslb.cloudresty.io/enabled` and `nautiluslb.cloudresty.io/configurations`, and the binding rules. See [configuration.md](configuration.md#binding-rules-the-security-model).
- Container image contract. The binary is `/nautiluslb/nautiluslb` (the `ENTRYPOINT`), the working directory is `/nautiluslb`, and the config is read from `/nautiluslb/config.yaml`. The image is distroless, runs as UID/GID 65532 and has no shell. Mounted files must be readable by UID 65532.
- Strict parsing, all-errors-at-once validation, exit 1 on an invalid config or a listener that cannot bind.
- Connection semantics: up to 3 backends tried per connection, connect-only timeout, no idle timeout by default, 2-minute half-close and write-stall bounds, fail-open when no backend is healthy.

New and optional: `protocol: tls` with SNI routes, `protocol: udp`, balancing algorithms, weights, configurable health checks, limits, ACLs, PROXY protocol, hot reload on SIGHUP, graceful drain. See [configuration.md](configuration.md).

## Before and after

v1.x:

```yaml
settings:
  kubeconfigPath: "/nautiluslb/kubeconfig"

configurations:
  - name: http_traffic_configuration
    listenerAddress: ":80"
    requestTimeout: 5
    backendPortName: "http"
    namespace: "ingress-nginx"

  - name: mongodb_internal_service
    listenerAddress: "10.0.0.10:27017"
    requestTimeout: 10
    backendPortName: "mongodb"
    namespaces: ["development"]
```

v2.0.0:

```yaml
apiVersion: nautiluslb.cloudresty.io/v2
kind: Config

settings:
  kubernetes:
    kubeconfig: "/nautiluslb/kubeconfig"

configurations:
  - name: http_traffic_configuration     # names unchanged: Service annotations still match
    listenerAddress: ":80"
    dialTimeout: 5s
    backendPortName: "http"
    namespaces: ["ingress-nginx"]

  - name: mongodb_internal_service
    listenerAddress: "10.0.0.10:27017"
    dialTimeout: 10s
    backendPortName: "mongodb"
    namespaces: ["development"]
```

Only the two header lines are strictly required. The v1 file above, with `apiVersion` and `kind` added, loads with four deprecation warnings: `kubeconfigPath`, `namespace`, and `requestTimeout` twice. Run `nautiluslb --validate --config config.yaml` to see them.

### RBAC

Each allowlisted namespace needs this (`Role` + `RoleBinding`):

```yaml
rules:
  - apiGroups: [""]
    resources: ["services"]
    verbs: ["list", "watch"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["list", "watch"]
```

Cluster-wide (`ClusterRole` + `ClusterRoleBinding`):

```yaml
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["list", "watch"]
```

A configuration with `namespaces: ["*"]` needs the Services and EndpointSlices rule as a `ClusterRole` instead. Ready-made manifests are in [`deploy/kubernetes/`](../deploy/kubernetes/): `rbac-cluster.yaml`, `rbac-namespaced.yaml` and `rbac-clusterwide.yaml`. See [security.md](security.md#rbac-least-privilege).

If the new verbs are missing, discovery never syncs and `/readyz` stays 503 with `discovery not synced`. Listeners still serve after 60s, but every connection is rejected with `no_backend` until the informers sync. Pools that already had endpoints are never wiped by an unsynced store.

## Checklist

1. [ ] Add `apiVersion: nautiluslb.cloudresty.io/v2` and `kind: Config` to `config.yaml`.
2. [ ] Rename `settings.kubeconfigPath` to `settings.kubernetes.kubeconfig`, `requestTimeout: N` to `dialTimeout: Ns`, and `namespace: x` to `namespaces: [x]`. This is optional for v2.0.0 but removes the warnings.
3. [ ] Run `nautiluslb --validate --config config.yaml` with the v2 binary and fix every error and deprecation.
4. [ ] Apply the new RBAC (`watch`, `endpointslices`) **before** rolling out v2.
5. [ ] Decide on the admin server. Keep `127.0.0.1:9090` and scrape locally, move it to a private address (and firewall it), or disable it with `""`. Make sure port 9090 is free on hosts that run NautilusLB with host networking.
6. [ ] Update log pipelines. Per-connection `Info` lines are gone, and access-log JSON lines appear on stdout. Set `settings.accessLog.output` to a file path to separate them, or set `enabled: false`. See [operations.md](operations.md#access-log).
7. [ ] Point liveness/readiness probes or the VIP health check at `/healthz` and `/readyz`. See [ha.md](ha.md).
8. [ ] Give the process time to drain. The stop timeout (`terminationGracePeriodSeconds`, systemd `TimeoutStopSec`) must be at least `drain.timeout + readinessDelay + 15s` (48s by default; the shipped manifests use 48 and `TimeoutStopSec=60`). See [operations.md](operations.md#stop-timeout).
9. [ ] Add Prometheus scraping and the alerts in [metrics.md](metrics.md#example-alerts).
10. [ ] If you build from source, use `app/cmd/nautiluslb`.
11. [ ] Roll out one instance, check `/readyz` and `nautiluslb_pool_backends`, then the rest.

Rollback: v1.x rejects a v2 file (`apiVersion`, `kind` and other new keys are unknown to its strict parser). Keep the v1 file to roll back with.
