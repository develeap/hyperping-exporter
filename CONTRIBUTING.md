# Contributing to hyperping-exporter

Thank you for helping improve this project. Contributions of all kinds are welcome — bug fixes, new metrics, documentation improvements, and CI enhancements.

## Prerequisites

| Tool | Version |
|------|---------|
| Go | 1.22+ |
| golangci-lint | v2+ |
| Docker | any recent |
| GNU Make | any |

## Getting Started

```bash
git clone https://github.com/develeap/hyperping-exporter.git
cd hyperping-exporter
make build
```

## Running Tests

Tests use pre-recorded HTTP interactions via [go-vcr](https://github.com/dnaeon/go-vcr). **No real API key is required** for the test suite.

```bash
make test          # runs with race detector and coverage
```

To run a single test:

```bash
go test -run TestFunctionName ./internal/collector/
```

### Real API Tests

If you need to validate against the live Hyperping API:

```bash
export HYPERPING_API_KEY=your_key_here
go test ./... -run TestRealAPI
```

## Linting

```bash
make lint
```

The CI pipeline enforces a clean lint pass. Fix all reported issues before opening a PR.

## VCR Cassettes

Recorded HTTP interactions are stored in `internal/client/testdata/` as `.yaml` files. These cassettes allow the test suite to run offline and deterministically.

**To update cassettes** (e.g. after adding a new API call):
1. Delete the relevant `.yaml` file(s) in `internal/client/testdata/`
2. Set `HYPERPING_API_KEY` to a valid key
3. Re-run `make test` — go-vcr will record fresh interactions

Commit the updated cassette files alongside your code change.

## Pull Request Requirements

- **Conventional commits** — use `feat:`, `fix:`, `refactor:`, `test:`, `docs:`, `ci:`, `chore:`
- **Tests pass** — `make test` must succeed with no race conditions
- **Lint clean** — `make lint` must produce no errors
- **Coverage** — maintain 90%+ test coverage; the CI pipeline enforces this
- **One concern per PR** — keep PRs focused; split unrelated changes

## Branch Naming

```
feature/short-description
fix/short-description
docs/short-description
```

## Commit Signing

Please sign your commits:

```bash
git commit -S -m "feat: add SLA breach metric"
```

If you haven't set up GPG signing, see [GitHub's guide](https://docs.github.com/en/authentication/managing-commit-signature-verification).

## Security Vulnerabilities

Do **not** open a public issue for security vulnerabilities. See [SECURITY.md](SECURITY.md) for the responsible disclosure process.
