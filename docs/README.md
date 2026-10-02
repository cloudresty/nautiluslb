# NautilusLB documentation

NautilusLB is a Layer 4 load balancer for TCP, TLS passthrough (SNI routing) and UDP. It runs at the edge of, or outside, a Kubernetes cluster. It forwards connections to the Services that opt in with annotations. It discovers those Services, their EndpointSlices and the cluster's Nodes through Kubernetes informers.

| Document | Read it when you need to |
| --- | --- |
| [configuration.md](configuration.md) | write or review `config.yaml`: every field, its default, validation, environment override and reload behaviour; Service annotations and the binding rules |
| [security.md](security.md) | understand the threat model: who can attach a backend to a listener, PROXY protocol trust, admin exposure, RBAC |
| [metrics.md](metrics.md) | scrape and alert on NautilusLB: every metric, its labels, cardinality and example alerts |
| [operations.md](operations.md) | run it: signals, reload, drain, sizing, kernel settings, troubleshooting by access-log result |
| [ha.md](ha.md) | run two or more instances behind a floating IP (keepalived/VRRP, kube-vip) |
| [upgrading-v2.md](upgrading-v2.md) | move a v1.x deployment to v2.0.0 |

## Quick start

1. Copy [`app/config.example.yaml`](../app/config.example.yaml) to `config.yaml` and edit it. Run `nautiluslb --validate --config config.yaml` to check it. This loads and validates the file, prints a summary and never touches the network.
2. Grant the [RBAC](security.md#rbac-least-privilege) the configuration needs. Use `list` + `watch` on Nodes, and on Services and EndpointSlices in each allowlisted namespace.
3. Annotate every backend Service. See [Service annotations](configuration.md#service-annotations).
4. Start `nautiluslb --config config.yaml`. Then check `curl -s 127.0.0.1:9090/readyz`, which returns `{"status":"ready"}` once discovery has synced.

## Command line

| Flag | Env | Meaning |
| --- | --- | --- |
| `--config <path>` | `NLB_CONFIG` (sets the default) | configuration file; default `config.yaml` in the working directory (`/nautiluslb/config.yaml` in the image) |
| `--validate` | | load and validate the configuration (environment overrides included), print a summary and deprecations, exit 0 (valid) or 1 (invalid) |
| `--pprof` | `NLB_PPROF` (via `settings.admin.pprof`) | serve `/debug/pprof/*` on the admin server |
| `--version` | | print version, commit, build date and Go version |
| `--help` | | usage |

The other `NLB_*` variables override `settings.*` keys. See [Environment overrides](configuration.md#environment-overrides).
