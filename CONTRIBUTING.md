# Contributing

Issues and pull requests are welcome, especially compatibility reports for other SC Series models and SCOS releases.

- `gofmt`, `go vet ./...` and `go test -race ./...` must pass before submitting.
- New Storage Center behaviour belongs in the fake array (`internal/sc/scfake`) with a test that reproduces it.
- Changes that touch the array or nodes should be checked with `hack/csi-e2e.sh all` and, when relevant, the failover scripts (see [docs/testing.md](docs/testing.md)). Mention in the PR what you ran and on which SCOS version.
- Never include array addresses, serial numbers, WWPNs, certificates or credentials from your environment in issues, logs or test fixtures.
- Sign off your commits (`git commit -s`, [DCO](https://developercertificate.org/)).
