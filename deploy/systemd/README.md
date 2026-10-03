# NautilusLB under systemd

Use this to run NautilusLB on a VM or bare-metal host in front of a Kubernetes
cluster, usually as a pair of hosts sharing a virtual IP. Inside the cluster,
use the Helm chart (`deploy/helm/nautiluslb`) or the raw manifests
(`deploy/kubernetes`) instead.

| Path | Owner and mode | Purpose |
|---|---|---|
| `/usr/local/bin/nautiluslb` | `root:root 0755` | binary |
| `/etc/nautiluslb/config.yaml` | `root:root 0644` | configuration (`apiVersion: nautiluslb.cloudresty.io/v1`) |
| `/etc/nautiluslb/kubeconfig` | `root:nautiluslb 0640` | cluster credentials |
| `/etc/systemd/system/nautiluslb.service` | `root:root 0644` | the unit in this directory |

## Install

### 1. Binary

Release assets are `nautiluslb-linux-amd64`, `nautiluslb-linux-arm64`,
`sha256sums.txt` and a cosign bundle (`*.sigstore.json`) for each of them,
signed keyless by the release workflow. Verification needs cosign v3.

```bash
VERSION=v1.0.0
ARCH=amd64            # or arm64
BASE=https://github.com/cloudresty/nautiluslb/releases/download/${VERSION}

curl -fsSLO "${BASE}/nautiluslb-linux-${ARCH}"
curl -fsSLO "${BASE}/sha256sums.txt"
curl -fsSLO "${BASE}/sha256sums.txt.sigstore.json"

# Verify the signature on the checksum list, then the binary against it.
cosign verify-blob --bundle sha256sums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/cloudresty/nautiluslb/.github/workflows/release.yaml@refs/tags/${VERSION}" \
  sha256sums.txt
sha256sum --check --ignore-missing sha256sums.txt

sudo install -m 0755 "nautiluslb-linux-${ARCH}" /usr/local/bin/nautiluslb
nautiluslb --version
```

### 2. Group, configuration and kubeconfig

The unit uses `DynamicUser=yes`: systemd allocates an ephemeral, unprivileged
user per run. There is no login account to manage, and the process cannot
write anywhere on the filesystem (`ProtectSystem=strict`). The only shared,
persistent identity is a static `nautiluslb` group. It lets the service read
the kubeconfig without making the credential world-readable.

```bash
sudo groupadd --system nautiluslb
sudo install -d -m 0755 /etc/nautiluslb
sudo install -m 0644 config.yaml /etc/nautiluslb/config.yaml
sudo install -m 0640 -g nautiluslb kubeconfig /etc/nautiluslb/kubeconfig
```

In `config.yaml`, point discovery at the kubeconfig and keep the admin server on
an address your health checker can reach:

```yaml
settings:
  kubernetes:
    kubeconfig: /etc/nautiluslb/kubeconfig
  admin:
    address: "127.0.0.1:9090"   # /metrics /healthz /readyz
```

The kubeconfig needs only what the in-cluster RBAC grants (see
`deploy/kubernetes/rbac-*.yaml`). Use a ServiceAccount token bound to those
roles, never an admin credential:

- `nodes`: list, watch (cluster)
- `services` and `discovery.k8s.io/endpointslices`: list, watch in each
  allowlisted namespace, or cluster-wide for `namespaces: ["*"]`

The nodes must be reachable from this host on their NodePorts.

### 3. Enable

```bash
sudo install -m 0644 nautiluslb.service /etc/systemd/system/nautiluslb.service
sudo systemctl daemon-reload
sudo nautiluslb --validate --config /etc/nautiluslb/config.yaml
sudo systemctl enable --now nautiluslb
curl -fsS http://127.0.0.1:9090/readyz
```

`ExecStartPre` validates the configuration too, so an invalid file fails the
start with a clear error instead of a restart loop.

## Reload, stop, restart

| Action | Command | Effect |
|---|---|---|
| Reload configuration | `systemctl reload nautiluslb` | sends `SIGHUP`. Changed listeners are updated in place, so connections survive. New addresses are bound before old ones drain. Any error rejects the whole reload and keeps the running configuration. `settings.*` changes need a restart. |
| Stop | `systemctl stop nautiluslb` | sends `SIGTERM`. `/readyz` returns 503, then after `drain.readinessDelay` (3s) open connections drain for up to `drain.timeout` (30s). |
| Restart | `systemctl restart nautiluslb` | stop (with drain), then start. |

`TimeoutStopSec=60` covers the worst-case stop with the defaults (about 42s:
`readinessDelay` + `drain.timeout` + up to 5s for force-closed connections +
up to 4s closing the access log and admin server). Keep it at least
`drain.timeout + readinessDelay + 15s`. If you raise `drain.timeout`, raise it
too with a drop-in (`systemctl edit nautiluslb`). Otherwise systemd kills a
drain that is still running.

To reload automatically on file changes, set `settings.reload.watchFile: true`.

## Logs and metrics

```bash
journalctl -u nautiluslb -f                 # follow
journalctl -u nautiluslb -b -p warning      # warnings and errors this boot
journalctl -u nautiluslb -o cat --since -10m | jq .   # JSON lines
```

Structured logs and the access log (`settings.accessLog.output: stdout`) go to
the journal. To write the access log to a file instead, use a path under
`/var/log/nautiluslb/`, which `LogsDirectory=` makes writable for the service.

Metrics are at `http://<admin.address>/metrics`. Set `admin.address` to a
private interface (not `0.0.0.0` on a public host) if Prometheus scrapes it
remotely.

## Kernel tuning

The proxy holds two sockets per connection and opens an outbound connection to
a backend for every client. On busy hosts, set these in
`/etc/sysctl.d/90-nautiluslb.conf` and apply them with `sysctl --system`:

```ini
# Accept backlog for the listeners (Go uses this as the listen() backlog).
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 65535
# More source ports for backend connections; each (node IP, NodePort) pair
# can have at most this many concurrent connections from one host address.
net.ipv4.ip_local_port_range = 1024 65535
# Reuse TIME_WAIT sockets for new outbound (backend) connections. Safe: it only
# affects connections this host initiates.
net.ipv4.tcp_tw_reuse = 1
# System-wide file handle ceiling; must exceed LimitNOFILE across services.
fs.file-max = 2097152
```

The unit already sets `LimitNOFILE=1048576`. Do not enable `tcp_tw_recycle`
(removed from Linux 4.12; it broke clients behind NAT).

## High availability

Run two hosts with keepalived (VRRP) sharing a virtual IP. Use
`curl -fsS http://127.0.0.1:9090/readyz` as the track script, so the VIP moves
during a drain or while discovery has not synced. NautilusLB needs no leader
election: both hosts balance independently. See [docs/ha.md](../../docs/ha.md)
for a complete keepalived configuration.

## Hardening summary

`systemd-analyze security nautiluslb` rates this unit about 1.7 ("OK"). The
service:

- holds only `CAP_NET_BIND_SERVICE`, with `NoNewPrivileges`;
- has a read-only filesystem, no access to `/home` or devices, and a private `/tmp`;
- can use only the `@system-service` syscall set, the inet, unix and netlink
  socket families, and native syscalls;
- cannot create namespaces, change personality, gain realtime scheduling, or
  map writable and executable memory.

`@system-service` already contains every syscall the proxy relies on. Checked
with `systemd-analyze syscall-filter` on systemd 257 (Debian 13): `splice`,
`vmsplice`, `tee`, `sendfile`, `copy_file_range` and `pipe2` are in it directly
or through `@ipc`, and `getsockopt`/`setsockopt` through `@network-io`. If a
site-wide or drop-in filter narrows the set, the symptoms differ by syscall,
because `SystemCallErrorNumber=EPERM` makes a blocked call fail with `EPERM`:

- `getsockopt` blocked: the `TCP_INFO` probe fails and every connection uses
  the generic copy (`nautiluslb_pipe_mode_total{mode="generic"}`); nothing breaks.
- `pipe2` blocked: Go cannot create its splice pipe and silently falls back to
  `read`/`write`; nothing breaks.
- `splice` blocked: Go itself falls back to `read`/`write` only on `EINVAL`,
  so NautilusLB probes `splice` once at the first eligible connection. When
  the probe gets `EPERM` it logs `splice(2) unavailable, using the generic copy
  path for all connections` and every connection uses the generic copy
  (`nautiluslb_pipe_mode_total{mode="generic"}`); nothing breaks.

If a future feature is blocked by one of these directives, relax that one
directive in a drop-in. Do not remove the whole block.
