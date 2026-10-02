# NautilusLB

Layer 4 load balancer (TCP, TLS passthrough with SNI routing, UDP) that runs at the edge of, or outside, a Kubernetes cluster and forwards each listener to the node/ClusterIP addresses of Services that opt in with annotations. Discovery uses informers on Nodes, Services and EndpointSlices.

- Module `github.com/cloudresty/nautiluslb` in `app/` (binary, no `/v2` suffix) · Go 1.27.1 · k8s.io trio v0.37.1 · YAML via `go.yaml.in/yaml/v3` · Prometheus `client_golang` · logging `cloudresty/emit` (>= v1.2.6, never downgrade: v1.2.5 crashed on long error strings).
- Follows the Cloudresty backend **Service Blueprint** and **Naming Conventions** (standards pointer in ~/.claude/GO.md), with flat `internal/<capability>` packages rather than domain/application/adapters layers.
- Branching: feature -> PR -> `develop`; `develop` -> PR -> `main`; tag `vX.Y.Z` on `main` triggers the release.
- User docs live in `docs/` (configuration, security, metrics, operations, ha, upgrading-v2). Keep them, `app/config.example.yaml` and the Helm chart in step with code changes.

## Entry point

`app/cmd/nautiluslb/main.go`: flags `--config` (default `config.yaml`, env `NLB_CONFIG`), `--validate`, `--pprof`, `--version`, `--help`. Exit 1 on an invalid config or a listener that cannot bind; the log message `Failed to load configuration` is grepped by e2e, keep it verbatim. Version/commit/date come from ldflags into `internal/version`.

## Packages (`app/internal/`)

- **config**: v2 types, defaults (`defaults.go`), `NLB_*` env overrides (`env.go`), strict load (`load.go`), validation with all errors joined (`validate.go`), annotation constants, pool specs.
- **backend**: immutable `Endpoint` from discovery + `Backend` with lock-free runtime state (health, active count, cap).
- **balancer**: pickers (round_robin, least_conn, source_ip_hash, random_two_choices), weights, slow start; lock-free hot path.
- **health**: active tcp/http probers and the rise/fall checker; passive ejection hold.
- **pool**: owns one pool's backends, picker and health loops; discovery writes endpoints, the data path picks without locks.
- **discovery**: informer manager (Nodes cluster-wide, Services + EndpointSlices per allowlisted namespace), debounced recompute, binding rules, ETP Local, node filter, address family.
- **kube**: Kubernetes client construction (in-cluster, then kubeconfig, then `$KUBECONFIG`, then `~/.kube/config`).
- **tcpproxy**: TCP/TLS listener and per-connection data path.
- **udpproxy**: UDP listener, per-client sessions on connected sockets, idle sweeper.
- **pipe**: bidirectional copy; Linux splice path + TCP_INFO watchdog; generic path with deadline wrappers.
- **proxyproto**: PROXY v1/v2 read and write.
- **sni**: ClientHello peek and host -> route matching.
- **acl**: CIDR allow/deny.
- **limits**: global gate and per-source counters.
- **metrics**: `Recorder` interface + Prometheus implementation (`nautiluslb_*`); no client IP or SNI labels.
- **accesslog**: one JSON line per connection/session via a bounded queue and one writer.
- **neterr**: classifies dial/IO errors into backend-attributable vs local causes.
- **admin**: `/metrics`, `/healthz`, `/readyz` (+ `/health/live`, `/health/ready`), opt-in pprof.
- **runtime**: lifecycle orchestrator: startup, reload diff, shutdown/drain; Reload and Shutdown share one mutex.
- **version**: build metadata.
- **integration**: build tag `integration`; real Runtime in process against a fake clientset and loopback backends.

Import rules: `discovery` never imports `pool` or `tcpproxy`; leaf packages import only stdlib/3rd-party.

## Data path (tcpproxy)

Accept -> ACL (before any read) -> limits (global, listener, per-source) -> inbound PROXY (trusted peers only) -> SNI peek (`tls`) -> pick (up to 3 backends, skip capped, fail open) -> dial (default 5s, cap 10s; passive eject only on backend-attributable causes) -> outbound PROXY -> `pipe.Run` (bytes already read are replayed first) -> one access-log record. UDP: ACL -> limits -> pick once per session -> connected socket.

## Lifecycle

- **Startup:** load config -> signal handler installed -> access log, kube client -> bind **every** listener (any failure unbinds all, exit 1) -> admin server (readyz 503 `starting`) -> pools, watchdog, discovery (wait <= 60s for sync) -> serve; ready once synced.
- **SIGTERM/SIGINT:** readyz 503 -> `drain.readinessDelay` -> stop discovery -> close listeners, wait `drain.timeout` -> force-close -> stop pools/watchdog -> flush access log -> admin server last -> exit 0. A second signal force-closes immediately.
- **SIGHUP / file watch:** per-configuration diff by name: unchanged untouched; same address+protocol updated in place (atomic swap, connections survive); new address bound first, then old drains; any parse/validate/bind error rejects the whole reload; `settings.*` is never hot-applied (warned).

## Must not regress

Full list with tests: `docs/operations.md#behavioural-guarantees`. The ones most easily broken:

- Explicit binding: enabled == "true" AND the annotation names the config (or `config/route`) AND namespace allowlisted AND named port with matching protocol. A port name alone never binds.
- `namespaces` is required; `"*"` must be alone.
- Scoped configurations read only their own namespaces' informer stores, never a cluster-wide store; the cluster-wide factory exists only if some config uses `"*"`.
- An unsynced or unreadable store never wipes a pool's backends.
- A failed dial never proxies a nil connection; the client is closed cleanly (`dial_failed`).
- No idle timeout on established connections by default (only `idleTimeout` if set, the 2-min half-close idle and the 2-min write-stall bound).
- Splice path: hand the raw `*net.TCPConn` to `ReadFrom`; never wrap it (wrappers disable splice). Bounds there are enforced by the watchdog.
- Local errors (EMFILE, ENFILE, EADDRNOTAVAIL, EAGAIN, ENOBUFS, EACCES) never eject a backend.
- Reload is all or nothing; startup binds all or exits.
- Readiness turns 503 before any listener closes.

## Commands

```bash
make test                 # go test -race ./... in app/
make lint                 # golangci-lint v2
make vuln                 # govulncheck
make fuzz                 # every Fuzz target, FUZZTIME=30s each
make bench                # benchmarks with -benchmem
make integration          # -tags integration ./internal/integration/...
make e2e                  # kind cluster + container; CASE=<name> runs one case, KEEP=1 keeps the cluster
make soak                 # long-running load, checks goroutine and RSS growth
make build-local          # binary at ./nautiluslb
make build                # docker image (linux/amd64)
helm lint deploy/helm/nautiluslb
bash deploy/check-drift.sh   # raw manifests in deploy/kubernetes must match helm template defaults
```

## External systems

- Kubernetes API via SharedInformers (`list` + `watch` on nodes, services, `discovery.k8s.io/endpointslices`; nothing else).
- Prometheus scrapes the admin server (`127.0.0.1:9090` by default).
- `cloudresty/emit` structured logging; `EMIT_LEVEL=error` keeps test output quiet.

## Traps

- **Dependencies:** never `go get -u all` / `go get -u ./...`. `k8s.io/{api,apimachinery,client-go}` move together to the same version (with their kube-openapi / structured-merge-diff); newer kube-openapi and structured-merge-diff v7 do not build with apimachinery v0.37. Let `go mod tidy` pick the rest.
- **e2e fixture readiness:** before starting NautilusLB, wait until every fixture answers on NodeIP:NodePort and settle >= 15s. Without it a wrongly bound backend fails its first probe and hides a hijack (found by mutation testing).
- **Docker Desktop** (macOS) is flaky for e2e: kind NodePorts and container networking time out intermittently. Re-run before debugging; CI (ubuntu) is authoritative.
- **Linux-only splice tests** (`pipe/*_linux*.go`) do not run on macOS. Run them in Docker:
  `docker run --rm -v "$PWD/app":/src -v "$(go env GOMODCACHE)":/go/pkg/mod -w /src golang:1.27.1 go test -race ./internal/pipe/...`
- **Image:** distroless static nonroot (UID 65532), no shell; binary `/nautiluslb/nautiluslb` is the ENTRYPOINT, config `/nautiluslb/config.yaml`. Mounted files must be readable by 65532. The root `.dockerignore` is an allowlist.
- `app/config.yaml` and `app/kubeconfig` are local and git-ignored.
