# Metrics

NautilusLB serves Prometheus metrics at `http://<settings.admin.address>/metrics`. The default address is `127.0.0.1:9090`. Responses are Prometheus text, or OpenMetrics when the scraper asks for it. Every NautilusLB metric has the `nautiluslb_` prefix. The standard Go runtime (`go_*`) and process (`process_*`) collectors are registered too.

To scrape from another host, set `settings.admin.address` to a reachable address, which is logged as a warning, and restrict access to it. See [security.md](security.md#admin-endpoint).

## Labels

| Label | Values | Bounded by |
| --- | --- | --- |
| `listener` | configuration `name` | number of configurations |
| `pool` | `<config>` (tcp/udp) or `<config>/<route>` (tls) | configurations + routes |
| `backend` | `ip:port` of a backend | backends per pool. For a NodePort Service this is the number of eligible nodes, per port, per Service. |
| `result`, `reason`, `state`, `to`, `cause`, `event`, `direction`, `mode`, `resource` | fixed sets, listed below | constant |

No label ever carries a client address, a client port or an SNI server name.

## perBackend

`settings.admin.metrics.perBackend` (default `true`) controls the `backend` label:

| Metric | `perBackend: true` | `perBackend: false` |
| --- | --- | --- |
| `backend_dial_total` | `listener, pool, backend, result` | `listener, pool, result` |
| `backend_connections_active` | `listener, pool, backend` | `listener, pool` |
| `backend_health_transitions_total` | `listener, pool, backend, to, cause` | `listener, pool, to, cause` |
| `backend_healthy` | `listener, pool, backend` | **not registered** |

Every other metric is unaffected. Cardinality with `perBackend: true` is about `pools × backends × (6 dial results + 4 transition combinations + 2 gauges)`. A NodePort pool on a 200-node cluster has 200 backends per bound Service port. If that is too many series, turn `perBackend` off and use `nautiluslb_pool_backends`. Backend series are not deleted when a backend leaves a pool. They stay at their last value until the process restarts.

## Catalogue

### Process and configuration

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `nautiluslb_build_info` | gauge (always 1) | `version`, `commit`, `go_version` | build identity |
| `nautiluslb_ready` | gauge | — | 1 while `/readyz` returns 200 |
| `nautiluslb_config_reload_total` | counter | `result` = `applied`, `rejected`, `unchanged` | outcome of each SIGHUP or file-watch reload |
| `nautiluslb_config_last_reload_timestamp_seconds` | gauge | — | Unix time of the last **applied** reload |
| `nautiluslb_accesslog_dropped_total` | counter | — | access-log records dropped because the queue (`settings.accessLog.bufferSize`) was full or the log was closing |
| `nautiluslb_drain_forced_total` | counter | `listener` | TCP/TLS connections and UDP sessions force-closed because a drain deadline (`settings.drain.timeout`) expired, at shutdown or when a reload retired the listener. Incremented once per listener drain by the number force-closed. |

### Connections (TCP and TLS listeners)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `nautiluslb_connections_accepted_total` | counter | `listener` | connections that passed the ACL and the limits |
| `nautiluslb_connections_rejected_total` | counter | `listener`, `reason` | connections or UDP sessions closed before proxying. See [rejection reasons](#rejection-reasons). |
| `nautiluslb_connections_active` | gauge | `listener` | accepted connections still open |
| `nautiluslb_connection_duration_seconds` | histogram | `listener`, `result` | lifetime of connections that reached proxying. Buckets: 16 exponential from 0.01s to 3600s. |
| `nautiluslb_connection_bytes_total` | counter | `listener`, `direction` = `in` (client→backend), `out` (backend→client) | proxied bytes, counted when a connection closes |
| `nautiluslb_pipe_mode_total` | counter | `mode` = `splice`, `generic` | which copy path each proxied connection used. See [operations.md](operations.md#linux-splice). |

For TCP and TLS, `accepted = closed + rejected` (post-accept reasons). Connections rejected by `acl` or `limit_*` are never counted as accepted. `result` on `connection_duration_seconds` takes the values `ok`, `client_reset`, `backend_reset`, `timeout`, `idle_timeout` and `panic`. Their meanings are in the [operations.md access-log table](operations.md#access-log).

### Backends and pools

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `nautiluslb_backend_dial_total` | counter | `listener`, `pool`, `backend`¹, `result` = `ok`, `refused`, `unreachable`, `timeout`, `local`, `unknown` | backend connect outcomes. `refused`, `unreachable` and `timeout` eject the backend. `local` (EMFILE, ENFILE, EADDRNOTAVAIL, EAGAIN, ENOBUFS, EACCES) never does. |
| `nautiluslb_backend_dial_duration_seconds` | histogram | `listener`, `pool` | connect latency. Buckets: 14 exponential from 0.001s to 10s. |
| `nautiluslb_backend_connections_active` | gauge | `listener`, `pool`, `backend`¹ | in-flight connections or UDP sessions per backend |
| `nautiluslb_backend_healthy` | gauge | `listener`, `pool`, `backend` | 1 healthy, 0 unhealthy. Set on transitions. Registered only with `perBackend: true`. |
| `nautiluslb_backend_health_transitions_total` | counter | `listener`, `pool`, `backend`¹, `to` = `healthy`, `unhealthy`, `cause` = `probe`, `passive` | health flaps. `passive` covers ejection after a failed dial, and re-admission after the hold when `health.type: none`. |
| `nautiluslb_pool_backends` | gauge | `listener`, `pool`, `state` = `healthy`, `unhealthy` | pool size by state |
| `nautiluslb_health_probe_duration_seconds` | histogram | `listener`, `pool`, `result` = `ok`, `fail` | active probe latency. A probe cancelled by shutdown or removal is not recorded. Buckets: 14 exponential from 0.001s to 10s. |

¹ dropped with `perBackend: false`.

### UDP

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `nautiluslb_udp_sessions_active` | gauge | `listener` | open sessions |
| `nautiluslb_udp_sessions_total` | counter | `listener`, `event` = `open`, `expired`, `error` | `expired` covers idle expiry and sessions closed by drain. `error` covers a backend read or write error, after which the backend is ejected when the error is refused or unreachable. |
| `nautiluslb_udp_datagrams_total` | counter | `listener`, `direction` = `in` (client→backend), `out` | forwarded datagrams |

UDP listeners do not emit `connections_accepted_total`, `connections_active` or `connection_duration_seconds`. Rejected new sessions are counted in `connections_rejected_total`.

### Discovery

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `nautiluslb_discovery_reconcile_total` | counter | `result` = `applied`, `unchanged`, `skipped` | `applied`: at least one pool's endpoints changed. `unchanged`: nothing changed. `skipped`: some pool was not recomputed because its namespace store, or the Nodes store a NodePort pool needs, has not synced. |
| `nautiluslb_discovery_reconcile_duration_seconds` | histogram | — | recompute time. Buckets: 14 exponential from 0.001s to 30s. |
| `nautiluslb_discovery_informer_synced` | gauge | `resource` = `nodes`, `services`, `endpointslices` | 1 when synced. For `services` and `endpointslices` it is 1 only when every namespace's informer has synced. |
| `nautiluslb_discovery_last_success_timestamp_seconds` | gauge | — | Unix time of the last `applied` or `unchanged` reconcile. A `skipped` reconcile does not advance it. |

## Rejection reasons

`reason` on `nautiluslb_connections_rejected_total`:

| Reason | Listener | Meaning |
| --- | --- | --- |
| `acl` | tcp, tls, udp | peer denied by `access.deny`, or not in a non-empty `access.allow` |
| `limit_global` | tcp, tls, udp | `settings.limits.maxConnections` reached |
| `limit_listener` | tcp, tls, udp | `limits.maxConnections` (tcp/tls) or `udp.maxSessions` reached |
| `limit_source` | tcp, tls, udp | `limits.maxConnectionsPerSource` or `udp.maxSessionsPerSource` reached for this client IP |
| `draining` | tcp, tls, udp | arrived while the listener was draining |
| `proxy_header` | tcp, tls | malformed PROXY header from a trusted peer, or missing with `proxyProtocol.in.required` |
| `sni_error` | tls | not a TLS ClientHello, ClientHello larger than `tls.maxClientHello`, or not received within `tls.peekTimeout` |
| `sni_no_route` | tls | the server name matches no route and there is no `tls.defaultRoute` |
| `no_backend` | tcp, tls, udp | the pool is empty, or every candidate is at `maxConnectionsPerBackend` (tcp/tls) |
| `limit_backend` | udp | every candidate backend is at `maxConnectionsPerBackend` |
| `dial_failed` | tcp, tls, udp | up to 3 backends were tried and none connected |
| `backend_write` | tcp, tls | writing the outbound PROXY header failed |
| `write_error` | udp | a backend reply could not be sent to the client. The datagram is dropped and the session continues. |
| `panic` | tcp, tls | a recovered panic before proxying, which is contained to that connection |

## Example alerts

```yaml
groups:
  - name: nautiluslb
    rules:
      - alert: NautilusLBPoolNoHealthyBackends
        # The pool is failing open (trying every backend) or is empty.
        expr: sum by (instance, listener, pool) (nautiluslb_pool_backends{state="healthy"}) == 0
        for: 2m
        labels: {severity: critical}
        annotations:
          summary: "{{ $labels.pool }} on {{ $labels.instance }} has no healthy backends"

      - alert: NautilusLBPoolEmpty
        # Nothing is bound: check annotations, namespaces allowlist, port name.
        expr: sum by (instance, listener, pool) (nautiluslb_pool_backends) == 0
        for: 10m
        labels: {severity: warning}

      - alert: NautilusLBDialErrorRatio
        expr: |
          sum by (instance, listener, pool) (rate(nautiluslb_backend_dial_total{result!="ok"}[5m]))
            /
          sum by (instance, listener, pool) (rate(nautiluslb_backend_dial_total[5m]))
            > 0.05
        for: 10m
        labels: {severity: warning}

      - alert: NautilusLBLocalDialErrors
        # File descriptors or ephemeral ports exhausted on the LB host itself.
        expr: sum by (instance) (rate(nautiluslb_backend_dial_total{result="local"}[5m])) > 0
        for: 5m
        labels: {severity: critical}

      - alert: NautilusLBDrainingWithOpenConnections
        # Shutting down (ready 0) with connections still open: whatever is
        # still open when settings.drain.timeout expires is force-closed.
        expr: |
          nautiluslb_ready == 0
            and on (instance)
          sum by (instance) (nautiluslb_connections_active) > 0
        for: 20s
        labels: {severity: info}

      - alert: NautilusLBDrainForced
        # A drain deadline expired with connections or sessions still open.
        # Raise settings.drain.timeout, or shorten client/backend keep-alives.
        expr: sum by (instance, listener) (increase(nautiluslb_drain_forced_total[10m])) > 0
        labels: {severity: warning}
        annotations:
          summary: "{{ $labels.listener }} on {{ $labels.instance }} force-closed connections when its drain timed out"

      - alert: NautilusLBAccessLogDrops
        expr: rate(nautiluslb_accesslog_dropped_total[5m]) > 0
        for: 10m
        labels: {severity: warning}
        annotations:
          summary: "Access log is dropping records: raise settings.accessLog.bufferSize or use a faster output"

      - alert: NautilusLBDiscoveryStale
        expr: time() - nautiluslb_discovery_last_success_timestamp_seconds > 900
        labels: {severity: critical}
        annotations:
          summary: "Discovery has not completed a full reconcile for 15m (RBAC, API server reachability)"

      - alert: NautilusLBInformerNotSynced
        expr: min by (instance, resource) (nautiluslb_discovery_informer_synced) == 0
        for: 5m
        labels: {severity: critical}

      - alert: NautilusLBNotReady
        expr: nautiluslb_ready == 0
        for: 5m
        labels: {severity: critical}

      - alert: NautilusLBReloadRejected
        expr: increase(nautiluslb_config_reload_total{result="rejected"}[15m]) > 0
        labels: {severity: warning}
        annotations:
          summary: "A config reload was rejected; the previous configuration is still running"

      - alert: NautilusLBRejectedConnections
        expr: sum by (instance, listener, reason) (rate(nautiluslb_connections_rejected_total{reason=~"limit_.*|no_backend|dial_failed"}[5m])) > 1
        for: 10m
        labels: {severity: warning}
```

`NautilusLBDiscoveryStale` uses 900s, three times the default `resyncPeriod` of 5m. Every resync runs a reconcile, so a healthy instance advances the timestamp at least that often. In a quiet cluster the timestamp moves only on resync or on change. Keep the threshold above `resyncPeriod`.

`NautilusLBDrainForced` fires when a drain deadline expired with work still open. A reload that retires a listener is visible to it (as with any counter, `increase()` cannot see the first increment of a series that was not exported at 0 before it). At shutdown the admin server stops last, but the process exits right after, so the final increment is seen only if a scrape lands between the force-close and the admin server stopping (up to about 2s). For shutdowns, `NautilusLBDrainingWithOpenConnections` is the reliable signal, and the exact count is also in the log:

- `Force-closed connections at shutdown` (field `forced`) at shutdown;
- `Force-closed connections of a retired listener` (fields `listener`, `forced`) when a listener retired by a reload is drained.
