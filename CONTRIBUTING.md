# Contributing to ProxyLM.GO

Thank you for your interest in contributing. ProxyLM.GO is an OpenAI-compatible HTTP
proxy for local LLMs (LM Studio, Ollama) with model-aware queueing, retry/failover,
SSE streaming, and a Bubble Tea TUI. It is a focused, single-purpose tool — not a
general-purpose proxy framework.

## Prerequisites

- **Go 1.25 or later** (required by `modernc.org/sqlite`; pure-Go, no CGO).
- **golangci-lint** — install via the [official instructions](https://golangci-lint.run/welcome/install/).
- A running LM Studio or Ollama instance is helpful for manual testing but not required
  for unit tests.

## Building and testing

```sh
# Build (substitute your OS binary extension as needed)
go build -ldflags "-s -w -X main.version=dev" -o bin/proxylm .

# Run the daemon
go run . serve

# Run the TUI client
go run . tui --connect ws://localhost:8080 --token <admin_key>

# Run all tests
go test ./...

# Coverage for the core packages
go test -cover ./internal/core/...

# Vet + lint
gofmt -l .
go vet ./...
golangci-lint run
```

All four commands (`go build`, `go test`, `go vet`, `golangci-lint`) must pass cleanly
before you open a pull request.

## Commit format

ProxyLM.GO uses **Conventional Commits**:

```
<type>(<optional scope>): <short summary>
```

Allowed types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`.

Examples:
```
feat(router): add weighted round-robin strategy
fix(scheduler): prevent duplicate wakeup on concurrent enqueue
docs: update API.md with /healthz endpoint
chore: bump golangci-lint to v2
```

The summary line must be ≤72 characters and written in the imperative mood ("add", "fix",
"update" — not "added" or "fixes").

The `Co-Authored-By:` trailer is welcome when a commit was developed with AI assistance:
```
Co-Authored-By: Claude <noreply@anthropic.com>
```

## Pull request process

1. **Fork** the repository and create a feature branch from `main`:
   ```sh
   git checkout -b fix/scheduler-wakeup
   ```
2. Make your changes. Write or update tests as described below.
3. Run the full pre-commit checklist (see below).
4. Open a PR against `main`. Fill in the PR template.
5. CI must be green. The maintainer will review and may request changes.
6. PRs are merged with **squash-merge**; the squash commit message follows Conventional
   Commits format. Branch-level history is preserved in the PR description.

## Code style

- `gofmt` is mandatory. Unformatted code will fail CI.
- `go vet ./...` must produce no output.
- `golangci-lint run` must produce no new warnings relative to `main`.
- All public functions that perform I/O must accept `context.Context` as their first
  parameter.
- No CGO. SQLite is `modernc.org/sqlite` only.
- No `panic` for expected errors — return `error` explicitly.
- Logging uses `log/slog` with JSON handler. Do not log API keys, admin tokens, or
  request body content.

## Tests

- New user-visible functionality requires at least one test.
- Tests follow the **table-driven** pattern:
  ```go
  tests := []struct {
      name string
      // ...
  }{
      // ...
  }
  for _, tc := range tests {
      t.Run(tc.name, func(t *testing.T) { ... })
  }
  ```
- Use `t.Helper()` in test utilities.
- Do not use `time.Sleep` for synchronisation in scheduler tests — use channels or
  `sync.WaitGroup`.
- **Coverage target:** `internal/core/...` packages must maintain ≥ 80 % statement
  coverage. Check with `go test -cover ./internal/core/...`.

## Pre-commit checklist

Before pushing or opening a PR, confirm:

- [ ] `go build ./...` succeeds.
- [ ] `go test ./...` passes (no failures, no data races with `-race`).
- [ ] `gofmt -l .` returns no files.
- [ ] `golangci-lint run` returns no new issues.
- [ ] `CHANGELOG.md` updated under `[Unreleased]` for any user-visible change.
- [ ] Relevant documentation in `docs/` updated if the change affects architecture,
      API contract, or SRS requirements.

## Reporting bugs

Use the [Bug report issue template](https://github.com/MaxWD/ProxyLM.GO/issues/new?template=bug_report.yml).
Please redact all API keys and tokens before pasting logs or config snippets.

## Security vulnerabilities

**Do not open a public issue for security bugs.** See
[SECURITY.md](./SECURITY.md) for the private reporting process.

## DCO / CLA

No Contributor License Agreement or Developer Certificate of Origin is required.
By submitting a pull request you agree that your contribution will be licensed
under the same [MIT License](./LICENSE) that covers this project.

---

## Для русскоязычных контрибьюторов

Спасибо за интерес к проекту. Правила те же:

- Коммиты — в формате Conventional Commits на **английском**: `feat:`, `fix:`, `docs:` и т.д.
- Перед PR — `go test ./...`, `go vet ./...`, `golangci-lint run` без ошибок.
- CHANGELOG.md — обновить секцию `[Unreleased]`, если изменение видно пользователю.
- Баги и уязвимости — через шаблоны Issues; для security-уязвимостей — только через
  [приватный advisory](https://github.com/MaxWD/ProxyLM.GO/security/advisories/new) или
  email из `SECURITY.md`.
- PR-ы вливаются squash-merge'ем; squash-сообщение пишет мейнтейнер.
