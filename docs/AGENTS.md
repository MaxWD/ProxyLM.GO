# Specialized Agents for ProxyLM.GO Development

Each role below is a separate executor (sub-agent) to whom a portion of the work is delegated. The role name matches the filename in `.claude/agents/<role>.md` and is used as the `owner` in the task list.

## Document and Code Ownership Map

| File / directory            | Owner                 | Audience                              |
|-----------------------------|-----------------------|---------------------------------------|
| `docs/ARCHITECTURE.md` (EN, primary) / `docs/ARCHITECTURE.ru.md` (RU, parallel) | tech-writer | internal team / agents |
| `docs/SRS.md` (EN) / `docs/SRS.ru.md` (RU) | tech-writer | implementors                       |
| `docs/API.md` (EN) / `docs/API.ru.md` (RU) | tech-writer | backend + integrators              |
| `docs/AGENTS.md` (EN, this) / `docs/AGENTS.ru.md` (RU) | tech-writer | role orchestration        |
| `docs/FUTURE.md` (EN) / `docs/FUTURE.ru.md` (RU) | tech-writer | idea parking (read-only for executors) |
| `CLAUDE.md` (root, RU-only)  | tech-writer           | Claude Code (AI assistant)            |
| `README.md` (EN, primary) / `README.ru.md` (RU, parallel) | go-devops-cli (content) + tech-writer (translation sync) | external users |
| `config.example.yaml`       | go-backend-engineer   | users                                 |
| `go.mod` / `go.sum`         | go-devops-cli         | build / dependencies                  |
| `main.go`, `cmd/*`          | go-devops-cli         | entry point and CLI                   |
| `internal/core/*`           | go-backend-engineer   | core                                  |
| `internal/api/*`            | go-backend-engineer   | HTTP server                           |
| `internal/storage/*`        | go-backend-engineer   | SQLite                                |
| `internal/config/*`         | go-backend-engineer   | config                                |
| `internal/logging/*`        | go-backend-engineer   | logging                               |
| `internal/ipc/server.go`    | go-backend-engineer   | publisher                             |
| `internal/ipc/client.go`    | go-tui-engineer       | TUI consumer                          |
| `internal/ipc/messages.go`  | tech-writer (schemas) / go-backend-engineer (types) | both use |
| `internal/tui/*`            | go-tui-engineer       | TUI application                       |
| `internal/service/*`        | go-devops-cli         | service install / lifecycle           |
| `scripts/*`                 | go-devops-cli         | build / packaging                     |
| `test/integration/*`        | go-qa-tests           | e2e tests                             |
| `*_test.go` (unit)          | package owner         | unit tests                            |
| `LICENSE`                   | github-publisher      | license text (MIT)                    |
| `SECURITY.md`               | github-publisher      | CVE disclosure policy                 |
| `CONTRIBUTING.md`           | github-publisher      | contributor guide                     |
| `CODE_OF_CONDUCT.md`        | github-publisher      | Contributor Covenant                  |
| `CHANGELOG.md`              | github-publisher (structure) / tech-writer (content) | release notes |
| `.github/workflows/*`       | github-publisher      | CI/CD on GitHub Actions               |
| `.github/ISSUE_TEMPLATE/*`  | github-publisher      | bug / feature forms                   |
| `.github/PULL_REQUEST_TEMPLATE.md` | github-publisher | PR checklist                   |
| `.github/dependabot.yml`    | github-publisher      | dependency auto-update                |
| `.github/FUNDING.yml`       | github-publisher      | GitHub Sponsors (optional)            |
| `.goreleaser.yml` (opt.)    | go-devops-cli (build) / github-publisher (release pipeline) | release pipeline |

## 1. tech-writer (Architect / Technical Writer)

**Goal:** keep `docs/ARCHITECTURE.md`, `docs/SRS.md`, `docs/API.md`, `docs/AGENTS.md`, and `CLAUDE.md` up to date. Translate architectural changes into verifiable requirements and into instructions for the AI assistant.

**Responsibilities:**
- Record the API contract (OpenAI v1, SSE format, `/admin/stream` protocol).
- Describe data structures (Request, ServerState, ModelInfo) and their lifecycle.
- Describe scheduler invariants and test cases for them (in particular: "model is not switched while there are requests for it").
- List acceptance criteria for MVP.
- Precisely enumerate what is **not** in scope for MVP.
- Maintain `CLAUDE.md` in the root: code conventions, commands for running tests/lint, entry points, core invariants, links to ARCHITECTURE/SRS/API. This file is context for the AI assistant, not for humans; keep it focused (no duplication of README), but without strict line limits.
- Maintain `docs/FUTURE.md` — the "idea parking lot". At each release or significant change, revise the list: remove implemented items, record new ideas that emerged during work. From FUTURE.md **nothing is implemented by the agent or Claude Code** — items are executed only on explicit user request (see §FUTURE-RULE in CLAUDE.md).
- **Bilingual sync (BILINGUAL-RULE — see CLAUDE.md):** keep every English / Russian pair in `docs/` aligned (`docs/X.md` ↔ `docs/X.ru.md`). Any semantic change to one language version must be reflected in the other in the same commit. Cosmetic fixes (typos, grammar) may be made in a single language. Structure (section numbering, FR/NFR/INV/AC ids) must remain identical between pairs. Cross-references in EN files point to EN (`docs/SRS.md`); in RU files — to RU (`docs/SRS.ru.md`).
- **README translation sync:** tech-writer co-owns `README.ru.md` translation accuracy. Content (features, commands, install) is updated by go-devops-cli in `README.md`; tech-writer keeps `README.ru.md` aligned.

**Deliverable format:** `docs/SRS.md` (+ `.ru.md`) + `docs/API.md` (+ `.ru.md`) + `docs/ARCHITECTURE.md` (+ `.ru.md`) + `docs/AGENTS.md` (+ `.ru.md`) + `docs/FUTURE.md` (+ `.ru.md`) + `CLAUDE.md`. Each EN file has a parallel `.ru.md` of equal authority.

## 2. go-backend-engineer (Backend / Core)

**Goal:** implement the proxy core in Go.

**Responsibilities:**
- `internal/config/config.go` — Go config structs with `yaml` tags + explicit validation.
- `internal/config/template.go` — embedded template (`//go:embed config.example.yaml`) + auto-generation.
- `internal/core/models.go` — types `RequestRecord`, `ServerInfo`, `ModelInfo`, statuses.
- `internal/core/scheduler.go` — per-server worker goroutine, drain-current-then-switch.
- `internal/core/router.go` — server selection for (model).
- `internal/core/retry.go` — retry policy + failover (with `context.Context` cancellation).
- `internal/core/discovery.go` — periodic poll `/v1/models` via `time.Ticker`.
- `internal/core/backends/openai.go` — client to OpenAI-compatible servers (LM Studio, Ollama).
- `internal/api/*` — `net/http` + `chi` application, authorization, routes `/v1/*`, `/admin/stream`, graceful shutdown.
- `internal/api/streaming.go` — SSE proxying (`http.Flusher`) + token counting.
- `internal/storage/db.go` — `database/sql` over `modernc.org/sqlite`, migrations via `//go:embed`.
- `internal/storage/history.go` — async writer via channel, cleanup by retention.
- `internal/logging/slog.go` — `log/slog` setup (JSON handler, levels).
- `internal/ipc/server.go` — WebSocket publisher (`coder/websocket`).
- Unit tests for scheduler/router/retry (table-driven, coverage ≥ 80%).

**Implementation priority:** non-streaming end-to-end path first, then streaming.

**Idiomatic Go:**
- `context.Context` as first parameter in all public functions with I/O.
- Explicit `error` return, no panic in normal flow.
- Concurrency: goroutines + channels for queues; `sync.Mutex` + `atomic.*` for shared state.
- One `*http.Client` per backend with a configured `*http.Transport`.

## 3. go-tui-engineer (TUI)

**Goal:** Bubble Tea application with a btop-style interface + IPC client to the daemon.

**Responsibilities:**
- `internal/ipc/client.go` — WebSocket client (`coder/websocket`), subscribing to `state`/`log`, reconnection with backoff.
- `internal/tui/app.go` — Bubble Tea `Model` / `Update` / `View`. Message sources: WS frame, `tea.Tick` for auto-hide, key events.
- `internal/tui/widgets.go` — `HeaderBar`, `RequestTable` (with auto-removal via `show_completed_minutes`), `LogPane`.
- `internal/tui/styles.go` — `lipgloss` styles (border, foreground, padding, layout).
- `internal/tui/keys.go` — hotkeys: F5 refresh, F10 quit, q quit, / search.
- Correct operation in Windows Terminal / cmd / PowerShell (ASCII fallback via env `PROXYLM_NO_UNICODE=1`).

**Architectural constraints:**
- TUI ↔ daemon — only via WebSocket; no direct access to the database or core.
- No side I/O in `Update()` — only via `tea.Cmd`.

## 4. go-devops-cli (CLI / Packaging / Launch)

**Goal:** a single way to run on Windows + Linux + macOS, with a clear README. **Owner of `README.md` and the build.**

**Responsibilities:**
- `main.go` — entry point, cobra root initialization.
- `cmd/*.go` — `serve`, `tui`, `config init|validate`, `service install|uninstall|start|stop|status`, `version`. Uses `spf13/cobra`.
- `internal/service/service.go` — integration with `github.com/kardianos/service` (Windows Service / systemd / launchd).
- `scripts/build.ps1`, `scripts/build.sh`, `scripts/build-all.ps1` — build and cross-compilation (`GOOS`/`GOARCH`).
- `README.md` (root) — project description, downloading the pre-built binary, quick start (3 commands: place → `./proxylm serve` → `./proxylm tui`), TUI screenshot/mockup, link to `docs/ARCHITECTURE.md` for details. Section on `service install` for Windows and Linux.
- `go.mod` / dependencies.
- Documentation on release packaging (optionally, GitHub Actions for auto-building artifacts).

## 5. github-publisher (Publication / Release Manager)

**Goal:** prepare and maintain the project for publication on GitHub under `MaxWD/ProxyLM.GO` (public) following global open-source practices.

**Responsibilities:**
- Root open-source files: `LICENSE` (MIT, Copyright (c) Maxim Dolgushev), `SECURITY.md`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md` (Contributor Covenant 2.1), `CHANGELOG.md` (Keep a Changelog).
- `.github/` infrastructure: `workflows/ci.yml` (build+test+lint on Linux/Windows/macOS), `workflows/release.yml` (cross-build on git tag v*, 5 targets: win-amd64, linux-amd64, linux-arm64, darwin-amd64, darwin-arm64), `workflows/govulncheck.yml`, `dependabot.yml`, `ISSUE_TEMPLATE/*`, `PULL_REQUEST_TEMPLATE.md`.
- Pre-publish secret/privacy audit: grep patterns for API keys and tokens, `.gitignore` check (config.yaml, *.db, bin/, *.exe, .git/COMMIT_MSG_*.txt), git history audit.
- Release pipeline: artifact packaging with README+LICENSE+config.example.yaml, SHA256 checksums, `gh release create` with body from CHANGELOG.
- Repository metadata: recommendations for topics, description, social preview image.
- Coordination: delegates to tech-writer for English README and CHANGELOG content; to go-devops-cli for GoReleaser/build scripts and go.mod path; to go-qa-tests for coverage badge and pre-release smoke; to backend/tui for audit of hardcoded personal data.
- List of things requiring human decisions: repository creation, branch protection rules, GPG commit signing, topics, FUNDING.yml, versioning strategy for the first public release.

**What it does NOT do:**
- Does not execute `git push`, `gh release create`, `gh repo create` without explicit request.
- Does not rewrite git history (`filter-repo`) without explicit user decision.
- Does not list Claude/Anthropic as copyright holder in LICENSE (Claude attribution — only the `Co-Authored-By:` trailer in commits).
- Does not directly edit code in `internal/*`, `cmd/*`, `docs/*` — only via delegation.

**Deliverable format:** `LICENSE`, `SECURITY.md`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `CHANGELOG.md` (structure), `.github/**` (complete), pre-publish audit report, action checklist for the user before publishing.

## 6. go-qa-tests (Tests)

**Goal:** cover critical logic and ensure regression safety.

**Responsibilities:**
- Unit tests — alongside packages (Go convention `*_test.go`):
  - `internal/core/scheduler_test.go` — scheduler invariants (test cases from SRS, INV-1..INV-8).
  - `internal/core/router_test.go` — server selection, edge cases (no such model; all servers down).
  - `internal/core/retry_test.go` — backoff + failover.
- `test/integration/api_e2e_test.go` — spin up `net/http.ServeMux` + mock backends via `httptest.Server`, run scenarios:
  - 10 requests for model A + 5 for model B → model A is never unloaded while its queue is non-empty.
  - Server crashes mid-request → failover to the backup server.
  - Streaming response (mock SSE via `httptest.Server` + `http.Flusher`).
- Core coverage ≥ 80% lines (`go test -cover ./internal/core/...`).
- NFR-1 benchmark (`go test -bench` on mock backend).

**Test conventions:**
- Table-driven (`tests := []struct{name string; ...}{...}` + `t.Run(tt.name, ...)`).
- `t.Helper()` in utilities.
- No `time.Sleep` in scheduler tests — use sync channels / `sync.WaitGroup`.

## Work Order

1. **tech-writer** produces the specification from the architecture → review with user.
2. In parallel:
   - **go-backend-engineer** builds the core skeleton (config + storage + api without streaming + scheduler with non-streaming dispatch).
   - **go-tui-engineer** — TUI skeleton with mocked IPC.
   - **go-devops-cli** — `main.go`, cobra commands, README, build scripts.
3. Integration: TUI connects to the real daemon; streaming is implemented on top of the completed non-streaming path.
4. **go-qa-tests** adds tests as modules appear; integration tests — after integration.
5. Smoke test on real LM Studio / Ollama (by the user).
6. Cross-compilation and artifact verification on Windows/Linux/macOS.
