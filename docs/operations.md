# Operations

- [Signals](#signals)
- [Startup](#startup)
- [Reload](#reload)
- [Shutdown and drain](#shutdown-and-drain)
- [Access log](#access-log)
- [Troubleshooting](#troubleshooting)
- [Sizing](#sizing)
- [Linux splice](#linux-splice)
- [Kernel settings](#kernel-settings)
- [Behavioural guarantees](#behavioural-guarantees)

## Signals

| Signal | Effect |
| --- | --- |
| `SIGHUP` | [reload](#reload) the configuration file |
| `SIGTERM`, `SIGINT` | graceful [shutdown](#shutdown-and-drain) |
| second `SIGTERM`/`SIGINT` during shutdown | stop waiting for connections and force-close them now |

The signal handler is installed before any listener binds, so a SIGTERM during startup always reaches the shutdown path. Under systemd, use `ExecReload=/bin/kill -HUP $MAINPID`, as in [`deploy/systemd/nautiluslb.service`](../deploy/systemd/nautiluslb.service). Set `TimeoutStopSec` and Kubernetes `terminationGracePeriodSeconds` to at least `drain.timeout + readinessDelay + 15s`; see [stop timeout](#stop-timeout).

## Startup

1. Load and validate the configuration. Any error exits 1 with `Failed to load configuration`.
2. Open the access log and build the Kubernetes client.
3. Bind **every** listener. If any bind fails, all listeners are released and the process exits 1 (`Failed to bind listener`). NautilusLB never serves a partial set of listeners.
4. Start the admin server. Until the remaining steps finish, `/readyz` returns 503 with reason `starting`. A bind error here exits 1.
5. Start pools (health checks) and discovery. Wait up to 60s for the informers' first sync.
6. Serve the listeners. `/readyz` turns 200 once discovery has synced. If the sync took longer than 60s, listeners serve anyway: every connection gets `no_backend`, and readiness says `discovery not synced` until it syncs.

## Reload

`SIGHUP`, or a modified file when `settings.reload.watchFile: true` (polled every 2s), triggers a reload:

1. The file is loaded and validated exactly like at startup. An error logs `Failed to reload configuration`, counts `config_reload_total{result="rejected"}` and changes nothing.
2. Configurations are compared by `name`. Each one is unchanged, updated in place (same address and protocol), replaced (address or protocol changed), added or removed. See the [table in configuration.md](configuration.md#hot-reload).
3. New pools are built and every added or replaced listener is **bound**. Any failure undoes what this reload bound or built and rejects the whole reload: `Configuration reload rejected, keeping the running configuration`.
4. Commit: updated listeners and pools switch to the new rules atomically. New listeners start serving. Removed or replaced listeners drain in the background for up to `drain.timeout` (`Force-closed connections of a retired listener` and `nautiluslb_drain_forced_total` if any remain). A replaced configuration keeps its pool objects for the pool keys it still has, so endpoints and health state survive an address move.
5. `settings.*` changes are not applied: `Settings changed but are not hot-reloadable, restart to apply` lists them.

Unchanged configurations are never touched: they keep the same listener, the same connections and the same health and balancer state. Connections already established on an updated listener keep the rules they were accepted under. Reload and shutdown are serialised, so a SIGTERM waits for a reload in progress.

Because step 3 binds new sockets before step 4 releases old ones:

- **Moving a listener address from one configuration to another in a single reload fails** with `bind: address already in use` (EADDRINUSE), and the whole reload is rejected. Do it in two reloads: remove the address from the old configuration and reload, then add it to the new one and reload again. A retired TCP/TLS listener closes its socket at once; a retired UDP listener keeps it until its sessions drain (up to `drain.timeout`), so wait for that before the second reload.
- **Changing a configuration's own `listenerAddress`** works in one reload: the new address is bound first, then the old listener drains.

## Shutdown and drain

1. `/readyz` returns 503 (`shutting down`) and `nautiluslb_ready` becomes 0.
2. Wait `settings.drain.readinessDelay` (3s) while still accepting, so a VIP or endpoint controller can move traffic away. See [ha.md](ha.md#drain-timing).
3. Stop discovery, so no endpoint update races a draining pool.
4. Close every listening socket, so new TCP connections are refused by the kernel. UDP listeners stop opening new sessions, and existing sessions keep forwarding. Wait up to `settings.drain.timeout` (30s) for TCP connections to finish and UDP sessions to idle out.
5. Force-close what is left (`Force-closed connections at shutdown`, field `forced`; counted per listener in `nautiluslb_drain_forced_total`).
6. Stop health checks, flush the access log (2s) and stop the admin server last, so a final scrape sees the drain. Then exit 0.

### Stop timeout

The worst case from SIGTERM to exit is `readinessDelay` (3s) + stopping discovery + `drain.timeout` (30s) + waiting for force-closed connections to finish (up to 5s) + stopping pools and the watchdog + closing the access log (up to 2s) + stopping the admin server (up to 2s), about 42s with the defaults. Give the supervisor `drain.timeout + readinessDelay + 15s`:

- Kubernetes: `terminationGracePeriodSeconds: 48` by default. The Helm chart derives it from `config.settings.drain` (each duration rounded up to whole seconds, then + 15), so `timeout: 1m30s` with `readinessDelay: 500ms` gives 90 + 1 + 15 = 106.
- systemd: `TimeoutStopSec=60` in [`deploy/systemd/nautiluslb.service`](../deploy/systemd/nautiluslb.service).

A shorter timeout makes the supervisor SIGKILL a drain in progress, which skips the force-close log line and the final access-log flush.

## Access log

There is one JSON line per TCP/TLS connection, written when it closes, and one per UDP session, written when it ends. TCP/TLS connections rejected before proxying (ACL, limits, PROXY, SNI, no backend, dial failure) also get a line. Rejected UDP datagrams that would have opened a session get none; they are counted only in `nautiluslb_connections_rejected_total`. Records go through a bounded queue (`settings.accessLog.bufferSize`) to one writer, and the proxy never waits for the log. Overflow drops records and counts them in `nautiluslb_accesslog_dropped_total`.

```json
{"time":"2026-10-02T12:00:00.123Z","listener":"https","protocol":"tls","pool":"https/web","backend":"10.0.1.7:30443","client":"198.51.100.9:51514","local":"10.0.0.10:443","proxySrc":"203.0.113.5:40022","sni":"www.example.com","durationMs":5321.4,"bytesIn":1834,"bytesOut":90211,"result":"ok","mode":"splice"}
```

| Field | Meaning |
| --- | --- |
| `time` | connection start (UTC) |
| `listener`, `protocol`, `pool` | configuration name, `tcp`/`tls`/`udp`, pool key |
| `backend` | `ip:port` that served it (absent if none) |
| `client` | transport peer `ip:port` |
| `local` | local `ip:port` (for UDP, the listener address) |
| `proxySrc` | source from an accepted inbound PROXY header |
| `sni` | ClientHello server name (tls) |
| `durationMs`, `bytesIn`, `bytesOut` | lifetime. `in` is client→backend and `out` is backend→client. |
| `result`, `error` | outcome (below) and the error text, if any |
| `mode` | `splice` or `generic` copy path |

The file output is opened once, in append mode. Rotate it with `copytruncate` (logrotate), or log to stdout and let the runtime collect it.

## Troubleshooting

### By access-log `result`

| `result` | Meaning | Look at |
| --- | --- | --- |
| `ok` | finished cleanly (both sides closed, or the UDP session idled out) | — |
| `client_reset` | the client side ended the connection with an error | client network, client timeouts |
| `backend_reset` | the backend side ended with an error. Also used when writing the outbound PROXY header fails, and when a UDP backend returns ICMP unreachable. | backend logs. Check that backends expect PROXY when `proxyProtocol.out` is set. |
| `timeout` | a safety bound fired: after a half-close the other side was idle for 2 min, or a peer stopped reading with data pending for 2 min | stuck peers |
| `idle_timeout` | `idleTimeout` elapsed with no bytes either way | raise or remove `idleTimeout` |
| `no_backend` | the pool is empty, or every candidate is at `maxConnectionsPerBackend` | [binding rules](configuration.md#binding-rules-the-security-model), `nautiluslb_pool_backends`, `Service is enabled but names no known configuration` warnings, RBAC |
| `dial_failed` | up to 3 backends tried, none connected | `nautiluslb_backend_dial_total{result}`, reachability from the LB host to node IPs or NodePorts or ClusterIPs, firewall |
| `acl` | denied by `access` | `access.allow`/`deny`. The check uses the transport peer, not the PROXY source. |
| `limit` | a connection limit was hit | `connections_rejected_total{reason=~"limit_.*"}` names which one |
| `proxy_header` | bad or missing PROXY header from a trusted peer | the upstream balancer's PROXY setting, `trustedCIDRs` |
| `sni_no_route` | no route for the server name and no `defaultRoute` | `sni` field, `tls.routes[].hosts` |
| `sni_error` | not TLS, ClientHello too large, or no ClientHello within `peekTimeout` | non-TLS clients on a `tls` listener, `tls.maxClientHello` |
| `draining` | a TCP/TLS connection arrived while its listener was draining, or a UDP session was closed by a drain | expected during shutdown and reload |
| `panic` | a recovered panic, contained to this connection | report a bug with the `Recovered panic` log line |

### By rejection reason

`nautiluslb_connections_rejected_total{reason}` uses the reasons listed in [metrics.md](metrics.md#rejection-reasons). Quick mapping:

- `limit_global`: raise `settings.limits.maxConnections` (restart required), or add capacity.
- `limit_listener`: raise `limits.maxConnections`, or `udp.maxSessions` for UDP (hot-reloadable).
- `limit_source`: a single client IP is over `maxConnectionsPerSource`/`maxSessionsPerSource`. Behind a NAT or an upstream balancer, all clients share one IP. Use PROXY protocol in, but note that the per-source limit still applies to the transport peer.
- `no_backend` / `limit_backend`: see above. `limit_backend` (UDP) means every candidate is at `maxConnectionsPerBackend`.
- `dial_failed` with `backend_dial_total{result="local"}`: the LB host is out of file descriptors or ephemeral ports. Backends are not ejected for this. See [Sizing](#sizing).

### Common log lines

| Message | Meaning |
| --- | --- |
| `Service is enabled but names no known configuration, ignoring it` | the annotation entry has a typo, or names a `tls` configuration without `/<route>` |
| `Service names configuration X but its namespace is not in that configuration's namespaces allowlist; ignoring` | the namespace is missing from `namespaces` |
| `Invalid weight annotation, using 1` | `nautiluslb.cloudresty.io/weight` is not an integer in 1..100 |
| `Unsupported service type in discovery, ignoring it` | an `ExternalName` Service is bound |
| `Discovery not synced yet, serving without endpoints` | informers did not sync within 60s. Check RBAC (`list` + `watch`) and API reachability. |
| `Admin address is not loopback; metrics and pprof are exposed` | `settings.admin.address` is reachable from the network |
| `Deprecated configuration` | v1 keys or bare-integer durations; see [upgrading-v2.md](upgrading-v2.md) |

Raise `settings.logLevel` to `debug` (restart required) to log each backend dial failure with its cause and whether it ejected the backend.

## Sizing

### File descriptors

| Per | Descriptors |
| --- | --- |
| TCP/TLS connection being proxied | 2 (client + backend) |
| UDP session | 1 (connected backend socket) |
| listener | 1 |
| health probe in flight | 1 per backend per probe |

Set `LimitNOFILE` or the container's `nofile` to at least `2 × peak connections + UDP sessions + 1024`. The shipped systemd unit sets `LimitNOFILE=1048576`. Exhaustion shows as `backend_dial_total{result="local"}` and `dial_failed`, never as ejected backends.

### Ephemeral ports

Every proxied TCP connection, and every UDP session, uses one local ephemeral port toward its backend. The limit is per backend `ip:port`, at about the size of `net.ipv4.ip_local_port_range` (28,232 ports by default) concurrent connections, plus sockets in `TIME_WAIT`. With many short connections to few backends, widen the range (`1024 65535`), enable `net.ipv4.tcp_tw_reuse=1`, or add backends. NodePort pools spread across nodes, which multiplies the available tuples.

### UDP memory

Each UDP session holds a receive buffer of `udp.bufferSize` bytes, plus a goroutine and a socket, about 10KB in total:

```text
memory ≈ maxSessions × (bufferSize + ~10KB)          per udp listener
default: 4096 × (65535 B + ~10KB) ≈ 310 MB
DNS-sized: 20000 × (4096 B + ~10KB) ≈ 287 MB
```

Lower `udp.bufferSize` to the largest datagram your protocol sends. 512–4096 is typical for DNS, syslog and game traffic. Keep `udp.maxSessions` bounded; `0` means unlimited and is warned about at load. Each listener socket also requests a 4 MiB kernel receive buffer, which the kernel caps at `net.core.rmem_max`.

### CPU

On the [splice path](#linux-splice), payload bytes do not cross into user space. CPU is dominated by connection setup (accept, dial, PROXY/SNI parsing) and by the access log. At very high connection rates, disable the access log (`NLB_ACCESS_LOG_ENABLED=false`) or send it to a file.

## Linux splice

On Linux (all architectures except 386), a TCP/TLS connection is copied with `splice(2)` when both sockets are plain TCP sockets and the kernel exposes the `TCP_INFO` byte counters that NautilusLB needs: `tcpi_bytes_acked`, `tcpi_bytes_received` and `tcpi_notsent_bytes`. **Linux 4.6 or newer** provides them. Otherwise, as on older kernels or other operating systems, the generic user-space copy is used with identical behaviour. The data a TLS or PROXY listener has already read (the ClientHello, PROXY leftovers) is written first, and the rest of the connection is spliced.

NautilusLB probes `splice(2)` once, at the first connection eligible for it. If a seccomp profile or systemd `SystemCallFilter` blocks the call (`EPERM`, `ENOSYS`, `EACCES`), it logs one warning, `splice(2) unavailable, using the generic copy path for all connections`, and serves every connection on the generic path for the life of the process. The probe matters because Go's own `ReadFrom` falls back to a user-space copy only on `EINVAL`: without it, a blocked `splice` would fail each connection with no data forwarded.

On the splice path, the 2-minute half-close and write-stall bounds and `idleTimeout` are enforced by one watchdog that samples `TCP_INFO` every 10s, so they fire with up to 10s of extra delay. The generic path enforces them with socket deadlines.

Check which path runs:

```promql
sum by (mode) (rate(nautiluslb_pipe_mode_total[5m]))
```

The access log's `mode` field shows it per connection.

## Kernel settings

| sysctl | Why | Suggested |
| --- | --- | --- |
| `fs.file-max`, `fs.nr_open` | descriptor ceiling above `LimitNOFILE` | ≥ 2 × `LimitNOFILE` |
| `net.ipv4.ip_local_port_range` | ephemeral ports toward backends | `1024 65535` |
| `net.ipv4.tcp_tw_reuse` | reuse `TIME_WAIT` ports for outgoing dials | `1` |
| `net.core.somaxconn`, `net.ipv4.tcp_max_syn_backlog` | accept backlog under connection bursts | `4096` or more |
| `net.core.rmem_max` | lets the UDP listener's 4 MiB receive buffer take effect | `4194304` or more |
| `net.ipv4.ip_nonlocal_bind` (`net.ipv6.ip_nonlocal_bind`) | binding a VIP the host does not hold yet | `1` (VRRP with VIP-specific `listenerAddress`) |
| `net.ipv4.ip_unprivileged_port_start` | non-root UID 65532 binding ports < 1024 | `0`, or grant `CAP_NET_BIND_SERVICE` |
| `net.netfilter.nf_conntrack_max` | if the host tracks connections (iptables or a firewall on the LB host) | ≥ 2 × peak connections |

## Behavioural guarantees

These hold in every release. A change that breaks one is a bug.

1. **A failed dial never proxies to nothing.** If no backend connects, the client connection is closed cleanly (`dial_failed`).
2. **Connect is bounded.** `dialTimeout` defaults to 5s and is capped at 10s. It bounds only the connect, never an established connection.
3. **Retry budget.** A connection tries at most 3 distinct backends.
4. **Fail-open.** If no backend in a pool is healthy, every backend is tried rather than none.
5. **No idle timeout by default.** Established connections stay open for as long as both sides keep them open: websockets, database sessions, AMQP. Dead peers are detected by TCP keepalive (30s idle, 10s × 3 probes, about 60s). After one side half-closes, the other direction must move a byte at least every 2 minutes. A write may stall for at most 2 minutes while data is waiting.
6. **Half-close is forwarded.** A clean EOF from one side is passed on as a half-close, and both directions always finish. An error on either side closes both.
7. **Failures are contained.** A panic in one connection closes that connection only.
8. **Rediscovery keeps state.** A backend that is still present after an endpoint update keeps its health state, connection counters and probe loop. Probe loops are added and removed as backends come and go.
9. **Discovery never empties a pool by accident.** A namespace or Nodes store that has not synced, or cannot be read, leaves the current backends in place.
10. **Shutdown is ordered.** Readiness turns 503 before any listener closes. The signal handler exists before anything binds.
11. **All or nothing.** All listeners bind at startup or the process exits. A reload applies completely or not at all. Accept errors are retried with backoff (up to 1s) instead of exiting.
12. **Explicit binding.** A Service is a backend only if it is enabled, names the configuration (or route), lives in an allowlisted namespace and has the named port with the right protocol. See [security.md](security.md).
13. **`namespaces` is required**, and `"*"` must be alone.
14. **Strict configuration.** The file must be a single YAML document. Unknown keys are errors, all errors are reported together, names are unique and listener conflicts are detected.
15. **ClusterIP Services are dialled on `port`**, not `targetPort`.
16. **Scoped configurations need only scoped RBAC.** Cluster-wide informers exist only if some configuration uses `"*"`.
17. **Local resource errors never eject backends** (EMFILE, ENFILE, EADDRNOTAVAIL, EAGAIN, ENOBUFS, EACCES).
