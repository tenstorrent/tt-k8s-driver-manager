# Contributing to tt-k8s-driver-manager

Thank you for your interest in contributing. This document explains how to
report problems, propose changes, and get a pull request merged.

## Code of Conduct

This project follows the [Contributor Covenant Code of Conduct](CODE_OF_CONDUCT.md).
By participating you agree to uphold it. Report unacceptable behavior to
ospo@tenstorrent.com.

## Reporting bugs and requesting features

- Report bugs and request features through
  [GitHub Issues](https://github.com/tenstorrent/tt-k8s-driver-manager/issues).
- Before opening an issue, search existing issues to avoid duplicates.
- For bugs, include the operator version, Kubernetes version, host kernel
  version, the `TenstorrentDriverPolicy` or `TenstorrentFirmwarePolicy`
  manifest involved, and relevant controller and builder pod logs.
- Do **not** report security vulnerabilities through public issues. Follow the
  process in [SECURITY.md](SECURITY.md) instead.

## Submitting changes

Bug fixes and new functionality are submitted as pull requests against the
`main` branch.

1. Fork the repository and create a branch from `main`.
2. Make your change, including tests where the change affects behavior.
3. Run the checks listed below and make sure they pass.
4. Open a pull request. Describe what the change does and why, and link any
   related issues.

Pull requests are reviewed on a weekly cadence. A maintainer may ask for
changes before merging. Pull requests are merged with a squash merge, so keep
the pull request title and description accurate; they become the commit
message on `main`.

### Developer workflow

```bash
go build ./...                          # build the manager binary
go test ./... -race -count=1            # run unit tests
go vet ./...                            # static checks
make generate                           # regenerate CRDs, RBAC, and DeepCopy after editing api/
make helm-lint                          # lint the Helm chart
```

If you change anything under `api/`, run `make generate` and commit the
regenerated files under `config/` and `api/` together with your change.

### Coding standards

- Format Go code with `gofmt`.
- Keep controller reconcile loops idempotent; every code path must be safe to
  re-run.
- Add or update documentation under `docs/` for any user-visible change to the
  custom resources, labels, metrics, or Helm values.

### License headers

Every source file must carry an SPDX header. For new files written for this
project, use:

```go
// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.
```

Use the comment syntax appropriate to the file type (`#` for shell, Makefile,
Dockerfile, and YAML). Do not modify the headers of third-party files.

### Commit messages

Use a short imperative summary line (72 characters or fewer), optionally
followed by a blank line and a longer explanation of the motivation for the
change.

## License

By contributing, you agree that your contributions to source code are
licensed under the [Apache License 2.0](LICENSE), and that your
contributions to documentation and images under `docs/` are licensed under
the [Creative Commons Attribution 4.0 International License](LICENSE-DOCS).
