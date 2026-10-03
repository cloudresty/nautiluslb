# Contributing to NautilusLB

Thank you for your interest in contributing to NautilusLB! We welcome contributions of all kinds, including bug reports, feature requests, code, and documentation improvements.

---

## Branches and Releases

| Branch | Receives | From |
| --- | --- | --- |
| feature or fix branch (`feat/...`, `fix/...`) | your commits | branched from `develop` |
| `develop` | pull requests from feature branches | integration branch; CI runs on every push and PR |
| `main` | pull requests from `develop` only | release branch |

Releases are cut by tagging `main` with `vX.Y.Z`. The tag triggers the release pipeline, which runs every check, then publishes the multi-arch image, the binaries and the Helm chart and signs them with cosign. Do not tag `develop` or feature branches.

---

## How to Contribute

### 1. Fork and Clone

Fork [cloudresty/nautiluslb](https://github.com/cloudresty/nautiluslb), then:

```bash
git clone https://github.com/your-username/nautiluslb.git
cd nautiluslb
git checkout develop
git checkout -b feat/my-change
```

### 2. Make Your Changes

- The Go module lives in `app/` (entry point `app/cmd/nautiluslb`, packages under `app/internal/`). Run the `make` targets from the repository root.
- Follow the existing code style. Add or update tests with every behaviour change.
- Update the user documentation in `docs/` when usage, configuration, metrics or operations change.
- Read the behavioural guarantees in [docs/operations.md](docs/operations.md#behavioural-guarantees) before touching the data path, discovery or the lifecycle. Breaking one is a bug.

### 3. Run the Checks

```bash
make test          # unit tests with the race detector
make lint          # golangci-lint v2
make vuln          # govulncheck
make integration   # in-process end-to-end tests (real runtime, fake Kubernetes clientset, loopback backends)
make fuzz          # every Fuzz target for FUZZTIME (default 30s) each
make bench         # benchmarks with allocation stats
helm lint deploy/helm/nautiluslb
bash deploy/check-drift.sh   # deploy/kubernetes must match the chart's rendered defaults
```

With Docker, [kind](https://kind.sigs.k8s.io/) and kubectl installed, run the end-to-end tests. They create a throwaway kind cluster, run NautilusLB against it and delete everything afterwards:

```bash
make e2e               # every case
make e2e CASE=drain    # one case
KEEP=1 make e2e        # keep the cluster and container for debugging
make soak              # long-running load; checks goroutine and memory growth
```

Set `EMIT_LEVEL=error` to keep test output quiet. CI runs these checks on every pull request.

### 4. Linux-only Tests on macOS

The zero-copy `splice(2)` path and its tests (`app/internal/pipe/*_linux*.go`) only build on Linux. On macOS, run them in a container, sharing your module cache so dependencies are not downloaded again:

```bash
docker run --rm \
  -v "$PWD/app":/src \
  -v "$(go env GOMODCACHE)":/go/pkg/mod \
  -w /src \
  golang:1.27.1 \
  go test -race ./internal/pipe/... ./internal/tcpproxy/...
```

### 5. Commit and Push

- One logical change per commit. Write the subject in the imperative mood, at most about 72 characters, without a trailing period (`Add per-backend connection cap to UDP pools`).
- Use the body to explain why, and anything a reviewer cannot see in the diff (invariants, trade-offs, follow-ups).
- Run `gofmt` on touched Go files and make sure `make test` and `make lint` pass before pushing.

```bash
git push origin feat/my-change
```

### 6. Open a Pull Request

Open a pull request against `develop` (never `main`). Describe the change, how you tested it, and reference related issues.

---

## Adding a Metric

1. Register it in [`app/internal/metrics/metrics.go`](app/internal/metrics/metrics.go): add a method to the `Recorder` interface, implement it on the Prometheus recorder and on the no-op recorder (`NewNop`), and create the collector through the `factory` helpers, which add the `nautiluslb_` prefix. Labels must be bounded; never add a client address, client port or SNI name.
2. Add its name to the catalogue test `TestEveryCatalogueMetricRegistered` in [`app/internal/metrics/metrics_test.go`](app/internal/metrics/metrics_test.go) and exercise it there.
3. Document it in [docs/metrics.md](docs/metrics.md): catalogue row (type, labels, meaning), the `perBackend` table if it has a `backend` label, and any rejection reason or label value it adds.

## Adding a Configuration Field

1. **Type:** add the field to the struct in [`app/internal/config/config.go`](app/internal/config/config.go) with a camelCase `yaml` tag. Use `config.Duration` for durations.
2. **Default:** set it in [`defaults.go`](app/internal/config/defaults.go).
3. **Validation:** report problems in [`validate.go`](app/internal/config/validate.go), appended to the joined error list with the usual `configurations[<i>] (<name>)` prefix. Never stop at the first error.
4. **Environment override** (only for `settings.*` keys that operators need to override per host): add it in [`env.go`](app/internal/config/env.go) as `NLB_*`.
5. **Tests:** cover defaulting, valid and invalid values in the config package tests.
6. **Docs:** add a row to [docs/configuration.md](docs/configuration.md) (and to its environment table if you added a variable), state whether a hot reload applies it, and add the key to [`app/config.example.yaml`](app/config.example.yaml). If the Helm chart's default `config` should set it, update [`deploy/helm/nautiluslb/values.yaml`](deploy/helm/nautiluslb/values.yaml) and the raw manifests in `deploy/kubernetes/`.

---

## Dependencies

Never run `go get -u all` or `go get -u ./...`. Bump `k8s.io/api`, `k8s.io/apimachinery` and `k8s.io/client-go` together to the same version and let `go mod tidy` choose the rest; their transitive dependencies must move as one set.

---

## Code of Conduct

By participating in this project, you agree to abide by the [Contributor Covenant Code of Conduct](https://www.contributor-covenant.org/version/2/0/code_of_conduct/).

---

## Reporting Issues

If you find a bug or have a feature request, please [open an issue](https://github.com/cloudresty/nautiluslb/issues) and provide as much detail as possible: version (`nautiluslb --version`), the relevant part of `config.yaml`, logs and, for data-path problems, the access-log `result` field.

Report security issues privately to the maintainers instead of opening a public issue.

---

## Questions

If you have questions or need help, feel free to open an issue or start a discussion.

---

Thank you for helping make NautilusLB better!
