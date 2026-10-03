# NautilusLB performance suite

Measures NautilusLB v2 against a direct connection, the published v1 image
(`cloudresty/nautiluslb:v0.0.11`, same code as v1.0.0) and HAProxy 3.2 in
`mode tcp`, on one kind cluster. Not part of CI: it needs ~45 minutes (plus 15
for the soak) and Docker with at least 6 CPUs.

## Topology

```
client container ──▶ [proxy container] ──▶ NodeIP:NodePort (kind node, kube-proxy iptables) ──▶ pod
 (fortio / iperf3 /    v2 | v1 | haproxy     externalTrafficPolicy: Local
  idleconn holder)     | haproxy-splice
```

Everything runs on the `kind` Docker network of one Docker VM, so **results
are relative**: client, proxy, kube-proxy and backend share the same CPUs.
Only one proxy runs at a time. Every proxy and client container gets
`--ulimit nofile=1048576`, `ip_local_port_range=1024 65535`,
`tcp_tw_reuse=1`, `somaxconn=65535`; nothing is CPU-pinned or limited.

| Path | What |
| :--- | :--- |
| `direct` | client → NodeIP:NodePort |
| `v2` | image built from this repository (`build/Dockerfile`), v2 config, admin + pprof on `:9090`, access log on (default, stdout) unless stated |
| `v1` | `cloudresty/nautiluslb:v0.0.11` (pinned by digest), v1 config (no `apiVersion`, `settings.kubeconfigPath`, `namespace`) |
| `haproxy` | `haproxy:3.2` (pinned), `mode tcp`, static `server NodeIP:NodePort check`, defaults otherwise |
| `haproxy-splice` | the same plus `option splice-auto` (HAProxy does not splice by default) |

Fixtures (`manifests.yaml`, images pinned by digest): an iperf3 server and an
nginx serving `/small` (64 B) and `/1m` (1 MiB), each behind a NodePort
Service annotated for v2 (`enabled` + `configurations`) and v1 (`enabled`,
port name). Both Services use `externalTrafficPolicy: Local`, see below.

## Running

```bash
# everything except the soak, then clean up
test/perf/run.sh

# phase by phase, keeping the cluster between runs
KEEP=1 PHASES=setup test/perf/run.sh
KEEP=1 PHASES=throughput,rate TARGETS="v2 haproxy" test/perf/run.sh
KEEP=1 PHASES=soak SOAK_SECONDS=900 test/perf/run.sh
PHASES=cleanup test/perf/run.sh
```

Phases: `setup throughput rate ratestd latency bigfile alog idle pipes profile
soak cleanup` (see the header of `run.sh`). Results go to
`$OUT/results.tsv` (default `$TMPDIR/nautiluslb-perf/results`) with every
raw fortio/iperf3 JSON, the pprof profiles and the binary they were taken
from. The cluster is `nautiluslb-perf`, containers are `nautiluslb-perf-*`;
the caller's kube context is never used.

`pipesizes.py PID` prints the buffer sizes of every pipe a process holds (run
it in a `--pid host --privileged` container, e.g. netshoot).

## Methodology notes and traps

- **TIME_WAIT/PAWS on the NodePort hop.** With `externalTrafficPolicy:
  Cluster`, kube-proxy SNATs NodePort traffic to the node IP. Under
  connection churn from one source (the client, or any proxy, which is always
  one source) the SNAT'd tuples collide with nginx's TIME_WAIT sockets and
  the SYNs are dropped by PAWS (`TcpExtPAWSTimewait`), costing a 1 s SYN
  retransmit: the direct path did ~3,000 conns/s with p99 1.3 s. With
  `Local` it does ~49,000 conns/s, p99 6 ms. This applies to production:
  **NautilusLB in front of high-churn `Cluster` NodePorts will see the same
  1 s stalls**; prefer `externalTrafficPolicy: Local` (supported by v2).
- **v1 does not forward FIN.** v1 never half-closes either side on a clean
  EOF, so a server that ends a response by closing (HTTP `Connection:
  close`) leaves the client waiting: fortio's default client times out on
  every request (5 s). `ratestd` uses fortio's Go `net/http` client (stops at
  Content-Length) so v1 gets a number. The same bug makes v1 keep both
  sockets of every connection the client closed while the backend stays
  silent (see the idle results), and a probe connection through v1 leaves
  iperf3's single-client server busy.
- fortio's fast client buffers 128 KiB per response by default; the 1 MiB
  test uses `-httpbufferkb 2048` (otherwise half the requests fail).
- `idle` holds connections that never send a byte; nginx keeps them for
  `client_header_timeout 3600s`.
- RSS, fd and thread counts are read from the VM's `/proc/<pid>` through a
  `--pid host` helper (works for distroless images); goroutines from
  `/debug/pprof/goroutine?debug=1`; v2 also exports `process_*` metrics.

## Results

Run 2026-10-03 on Docker Desktop 29.8.1 (linuxkit kernel 7.0.14, aarch64,
**6 CPUs, 23.4 GiB**), kind v0.33.0 / node v1.37.0, kube-proxy iptables,
NautilusLB `v1.0.0-15-gb28d87a` (b28d87a). One run per cell; run-to-run noise
on this VM is about ±10% for rates and much more for p99.9.

| Metric | direct | v2 | v1 (v0.0.11) | HAProxy | HAProxy splice-auto |
| :--- | ---: | ---: | ---: | ---: | ---: |
| iperf3 1 stream, Gbit/s | 57.5 | **62.0** | 63.6 | 13.6 | 39.8 |
| iperf3 8 streams, Gbit/s | 210.2 | **122.8** | 105.4 | 41.9 | 100.6 |
| 1 MiB GETs, keep-alive, c16, MiB/s | 14,855 | **8,175** | 6,687 | 5,091 | 9,136 |
| new conn/request c64, conns/s | 47,359 | **16,839** | 12 ¹ | 27,471 | 26,536 |
| — p50 / p99 / p99.9 ms | 0.98 / 6.6 / 12.9 | 3.18 / 13.7 / 31.3 | 5,005 ¹ | 1.83 / 10.2 / 18.8 | 1.90 / 10.1 / 19.4 |
| new conn/request c256, conns/s | 48,798 | **19,952** | 51 ¹ | 27,931 | 29,434 |
| — p50 / p99 / p99.9 ms | 4.3 / 19.9 / 33.8 | 11.5 / 36.1 / 49.9 | 5,012 ¹ | 7.7 / 37.9 / 69.3 | 7.5 / 30.0 / 50.6 |
| new conn/request c64, Go client ², conns/s | 26,878 | **13,518** | 13,572 | 14,838 | 18,848 |
| — p50 / p99 ms | 1.73 / 10.1 | 4.00 / 16.7 | 4.03 / 15.8 | 3.38 / 18.8 | 2.86 / 11.5 |
| reused conns c64 @ 5000 qps, p50 / p99 / p99.9 ms | 0.53 / 3.58 / 14.9 | 0.60 / 2.91 / 15.0 | 0.57 / 1.95 / 5.3 | 0.55 / 1.88 / 2.9 | 0.55 / 1.91 / 4.8 |
| errors under load | 0 | 0 | 100% ¹ | 0 | 0 |

¹ v1 never forwards the backend's FIN, so every `Connection: close` request
waits for fortio's 5 s timeout (see Methodology). ² fortio `-stdclient`.

### v2 copy path

`nautiluslb_pipe_mode_total{mode="splice"}` rose by exactly the number of
iperf3 connections (1 → 12: 2 for `-P 1`, 9 for `-P 8`), `generic` never
appeared, and the access log records `"mode":"splice"`. The CPU profile under
`-P 8` is 98% `splice(2)` syscalls (`internal/poll.splicePump` 75%,
`spliceDrain` 24%); no NautilusLB code is visible.

**But splice degrades once ~32 connections are open.** Go asks for 1 MiB
pipes (`F_SETPIPE_SZ`) and holds one per direction for the whole life of a
connection (`TCPConn.ReadFrom` keeps it while blocked in `spliceDrain`). The
kernel's per-user budget `fs.pipe-user-pages-soft` (16384 pages = 64 MiB) is
gone after 64 pipes; every later pipe gets 2 pages (8 KiB) (`phase_pipes`,
`pipesizes.py`):

| iperf3 `-P 1` through | alone | with 1,000 idle connections held |
| :--- | ---: | ---: |
| v2 (UID 65532), two runs | 75.9 / 78.0 Gbit/s | **13.3 / 12.8 Gbit/s** (pipes: 64 × 1 MiB, 1,936 × 8 KiB) |
| v2 as root (`--user 0`) | 69.3 | **12.6** (13 × 1 MiB: root's budget is shared with every root process) |
| HAProxy (control) | 14.2 | 13.8 |
| HAProxy splice-auto (control) | 34.1 | 39.2 |

### Access log (v2, new conn/request c64, alternating rounds)

| | round 1 | round 2 | round 3 | mean |
| :--- | ---: | ---: | ---: | ---: |
| on (stdout, Docker json-file driver) | 18,531 ³ | 16,345 | 17,215 | 17,364 |
| off | 18,792 ³ | 20,137 | 19,485 | 19,471 |

³ from the main run. The access log costs ~11% of the connection rate here
(in-process JSON encoding is ~3% of CPU; the rest is the log driver on the
same VM); `accesslog_dropped_total` stayed 0 at ~20k records/s.

### 10,000 idle connections

| | RSS 0 → 10k | RSS per conn | cgroup per conn | fds per conn | goroutines | after close |
| :--- | :--- | ---: | ---: | ---: | :--- | :--- |
| v2 | 31.7 → 202.4 MiB | 17.1 KiB | 28.1 KiB | 6 | 44 → 20,044 | goroutines 44 at once; 40,010 fds and 220 MiB RSS held by Go's splice-pipe `sync.Pool` until two GCs; **at +270 s: 10 fds, 64.6 MiB** |
| v1 | 30.4 → 229.7 MiB | 19.9 KiB | 30.9 KiB | 6 | n/a | **leaked: 60,012 fds and all memory still held** (no FIN forwarding) |
| HAProxy | 43.1 → 78.8 MiB | 3.6 KiB | 11.4 KiB | 2 | n/a | 25 fds |

v2 breakdown at 10k (`/metrics`): `go_memstats_stack_inuse_bytes` 118 MiB
(two goroutines × ~6 KiB per connection), heap in use 41 MiB (~3.8 KiB per
connection). 6 fds = client + upstream + two splice pipes (2 fds each).

### Soak: v2, 15 min, fortio c256, new connection per request

15,824,482 connections (17,582/s), **0 errors**, p99 45.6 ms, no panic or
error log, `accesslog_dropped_total` 0.

| t (s) | goroutines | RSS MiB | open fds | heap MiB | active conns |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 0 | 44 | 31.7 | 15 | 5.2 | 0 |
| 62 | 385 | 41.1 | 883 | 7.9 | 43 |
| 186 | 473 | 40.7 | 1,409 | 8.8 | 146 |
| 310 | 364 | 41.5 | 1,491 | 8.8 | 176 |
| 433 | 342 | 41.9 | 1,464 | 9.9 | 139 |
| 557 | 302 | 42.2 | 1,379 | 9.1 | 141 |
| 681 | 480 | 40.8 | 1,425 | 9.9 | 140 |
| 804 | 326 | 41.4 | 1,513 | 8.0 | 231 |
| 866 | 237 | 41.6 | 1,139 | 8.8 | 133 |
| end + 10 s | **44** | 41.1 | 845 ⁴ | 8.8 | **0** |

Flat: RSS 39.6–42.2 MiB, heap 7.9–9.9 MiB after the first minute;
goroutines and fds track active connections. ⁴ pooled splice pipes, released
after two GCs (see the idle test).

### CPU profile of v2 under connection churn (c64, 20 s, 2.2 CPUs)

~77% of samples are syscalls. Cumulative, by NautilusLB line:

| cum | where |
| ---: | :--- |
| 34.6% | backend dial, `internal/tcpproxy/handle.go:276` (kernel `connect` incl. the NodePort path) |
| 21.9% | `spliceCopy` → `TCPConn.ReadFrom`, `internal/pipe/splice_linux.go:129` (splicePump 16%, spliceDrain 5%) |
| 13.4% | half-close forwarding, `closeWrite` `internal/pipe/pipe.go:339` (`shutdown(2)`) |
| 7.1% | teardown closes, `internal/tcpproxy/handle.go:341,343` |
| 5.3% | `epoll_ctl` registering/unregistering 2 sockets + 2 pipes per connection |
| 3.3% | access-log writer goroutine, `internal/accesslog/accesslog.go:193,200` (JSON encode + flush) |
| 2.1% | `SetKeepAliveConfig` on accepted and dialled sockets (4 `setsockopt` each) |
| 1.5% | Prometheus `WithLabelValues` lookups (label hashing) in `internal/metrics/metrics.go:160-181` |
| 1.3% | splice-path setup: `pipe.newEntry` + two `TCP_INFO` probes, `internal/pipe/splice_linux.go:108,112`, `tcpinfo_linux.go:52` |
| 0.9% | `context.WithTimeout` per dial, `internal/tcpproxy/handle.go:274` |

### Optimisation opportunities (ranked, not implemented)

1. **Splice pipes held for the connection's lifetime** (`internal/pipe/splice_linux.go:129`
   hands the copy to `TCPConn.ReadFrom`). Past ~32 open connections every new
   pipe is 8 KiB and single-stream throughput falls 76 → 13 Gbit/s; idle
   connections cost 6 fds instead of 2. Fix: own splice loop that waits for
   readability first (`RawConn.Read`), takes a pipe only while data is in
   flight and returns it, with 64 KiB pipes; or fall back to the generic copy
   when `F_SETPIPE_SZ` is refused. Operator workaround: raise
   `fs.pipe-user-pages-soft` or grant `CAP_SYS_RESOURCE`. Expected: 5–6×
   throughput under realistic concurrency, −4 fds and the 40k-fd post-close
   spike gone.
2. **Connection rate 39% below HAProxy** (16.8k vs 27.5k conns/s). Per
   connection v2 pays splice setup (pool pipe, two `TCP_INFO` probes,
   watchdog registration) and two goroutines for a 64-byte exchange. Start
   every connection on the generic path and switch to splice after the first
   N KiB. Expected 10–25%; needs a prototype to confirm.
3. **Access log** (~11% of the connection rate): `accesslog.go:199-201`
   flushes whenever the queue is momentarily empty, i.e. one `write(2)` per
   record at moderate rates; encoding goes through `encoding/json`
   reflection. Flush on size or every ~100 ms and append fields by hand.
4. **Per-connection syscalls/allocations, ~4% together**: keepalive
   `setsockopt` ×8 per connection (`SetKeepAliveConfig`); Prometheus label
   lookups per event (cache the per-listener children); `context.WithTimeout`
   per dial (`handle.go:274`, use `Dialer.Timeout`).
5. **Idle memory 17 KiB/connection vs HAProxy 3.6 KiB**: two goroutine stacks
   per connection dominate (118 MiB of stacks at 10k). Only an event-driven
   copy would change this; worth it only for 100k+ connection targets.
