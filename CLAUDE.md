# NautilusLB

Open-source Layer 4 (TCP) load balancer that runs outside or at the edge of a Kubernetes cluster and forwards each listener port to the NodePorts (or ClusterIP:port) of annotated Services.

- Module: `github.com/cloudresty/nautiluslb` in `app/` · Go 1.27.1 · k8s client v0.37.1 · YAML via `go.yaml.in/yaml/v3`
- Follows the Cloudresty backend **Service Blueprint** and **Naming Conventions** (standards pointer in ~/.claude/GO.md).
- Branching: PRs target `develop`; `develop` -> PR -> `main`, then tag `vX.Y.Z` (CI publishes the image).

## Architecture

Entry point: `app/cmd/nautiluslb/main.go` (flags `--config` (default `config.yaml`, env `NLB_CONFIG`), `--version`); exits 1 on invalid config or a listener that cannot bind. Version/commit/date are injected by ldflags into `internal/version` (Makefile, Dockerfile build args).

Packages under `app/internal/`:

- **config**: config types, annotation constants, strict validation, `config.Load` (strict decode: `KnownFields`, single document).
- **discovery**: client init (in-cluster first, then `kubeconfigPath`, then `~/.kube/config`); discovery POLLS nodes + services every 30s (no watches/informers). Only `list nodes` and `list services` are called.
- **tcpproxy**: listener, round-robin selection, health checks (TCP connect, fixed 10s), connection proxying. No connection pooling.
- **backend**: backend server state (atomics).
- **version**: build metadata, `version.String()`.

## Domain rules

- A Service is a backend of configuration C only if it has `nautiluslb.cloudresty.io/enabled: "true"` AND `nautiluslb.cloudresty.io/configurations` listing C.name (comma-separated), is in one of C's `namespaces` (`["*"]` = cluster-wide; legacy `namespace` is merged), and has a port named C.backendPortName. Before v1.0.1 the port name alone matched (traffic hijack).
- NodePort/LoadBalancer -> every node InternalIP : nodePort (nodePort 0 skipped). ClusterIP -> clusterIP : port (not targetPort). Headless skipped.
- Config is strict: unknown keys, bad names (`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`), bad `listenerAddress` (`:port` / `IP:port`), duplicates, conflicting listeners and empty namespaces are all errors, reported together. Example: `app/config.example.yaml` (`app/config.yaml` and `app/kubeconfig` are local, git-ignored).

## Build, image, CI

- Build context is the repo root; the root `.dockerignore` is an allowlist (`app/` minus kubeconfig/config.yaml). There is no `build/.dockerignore` (Docker never read it).
- Image: `build/Dockerfile`, distroless static nonroot (UID 65532), no shell. Contract: binary `/nautiluslb/nautiluslb` (ENTRYPOINT), WORKDIR `/nautiluslb`, config at `/nautiluslb/config.yaml`. Base images pinned by digest.
- CI `.github/workflows/ci.yaml`: test (race), lint (golangci-lint v2, pinned), vuln (govulncheck, pinned), docker-build, e2e; release on tag needs all of them; `latest` only moves when the tag is the highest semver tag. Actions pinned to SHAs.

## Commands

```bash
make test         # go test -race ./... in app/
make lint         # golangci-lint run in app/ (v2)
make vuln         # govulncheck
make e2e          # test/e2e/run.sh (docker + kind + kubectl); KEEP=1 keeps cluster/container
make build-local  # binary at ./nautiluslb
make build        # docker image (linux/amd64)
make shell        # bash in the builder stage (runtime image has no shell)
```

## E2E design (test/e2e/)

kind cluster `nautiluslb-e2e` (separate kubeconfig, never touches ~/.kube/config). NautilusLB runs as a plain container on the `kind` docker network (not in the cluster), with a token kubeconfig for the least-privilege SA in `rbac.yaml`.

Fixtures in `manifests.yaml`: `bound` (must get all traffic), `intruder` (same namespace, names another config), `outsider` (names the config, namespace outside the allowlist). Also asserts invalid configs exit non-zero.

The script waits until every fixture answers on its NodePort and settles past one health-check round before asserting; without that, a wrongly bound backend can fail its first check and hide a hijack.

## Dependency trap

Never `go get -u all` / `go get -u ./...`. The k8s.io modules (api, apimachinery, client-go, and their kube-openapi / structured-merge-diff) must move as one pinned set: newer kube-openapi and structured-merge-diff v7 do not build with apimachinery v0.37. Bump `k8s.io/{api,apimachinery,client-go}` together to the same version and let `go mod tidy` pick the rest.
