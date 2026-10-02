# Security

- [Threat model](#threat-model)
- [Tenant hijack prevention](#tenant-hijack-prevention)
- [PROXY protocol trust](#proxy-protocol-trust)
- [SNI is routing, not authentication](#sni-is-routing-not-authentication)
- [UDP source spoofing](#udp-source-spoofing)
- [Admin endpoint](#admin-endpoint)
- [RBAC least privilege](#rbac-least-privilege)
- [Process and image hardening](#process-and-image-hardening)
- [Signed releases](#signed-releases)
- [Checklist](#checklist)

## Threat model

NautilusLB is a Layer 4 proxy facing untrusted networks. It forwards to backends inside a Kubernetes cluster that it discovers through the API.

| Actor | Can | Must not be able to |
| --- | --- | --- |
| Internet client | open connections and send bytes and datagrams to listeners | reach a backend that no configuration binds, read admin or pprof data, exhaust the process beyond configured limits, forge its source address in logs or ACLs |
| Cluster tenant (can create Services in its own namespace) | annotate its own Services | join a pool of a configuration that does not allowlist its namespace. Within an allowlisted namespace, join a pool its Service does not name. |
| Upstream balancer (sends PROXY headers) | set the client address that NautilusLB logs and hashes on | do so unless its address is in `trustedCIDRs` |
| Operator | edit `config.yaml`, RBAC and the host | — |

What NautilusLB does **not** do: terminate TLS, authenticate clients, inspect payloads or rate-limit by request. Put those in the backends, or in a TLS-terminating proxy behind NautilusLB.

## Tenant hijack prevention

The binding rules are the core control. A Service receives traffic from a pool only when **all** of these hold. They are listed in full in [configuration.md](configuration.md#binding-rules-the-security-model):

1. `nautiluslb.cloudresty.io/enabled: "true"`;
2. `nautiluslb.cloudresty.io/configurations` names the pool explicitly (`<config>` or `<config>/<route>`);
3. the Service's namespace is in the configuration's `namespaces` allowlist;
4. a port named `backendPortName` with the matching protocol exists.

Consequences:

- A port name is never enough. A tenant cannot name a port `https` and receive part of `:443`.
- The allowlist is enforced by the operator in `config.yaml`, not by the tenant's annotation. Annotating a Service in a namespace that is not allowlisted does nothing, apart from a log warning.
- For a `tls` configuration, each SNI route is a separate pool and needs its own `<config>/<route>` entry. A Service bound to `https/api` never receives `www.example.com` traffic.
- Keep `namespaces` to namespaces whose Service creators you trust with that listener. `["*"]` hands this decision to annotations alone: any namespace whose users can create a Service with the right annotations joins the pool. Use it only on single-tenant clusters. It also needs cluster-wide RBAC.
- Weights (`nautiluslb.cloudresty.io/weight`) shift load only among Services that are already admitted.

Each scoped configuration reads only its own namespaces' informers. A missing permission elsewhere cannot stall it, and a missing permission for its own namespaces never wipes its existing backends.

## PROXY protocol trust

- A PROXY header is parsed only from peers in `proxyProtocol.in.trustedCIDRs`. From any other peer, the bytes are forwarded as ordinary payload and the transport address is used. An attacker therefore cannot inject a fake client address by prefixing `PROXY TCP4 ...`.
- Set `proxyProtocol.in.required: true` when every trusted peer is a balancer that always sends a header. A trusted peer that then omits it is rejected (`proxy_header`) instead of being treated as the client.
- Keep `trustedCIDRs` to the balancers' exact addresses, never to client-facing ranges.
- `access` (ACL) and `limits.maxConnectionsPerSource` apply to the **transport peer**, which is the upstream balancer when PROXY is in use. Filter clients at that balancer, or in the backends using the PROXY address that NautilusLB forwards with `proxyProtocol.out`. `source_ip_hash`, `proxyProtocol.out` and the access log's `proxySrc` use the PROXY source.
- `proxyProtocol.out` tells backends the client address. Enable it only towards backends that are configured to require PROXY. A backend that does not expect it treats the header as garbage, and a backend that accepts it from anywhere can be spoofed by anything else that reaches it.

## SNI is routing, not authentication

The SNI server name is sent in clear by the client and chosen by the client. NautilusLB uses it only to pick a pool. It does not check that the client completes a handshake for that name, or that the certificate the backend presents matches it. Any client can reach any route by sending its name, or reach `defaultRoute` by sending none.

Treat every route as reachable by every client that can reach the listener. Authorisation belongs in the backend, through TLS client certificates or application authentication. If a route must be private, give it a separate listener on a private address with an `access` allowlist.

## UDP source spoofing

UDP has no handshake. A spoofed source address can open sessions, each holding a backend socket and a buffer, and the backend's replies go to the spoofed address (reflection). The mitigations:

- `udp.maxSessions` (default 4096) and `udp.maxSessionsPerSource` bound how many sessions spoofed traffic can hold;
- `udp.sessionIdleTimeout` bounds how long an idle session holds them;
- `access.allow` restricts which sources may open sessions at all, where clients are known;
- network-level anti-spoofing (BCP 38) upstream.

The per-source limit counts the claimed source IP, so a spoofer varying sources is bounded only by `maxSessions` and `settings.limits.maxConnections`. Size them with [UDP memory](operations.md#udp-memory) in mind.

## Admin endpoint

- The default is `127.0.0.1:9090`, loopback only. `/metrics`, `/healthz` and `/readyz` are unauthenticated and accept only GET and HEAD.
- Binding a non-loopback address logs `Admin address is not loopback; metrics and pprof are exposed`. If you do this, for example so Prometheus can scrape from another host, use a private address and firewall it, or use a Kubernetes NetworkPolicy. Metrics carry no client addresses or SNI names. They do reveal configuration names and backend addresses.
- `/debug/pprof/*` is **off** by default. It is enabled only by `--pprof`, `settings.admin.pprof: true` or `NLB_PPROF=true`. pprof exposes heap and goroutine contents and lets callers run 30s CPU profiles. Enable it only temporarily, and only on a loopback admin address.
- `settings.admin.address: ""` disables the admin server entirely.

## RBAC least privilege

NautilusLB only reads from the API. It needs `list` and `watch` (informers), and nothing else: no `get` on Secrets, no write verbs, no Leases.

| Resource | Scope | Verbs | When |
| --- | --- | --- | --- |
| `nodes` | ClusterRole | `list`, `watch` | always (node addresses for NodePort backends) |
| `services` | Role per allowlisted namespace | `list`, `watch` | scoped configurations |
| `endpointslices` (`discovery.k8s.io`) | Role per allowlisted namespace | `list`, `watch` | scoped configurations (`externalTrafficPolicy: Local`) |
| `services`, `endpointslices` | ClusterRole | `list`, `watch` | only if a configuration uses `namespaces: ["*"]` |

The manifests are [`deploy/kubernetes/rbac-cluster.yaml`](../deploy/kubernetes/rbac-cluster.yaml), [`rbac-namespaced.yaml`](../deploy/kubernetes/rbac-namespaced.yaml) and [`rbac-clusterwide.yaml`](../deploy/kubernetes/rbac-clusterwide.yaml). The Helm chart renders the same rules. Outside the cluster, build the kubeconfig from a token for this ServiceAccount, never from an administrator's kubeconfig. Mount it read-only, readable by UID 65532 (`chmod 0400`).

## Process and image hardening

- The container image is `gcr.io/distroless/static-debian13:nonroot`. It runs as UID/GID 65532 with a static binary and has no shell or package manager.
- Ports below 1024 need either `net.ipv4.ip_unprivileged_port_start=0`, which is a namespaced pod sysctl unless `hostNetwork`, or `CAP_NET_BIND_SERVICE`. Do not run as root to get them. The Helm chart's `privilegedPortsMode` documents both options.
- Kubernetes: `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`. The chart sets these.
- systemd: the shipped [`nautiluslb.service`](../deploy/systemd/nautiluslb.service) runs unprivileged, with `AmbientCapabilities=CAP_NET_BIND_SERVICE` and filesystem and syscall hardening directives.
- Configuration is parsed strictly. A misspelt security key such as `acess:` is a startup error, never a silently open listener.
- Connection handling is bounded: dial 10s max, a 5s PROXY header wait, `tls.peekTimeout` and `tls.maxClientHello` for the ClientHello, and the half-close and write-stall bounds. Panics are contained per connection.

## Signed releases

Release artifacts are signed with Sigstore cosign keyless signing from the GitHub Actions release workflow of `cloudresty/nautiluslb`. The artifacts are the binaries (`sha256sums.txt`), the container image and the Helm chart (OCI). Verify the image before deploying:

```bash
cosign verify cloudresty/nautiluslb:v2.0.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/cloudresty/nautiluslb/\.github/workflows/'
```

Pin the image by digest (`cloudresty/nautiluslb@sha256:...`) in production.

## Checklist

- [ ] Every configuration's `namespaces` lists only namespaces you trust for that listener; no `["*"]` on multi-tenant clusters.
- [ ] Internal services (databases, brokers) listen on a private address (`"10.0.0.10:27017"`) and/or have an `access.allow` list.
- [ ] `trustedCIDRs` contains only your balancers. `required: true` where they always send PROXY.
- [ ] The admin server is on loopback or firewalled, and pprof is off.
- [ ] RBAC is limited to `list`/`watch` on nodes, services and endpointslices. Namespaced Roles are used where possible.
- [ ] The kubeconfig, if any, is a dedicated ServiceAccount token, read-only and mode 0400.
- [ ] UDP listeners have `maxSessions` and `maxSessionsPerSource` set.
- [ ] The image is verified with cosign and pinned by digest.
