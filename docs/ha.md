# High availability

NautilusLB keeps no state that another instance needs. Each instance discovers Services from the Kubernetes API on its own, health-checks backends on its own and proxies connections it accepted itself. High availability therefore comes from running two or more identical instances and moving a floating IP (VIP) between them. NautilusLB's part is to report truthfully, on `/readyz`, whether it should receive new traffic.

- [Why there is no leader election](#why-there-is-no-leader-election)
- [Readiness contract](#readiness-contract)
- [keepalived / VRRP pair](#keepalived--vrrp-pair)
- [kube-vip in the cluster](#kube-vip-in-the-cluster)
- [Drain timing](#drain-timing)
- [Client affinity across instances](#client-affinity-across-instances)

## Why there is no leader election

- **Stateless data path.** Each instance owns only the connections it accepted. Nothing needs to be handed over, so there is nothing to elect a leader for.
- **Discovery is read-only.** Every instance watches the same Nodes, Services and EndpointSlices and computes the same pools. Discovery writes nothing to the cluster, so concurrent instances do not conflict. It needs no `leases` RBAC.
- **Active-active works.** Instances can all serve at once, behind DNS round-robin, ECMP, an upstream L4 balancer or several VIPs. A VIP mechanism only decides which instance receives the traffic for one address. That decision belongs to the VIP tool (VRRP, kube-vip), not to NautilusLB.

## Readiness contract

`GET /readyz` (alias `/health/ready`) on the [admin address](configuration.md#settingsadmin):

| State | Response | `reason` |
| --- | --- | --- |
| starting, listeners bound, discovery not yet started or synced | `503` | `starting` |
| discovery has not synced within 60s (serving anyway, every connection gets `no_backend` until endpoints arrive) | `503` | `discovery not synced` |
| discovery failed to start (the process then exits 1) | `503` | `discovery failed` |
| listeners serving and discovery synced | `200 {"status":"ready"}` | — |
| Kubernetes API unreachable after the first sync | `200 {"status":"ready"}` | — (pools keep serving their last known endpoints) |
| SIGTERM/SIGINT received | `503` | `shutting down` |

Readiness is a one-way latch after the first sync. Losing the Kubernetes API later does not flip it: the informers keep their cache and reconnect with backoff, every pool keeps its last endpoints, and the peer instance sees the same API, so moving the VIP would gain nothing. Watch `nautiluslb_discovery_watch_errors_total` and `nautiluslb_discovery_last_success_timestamp_seconds` instead (see the [example alerts](metrics.md#example-alerts)).

`/readyz` does not reflect backend health. A pool with no healthy backend fails open and still proxies. Alert on that with [metrics](metrics.md#example-alerts). Do not move the VIP for it, because the peer sees the same backends.

`/healthz` (alias `/health/live`) returns 200 whenever the admin server is up. Use it for liveness only.

## keepalived / VRRP pair

Two hosts, `lb-a` (`10.0.0.11`) and `lb-b` (`10.0.0.12`), share the VIP `203.0.113.10`. Both run NautilusLB with the same `config.yaml`.

### NautilusLB settings for a VRRP pair

```yaml
apiVersion: nautiluslb.cloudresty.io/v1
kind: Config
settings:
  admin:
    address: "127.0.0.1:9090"   # the track script runs on the same host
  drain:
    readinessDelay: 5s          # > keepalived detection + VRRP failover; see Drain timing
    timeout: 30s
configurations:
  - name: https
    protocol: tls
    listenerAddress: ":443"     # wildcard: binds whether or not this host holds the VIP
    namespaces: [ingress-nginx]
    backendPortName: https
    tls:
      routes:
        - {name: web, hosts: ["example.com"]}
```

The backup must be able to bind its listeners while it does not hold the VIP. Use a wildcard `listenerAddress` such as `":443"`. To bind the VIP itself, as in `"203.0.113.10:443"`, set `net.ipv4.ip_nonlocal_bind=1` (and `net.ipv6.ip_nonlocal_bind=1` for IPv6) on both hosts. Otherwise the backup fails to start, because NautilusLB binds every listener at startup or exits.

### /etc/keepalived/keepalived.conf on lb-a

```text
global_defs {
    router_id lb-a
    enable_script_security
    script_user keepalived_script
}

vrrp_script chk_nautiluslb {
    # 503 (or no answer) fails the check.
    script "/usr/bin/curl -fsS -o /dev/null --max-time 1 http://127.0.0.1:9090/readyz"
    interval 1        # seconds between checks
    timeout 2
    fall 2            # 2 consecutive failures -> FAULT
    rise 2            # 2 consecutive successes -> eligible again
    # No "weight": a failing check puts the instance in FAULT, which releases
    # the VIP at once, regardless of the peer's priority.
}

vrrp_instance VI_NLB {
    state BACKUP              # both nodes BACKUP + nopreempt: no fail-back flap
    nopreempt
    interface eth0
    virtual_router_id 51
    priority 150              # lb-b: 100
    advert_int 1

    # Unicast VRRP for networks without multicast (most clouds). Remove for multicast.
    unicast_src_ip 10.0.0.11  # lb-b: 10.0.0.12
    unicast_peer {
        10.0.0.12             # lb-b: 10.0.0.11
    }

    virtual_ipaddress {
        203.0.113.10/24 dev eth0
    }

    track_script {
        chk_nautiluslb
    }
}
```

On `lb-b`, change `router_id`, `priority`, `unicast_src_ip` and `unicast_peer`. Create the unprivileged `keepalived_script` user that runs the check. `curl` must exist on the host, because the NautilusLB image has no shell or curl.

Behaviour:

- **Process dies or hangs.** `/readyz` stops answering, and after `fall × interval` (2s) the instance enters FAULT. The peer takes the VIP after its skew time (under 1s at `advert_int 1`) and sends gratuitous ARPs.
- **Planned stop or restart** (`systemctl stop`, `restart` or `reload` with a changed address). See [Drain timing](#drain-timing).
- **Startup.** The check fails until discovery has synced. An instance never takes the VIP before it has backends.
- **`nopreempt`.** A recovered `lb-a` does not take the VIP back from a healthy `lb-b`. That avoids a second failover for no reason.

## kube-vip in the cluster

When NautilusLB runs inside the cluster on dedicated edge nodes, kube-vip can announce the VIP with ARP (L2 mode).

1. Run NautilusLB as a DaemonSet with `hostNetwork: true` on the edge nodes. With the chart in [`deploy/helm/nautiluslb`](../deploy/helm/nautiluslb), use `mode: DaemonSet`, `hostNetwork: true` and a `nodeSelector`. Its listeners then bind on the node's addresses. Ports below 1024 need `net.ipv4.ip_unprivileged_port_start` lowered on the node, or the chart's `privilegedPortsMode: root`; see the chart's `values.yaml`. Its readiness probe is `GET /readyz`.
2. Expose it with a `LoadBalancer` Service. Set `externalTrafficPolicy: Local`, select the NautilusLB pods and list the listener ports. kube-vip, in services mode with ARP and per-service leader election, assigns the VIP and announces it from one node. With `externalTrafficPolicy: Local`, kube-vip announces only from a node that has a **ready** local endpoint. So NautilusLB's readiness decides where the VIP may live: when `/readyz` turns 503 on SIGTERM, the pod leaves the Service endpoints and kube-vip moves the VIP. Confirm this `externalTrafficPolicy: Local` behaviour in your kube-vip version's documentation.
3. `externalTrafficPolicy: Local` also preserves the client source IP, which matters for ACLs, per-source limits, `source_ip_hash` and the access log.
4. Do **not** put NautilusLB's own annotations (`nautiluslb.cloudresty.io/*`) on this Service.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: nautiluslb-vip
  namespace: nautiluslb
spec:
  type: LoadBalancer
  loadBalancerIP: 203.0.113.10        # or kube-vip's address annotation / IPAM
  externalTrafficPolicy: Local
  selector:
    app.kubernetes.io/name: nautiluslb
  ports:
    - {name: https, port: 443, targetPort: 443, protocol: TCP}
    - {name: http,  port: 80,  targetPort: 80,  protocol: TCP}
```

Endpoint removal goes through kubelet's readiness probe period, the EndpointSlice controller, and kube-vip's own watch and leader election. That path is slower than a local keepalived check. Size `readinessDelay` for it, typically 10s or more, with the readiness probe at `periodSeconds: 1`–`2` and `failureThreshold: 1`. Keep `terminationGracePeriodSeconds` at least `drain.timeout + readinessDelay + 15s` (see [operations.md](operations.md#stop-timeout)).

## Drain timing

On SIGTERM the shutdown sequence is:

1. `/readyz` turns 503 and `nautiluslb_ready` becomes 0;
2. NautilusLB waits `settings.drain.readinessDelay`, still accepting connections;
3. discovery stops and every listener closes, so the kernel refuses new connections to that host;
4. open connections get up to `settings.drain.timeout` to finish, and the rest are force-closed;
5. health checks stop, the access log is flushed, and the admin server stops last.

`readinessDelay` must cover the time the VIP needs to move. If it is too short, the listeners close while the VIP still points at this host, and new connections are refused. For keepalived:

```text
readinessDelay  >=  interval × fall  +  interval (check phase)  +  advert_int (failover)  +  margin
5s              =   1s × 2           +  1s                       +  1s                     +  1s
```

The default `3s` is enough only for a tighter keepalived configuration. Measure a failover and set the value from the measurement.

**What drain does and does not save with a VIP.** Once the VIP moves, packets of connections that were established through it arrive at the new holder. The new holder has no socket for them and resets them. A VIP move therefore ends established connections, and no proxy-level drain can prevent it. Those connections are proxied by user-space sockets, so no kernel conntrack sync applies. `readinessDelay` ensures that **new** connections land on the peer instead of being refused. `drain.timeout` matters where traffic still reaches the old instance after it turns not-ready: DNS round-robin, ECMP, an upstream balancer that honours health checks, or connections opened directly to a host address. For long-lived protocols such as databases, websockets and AMQP, clients must reconnect on failover. Schedule planned stops accordingly.

## Client affinity across instances

`source_ip_hash` is computed with a process-independent hash (FNV-1a over the client IP, on a ring built from the backend addresses and weights). Two instances that see the same backend set map a client IP to the **same backend**. After a failover, a client reconnecting through the new VIP holder lands on the backend it used before. Each instance has its own health view, so the mapping differs only for clients whose usual backend one instance considers unhealthy. `round_robin`, `least_conn` and `random_two_choices` are per-instance and give no cross-instance affinity.
