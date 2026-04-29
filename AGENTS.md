# Repository Guidelines

## Project Structure & Module Organization

This is a Go service for autoscaling ephemeral GitHub Actions runners. The executable entrypoint is `cmd/gharp`; package code is under `internal/`. Key packages include `config` for environment parsing, `github` for GitHub App auth/API calls, `httpapi/handlers` for setup and webhook routes, `scheduler` and `reconciler` for orchestration, `runner` for Docker startup, and `store` for SQLite persistence. SQL schema is in `internal/store/schema.sql`; setup templates are in `internal/httpapi/handlers/templates`. Operational docs live in `docs/`, with container definitions in `Dockerfile` and `docker-compose.yml`.

## Build, Test, and Development Commands

- `go test ./...`: run the standard unit test suite.
- `go test -race ./...`: run tests with the race detector, matching CI.
- `go vet ./...`: run Go static checks used by CI.
- `go build ./...`: compile all packages.
- `go build -trimpath -ldflags="-s -w" -o gharp ./cmd/gharp`: build the release-style local binary.
- `go test -tags smoke -count=1 ./cmd/gharp`: run smoke tests.
- `go test -tags integration -count=1 ./cmd/gharp`: run integration tests.
- `docker compose up --build`: run with Docker Compose; provide `.env` with at least `BASE_URL`.

## Coding Style & Naming Conventions

Use standard Go formatting: run `gofmt` on edited Go files and keep imports organized. Follow idiomatic Go naming: exported identifiers use `PascalCase`, unexported identifiers use `camelCase`, and package names stay short and lowercase. Keep package responsibilities narrow; extend the `internal/*` package that owns the behavior.

Linting is configured in `.golangci.yml` with standard linters plus `errorlint`, `gocritic`, `misspell`, `unconvert`, and `unparam`.

## Testing Guidelines

Tests are colocated with code and named `*_test.go`. Prefer table-driven tests for validation and state-machine behavior. Use existing build tags for broader checks: `smoke` and `integration`. Some tests use `httptest` and local listeners, so restricted sandboxes may fail even when normal local and CI environments pass.

## Commit & Pull Request Guidelines

Commit history follows conventional-style messages such as `feat(reconciler): ...`, `fix: ...`, `docs: ...`, and `chore(deps): ...`. Keep subjects imperative and scoped when useful.

Pull requests should include a concise behavior summary, linked issue if applicable, configuration or deployment notes, and test evidence such as `go test -race ./...`. Include screenshots only for setup UI/template changes.

## Security & Configuration Tips

Never commit GitHub App credentials, private keys, `.env`, or SQLite data files. `BASE_URL` is sticky after setup, and self-hosted runners should only be installed on trusted private repositories. Review `docs/configuration.md` before changing environment variables or runner command templates.
