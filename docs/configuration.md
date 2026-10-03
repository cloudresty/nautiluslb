# Configuration reference

NautilusLB reads one YAML file. By default this is `config.yaml` in the working directory. Set another path with `--config` or `NLB_CONFIG`. A complete, commented example is [`app/config.example.yaml`](../app/config.example.yaml).

- [Loading rules](#loading-rules)
- [Top level](#top-level)
- [settings](#settings)
- [Environment overrides](#environment-overrides)
- [configurations](#configurations)
- [Service annotations](#service-annotations)
- [Binding rules](#binding-rules-the-security-model)
- [What a backend is](#what-a-backend-is)
- [Examples](#examples)
- [Hot reload](#hot-reload)

## Loading rules

- **Strict parsing.** An unknown key is an error, not a silently ignored line. The file must hold exactly one YAML document.
- **All errors at once.** Validation reports every problem in one message. Problems in a configuration are prefixed `configurations[<i>] (<name>)`. An invalid file stops startup with exit status 1 and the log message `Failed to load configuration`. On reload, an invalid file is rejected and the running configuration is kept.
- **Order.** The loader parses the file, applies defaults, applies the `NLB_*` [environment overrides](#environment-overrides) and then validates the result.
- **Durations** are strings parsed by Go's `time.ParseDuration`: `"200ms"`, `"30s"`, `"5m"`, `"1h"`. A bare integer is still accepted and read as seconds (v0.x style). It is deprecated and logged as a warning, except `0`.
- **Deprecations** are logged at startup as `Deprecated configuration` warnings, and `--validate` prints them. See [upgrading-v1.md](upgrading-v1.md).

## Top level

| Key | Type | Required | Validation |
| --- | --- | --- | --- |
| `apiVersion` | string | yes | must be `nautiluslb.cloudresty.io/v1`. A file with neither `apiVersion` nor `kind` is reported as a legacy v0.x file. |
| `kind` | string | yes | must be `Config` |
| `settings` | object | no | see [settings](#settings) |
| `configurations` | list | yes | at least one entry |

## settings

Process-wide settings. **`settings.*` is not hot-reloadable.** A reload that changes it applies the rest of the file. It logs which `settings` changes were ignored, and those changes take effect at the next restart.

### settings (root)

| Key | Type | Default | Validation | Env |
| --- | --- | --- | --- | --- |
| `settings.logLevel` | string | `info` | `debug`, `info`, `warn`, `error` (also `information`, `warning`; case-insensitive) | `NLB_LOG_LEVEL` |
| `settings.kubeconfigPath` | string | — | **deprecated** alias of `settings.kubernetes.kubeconfig`. If both keys are set to different values, validation fails. | |

### settings.kubernetes

| Key | Type | Default | Validation | Env |
| --- | --- | --- | --- | --- |
| `settings.kubernetes.kubeconfig` | string | `""` | kubeconfig path used outside the cluster. Precedence: in-cluster config first, then this path, then `$KUBECONFIG`, then `~/.kube/config`. | `NLB_KUBECONFIG` |
| `settings.kubernetes.qps` | number | `20` | > 0 (client-go rate limit) | |
| `settings.kubernetes.burst` | int | `40` | ≥ `qps` | |
| `settings.kubernetes.userAgent` | string | `nautiluslb/<version>` | free text | |

### settings.discovery

| Key | Type | Default | Validation |
| --- | --- | --- | --- |
| `settings.discovery.resyncPeriod` | duration | `5m` | > 0. Informer resync, which triggers a full recompute. |
| `settings.discovery.debounce` | duration | `200ms` | ≥ 0. Quiet time after the last event before a recompute. Events are coalesced for at most 2s. |
| `settings.discovery.nodes.readyOnly` | bool | `true` | drop nodes whose `Ready` condition is not `True` |
| `settings.discovery.nodes.skipUnschedulable` | bool | `true` | drop cordoned nodes (`spec.unschedulable`) |
| `settings.discovery.nodes.skipControlPlane` | bool | `false` | drop nodes labelled `node-role.kubernetes.io/control-plane` |
| `settings.discovery.nodes.addressFamily` | string | `ipv4` | `ipv4`, `ipv6`, `prefer-ipv4` or `prefer-ipv6`. Selects the node address (InternalIP preferred over ExternalIP) and the ClusterIP of a dual-stack Service. `ipv4` and `ipv6` are strict, and a node with no address in that family is dropped. The `prefer-*` values fall back to the other family. |
| `settings.discovery.nodes.selector` | map | `{}` | node label `key: value` pairs, all of which must match. Keys must be qualified names and values valid label values. |

The node filter applies to NodePort and LoadBalancer Services, where every eligible node is a backend.

### settings.admin

| Key | Type | Default | Validation | Env |
| --- | --- | --- | --- | --- |
| `settings.admin.address` | string | `127.0.0.1:9090` | `host:port`, or `""` to disable the admin server. A non-loopback address is logged as a warning. | `NLB_ADMIN_ADDRESS` (a set but empty value disables) |
| `settings.admin.pprof` | bool | `false` | also enabled by `--pprof` | `NLB_PPROF` |
| `settings.admin.metrics.perBackend` | bool | `true` | emit `backend`-labelled series. See [metrics.md](metrics.md#perbackend). | |

Admin endpoints accept only GET and HEAD. The endpoints are:

| Path | Response |
| --- | --- |
| `/metrics` | Prometheus text or OpenMetrics |
| `/healthz` and `/health/live` | `200 {"status":"ok"}` while the process serves HTTP |
| `/readyz` and `/health/ready` | `200 {"status":"ready"}`, or `503 {"status":"not ready","reason":"..."}` |
| `/debug/pprof/*` | only when pprof is enabled |

### settings.accessLog

| Key | Type | Default | Validation | Env |
| --- | --- | --- | --- | --- |
| `settings.accessLog.enabled` | bool | `true` | one JSON line per connection or UDP session. See [operations.md](operations.md#access-log). | `NLB_ACCESS_LOG_ENABLED` |
| `settings.accessLog.output` | string | `stdout` | `stdout`, `stderr` or an absolute file path. A file is opened once, in append mode, with mode 0640. | |
| `settings.accessLog.bufferSize` | int | `4096` | > 0. Records queued for the writer. On overflow, records are dropped and counted in `nautiluslb_accesslog_dropped_total`. The proxy never blocks on the log. | |

### settings.limits, settings.drain, settings.reload

| Key | Type | Default | Validation | Env |
| --- | --- | --- | --- | --- |
| `settings.limits.maxConnections` | int | `0` | ≥ 0. Process-wide cap on concurrent TCP/TLS connections plus UDP sessions. `0` means unlimited. | `NLB_MAX_CONNECTIONS` |
| `settings.drain.readinessDelay` | duration | `3s` | ≥ 0. Time between `/readyz` turning 503 and the listeners closing on shutdown. See [ha.md](ha.md#drain-timing). | |
| `settings.drain.timeout` | duration | `30s` | > 0. How long shutdown waits for open connections before force-closing them. | `NLB_DRAIN_TIMEOUT` |
| `settings.reload.watchFile` | bool | `false` | poll the file's modification time every 2s and reload on change, like SIGHUP | |

## Environment overrides

An environment variable that is set always wins over the file, even when its value is empty. Invalid values are reported together, and each report names its variable. Only these variables exist:

| Variable | Overrides | Format |
| --- | --- | --- |
| `NLB_CONFIG` | `--config` default | path |
| `NLB_LOG_LEVEL` | `settings.logLevel` | level name |
| `NLB_KUBECONFIG` | `settings.kubernetes.kubeconfig` (also clears the deprecated `kubeconfigPath`) | path |
| `NLB_ADMIN_ADDRESS` | `settings.admin.address` | `host:port`, or empty to disable |
| `NLB_PPROF` | `settings.admin.pprof` | boolean (`true`, `false`, `1`, `0`, ...) |
| `NLB_ACCESS_LOG_ENABLED` | `settings.accessLog.enabled` | boolean |
| `NLB_MAX_CONNECTIONS` | `settings.limits.maxConnections` | integer ≥ 0 |
| `NLB_DRAIN_TIMEOUT` | `settings.drain.timeout` | duration (`30s`) or integer seconds |

## configurations

Each entry is one listener. A `tcp` or `udp` configuration feeds one pool, keyed `<name>`. A `tls` configuration feeds one pool per SNI route, keyed `<name>/<route>`. Every pool has its own balancer, health checker and per-backend connection caps.

Unless stated otherwise, every key below is applied by a [hot reload](#hot-reload) without dropping established connections. A connection keeps the rules it was accepted under.

### Listener

| Key | Type | Default | Validation |
| --- | --- | --- | --- |
| `name` | string | — (required) | `^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`, unique. Referenced by the Service annotation. |
| `protocol` | string | `tcp` | `tcp`, `tls` or `udp`. A change replaces the listener on reload. |
| `listenerAddress` | string | — (required) | `":port"` (all addresses) or `"<ip>:port"`, port 1–65535. Two listeners of the same family conflict when they use the same port and either one is a wildcard, or both use the same IP. `tcp` and `tls` share the TCP family and `udp` is separate, so `:53` can be both a `tcp` and a `udp` listener. A change replaces the listener on reload. |
| `namespaces` | list of string | — (required) | DNS-1123 labels, or `["*"]` alone for cluster-wide discovery. See [namespaces](#namespaces-allowlist). |
| `namespace` | string | — | **deprecated** single-namespace alias, merged into `namespaces` |
| `backendPortName` | string | — | Kubernetes port name (IANA_SVC_NAME). Required unless every TLS route sets its own. |
| `dialTimeout` | duration | `5s` | ≥ 0. `0` means the default. Values above 10s are capped at 10s, with a warning. This bounds the backend **connect** only, never an established connection. Applies to `tcp` and `tls`. A UDP backend socket is opened with a fixed 5s bound. |
| `requestTimeout` | int (seconds) | — | **deprecated** alias of `dialTimeout`. If both are set and differ, validation fails. |
| `idleTimeout` | duration | `0` | ≥ 0. `0` means an established connection never times out. Otherwise the connection is closed after this long with no bytes in either direction. Not allowed with `udp`; use `udp.sessionIdleTimeout` instead. |

### balancer

| Key | Type | Default | Validation |
| --- | --- | --- | --- |
| `balancer.algorithm` | string | `round_robin` | `round_robin`, `least_conn`, `source_ip_hash`, `random_two_choices` |
| `balancer.slowStart` | duration | `0` | ≥ 0. `0` disables slow start. After a backend turns healthy, its weight ramps linearly from 10% to 100% over this period. Ignored by `source_ip_hash`, so a recovering backend does not reshuffle clients. |

| Algorithm | Behaviour |
| --- | --- |
| `round_robin` | Smooth weighted round-robin. With slow start, the schedule is re-derived in steps while a backend ramps. |
| `least_conn` | Lowest `active connections / weight`. Ties rotate. |
| `random_two_choices` | Samples two eligible backends and takes the one with the lower `active / weight`. |
| `source_ip_hash` | Consistent hash ring over the client IP. The client IP is the PROXY header source when one was accepted. The ring uses 160 × min(weight, 10) virtual nodes per backend, so weights above 10 count as 10 here. The hash is deterministic across processes, so every instance maps a client to the same backend when they see the same backend set. Unhealthy and capped backends are skipped by walking the ring. |

For every algorithm, backends at their `maxConnectionsPerBackend` cap are skipped. A new connection tries up to 3 distinct backends. If no backend is healthy, the pool **fails open** and picks from all its backends.

### health

| Key | Type | Default | Validation |
| --- | --- | --- | --- |
| `health.type` | string | `tcp` (`none` for `udp`) | `tcp` (connect), `http` (GET) or `none` (passive only) |
| `health.interval` | duration | `10s` | must be greater than `timeout` |
| `health.timeout` | duration | `2s` | > 0 |
| `health.rise` | int | `1` | ≥ 1. Consecutive successes needed to become healthy. |
| `health.fall` | int | `3` | ≥ 1. Consecutive failures needed to become unhealthy. |
| `health.jitter` | number | `0.2` | 0 ≤ jitter < 1. Each interval varies uniformly by ±jitter×interval, and the first probe is delayed uniformly within one interval. An explicit `0` is honoured. |
| `health.ejectionHold` | duration | `10s` | ≥ 0. After a passive ejection, probe successes are ignored for this long. |
| `health.port` | int | `0` | 0–65535. If not 0, the probe goes to the backend's IP on this port instead of the backend port. Required for `tcp` or `http` probes on a `udp` configuration. |
| `health.http.path` | string | `/` | must start with `/` |
| `health.http.host` | string | `""` | overrides the `Host` header |
| `health.http.expectStatus` | list of int | `[200, 204]` | each 100–599 |

**Passive ejection.** A failed backend dial ejects the backend immediately, for `ejectionHold`, when the error points at the backend: connection refused, host or network unreachable, or connect timeout. Local resource errors never eject a backend, so local exhaustion cannot empty a pool. These are `EMFILE`, `ENFILE`, `EADDRNOTAVAIL`, `EAGAIN`, `ENOBUFS` and `EACCES`. With `type: none`, an ejected backend is re-admitted at the first check after its hold expires. HTTP probes do not follow redirects and read at most 64KB of the body.

### limits

| Key | Type | Default | Validation |
| --- | --- | --- | --- |
| `limits.maxConnections` | int | `0` | ≥ 0. Concurrent connections on this listener; `0` means unlimited. Not used by `udp`; use `udp.maxSessions`. |
| `limits.maxConnectionsPerSource` | int | `0` | ≥ 0. Concurrent connections per client IP. The client IP is the transport peer, not the PROXY source. Not used by `udp`; use `udp.maxSessionsPerSource`. |
| `limits.maxConnectionsPerBackend` | int | `0` | ≥ 0. Concurrent connections, or UDP sessions, per backend. The picker skips a backend at its cap. On reload, a new cap applies to backends created after the reload. Existing backends keep their cap until discovery replaces them. |

Admission order for each connection is: ACL, then the global limit, then the listener limit, then the per-source limit.

### access

| Key | Type | Default | Validation |
| --- | --- | --- | --- |
| `access.allow` | list of CIDR | `[]` | `netip` prefixes such as `10.0.0.0/8` or `2001:db8::/32`. An empty list allows every address. |
| `access.deny` | list of CIDR | `[]` | evaluated first; deny wins |

The ACL is checked against the transport peer address before anything is read from the client.

### proxyProtocol

| Key | Type | Default | Validation |
| --- | --- | --- | --- |
| `proxyProtocol.in.trustedCIDRs` | list of CIDR | `[]` | A PROXY v1/v2 header is parsed only from these peers, within 5s. From any other peer, the bytes are proxied untouched. |
| `proxyProtocol.in.required` | bool | `false` | A trusted peer **must** send a header, or the connection is rejected (`proxy_header`). Requires `trustedCIDRs`. |
| `proxyProtocol.out` | string | `""` | `""`, `v1` or `v2`. Sends a PROXY header to the backend. Its source is the effective client address, which is the inbound header's source when one was accepted, otherwise the peer. Not allowed with `udp`. |

Inbound PROXY applies to `tcp` and `tls` listeners. A `udp` listener does not parse PROXY headers.

### tls (protocol: tls only)

TLS is never terminated. NautilusLB reads the ClientHello, matches the SNI server name to a route and replays the bytes it read to the chosen backend.

| Key | Type | Default | Validation |
| --- | --- | --- | --- |
| `tls.peekTimeout` | duration | `5s` | > 0. Time allowed for the client to send its ClientHello. |
| `tls.maxClientHello` | int | `16384` | 1–65535 bytes |
| `tls.defaultRoute` | string | `""` | must name a route. Used when the ClientHello has no SNI or the name matches no route. With `""`, such connections are closed (`sni_no_route`). |
| `tls.routes` | list | — (required) | at least one route |
| `tls.routes[].name` | string | — | same pattern as `name`, unique within the configuration. The pool key is `<config>/<route>`. |
| `tls.routes[].hosts` | list of string | — | at least one host. Hosts are lowercase DNS names. A wildcard is allowed only as a leading `*.` and matches **exactly one** label: `*.example.com` matches `a.example.com`, but not `example.com` or `a.b.example.com`. A host may appear in only one route. An exact match wins over a wildcard. |
| `tls.routes[].backendPortName` | string | configuration's | port name override |
| `tls.routes[].balancer` | object | configuration's | override `algorithm` and/or `slowStart` |
| `tls.routes[].health` | object | configuration's | field-wise override. A zero or empty field inherits, so `jitter: 0` cannot be set per route. |
| `tls.routes[].limits` | object | configuration's | only `maxConnectionsPerBackend` may be set; other keys are errors |

### udp (protocol: udp only)

A UDP session is keyed by the client address and port. Its backend is picked once. It forwards over a connected socket, so every datagram of a flow reaches the same backend, until the session has been idle for `sessionIdleTimeout`.

| Key | Type | Default | Validation |
| --- | --- | --- | --- |
| `udp.sessionIdleTimeout` | duration | `60s` | > 0. Idle sessions are swept every `sessionIdleTimeout/4`. |
| `udp.maxSessions` | int | `4096` | ≥ 0. Per-listener session cap. An explicit `0` means unlimited and is warned about at load. |
| `udp.maxSessionsPerSource` | int | `0` | ≥ 0. Sessions per client IP; `0` means unlimited. |
| `udp.bufferSize` | int | `65535` | 512–65535. Receive buffer per session. Memory is about `maxSessions × (bufferSize + ~10KB)`. See [operations.md](operations.md#udp-memory). |

The only health signal for UDP backends is passive. An ICMP port-unreachable reply surfaces as connection-refused on the connected socket. That ejects the backend and ends the session. `udp` configurations reject `tls`, `idleTimeout` and `proxyProtocol.out`. `tcp` configurations reject a `tls` or `udp` block.

## Service annotations

| Annotation | Value | Meaning |
| --- | --- | --- |
| `nautiluslb.cloudresty.io/enabled` | `"true"` (exactly) | the Service may be a backend |
| `nautiluslb.cloudresty.io/configurations` | comma-separated entries, whitespace-trimmed | each entry is `<config>` for a `tcp`/`udp` configuration, or `<config>/<route>` for a route of a `tls` configuration |
| `nautiluslb.cloudresty.io/weight` | `"1"` … `"100"` | weight of every endpoint of this Service, default `1`. An invalid value counts as `1` and is logged once. |

A plain `<config>` entry naming a `tls` configuration binds **no** route. Name each route explicitly, as in `https/web`. An enabled Service whose entries name no known pool is logged once, with a warning. So is a Service that names a pool whose namespace allowlist excludes the Service's namespace.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: ingress-nginx-controller
  namespace: ingress-nginx
  annotations:
    nautiluslb.cloudresty.io/enabled: "true"
    nautiluslb.cloudresty.io/configurations: "http,https/web"
    nautiluslb.cloudresty.io/weight: "10"
spec:
  type: NodePort
  ports:
    - {name: http,  port: 80,  targetPort: http,  protocol: TCP}
    - {name: https, port: 443, targetPort: https, protocol: TCP}
```

## Binding rules (the security model)

A Service is a backend of a pool only when **all** of these hold:

1. `nautiluslb.cloudresty.io/enabled` is exactly `"true"`;
2. `nautiluslb.cloudresty.io/configurations` contains an entry naming the pool: `<config>`, or `<config>/<route>` for a TLS route;
3. the Service's namespace is in the configuration's `namespaces`, or the list is `["*"]`;
4. it has a port named the pool's `backendPortName` **with the right protocol**: `UDP` for `udp` pools, and `TCP` or unset otherwise.

A port name alone never binds a Service. Without rule 2, any tenant who can create a Service could name a port `https` and receive part of a public listener's traffic. Without rule 3, a tenant in another namespace could name your configuration. See [security.md](security.md#tenant-hijack-prevention).

### namespaces allowlist

`namespaces` is required. Each configuration discovers Services and EndpointSlices only in its own namespaces, using one informer per namespace. This needs only namespaced RBAC.

`["*"]` opts that configuration into cluster-wide discovery. It needs cluster-wide `list`/`watch` on Services and EndpointSlices, and it must be the only entry. Other configurations stay scoped. A namespace whose informer has not synced, for example because of missing RBAC, never empties a pool: the pool keeps its current backends until the data is available.

## What a backend is

Backends are node or ClusterIP addresses, never pod IPs. Kubernetes routes the connection on to a pod.

| Service type | Backend addresses | Notes |
| --- | --- | --- |
| `NodePort`, `LoadBalancer` | `<node address>:<nodePort>` for every eligible node (see [node filter](#settingsdiscovery)) | Ports without a node port (`allocateLoadBalancerNodePorts: false`) are skipped. With `externalTrafficPolicy: Local`, only nodes that host a ready endpoint of the Service, according to its EndpointSlices, are used. |
| `ClusterIP` | `<clusterIP>:<port>` (the Service `port`, not `targetPort`) | reachable only where ClusterIPs are routed (in-cluster or on a node). With dual-stack, `addressFamily` selects the ClusterIP. Headless Services are skipped. |
| other (`ExternalName`) | none | logged once and ignored |

## Examples

Every example below is a complete file that `--validate` accepts. Merge the `configurations` you need into one file.

### Plain TCP with least connections and limits

```yaml
apiVersion: nautiluslb.cloudresty.io/v1
kind: Config
configurations:
  - name: mongodb
    listenerAddress: "10.0.0.10:27017"   # private address only
    namespaces: [databases]
    backendPortName: mongodb
    balancer:
      algorithm: least_conn
    limits:
      maxConnections: 2000
      maxConnectionsPerSource: 100
      maxConnectionsPerBackend: 500
    dialTimeout: 3s
    access:
      allow: ["10.0.0.0/8"]
      deny: ["10.66.0.0/16"]
```

### TLS passthrough with SNI routes

```yaml
apiVersion: nautiluslb.cloudresty.io/v1
kind: Config
configurations:
  - name: https
    protocol: tls
    listenerAddress: ":443"
    namespaces: [ingress-nginx, apps]
    backendPortName: https
    balancer: {algorithm: round_robin, slowStart: 30s}
    tls:
      defaultRoute: web                 # no SNI / unknown name -> web
      routes:
        - name: web                     # Services bind with "https/web"
          hosts: ["example.com", "*.example.com"]
        - name: api                     # Services bind with "https/api"
          hosts: ["api.example.com"]    # exact match wins over *.example.com
          backendPortName: api-https
          balancer: {algorithm: least_conn}
          health:
            type: http
            port: 8081
            http: {path: /ready}
          limits: {maxConnectionsPerBackend: 200}
```

### UDP

```yaml
apiVersion: nautiluslb.cloudresty.io/v1
kind: Config
configurations:
  - name: dns
    protocol: udp
    listenerAddress: ":53"
    namespaces: [dns]
    backendPortName: dns-udp            # Service port must be protocol: UDP
    balancer: {algorithm: source_ip_hash}
    udp:
      sessionIdleTimeout: 30s
      maxSessions: 20000
      maxSessionsPerSource: 50
      bufferSize: 4096                  # 20000 x (4KB + ~10KB) ~= 287MB worst case
```

### PROXY protocol in and out

An upstream L4 balancer at `192.0.2.0/28` prepends PROXY headers. The backends (ingress-nginx with `use-proxy-protocol: "true"`) expect PROXY v2.

```yaml
apiVersion: nautiluslb.cloudresty.io/v1
kind: Config
configurations:
  - name: http
    listenerAddress: ":80"
    namespaces: [ingress-nginx]
    backendPortName: http
    proxyProtocol:
      in:
        trustedCIDRs: ["192.0.2.0/28"]
        required: true                  # trusted peers must send one
      out: v2
```

### HTTP health probe and passive ejection tuning

```yaml
apiVersion: nautiluslb.cloudresty.io/v1
kind: Config
configurations:
  - name: http
    listenerAddress: ":80"
    namespaces: [ingress-nginx]
    backendPortName: http
    health:
      type: http
      interval: 5s
      timeout: 1s
      rise: 2
      fall: 2
      jitter: 0.1
      ejectionHold: 30s
      http:
        path: /healthz
        host: health.internal
        expectStatus: [200]
```

## Hot reload

Send `SIGHUP`, or set `settings.reload.watchFile: true`, to reload. The reload is all or nothing: any parse, validation or bind error rejects the whole reload and leaves everything running as it was. The runtime compares configurations by `name`:

| Change | Effect |
| --- | --- |
| unchanged | not touched: same listener, pool, health state and balancer state |
| same `listenerAddress` and `protocol`, other fields changed | updated in place. New connections use the new rules, and established connections keep theirs. Pool endpoints and health state are kept. |
| `listenerAddress` or `protocol` changed | the new socket is bound **first**, then the old listener drains (up to `settings.drain.timeout`). The new address must be free while the old one is still open. Switching between `tcp` and `tls` on the same address cannot bind, so the reload is rejected and the change needs a restart. |
| added | bound, then served |
| removed | drains, then its pools stop |
| `settings.*` | not applied; listed in a warning |

Because new sockets are bound before old ones are released, two moves are rejected in a single reload with `bind: address already in use` (EADDRINUSE):

- **Moving an address from one configuration to another.** Removing `listenerAddress: ":443"` from configuration `a` and adding it to configuration `b` (or renaming a configuration, which is a remove plus an add) in the same file change tries to bind `:443` for `b` while `a` still holds it. Do it in **two reloads**: first remove it from `a` (or remove `a`) and reload, then add it to `b` and reload again. A TCP/TLS address is free as soon as the first reload commits, because a retired TCP/TLS listener closes its socket before it drains connections; the address is unserved until the second reload. A retired UDP listener keeps its socket while its sessions drain (up to `settings.drain.timeout`), so wait for that drain to finish before the second reload.
- **Changing a configuration's own address** is fine: the new address is bound first, then the old listener drains. Only reusing the *old* address for something else in the same reload fails.

The reload procedure and drain order are described in [operations.md](operations.md#reload).
