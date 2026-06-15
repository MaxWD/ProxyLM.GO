# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.13.1] - 2026-06-15

### Fixed

- **Bumped Go to 1.25.11** to pick up standard-library security fixes flagged by `govulncheck`: GO-2026-5039 (`net/textproto` — arbitrary inputs included in errors without escaping) and GO-2026-5037 (`crypto/x509` — inefficient candidate hostname parsing). Both are reachable from the OpenAI backend's `ListModels` HTTP call. No application code changed; the project's CI derives the Go version from `go.mod`, so the single directive bump rebuilds against the patched stdlib.

## [0.13.0] - 2026-06-01

### Added

- **Distinct per-server colors in the TUI**. Server chips are now colored by their index in the priority-sorted list against an expanded 12-color palette (was a 5-color FNV hash of the name, which collided into repeats at 3–4 servers). Colors are now distinct for up to 12 servers and stable across snapshots. The request table's `Server` column reuses the same color via a shared `serverName → index` map. `ServerColor(name)` was replaced by `ServerColorByIndex(idx)` / `serverColorFor(name, index)`.
- **Pulsing in-flight indicator**. A healthy server that is actively processing a request now shows a *pulsing* lamp (`●` brightness breathing over ~640 ms via a 4-frame ANSI cycle); an idle server shows a steady lamp. This makes "working vs idle" obvious — previously, once `current_model` was set it stayed displayed and the two states were indistinguishable. Driven by a fast animation tick (~160 ms) that runs **only while at least one server is in-flight** (no redraw churn on an idle dashboard).
- **Priority-ordered server list**. `ipc.ServerState` gained `priority`; the TUI sorts servers ascending by priority (lower number = higher preference, on top), tiebreak by name. Order is now deterministic instead of following config order.
- **Loaded-model probe (real VRAM/RAM state)**. Backends of `type: ollama` / `lmstudio` / `llamacpp` are now probed via their native management endpoint (Ollama `GET /api/ps`, LM Studio `GET /api/v1/models` `loaded_instances`, llama.cpp `GET /models` `status`) during discovery to learn which models are *actually loaded in memory* — distinct from `current_model` (what the proxy last dispatched). Surfaced in the server-detail Info pane (`In memory: …` / `n/a` / `— (none)`) and as an `⏏` marker on the server chip when the proxy's `current_model` is no longer resident (idle-unloaded). Opt-in: plain `openai` and `anthropic` backends are never probed. Implemented via the optional `backends.LoadedModelsProber` interface (no change to the core `Backend` contract). New `ipc.ServerState` fields: `loaded_models`, `loaded_models_probed`.
- **Generation tail in the TUI** (hotkey `t`). For an in-flight **streaming** request, the last ~160 generated characters are captured in-memory and shown in the request Info pane on demand. Hidden by default (it is response content — privacy); the tail is never written to the database or logs. New `ipc.RequestState.last_tokens` field, fed by an in-memory `core.LiveTail` store written by the streaming relay and overlaid onto running streaming requests in the IPC snapshot.

### Changed

- **`backends[].type`** now accepts `lmstudio` and `llamacpp` in addition to `openai` / `ollama` / `anthropic`. All non-anthropic types speak the OpenAI-compatible protocol; `ollama` / `lmstudio` / `llamacpp` additionally enable the loaded-model probe. `Config.Validate()` accepts the new values.

## [0.12.1] - 2026-05-30

### Changed

- **TUI color of performance metrics corrected**. v0.12.0 tinted the *entire* server performance metric (`t_load · ↓tok/s · ↑tok/s`) teal in the server-list header — that was not the intended target. The header metric is reverted to the neutral dim color. Instead, in the server-detail Info pane (bottom panel) per-model table, only the **`±margin` part** of each `tok/s` value (the 95% confidence-interval error bound) is now tinted teal, so the point estimate stays readable while the uncertainty stands out. ANSI-aware right-justified padding (`padLeftDisplay`) keeps the table columns aligned; the new `styleCIMargin` helper colors only the `±…` suffix. `StylePerf` was renamed to `StyleCI` accordingly.

## [0.12.0] - 2026-05-30

### Added

- **Two-stage `t_load` estimation in `PerfTracker`**. Because INV-2 keeps a model loaded until its queue drains, model reloads are rare — a steady-state server typically yields a *single* `loaded=1` observation among hundreds of `loaded=0`. The previous joint 3-variable NNLS determined `t_load` from essentially one residual and frequently collapsed it to `0` on the slightest noise, which surfaced in the TUI as `—` even though load time always physically exists. When ≥ 2 clean `loaded=0` observations are available, `fitRegression` now estimates the token coefficients (`k_in`, `k_out`) from those clean points, then derives `t_load` as the clamped mean of the residuals over `loaded=1` observations. This recovers a stable `t_load` from a single reload. Falls back to the joint 3-variable NNLS in the degenerate case of fewer than two clean points.
- **Models overlay in the TUI** (hotkey `m`). Lists all discovered models on the currently selected server in a centered overlay, with the active model marked `▶`. Long lists collapse to `… (+N more)`. Closes on `m` / `Esc` / `q`. Added to the help overlay (`F1`) and footer.
- **`t_load` confidence marker in the TUI**. `ipc.ServerState` gained `t_load_loaded` (number of reload observations behind the header `t_load` estimate). The TUI now renders three distinct cases: `—` (no reload ever observed — genuinely unknown), `1.4s*` (estimate exists but rests on `< 3` reload samples — low confidence), and `1.4s` (≥ 3 samples — confident).

### Changed

- **TUI server performance metrics are now rendered in a distinct teal color** (`StylePerf`) instead of the generic dim gray, so `t_load · ↓tok/s · ↑tok/s` reads as its own block. The duplicated metric-rendering logic across the header chip, selected chip, and Info pane was centralized into `serverMetricText` / `fmtTLoad`.
- **TUI strings standardized to English**. The slow-server alert is now `!!! SLOW !!!` (was Russian), and the footer / Info-pane hints use English (`Tab switch pane`, `Click select`, `m Models`).

### Fixed

- **`t_load` no longer shows `—` for servers that have served at least one reload**. See the two-stage estimation above.

## [0.11.0] - 2026-05-24

### Added

- **Anthropic Messages API endpoint** (`POST /v1/messages`). Clients using the Anthropic SDK can now connect to ProxyLM.GO directly. Request validation enforces the Anthropic contract: `model` and `max_tokens` are required; streaming uses the Anthropic SSE event format (`event: <type>\ndata: {...}`). Error responses follow the Anthropic error shape (`{"type":"error","error":{...}}`).
- **Cross-protocol translation layer** (`internal/api/translate/`). All four client↔backend protocol combinations work transparently:
  - OpenAI client → OpenAI backend (passthrough, unchanged)
  - OpenAI client → Anthropic backend (request + response translated)
  - Anthropic client → OpenAI backend (request + response translated)
  - Anthropic client → Anthropic backend (passthrough)
  Translation covers non-streaming responses, streaming SSE format conversion, system prompt extraction/injection, tool/function call format mapping, finish reason mapping, and usage field renaming (`prompt_tokens` ↔ `input_tokens`).
- **Anthropic backend client** (`internal/core/backends/anthropic.go`). Implements the `Backend` interface with `x-api-key` + `anthropic-version` authentication. Used when `backends[].type: anthropic`.
- **Dual authentication** on all `/v1/*` endpoints. Both `Authorization: Bearer <key>` (OpenAI-style) and `x-api-key: <key>` (Anthropic-style) are now accepted; `Authorization` takes precedence when both are present.
- **`Backend.Protocol()` method** added to the `Backend` interface. Returns `"openai"` or `"anthropic"`.
- **Unit tests**: 14 new tests in `internal/api/translate/` covering request translation (both directions), response translation (both directions, including roundtrip), streaming translation (OpenAI→Anthropic and Anthropic→OpenAI SSE event sequences), edge cases (empty input, model propagation).

### Changed

- **`backends[].type` is now functional**. Previously reserved and ignored (advisory only), the `type` field now selects the wire protocol for communicating with the backend: `openai` (default), `anthropic`, or `ollama` (alias for `openai`). Existing configs with `type: openai`, `type: ollama`, or no `type` field continue to work without modification.
- **`Config.Warnings()`** no longer emits advisory messages about the `type` field being reserved — the field is now functional.
- **`Config.Validate()`** now rejects unknown `type` values (only `""`, `openai`, `ollama`, `anthropic` are accepted).
- **`cmd/serve.go`** creates `backends.Anthropic` instances for `type: anthropic` backends instead of always creating `backends.OpenAI`.
- **TUI** `displayEndpoint` recognizes `/v1/messages` → `"messages"` in the per-model stats table.
- **`config.example.yaml`**: `type` field documentation updated from "reserved / ignored" to protocol selector; added commented-out Anthropic cloud backend example.
- **`docs/SRS.md` / `docs/SRS.ru.md`**: new §3.10 with FR-52..FR-60 covering Anthropic API, cross-protocol translation, and dual auth. FR-1 updated to include `/v1/messages`. FR-2 updated for dual auth. Document version → 0.11.0.
- **`docs/API.md` / `docs/API.ru.md`**: new §1.5 `POST /v1/messages` with request/response schema, streaming events, translation matrix, and error format. Auth section updated for dual auth. Document version → 0.11.0.
- **`docs/ARCHITECTURE.md` / `docs/ARCHITECTURE.ru.md`**: new §7 "Cross-Protocol Translation" describing the translation matrix, `internal/api/translate/` package, and streaming translator mechanics. Updated system diagram and code structure section. Document version → 0.11.0.
- **`README.md` / `README.ru.md`**: updated to reflect multi-protocol capabilities — description, features list, architecture diagram, Quick Start with Anthropic curl example, configuration table.

### Limitations

- `/v1/completions` and `/v1/embeddings` are not translatable to Anthropic backends (no Anthropic equivalent) — routed to such a backend, they return an error; the scheduler retries on other servers.
- Extended thinking (Anthropic-only) is passed through in Anthropic→Anthropic mode; stripped in cross-protocol mode.
- `POST /v1/messages/count_tokens` is not implemented (deferred).
- Prompt caching (`cache_control`) is passed through in same-protocol mode; stripped in cross-protocol mode.

## [0.10.0] - 2026-05-19

### Added

- **`fair_share_round_robin` routing strategy** (previously parked in `docs/FUTURE.md`). A new pull-strategy that extends `deferred_model_then_capable` with starvation protection: when `scheduler.max_consecutive_per_model > 0` and a server has dispatched N consecutive jobs for one model, the worker forcibly picks the next FIFO job for a *different* model (if any compatible one is queued). Falls back to ordinary drain if no other compatible model is available. Existing strategies are unchanged; the new behaviour is opt-in via `routing.strategy: fair_share_round_robin` + `scheduler.max_consecutive_per_model: <N>`.
- **`scheduler` config section** with `max_consecutive_per_model` (default `0` = disabled). Validated by `Config.Validate()`; ignored by other strategies.
- **Ridge regression in `PerfTracker`** (previously parked in `docs/FUTURE.md`). All normal-equation solvers (1/2/3-variable) now add `λI` to `X^T X` with `λ = 1e-4`. Stabilises estimates on collinear or low-sample data without observable impact on well-conditioned fits.
- **R² and 95% confidence intervals in `PerfStats`** (previously parked in `docs/FUTURE.md`). Each `(server, model, endpoint)` bucket now publishes `RSquared`, `FitQuality ∈ {"good","degraded",""}` (good if R² ≥ 0.70), and half-width 95% CI for each coefficient (`TLoadCI`, `KInCI`, `KOutCI`). The TUI server-detail Info pane renders `tok/s` as `38.5±5.1`, exposes an `R²` column, and flash-highlights rows with `fit_quality = "degraded"`.
- **Per-endpoint performance statistics** (previously parked in `docs/FUTURE.md`). The regression key changed from `(server, model)` to `(server, model, endpoint)`. `/v1/chat/completions` and `/v1/embeddings` (and any other path) are tracked in separate buckets, so mixing request types no longer distorts the fit. `core.Job.Endpoint` propagates the value from `RequestRecord.Endpoint` into `recordPerf`; `ipc.ModelStats.Endpoint` is published in `state_snapshot`.
- **Unit tests**: `internal/core/fair_share_test.go` covers forced switch after limit, fallback to drain, visited respect, and disabled-limit equivalence to `PopFor`. Existing perf tests updated for the new `Record(server, model, endpoint, …)` signature.

### Changed

- **`PerfTracker` public API**: `Record`, `Snapshot`, `Predict`, `ServerSummary`, `ModelSummary` now take/return endpoint. Callers updated (`Scheduler.recordPerf`, `ipc.Hub.buildSnapshot`).
- **`Scheduler`** gained `NewSchedulerWithOptions(... SchedulerOptions)` for non-default knobs (currently `MaxConsecutivePerModel`); `NewScheduler` remains as a thin wrapper for tests and unchanged call sites.
- **`docs/SRS.md` / `docs/SRS.ru.md`** §3.7 FR-41 updated to describe ridge regression, the new `(server, model, endpoint)` key, R², CI, and `fit_quality`. §3.3 FR-16 adds `fair_share_round_robin` to the list of supported strategies. Document version → 0.10.0.
- **`docs/API.md` / `docs/API.ru.md`** §2.2 `state_snapshot` example and `ServerState` / `ModelStats` field tables expanded with `r_squared`, `fit_quality`, `t_load_ci`, `k_in_ci`, `k_out_ci`, `endpoint`. Document version → 0.10.0.
- **`docs/ARCHITECTURE.md` / `docs/ARCHITECTURE.ru.md`** §4 gains a "Pull strategies" subsection describing the shared `JobPool` and the new `fair_share_round_robin` scheduling rule.
- **`config.example.yaml`** documents the new strategy in the `routing.strategy` enumeration and adds the `scheduler.max_consecutive_per_model` section with usage guidance.

### Removed

- **`docs/FUTURE.md` / `docs/FUTURE.ru.md`** — four implemented parking-lot items (ridge regression, per-endpoint stats, R²/CI, max-consecutive starvation protection) deleted per `FUTURE-RULE`: their description survives in CHANGELOG / SRS §3.7 / ARCHITECTURE §4 / code docstrings. Remaining items renumbered sequentially (1..9, no gaps) in both EN and RU versions.

## [0.9.7] - 2026-05-19

### Changed

- **Positioning**: reframed the project as **backend-agnostic** in README, SRS, and config example. ProxyLM.GO sits in front of *any* OpenAI-compatible `/v1/*` server — local engines (LM Studio, Ollama, vLLM, llama.cpp) and remote APIs (OpenRouter, Groq, Together AI, OpenAI) are all configured the same way (`url` + optional `api_key`). The wire-level behavior was already backend-agnostic; this release aligns documentation and naming with the actual code.
- **`backends[].type` now optional**. The field is reserved for future native-protocol backends (Ollama `/api/*`, Anthropic, Gemini) and is *ignored by the MVP*. Empty value defaults to `openai`; any other value is accepted, but `Config.Warnings()` surfaces a one-line WARN at daemon start explaining that the value does not change runtime behavior. Existing configs with `type: openai` or `type: ollama` continue to work without modification.
- **`config.example.yaml`**: regenerated comments and example names (`lmstudio-desktop`, `ollama-rack`, commented-out `openrouter-cloud` showing cloud-fallback via `priority`).
- **`docs/API.md` §3.3**: renamed from "LM Studio / Ollama specifics" to "Backend compatibility notes"; expanded into a per-backend table with auth flavour and routing implications.

### Added

- **`Config.Warnings()`** — non-fatal advisory output collected alongside `Validate()`. Currently emits one line per backend with a non-`openai` `type` value. Logged at WARN level by `cmd/serve` on daemon start.
- **`docs/FUTURE.md`** — three new parked items recording risks raised by the QA review of the backend-agnostic concept: preflight `/v1/models` shape validation, 429-aware retry with `Retry-After` and jitter, per-backend `serialize_by_model` flag. All three are documented but explicitly out of scope for v0.9.x per FUTURE-RULE. (At the time of this release they were numbered #11/#12/#13; v0.10.0 cleaned up implemented items and renumbered the survivors to #7/#8/#9.)

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

[Unreleased]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.13.1...HEAD
[0.13.1]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.13.0...v0.13.1
[0.13.0]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.12.1...v0.13.0
[0.12.1]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.12.0...v0.12.1
[0.12.0]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.9.7...v0.10.0
[0.9.7]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.9.6...v0.9.7
[0.9.6]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.9.5...v0.9.6
[0.9.5]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.9.4...v0.9.5
[0.9.4]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.9.3...v0.9.4
[0.9.3]: https://github.com/MaxWD/ProxyLM.GO/releases/tag/v0.9.3
