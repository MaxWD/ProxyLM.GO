# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.10.0] - 2026-05-19

### Added

- **`fair_share_round_robin` routing strategy** (FUTURE.md #8). A new pull-strategy that extends `deferred_model_then_capable` with starvation protection: when `scheduler.max_consecutive_per_model > 0` and a server has dispatched N consecutive jobs for one model, the worker forcibly picks the next FIFO job for a *different* model (if any compatible one is queued). Falls back to ordinary drain if no other compatible model is available. Existing strategies are unchanged; the new behaviour is opt-in via `routing.strategy: fair_share_round_robin` + `scheduler.max_consecutive_per_model: <N>`.
- **`scheduler` config section** with `max_consecutive_per_model` (default `0` = disabled). Validated by `Config.Validate()`; ignored by other strategies.
- **Ridge regression in `PerfTracker`** (FUTURE.md #2). All normal-equation solvers (1/2/3-variable) now add `λI` to `X^T X` with `λ = 1e-4`. Stabilises estimates on collinear or low-sample data without observable impact on well-conditioned fits.
- **R² and 95% confidence intervals in `PerfStats`** (FUTURE.md #7). Each `(server, model, endpoint)` bucket now publishes `RSquared`, `FitQuality ∈ {"good","degraded",""}` (good if R² ≥ 0.70), and half-width 95% CI for each coefficient (`TLoadCI`, `KInCI`, `KOutCI`). The TUI server-detail Info pane renders `tok/s` as `38.5±5.1`, exposes an `R²` column, and flash-highlights rows with `fit_quality = "degraded"`.
- **Per-endpoint performance statistics** (FUTURE.md #3). The regression key changed from `(server, model)` to `(server, model, endpoint)`. `/v1/chat/completions` and `/v1/embeddings` (and any other path) are tracked in separate buckets, so mixing request types no longer distorts the fit. `core.Job.Endpoint` propagates the value from `RequestRecord.Endpoint` into `recordPerf`; `ipc.ModelStats.Endpoint` is published in `state_snapshot`.
- **Unit tests**: `internal/core/fair_share_test.go` covers forced switch after limit, fallback to drain, visited respect, and disabled-limit equivalence to `PopFor`. Existing perf tests updated for the new `Record(server, model, endpoint, …)` signature.

### Changed

- **`PerfTracker` public API**: `Record`, `Snapshot`, `Predict`, `ServerSummary`, `ModelSummary` now take/return endpoint. Callers updated (`Scheduler.recordPerf`, `ipc.Hub.buildSnapshot`).
- **`Scheduler`** gained `NewSchedulerWithOptions(... SchedulerOptions)` for non-default knobs (currently `MaxConsecutivePerModel`); `NewScheduler` remains as a thin wrapper for tests and unchanged call sites.
- **`docs/SRS.md` / `docs/SRS.ru.md`** §3.7 FR-41 updated to describe ridge regression, the new `(server, model, endpoint)` key, R², CI, and `fit_quality`. §3.3 FR-16 adds `fair_share_round_robin` to the list of supported strategies. Document version → 0.10.0.
- **`docs/API.md` / `docs/API.ru.md`** §2.2 `state_snapshot` example and `ServerState` / `ModelStats` field tables expanded with `r_squared`, `fit_quality`, `t_load_ci`, `k_in_ci`, `k_out_ci`, `endpoint`. Document version → 0.10.0.
- **`docs/ARCHITECTURE.md` / `docs/ARCHITECTURE.ru.md`** §4 gains a "Pull strategies" subsection describing the shared `JobPool` and the new `fair_share_round_robin` scheduling rule.
- **`config.example.yaml`** documents the new strategy in the `routing.strategy` enumeration and adds the `scheduler.max_consecutive_per_model` section with usage guidance.

### Removed

- **`docs/FUTURE.md` / `docs/FUTURE.ru.md`** items #2, #3, #7, #8 collapsed to "DONE in v0.10.0" stubs pointing at the implementation. Their parking-lot description is preserved for traceability.

## [0.9.7] - 2026-05-19

### Changed

- **Positioning**: reframed the project as **backend-agnostic** in README, SRS, and config example. ProxyLM.GO sits in front of *any* OpenAI-compatible `/v1/*` server — local engines (LM Studio, Ollama, vLLM, llama.cpp) and remote APIs (OpenRouter, Groq, Together AI, OpenAI) are all configured the same way (`url` + optional `api_key`). The wire-level behavior was already backend-agnostic; this release aligns documentation and naming with the actual code.
- **`backends[].type` now optional**. The field is reserved for future native-protocol backends (Ollama `/api/*`, Anthropic, Gemini) and is *ignored by the MVP*. Empty value defaults to `openai`; any other value is accepted, but `Config.Warnings()` surfaces a one-line WARN at daemon start explaining that the value does not change runtime behavior. Existing configs with `type: openai` or `type: ollama` continue to work without modification.
- **`config.example.yaml`**: regenerated comments and example names (`lmstudio-desktop`, `ollama-rack`, commented-out `openrouter-cloud` showing cloud-fallback via `priority`).
- **`docs/API.md` §3.3**: renamed from "LM Studio / Ollama specifics" to "Backend compatibility notes"; expanded into a per-backend table with auth flavour and routing implications.

### Added

- **`Config.Warnings()`** — non-fatal advisory output collected alongside `Validate()`. Currently emits one line per backend with a non-`openai` `type` value. Logged at WARN level by `cmd/serve` on daemon start.
- **`docs/FUTURE.md`** — three new parked items recording risks raised by the QA review of the backend-agnostic concept: preflight `/v1/models` shape validation (#11), 429-aware retry with `Retry-After` and jitter (#12), per-backend `serialize_by_model` flag (#13). All three are documented but explicitly out of scope for v0.9.x per FUTURE-RULE.

## [0.9.6] - 2026-05-18

### Added

- **TUI auto-reconnect**: when the WebSocket connection to the daemon drops, the client now retries indefinitely with exponential backoff (1s → 2s → 4s → … cap 30s) instead of exiting. Title shows `connecting…` / `reconnecting…` / live version. Exit only via `q` / `F10` / Ctrl-C.
- **TUI F1 help overlay**: new `renderHelpOverlay` listing all hotkeys; F1 / Esc / q close it. Previously F1 was declared in keymap and footer but had no handler.
- **TUI F5 refresh**: pressing F5 now sends a `request_snapshot` WS frame to the daemon, which replies with a fresh `state_snapshot`. Previously F5 was advertised in footer but did nothing.
- **TUI loading pane**: while waiting for the first snapshot after connect, the request pane shows `Waiting for daemon…` instead of empty borders.
- **TUI auth-error wrap**: `ipc.IsAuthError` detects 401/403 in dial errors; CLI surfaces `authentication failed — check --token: …` instead of a raw WS error.
- **api**: new `X-Request-Id` response-header middleware on `/v1/*`. Generates a UUIDv4 when the client does not supply one; reuses a valid client-supplied value.
- **core/discovery**: new `InitialHealthcheck(ctx)` — synchronous poll of every backend at daemon start, independent of `discovery.enabled`. Servers with explicit `backends[].models` skip the network probe and start healthy immediately.
- **ipc**: new `request_snapshot` client→server frame (Hub responds with `state_snapshot`). Documented in `docs/API.md` §2.3.

### Fixed

- **ipc/server**: `closeAll` on daemon shutdown leaked writer goroutines because `c.closed` was never closed. Added `sync.Once` per client and explicit `close(c.closed)` in `closeAll`.
- **core/router**: `rrIdx` for the `round_robin` strategy was a plain `int` mutated without a lock — a real data race under parallel `/v1/*` traffic. Replaced with `atomic.Int64`.
- **api/routes_openai**: added `defer r.Body.Close()` so the request body is always drained on early-return; prevents connection-pool exhaustion under load.
- **ipc/server**: WebSocket `Close` was reachable from both reader and writer goroutines. Wrapped in `sync.Once` to remove the double-close path.
- **core/scheduler**: `failPending` now wraps the send on `j.done` in `select { case … : default: }` — symmetric with `failServerQueue`. Removes a theoretical deadlock at shutdown when a job is mid-dispatch.
- **core/scheduler**: worker inner-loop now checks `ctx.Err()` between dispatches. Graceful shutdown no longer stalls proportionally to queue depth (NFR-4).
- **storage**: SQLite `busy_timeout` raised from 5000ms to 10000ms to give a margin over the 5s context timeout under concurrent WAL writes.
- **core/perf**: `PerfTracker.bySlot[key]` was unbounded. Capped at 1000 observations per `(server, model)` with FIFO drop — closes a slow memory leak in long-running daemons.
- **tui**: `View()` no longer mutates `m.selectedServerIdx` — clamp moved out of View, respecting Bubble Tea's read-only View contract.
- **tui**: `pageDown` / `pageUp` clamp `selectedIdx` against `visibleRequests()` immediately; eliminates a transient `selectedIdx = -1` window before the next tick.
- **tui**: `calcColWidths` now uses the terminal width — at width ≥ 100 the `model` and `tokens` columns shrink proportionally instead of silently overflowing the viewport.
- **tui**: footer is now responsive — at width < 80 it switches to `F1 Help · q Quit`. Prevents lipgloss truncation.
- **tui**: `json.Unmarshal` errors on envelope are surfaced in the footer via `m.lastParseError` instead of silently dropped.
- **tui**: `m.ctx` is propagated into `waitForMessage` instead of `context.Background()`. The reader goroutine now unblocks when the parent context is canceled.
- **tui**: 60s read-timeout on WebSocket reads defends against "silent" connection loss (NAT timeouts) — previously TUI could freeze indefinitely.
- **tui/styles**: `noUnicode()` no longer calls `os.Getenv` on every glyph render; cached as a package-level var.
- **test/integration**: replaced `time.Sleep(50ms)` synchronization in `TestE2E_ChatCompletions` with a polling loop (cap 2s, step 5ms) — eliminates flakiness on slow CI runners.

### Removed

- **ipc/messages**: dead message types `state_diff`, `log_line`, `ping` and the `LogLinePayload` struct. The daemon never emitted these; the documentation now reflects reality. The `state_snapshot` + `request_snapshot` pair is the entire wire protocol in v0.9.6.
- **api/docs**: `subscribe` and `ping` from `docs/API.md` §2.3 (never implemented).
- **tui**: `applyServerUpdate` (used only by the now-removed `TypeStateDiff` case), `flashEntry.model` field (write-only), styles `StyleModalBorder` / `StyleLogInfo` / `StyleLogWarn` / `StyleLogError` / `StyleLogDebug` (from previously-removed modal and LogPane).
- **core/scheduler**: `Job.Endpoint` / `Job.ClientName` / `Job.Stream` fields — the scheduler never read them; the same data lives in `RequestRecord`.
- **api/auth**: `ctxIsAdmin` context key — written by middleware, never read by any handler.
- **storage**: unused method `(*DB).Path()`.
- **test**: empty `test/unit/` directory.

### Changed

- **docs/API.md** & **docs/API.ru.md**: §2.2 `state_snapshot` rewritten to match the actual wire format (Envelope `{type, time, payload}`; field names `id` / `created_at` / `prompt_tokens` / `attempt` / `queue_depth` instead of the older `request_id` / `received_at` / `input_tokens` / `attempts` / `queue_size`). Document version → 0.9.6.
- **docs/SRS.md** & **docs/SRS.ru.md**: FR-46 / AC-22 rewritten to describe the real TUI layout (`paneHeader → paneRequests → paneInfo`, no modal). FR-37 expanded with the actual hotkey set. Added FR-48 (auto-reconnect), FR-49 (initial healthcheck), FR-50 (X-Request-Id middleware), FR-51 (F5 protocol) and matching AC-25..AC-28. Document version → 0.9.6.
- **docs/ARCHITECTURE.md** & **docs/ARCHITECTURE.ru.md**: TUI section updated (`paneLog` → `paneInfo`, ASCII mockup refreshed, F1 overlay described). Discovery section mentions initial healthcheck. IPC section reflects the trimmed wire protocol.
- **README.md** & **README.ru.md**: structurally aligned (EN/RU sections and order now match per BILINGUAL-RULE).
- **config.example.yaml**: default routing strategy bumped to `preserve_model_coverage` — better behavior for installations with overlapping model coverage across backends.
- **docs/img/**: removed an obsolete unused screenshot file.

## [0.9.5] - 2026-05-16

### Fixed

- **ipc/client**: HTTP response body was leaked on a failed WebSocket handshake. `coder/websocket.Dial` returns the `*http.Response` alongside the connection; it is now drained and closed unconditionally when non-nil.
- **core/router\_test**: replaced `err.(*ErrNoServer)` type assertion with `errors.As` so the test remains correct if the router ever wraps the sentinel error.
- **core/perf**: removed an ineffectual write (`bestRSS = r`) in the all-zero branch of `solveNNLS3` where `bestRSS` is not read afterward.

### Changed

- **ci**: `TestScheduler_Deferred_DrainBeforeSwap` was flaky under `-race` on Linux runners because three concurrent `Submit()` goroutines could push jobs in non-deterministic order. Replaced the approach with a hold-pattern: `j1` signals `j1Running` before blocking on a release channel; the test waits for the signal, guaranteeing the server is in-flight before `j2`/`j3` are submitted sequentially. No `time.Sleep` used.
- **golangci-lint**: migrated `.golangci.yml` from v1 to v2 format (`version: "2"`, `linters.default: none`, `linters.settings`, `linters.exclusions.rules`, new `formatters` section). Added targeted exclusion rules for idiomatic patterns (`defer x.Close()` error, `staticcheck` QF quick-fix hints, `contextcheck` on intentional `context.Background()` uses in history persistence and graceful shutdown). Dropped `gosimple` (merged into `staticcheck` in v2) and the `nilerr` linter (two flagged call-sites are intentional fallbacks with source comments).

## [0.9.4] - 2026-05-16

### Security

- Bumped `go` directive in `go.mod` from `1.25.0` to `1.25.10`, closing four stdlib CVEs that `govulncheck` flagged against the stale minimum:
  - **GO-2026-4971**: panic in `net.Dial`/`LookupPort` on Windows when a NUL byte appears in the address (fixed in `net@go1.25.10`).
  - **GO-2026-4947**: unexpected work during chain building in `crypto/x509` (fixed in `crypto/x509@go1.25.9`).
  - **GO-2026-4946**: inefficient policy validation in `crypto/x509` (fixed in `crypto/x509@go1.25.9`).
  - **GO-2026-4918**: infinite loop in HTTP/2 transport when given a malformed `SETTINGS_MAX_FRAME_SIZE` (fixed in `net/http@go1.25.10`).

### Fixed

- **ci**: added `defaults.run.shell: bash` to all workflow jobs. The Windows runner was using PowerShell, which split `-coverprofile=coverage.out` into two arguments; Go then tried to load `.out` as a package and failed. Forcing Bash via Git for Windows makes the command line consistent across all three OS runners.
- **ci**: upgraded `golangci-lint-action` from `v6` to `v7` with `install-mode: goinstall`. The prebuilt binary in v6 shipped its own Go 1.24 runtime, which caused `typecheck` failures on every package declaring `go 1.25` in `go.mod`. Building the linter from source with `goinstall` uses the same Go version installed by `setup-go`.

## [0.9.3] - 2026-05-16

Initial public release. See [README.md](README.md) for project description, quick start, and configuration reference.

[Unreleased]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.10.0...HEAD
[0.10.0]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.9.7...v0.10.0
[0.9.7]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.9.6...v0.9.7
[0.9.6]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.9.5...v0.9.6
[0.9.5]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.9.4...v0.9.5
[0.9.4]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.9.3...v0.9.4
[0.9.3]: https://github.com/MaxWD/ProxyLM.GO/releases/tag/v0.9.3
