# Upgrading to v2.0.0

v2.0.0 is the next release after v1.0.0 (the last published images are `v0.0.11` and `latest`, built from the same code). It closes a traffic-hijacking hole, changes the configuration format, the RBAC it needs, the logs it writes and the ports it opens, and replaces 30s polling with Kubernetes watches.

The security fix changes how Services are matched, so **every deployment needs edits to both `config.yaml` and the annotated Services**. Read the whole table before upgrading.

## Breaking changes

| # | Area | v1.0.0 and earlier | v2.0.0 |
| --- | --- | --- | --- |
| 1 | Service binding | any Service annotated `nautiluslb.cloudresty.io/enabled: "true"` whose port name matched joined a listener's pool | the Service must **also** name the configuration in `nautiluslb.cloudresty.io/configurations: "<name>[,<name>...]"` and live in one of the configuration's `namespaces`. Unbound Services are ignored, with one warning each. |
| 2 | Namespaces | an empty `namespace` meant **every namespace** | `namespaces` is **required** on every configuration. `["*"]` opts into cluster-wide discovery deliberately. |
| 3 | Config header | none | `apiVersion: nautiluslb.cloudresty.io/v2` and `kind: Config` are **required**. A file without them fails with `this looks like a v1 file (no apiVersion)`. |
| 4 | Parsing | unknown keys silently ignored | strict: unknown keys, duplicate names, conflicting listeners and malformed values stop startup with exit status 1, all errors reported at once. |
| 5 | Kubeconfig key | `settings.kubeconfigPath` | `settings.kubernetes.kubeconfig` (old key accepted, deprecated) |
| 6 | Connect timeout | `requestTimeout: 5` (integer seconds) | `dialTimeout: 5s` (duration; old key accepted, deprecated; capped at 10s) |
| 7 | Namespace key | `namespace: x` | `namespaces: [x]` (old key accepted and merged, deprecated) |
| 8 | `ClusterIP` Services | dialled on `targetPort` (the pod port) | dialled on `port`, the port the ClusterIP actually listens on. Headless Services and ports without a NodePort are skipped. |
| 9 | Listener address | a bare port (`"8080"`) was accepted, then failed to bind | `":8080"` or `"<ip>:8080"` only |
| 10 | RBAC | `list` on `nodes` and `services` | `list` + `watch` on `nodes`, `services` and `discovery.k8s.io/endpointslices` |
| 11 | Container image | Debian, runs as root, has a shell | distroless, runs as UID/GID 65532, no shell. Mounted files must be readable by UID 65532; `/root/.kube/config` no longer works. The binary is the `ENTRYPOINT` (pass flags such as `--validate` directly). |
| 12 | Admin port | none | HTTP server on `127.0.0.1:9090` by default: `/metrics`, `/healthz`, `/readyz`. Set `settings.admin.address: ""` to disable it. |
| 13 | Logs | one `Info` line per connection | one JSON **access-log** record per connection, on stdout by default. Per-connection application logs are `Debug` only. |
| 14 | Durations | integers | strings (`"30s"`). Bare integers are still read as seconds, with a deprecation warning. |
| 15 | Source entry point | `app/main.go` | `app/cmd/nautiluslb` (`go build ./cmd/nautiluslb` from `app/`). Only matters if you build from source. |

Unchanged: the annotation `nautiluslb.cloudresty.io/enabled`, the listener-per-configuration model, round-robin as the default algorithm, the image paths (`/nautiluslb/nautiluslb`, working directory `/nautiluslb`, config at `/nautiluslb/config.yaml`).

Fixed along the way (no action needed): a backend that refused a connection crashed the whole process in v1.0.0; a dial now times out after `dialTimeout`, up to 3 backends are tried, and a failure closes only that client. SIGTERM now drains instead of cutting every connection.

New and optional: `protocol: tls` with SNI routes, `protocol: udp`, balancing algorithms, weights, configurable health checks, limits, ACLs, PROXY protocol, hot reload on SIGHUP, graceful drain. See [configuration.md](configuration.md).

## Before and after

`config.yaml`, v1.0.0:

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
```

`config.yaml`, v2.0.0:

```yaml
apiVersion: nautiluslb.cloudresty.io/v2
kind: Config

settings:
  kubernetes:
    kubeconfig: "/nautiluslb/kubeconfig"

configurations:
  - name: http_traffic_configuration
    listenerAddress: ":80"
    dialTimeout: 5s
    backendPortName: "http"
    namespaces: ["ingress-nginx"]

  - name: mongodb_internal_service
    listenerAddress: "10.0.0.10:27017"
    dialTimeout: 10s
    backendPortName: "mongodb"
    namespaces: ["databases"]   # was cluster-wide in v1.0.0 (no namespace set)
```

The v1.0.0 file with only `apiVersion` and `kind` added does **not** load: the second configuration has no namespace, and v2 refuses to guess. Run `nautiluslb --validate --config config.yaml` to see every error and deprecation at once.

Each Service, before and after:

```yaml
metadata:
  annotations:
    nautiluslb.cloudresty.io/enabled: "true"
    # new, required: the configurations this Service serves
    nautiluslb.cloudresty.io/configurations: "http_traffic_configuration"
```

A Service that serves several listeners (for example an ingress controller on both `:80` and `:443`) lists them all, comma-separated. A TLS route is named `<configuration>/<route>`.

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

Cluster-wide (`ClusterRole` + `ClusterRoleBinding`), always required, even for ClusterIP-only pools:

```yaml
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["list", "watch"]
```

A configuration with `namespaces: ["*"]` needs the Services and EndpointSlices rule as a `ClusterRole` instead. Ready-made manifests are in [`deploy/kubernetes/`](../deploy/kubernetes/): `rbac-cluster.yaml`, `rbac-namespaced.yaml` and `rbac-clusterwide.yaml`. See [security.md](security.md#rbac-least-privilege).

If the new verbs are missing, discovery never syncs and `/readyz` stays 503 with `discovery not synced`. Listeners still serve after 60s, but every connection is rejected with `no_backend` until the informers sync.

## Checklist

1. [ ] Add `nautiluslb.cloudresty.io/configurations` to every annotated Service, naming the configurations it serves. Do this **first**: v1.0.0 ignores the new annotation, so it is safe to apply while v1.0.0 is still running.
2. [ ] Give every configuration `namespaces`. Where v1.0.0 had no `namespace`, list the namespaces its Services actually live in; use `["*"]` only if you really mean every namespace.
3. [ ] Add `apiVersion: nautiluslb.cloudresty.io/v2` and `kind: Config` to `config.yaml`.
4. [ ] Rename `settings.kubeconfigPath` to `settings.kubernetes.kubeconfig`, `requestTimeout: N` to `dialTimeout: Ns`, and `namespace: x` to `namespaces: [x]`. Optional for v2.0.0, but it removes the warnings.
5. [ ] Run `nautiluslb --validate --config config.yaml` with the v2 binary and fix every error and deprecation.
6. [ ] If any configuration uses `ClusterIP` Services where `port` differs from `targetPort`, confirm the backends listen on the Service `port` path you expect (v1.0.0 dialled the pod port).
7. [ ] Apply the new RBAC (`watch`, `endpointslices`) **before** rolling out v2.
8. [ ] Container deployments: make every mounted file (config, kubeconfig) readable by UID 65532, and stop mounting `/root/.kube/config`. Use a dedicated least-privilege kubeconfig, or run in-cluster with a ServiceAccount (the [Helm chart](../deploy/helm/nautiluslb) does this). Listeners below 1024 need the `net.ipv4.ip_unprivileged_port_start` sysctl or the chart's documented alternatives.
9. [ ] Decide on the admin server. Keep `127.0.0.1:9090` and scrape locally, move it to a private address (and firewall it), or disable it with `""`. Make sure port 9090 is free on hosts that run NautilusLB with host networking.
10. [ ] Update log pipelines. Per-connection `Info` lines are gone, and access-log JSON lines appear on stdout. Set `settings.accessLog.output` to a file path to separate them, or set `enabled: false`. See [operations.md](operations.md#access-log).
11. [ ] Point liveness/readiness probes or the VIP health check at `/healthz` and `/readyz`. See [ha.md](ha.md).
12. [ ] Give the process time to drain. The stop timeout (`terminationGracePeriodSeconds`, systemd `TimeoutStopSec`) must be at least `drain.timeout + readinessDelay + 15s` (48s by default; the shipped manifests use 48 and `TimeoutStopSec=60`). See [operations.md](operations.md#stop-timeout).
13. [ ] Add Prometheus scraping and the alerts in [metrics.md](metrics.md#example-alerts).
14. [ ] If you build from source, use `app/cmd/nautiluslb`.
15. [ ] Roll out one instance, check `/readyz`, `nautiluslb_pool_backends` and the warnings about unbound Services, then roll out the rest.

## Rollback

Keep the v1.0.0 `config.yaml` and roll back with it, **never with the v2 file**. v1.0.0 parses YAML loosely: it does not reject a v2 file, it silently ignores the keys it does not know, including `namespaces`. A v2 file run by v1.0.0 can therefore fall back to cluster-wide discovery and to port-name-only matching, which is the hijacking hole v2 closes. The `configurations` annotation on Services is harmless to v1.0.0 and can stay.
