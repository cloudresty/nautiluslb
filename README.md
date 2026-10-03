# NautilusLB

NautilusLB is an open-source Layer 4 load balancer for the edge of a Kubernetes cluster. It forwards TCP, TLS passthrough (routed by SNI, never terminated) and UDP traffic to the Kubernetes Services that opt in with annotations. It discovers those Services, their EndpointSlices and the cluster's Nodes through Kubernetes informers, so changes reach the backend pools within a fraction of a second, without a restart.

[![Go Tests](https://github.com/cloudresty/nautiluslb/actions/workflows/ci.yaml/badge.svg)](https://github.com/cloudresty/nautiluslb/actions/workflows/ci.yaml)
[![GitHub Tag](https://img.shields.io/github/v/tag/cloudresty/nautiluslb?label=Version)](https://github.com/cloudresty/nautiluslb/tags)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)

&nbsp;

## Table of Contents

- [How NautilusLB Works](#how-nautiluslb-works)
- [Why NautilusLB](#why-nautiluslb)
- [Key Features](#key-features)
- [Quick Start (Helm)](#quick-start-helm)
- [Configuration](#configuration)
- [Service Annotations](#service-annotations)
- [Kubernetes RBAC](#kubernetes-rbac)
- [Deployment Options](#deployment-options)
- [Monitoring](#monitoring)
- [High Availability](#high-availability)
- [Upgrading to v2.0.0](#upgrading-to-v200)
- [Verifying Releases](#verifying-releases)
- [Documentation](#documentation)
- [Contributing](#contributing)
- [License](#license)

🔝 [back to top](#nautiluslb)

&nbsp;

## How NautilusLB Works

NautilusLB sits in front of, or at the edge of, your Kubernetes cluster. Each entry in its configuration is one **listener** (`tcp`, `tls` or `udp`) and feeds one **pool** of backends; a `tls` listener feeds one pool per SNI route.

1. NautilusLB watches Nodes cluster-wide, and Services and EndpointSlices in each configuration's allowlisted namespaces.
2. A Service joins a pool only when it explicitly names that pool in its annotations, lives in an allowlisted namespace and has the configured port name with the right protocol (see [Service Annotations](#service-annotations)).
3. Backends are node or ClusterIP addresses, never pod IPs. For a `NodePort` or `LoadBalancer` Service a backend is `<node address>:<nodePort>` for every eligible node (only nodes hosting a ready endpoint when `externalTrafficPolicy: Local`). For a `ClusterIP` Service it is `<clusterIP>:<port>`. Kubernetes then routes the connection to a pod as usual.
4. For each client connection, NautilusLB applies the ACL and connection limits, optionally reads a PROXY header and the TLS ClientHello, picks a healthy backend with the pool's algorithm, connects (trying up to 3 backends) and copies bytes in both directions until both sides finish.

🔝 [back to top](#nautiluslb)

&nbsp;

## Why NautilusLB

NautilusLB takes a different approach from per-Service cloud load balancers and service meshes.

🔝 [back to top](#nautiluslb)

&nbsp;

### Reverse Discovery Architecture

Instead of every Service provisioning its own external load balancer, NautilusLB implements a **reverse discovery pattern**: it discovers opted-in Services from the Kubernetes API and presents them on its own listeners.

- **Resource efficiency:** one NautilusLB deployment replaces a cloud load balancer per Service.
- **Central control:** listeners, ACLs, limits and health checks live in one reviewed configuration file.
- **Works anywhere:** on-premise, bare metal, edge sites or any cloud, with no cloud-controller integration.
- **Fast convergence:** informers push Service, EndpointSlice and Node changes; there is no polling interval.

🔝 [back to top](#nautiluslb)

&nbsp;

### Security Advantages

- **Explicit binding:** a port name is never enough. A tenant who can create a Service cannot attach it to a listener unless the operator allowlisted its namespace and the Service names that listener.
- **Least privilege:** NautilusLB only reads from the API (`list` and `watch`), with namespaced Roles wherever possible, and runs as a non-root distroless image.
- **Controlled exposure:** internal services (databases, brokers) can be published on a private address only, behind a CIDR allowlist.
- **No TLS keys:** TLS is routed by SNI and passed through; NautilusLB never holds certificates.

🔝 [back to top](#nautiluslb)

&nbsp;

### Operational Benefits

- **Hot reload:** `SIGHUP` (or file watching) applies configuration changes without dropping established connections; an invalid file is rejected as a whole.
- **Graceful drain:** readiness turns 503 before any listener closes, so a VIP or endpoint controller can move traffic away first.
- **Observable:** Prometheus metrics, one JSON access-log line per connection and optional pprof.
- **Stateless:** instances share nothing, so high availability is two or more identical instances behind a floating IP.

🔝 [back to top](#nautiluslb)

&nbsp;

### Comparison to Traditional Solutions

| Aspect | Traditional Cloud LB | Service Mesh | NautilusLB |
| --- | --- | --- | --- |
| **Resource Usage** | High (per-service) | High (sidecar per pod) | Low (a few instances) |
| **Configuration** | Cloud-specific | Complex mesh config | One strict YAML file |
| **Cost** | Pay per LB instance | Infrastructure overhead | Single deployment cost |
| **Security** | Multiple entry points | Complex policy mesh | Single controlled entry |
| **Vendor Lock-in** | High | Medium | None |
| **Operational Overhead** | Medium-High | High | Low |

NautilusLB is a Layer 4 proxy: it does not terminate TLS, authenticate clients or inspect payloads. Put those in the backends, or in a TLS-terminating proxy behind NautilusLB.

🔝 [back to top](#nautiluslb)

&nbsp;

## Key Features

- **Protocols:** plain TCP, TLS passthrough with SNI routing (exact and `*.` wildcard hosts, default route) and UDP with per-client sessions.
- **Explicit Service binding:** enabled annotation + named configuration (or `<config>/<route>`) + namespace allowlist + named port with matching protocol. See [docs/security.md](docs/security.md#tenant-hijack-prevention).
- **Informer-based discovery:** Nodes, Services and EndpointSlices, with `externalTrafficPolicy: Local` support, node filtering (ready only, skip cordoned, skip control plane, label selector) and dual-stack address selection (`ipv4`, `ipv6`, `prefer-ipv4`, `prefer-ipv6`). An unsynced store never empties a pool.
- **Balancing:** `round_robin`, `least_conn`, `source_ip_hash` (consistent hash, identical across instances) and `random_two_choices`, with per-Service weights and slow start.
- **Health checks:** active `tcp` or `http` probes with `rise`/`fall`/`jitter`, plus passive ejection on backend-attributable dial failures. Local resource errors never eject a backend.
- **PROXY protocol:** inbound v1/v2 parsed only from trusted CIDRs (optionally required), outbound `v1` or `v2` to backends.
- **ACLs and limits:** CIDR allow/deny per listener; connection limits process-wide, per listener, per source IP and per backend; UDP session caps.
- **Efficient data path:** Linux zero-copy `splice(2)` with the half-close and write-stall bounds enforced out of band; no idle timeout on established connections by default.
- **Lifecycle:** all listeners bind or the process exits; graceful drain with readiness; hot reload via `SIGHUP` or file watching, all or nothing.
- **Observability:** Prometheus metrics, a JSON access log (bounded, never blocks the proxy) and opt-in pprof on a loopback admin server.
- **Packaging:** distroless non-root image (UID 65532) for `linux/amd64` and `linux/arm64`, a Helm chart, raw manifests, a hardened systemd unit and cosign-signed releases.

🔝 [back to top](#nautiluslb)

&nbsp;

## Quick Start (Helm)

**1. Write a values file.** The chart renders `config` into a ConfigMap and derives the container ports, the Service and the RBAC from it. This example listens on `:80` and `:443` and binds Services in `ingress-nginx`:

```yaml
# values.yaml
service:
  type: LoadBalancer
config:
  configurations:
    - name: http
      protocol: tcp
      listenerAddress: ":80"
      namespaces: [ingress-nginx]
      backendPortName: http
    - name: https
      protocol: tcp
      listenerAddress: ":443"
      namespaces: [ingress-nginx]
      backendPortName: https
```

**2. Install the chart.**

```bash
helm install nautiluslb oci://ghcr.io/cloudresty/charts/nautiluslb \
  --version 2.0.0 \
  --namespace nautiluslb --create-namespace \
  --values values.yaml
```

Every namespace listed in `namespaces` must exist at install time, because the chart creates a Role in each. See [`deploy/helm/nautiluslb/values.yaml`](deploy/helm/nautiluslb/values.yaml) for `mode: DaemonSet`, `hostNetwork`, privileged ports, the ServiceMonitor and the NetworkPolicy.

**3. Annotate the backend Service.**

```bash
kubectl -n ingress-nginx annotate service ingress-nginx-controller \
  nautiluslb.cloudresty.io/enabled="true" \
  nautiluslb.cloudresty.io/configurations="http,https"
```

The Service must have ports named `http` and `https` (TCP).

**4. Verify.**

```bash
kubectl -n nautiluslb port-forward deploy/nautiluslb 9090:9090 &
curl -s 127.0.0.1:9090/readyz      # {"status":"ready"} once discovery has synced
curl -s 127.0.0.1:9090/metrics | grep '^nautiluslb_pool_backends'
```

`nautiluslb_pool_backends{state="healthy"}` should be non-zero for the `http` and `https` pools. If a pool stays empty, check the annotations, the namespace allowlist and the port name; NautilusLB logs a warning for a Service that names an unknown pool or sits outside the allowlist.

🔝 [back to top](#nautiluslb)

&nbsp;

## Configuration

NautilusLB reads one YAML file: `config.yaml` in the working directory by default (`/nautiluslb/config.yaml` in the image), or the path given by `--config` / `NLB_CONFIG`. Parsing is strict: unknown keys are errors and every problem is reported at once. Check a file without touching the network:

```bash
nautiluslb --validate --config config.yaml
```

A minimal v2 configuration:

```yaml
apiVersion: nautiluslb.cloudresty.io/v2
kind: Config

settings:
  kubernetes:
    kubeconfig: /nautiluslb/kubeconfig   # outside the cluster only
  admin:
    address: "127.0.0.1:9090"            # /metrics /healthz /readyz; "" disables

configurations:
  - name: http
    listenerAddress: ":80"
    namespaces: [ingress-nginx]
    backendPortName: http

  - name: https
    protocol: tls                        # SNI routing, TLS passthrough
    listenerAddress: ":443"
    namespaces: [ingress-nginx]
    backendPortName: https
    tls:
      defaultRoute: web
      routes:
        - name: web                      # Services bind with "https/web"
          hosts: ["example.com", "*.example.com"]

  - name: mongodb
    listenerAddress: "10.0.0.10:27017"   # private address only
    namespaces: [databases]
    backendPortName: mongodb
    balancer:
      algorithm: least_conn
    access:
      allow: ["10.0.0.0/8"]
```

- Every field, default, environment override and reload behaviour: [docs/configuration.md](docs/configuration.md).
- A complete commented example: [`app/config.example.yaml`](app/config.example.yaml).
- Command-line flags (`--config`, `--validate`, `--pprof`, `--version`): [docs/README.md](docs/README.md#command-line).

🔝 [back to top](#nautiluslb)

&nbsp;

## Service Annotations

| Annotation | Value | Meaning |
| --- | --- | --- |
| `nautiluslb.cloudresty.io/enabled` | `"true"` (exactly) | the Service may be a backend |
| `nautiluslb.cloudresty.io/configurations` | comma-separated entries | `<config>` for a `tcp`/`udp` configuration, `<config>/<route>` for a route of a `tls` configuration |
| `nautiluslb.cloudresty.io/weight` | `"1"` … `"100"` (default `1`) | weight of every backend of this Service |

A Service is a backend of a pool only when **all** of these hold:

1. `nautiluslb.cloudresty.io/enabled` is `"true"`;
2. `nautiluslb.cloudresty.io/configurations` names the pool (`<config>`, or `<config>/<route>` for a TLS route);
3. its namespace is in the configuration's `namespaces` (or the configuration uses `["*"]`);
4. it has a port named `backendPortName` with the right protocol (`UDP` for `udp` pools, `TCP` otherwise).

```yaml
apiVersion: v1
kind: Service
metadata:
  name: ingress-nginx-controller
  namespace: ingress-nginx
  annotations:
    nautiluslb.cloudresty.io/enabled: "true"
    nautiluslb.cloudresty.io/configurations: "http,https/web"
spec:
  type: NodePort
  ports:
    - {name: http,  port: 80,  targetPort: http,  protocol: TCP}
    - {name: https, port: 443, targetPort: https, protocol: TCP}
```

A plain `<config>` entry naming a `tls` configuration binds no route; name each route explicitly. See [binding rules](docs/configuration.md#binding-rules-the-security-model).

🔝 [back to top](#nautiluslb)

&nbsp;

## Kubernetes RBAC

NautilusLB only reads from the API: `list` and `watch`, nothing else (no Secrets, no write verbs, no Leases).

| Resource | Scope | Verbs | When |
| --- | --- | --- | --- |
| `nodes` | ClusterRole | `list`, `watch` | always |
| `services`, `discovery.k8s.io/endpointslices` | Role in each allowlisted namespace | `list`, `watch` | scoped configurations |
| `services`, `discovery.k8s.io/endpointslices` | ClusterRole | `list`, `watch` | only if a configuration uses `namespaces: ["*"]` |

The Helm chart renders exactly these rules from `config.configurations[].namespaces`. The raw manifests are [`rbac-cluster.yaml`](deploy/kubernetes/rbac-cluster.yaml), [`rbac-namespaced.yaml`](deploy/kubernetes/rbac-namespaced.yaml) and [`rbac-clusterwide.yaml`](deploy/kubernetes/rbac-clusterwide.yaml).

Outside the cluster, build the kubeconfig from a token for this ServiceAccount, never from an administrator's kubeconfig. If the permissions are missing, `/readyz` stays 503 with `discovery not synced`. Details: [docs/security.md](docs/security.md#rbac-least-privilege).

🔝 [back to top](#nautiluslb)

&nbsp;

## Deployment Options

| Option | Where | Start here |
| --- | --- | --- |
| Helm chart | in the cluster (Deployment or DaemonSet, optional `hostNetwork`) | [Quick Start](#quick-start-helm), [`deploy/helm/nautiluslb`](deploy/helm/nautiluslb/values.yaml) |
| Raw manifests | in the cluster, via kustomize | `kubectl apply -k deploy/kubernetes` ([`deploy/kubernetes`](deploy/kubernetes/kustomization.yaml)); edit `configmap.yaml` and `rbac-namespaced.yaml` first |
| Docker | a host outside the cluster | [below](#docker) |
| systemd | VMs or bare metal, usually a pair sharing a VIP | [`deploy/systemd/README.md`](deploy/systemd/README.md) |

The raw manifests mirror the chart's defaults; `deploy/check-drift.sh` keeps them in sync.

🔝 [back to top](#nautiluslb)

&nbsp;

### Docker

```bash
docker run --detach \
  --name nautiluslb \
  --restart unless-stopped \
  --env NLB_ADMIN_ADDRESS=0.0.0.0:9090 \
  --volume /etc/nautiluslb/config.yaml:/nautiluslb/config.yaml:ro \
  --volume /etc/nautiluslb/kubeconfig:/nautiluslb/kubeconfig:ro \
  --publish 80:80 \
  --publish 443:443 \
  --publish 10.0.0.10:27017:27017 \
  --publish 127.0.0.1:9090:9090 \
  cloudresty/nautiluslb:v2.0.0
```

- Set `settings.kubernetes.kubeconfig: /nautiluslb/kubeconfig` in `config.yaml`. Use a dedicated, [least-privilege](#kubernetes-rbac) ServiceAccount token kubeconfig, never an administrator's `~/.kube/config`.
- The image is distroless and runs as UID/GID 65532 with no shell. Mounted files must be readable by that UID: `chown 65532:65532 kubeconfig && chmod 0400 kubeconfig`.
- Inside the container the admin server must listen on a non-loopback address to be published; publish it on the host's loopback (`127.0.0.1:9090:9090`) or a private, firewalled address.
- Publish internal services (databases, brokers) on a private host address only, as with `10.0.0.10:` above. With `--network host`, set that address in `listenerAddress` instead.
- Use a version tag, or better a digest, never `latest`, in production.

🔝 [back to top](#nautiluslb)

&nbsp;

## Monitoring

The admin server (default `127.0.0.1:9090`, GET and HEAD only) serves:

| Path | Purpose |
| --- | --- |
| `/metrics` | Prometheus text or OpenMetrics |
| `/healthz`, `/health/live` | liveness: 200 while the process serves HTTP |
| `/readyz`, `/health/ready` | readiness: 200 once discovery has synced; 503 while starting or draining |
| `/debug/pprof/*` | only with `--pprof`, `settings.admin.pprof: true` or `NLB_PPROF=true` |

Key metrics (all prefixed `nautiluslb_`; full catalogue, labels and example alerts in [docs/metrics.md](docs/metrics.md)):

| Metric | Watch for |
| --- | --- |
| `nautiluslb_pool_backends{state}` | a pool with no healthy backends (it fails open) or no backends at all (binding problem) |
| `nautiluslb_connections_active`, `nautiluslb_connections_accepted_total` | load per listener |
| `nautiluslb_connections_rejected_total{reason}` | ACL, limit, SNI, `no_backend` and `dial_failed` rejections |
| `nautiluslb_backend_dial_total{result}` | backend connect failures; `local` means descriptor or port exhaustion on the NautilusLB host |
| `nautiluslb_discovery_informer_synced` | missing RBAC or API connectivity |
| `nautiluslb_config_reload_total{result}` | rejected reloads |
| `nautiluslb_pipe_mode_total{mode}` | whether the Linux `splice` path is in use |
| `nautiluslb_accesslog_dropped_total` | access-log records dropped under load |

Every connection also produces one JSON access-log line on stdout (configurable, see [docs/operations.md](docs/operations.md#access-log)). Metric labels never carry client addresses or SNI names; set `settings.admin.metrics.perBackend: false` to drop the per-backend series on large NodePort clusters.

🔝 [back to top](#nautiluslb)

&nbsp;

## High Availability

NautilusLB keeps no shared state and needs no leader election. Run two or more identical instances and move a floating IP between them with keepalived (VRRP) or kube-vip, using `/readyz` as the health check.

On `SIGTERM`, `/readyz` turns 503 for `settings.drain.readinessDelay` (3s) before any listener closes, then open connections get `settings.drain.timeout` (30s) to finish. Give the supervisor at least `drain.timeout + readinessDelay + 15s` (48s by default). Full keepalived and kube-vip examples: [docs/ha.md](docs/ha.md).

🔝 [back to top](#nautiluslb)

&nbsp;

## Upgrading to v2.0.0

v2.0.0 follows v1.0.0 (published images up to `v0.0.11`). It closes a traffic-hijacking hole, so **every deployment needs edits to both `config.yaml` and the annotated Services**:

- **Services must name their configurations.** Add `nautiluslb.cloudresty.io/configurations: "<name>[,<name>...]"` next to `nautiluslb.cloudresty.io/enabled: "true"`. In v1.0.0, any enabled Service with a matching port name, in any namespace, joined a listener's pool. This annotation is ignored by v1.0.0, so apply it first.
- **Every configuration needs `namespaces`.** An empty namespace no longer means "everywhere"; `["*"]` opts into cluster-wide discovery deliberately.
- `apiVersion: nautiluslb.cloudresty.io/v2` and `kind: Config` are **required** at the top of `config.yaml`, and the file is parsed strictly: unknown keys and invalid values stop startup.
- `settings.kubeconfigPath` becomes `settings.kubernetes.kubeconfig`, `requestTimeout: 5` becomes `dialTimeout: 5s` and `namespace: x` becomes `namespaces: [x]`. The old keys still load, with deprecation warnings.
- `ClusterIP` Services are dialled on their `port`, not `targetPort`.
- RBAC needs `list` + `watch` on `nodes`, `services` and `discovery.k8s.io/endpointslices`. Apply it **before** rolling out v2.
- The image is distroless and runs as UID 65532: mounted files must be readable by it, and `/root/.kube/config` no longer works.
- An admin HTTP server opens on `127.0.0.1:9090` by default (`settings.admin.address: ""` disables it).
- Per-connection `Info` log lines are replaced by one JSON access-log record per connection.
- The stop timeout must cover the drain (48s by default).

Run `nautiluslb --validate --config config.yaml` with the v2 binary before switching. Roll back only with your v1.0.0 config file: v1.0.0 silently ignores keys it does not know, so it would run a v2 file without its namespace restrictions. The full table, before/after examples, the checklist and rollback notes are in [docs/upgrading-v2.md](docs/upgrading-v2.md).

🔝 [back to top](#nautiluslb)

&nbsp;

## Verifying Releases

Release artifacts (the binaries via `sha256sums.txt`, the container image and the Helm chart) are signed with Sigstore cosign keyless signing by this repository's release workflow. Verify them before deploying:

```bash
VERSION=v2.0.0
ISSUER=https://token.actions.githubusercontent.com
IDENTITY="https://github.com/cloudresty/nautiluslb/.github/workflows/release.yaml@refs/tags/${VERSION}"

# Container image (Docker Hub)
cosign verify "cloudresty/nautiluslb:${VERSION}" \
  --certificate-oidc-issuer "$ISSUER" --certificate-identity "$IDENTITY"

# Helm chart (GHCR; chart versions have no leading "v")
cosign verify "ghcr.io/cloudresty/charts/nautiluslb:${VERSION#v}" \
  --certificate-oidc-issuer "$ISSUER" --certificate-identity "$IDENTITY"

# Binaries: verify the signed checksum list, then the binaries against it
cosign verify-blob --bundle sha256sums.txt.sigstore.json \
  --certificate-oidc-issuer "$ISSUER" --certificate-identity "$IDENTITY" \
  sha256sums.txt
sha256sum --check --ignore-missing sha256sums.txt
```

Verification needs cosign v3. The identity pins the release workflow and the tag, so a signature made by any other workflow or ref is rejected. Each GitHub release also carries an SPDX SBOM per binary and a `.sigstore.json` bundle for every signed file; the container image has SBOM and provenance attestations attached.

Pin the image by digest (`cloudresty/nautiluslb@sha256:...`) in production. Verifying the binary checksums is shown in [`deploy/systemd/README.md`](deploy/systemd/README.md); details in [docs/security.md](docs/security.md#signed-releases).

🔝 [back to top](#nautiluslb)

&nbsp;

## Documentation

| Document | Read it when you need to |
| --- | --- |
| [docs/README.md](docs/README.md) | get started and see the command-line flags |
| [docs/configuration.md](docs/configuration.md) | write or review `config.yaml`, annotations and binding rules |
| [docs/security.md](docs/security.md) | understand the threat model, PROXY trust, admin exposure and RBAC |
| [docs/metrics.md](docs/metrics.md) | scrape and alert on NautilusLB |
| [docs/operations.md](docs/operations.md) | run it: signals, reload, drain, sizing, kernel settings, troubleshooting |
| [docs/ha.md](docs/ha.md) | run two or more instances behind a floating IP |
| [docs/upgrading-v2.md](docs/upgrading-v2.md) | move a v1.x deployment to v2.0.0 |

🔝 [back to top](#nautiluslb)

&nbsp;

## Contributing

Contributions are welcome! See [CONTRIBUTING.md](CONTRIBUTING.md) for the branch model, the `make` targets (unit, integration, fuzz and kind-based end-to-end tests) and how to add a metric or a configuration field.

🔝 [back to top](#nautiluslb)

&nbsp;

## License

NautilusLB is released under the [MIT License](LICENSE.txt).

🔝 [back to top](#nautiluslb)

&nbsp;

---

&nbsp;

An open source project brought to you by the [Cloudresty](https://cloudresty.com) team.

[Website](https://cloudresty.com) &nbsp;|&nbsp; [LinkedIn](https://www.linkedin.com/company/cloudresty) &nbsp;|&nbsp; [BlueSky](https://bsky.app/profile/cloudresty.com) &nbsp;|&nbsp; [GitHub](https://github.com/cloudresty) &nbsp;|&nbsp; [Docker Hub](https://hub.docker.com/r/cloudresty/)

&nbsp;
