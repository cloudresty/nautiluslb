# Contributing to NautilusLB

Thank you for your interest in contributing to NautilusLB! We welcome contributions of all kinds, including bug reports, feature requests, code, and documentation improvements.

---

## How to Contribute

### 1. Fork the Repository

Click the "Fork" button at the top right of the [NautilusLB GitHub page](https://github.com/cloudresty/nautiluslb) to create your own copy of the repository.

### 2. Clone Your Fork

```bash
git clone https://github.com/your-username/nautiluslb.git
cd nautiluslb
```

### 3. Create a Branch

Create a new branch for your changes:

```bash
git checkout develop
git checkout -b my-feature-or-bugfix
```

### 4. Make Your Changes

- Follow the existing code style and conventions.
- Add or update tests as appropriate.
- Update documentation if your changes affect usage or configuration.

### 5. Run Tests

The Go module lives in `app/`, so run the checks from the repository root through `make`:

```bash
make test   # unit tests with the race detector (go test -race ./... in app/)
make lint   # golangci-lint v2
make vuln   # govulncheck
```

If you have Docker, [kind](https://kind.sigs.k8s.io/) and kubectl, also run the end-to-end test. It creates a throwaway kind cluster, runs NautilusLB against it and deletes everything afterwards (`KEEP=1` keeps the cluster for debugging):

```bash
make e2e
```

CI runs all of these on every pull request.

### 6. Commit and Push

Commit your changes with a clear message:

```bash
git add .
git commit -m "Describe your change"
git push origin my-feature-or-bugfix
```

### 7. Open a Pull Request

Go to your fork on GitHub and open a pull request (PR) against the `develop` branch of the upstream repository (`main` only receives releases from `develop`). Please include a clear description of your changes and reference any related issues.

---

## Code of Conduct

By participating in this project, you agree to abide by the [Contributor Covenant Code of Conduct](https://www.contributor-covenant.org/version/2/0/code_of_conduct/).

---

## Reporting Issues

If you find a bug or have a feature request, please [open an issue](https://github.com/cloudresty/nautiluslb/issues) and provide as much detail as possible.

---

## Style Guide

- Use `gofmt` to format your code.
- Write clear, concise commit messages.
- Document exported functions and types.
- Add or update unit tests for new features or bug fixes.

---

## Questions

If you have questions or need help, feel free to open an issue or start a discussion.

---

Thank you for helping make NautilusLB better!
