# ProxyLM.GO — Software Requirements Specification (SRS)

Document version: 0.10.0
Baseline: ProxyLM.GO v0.9.7
Related documents: [`ARCHITECTURE.md`](./ARCHITECTURE.md), [`API.md`](./API.md), [`AGENTS.md`](./AGENTS.md)

---

## 1. Introduction

### 1.1. Purpose

ProxyLM.GO is an HTTP proxy in Go placed in front of any OpenAI-compatible LLM backend — local engines (LM Studio, Ollama, vLLM, llama.cpp) or remote APIs (OpenRouter, Groq, Together AI, OpenAI). The proxy is backend-agnostic: it operates purely on the OpenAI `/v1/*` contract and is unaware of what software is running behind the URL. The primary goal is to **serialize requests by model** on single-model backends, preventing constant model reloads into VRAM that occur when multiple clients send requests to multiple models in arbitrary order.

Delivered as a **single portable binary**: the same executable can run as a daemon (service) or as a TUI client to a running daemon. On first run, `config.yaml` and `proxylm.db` are automatically created alongside the binary. Cross-compiles to any OS (`GOOS`/`GOARCH`) without a CGO toolchain.

This document describes functional and non-functional requirements, scheduler invariants, acceptance criteria, and explicit out-of-scope items for v0.9.3 baseline.

### 1.2. Scope

The product applies to installations where:
- Multiple internal services (`service-a`, `service-b`, …) share one or more local LLM servers with limited VRAM.
- Requests may target different models, while loading all models into VRAM simultaneously is not feasible.
- A single authentication point, request history, and TUI observability are required.

### 1.3. Goals

- G1. Minimize the number of model swap operations in VRAM under a mixed-request stream.
- G2. Provide clients with an OpenAI-compatible API without code changes on their side.
- G3. Give the operator real-time TUI observability (requests, servers, log).
- G4. Maintain resilience to transient backend failures (retry + failover).
- G5. Be cross-platform (Windows 10/11, Linux, macOS) and distributed as a single binary.

### 1.4. Terms and Abbreviations

| Term                    | Definition                                                                          |
|-------------------------|-------------------------------------------------------------------------------------|
| LLM                     | Large Language Model                                                                |
| Backend                 | Any OpenAI-compatible LLM server behind the proxy (LM Studio, Ollama, vLLM, llama.cpp, OpenAI, OpenRouter, Groq, Together AI, …) |
| Model affinity          | Routing strategy preferring the server where the model is already loaded            |
| Model swap              | Evicting the current model and loading another into VRAM                            |
| In-flight               | A request sent to the backend whose response has not yet been fully received        |
| OpenAI API              | REST contract `POST /v1/chat/completions` etc., standardized by OpenAI              |
| SSE                     | Server-Sent Events; OpenAI streaming format (`data: {...}\n\n`, `data: [DONE]`)     |
| TUI                     | Text User Interface (Bubble Tea-based)                                              |
| IPC                     | Inter-process communication; here — WebSocket between daemon and TUI                |
| Daemon                  | ProxyLM.GO server process (`proxylm serve`)                                         |
| Client                  | External API consumer identified by API key                                         |
| Discovery               | Periodic polling of models from backends                                            |
| Healthy / unhealthy     | Backend state based on discovery results / failed requests                          |
| FR / NFR / INV          | Functional / Non-Functional Requirement / Invariant — requirement IDs               |
| Goroutine               | Go lightweight thread; ProxyLM.GO concurrency model                                 |
| Channel                 | Typed message-passing channel between goroutines                                    |
| `context.Context`       | Standard cancellation/deadline propagation mechanism in Go                          |

---

## 2. Stakeholders and User Roles

| Role                | Description                                                                                 | Access channel                              |
|---------------------|---------------------------------------------------------------------------------------------|---------------------------------------------|
| Client service      | LLM functionality consumer; sends requests in OpenAI-compatible format                      | HTTP API (`/v1/*`) with Bearer api-key      |
| Operator / admin    | Person monitoring load and errors; starts / stops the daemon                               | TUI (via WebSocket `/admin/stream`), CLI    |
| Integrator / DevOps | Installs, configures, and deploys the proxy                                                 | CLI (`proxylm serve`, `proxylm service …`)  |

ProxyLM.GO is **not** intended for exposure to the public internet — it is designed for a trusted internal network.

---

## 3. Functional Requirements (FR)

Each requirement is a verifiable statement. The word "MUST" denotes obligation.

### 3.1. HTTP API

| ID    | Requirement |
|-------|-------------|
| FR-1  | The proxy MUST accept HTTP requests on paths `POST /v1/chat/completions`, `POST /v1/completions`, `POST /v1/embeddings`, `GET /v1/models`, `GET /healthz`. |
| FR-2  | The proxy MUST require the header `Authorization: Bearer <key>` for all paths under `/v1/*` and `/admin/*`, except `GET /healthz`. |
| FR-3  | The proxy MUST validate the presented key against `auth.api_keys` (for `/v1/*`) or against `auth.admin_key` (for `/admin/*`). On mismatch — `401 Unauthorized`. |
| FR-4  | The proxy MUST log and store in history the **client name** (`auth.api_keys[].name`), not the key itself. |
| FR-5  | The proxy MUST return `404 model_not_found` if the requested `model` is absent from all healthy servers. |
| FR-6  | The proxy MUST support the request field `stream: true` and proxy the response as `text/event-stream` without buffering the full body. |
| FR-7  | The proxy MUST return `GET /v1/models` with an aggregated list of unique models known across all healthy servers. |
| FR-8  | The proxy MUST return `GET /healthz` without authentication with body `{"status": "ok"}` and code 200 when the daemon is operational. |
| FR-9  | The proxy MUST forward unknown request body fields to the backend without modification (passthrough). |

### 3.2. Scheduler (model-aware queue)

| ID     | Requirement |
|--------|-------------|
| FR-10  | For each configured backend the proxy MUST maintain a separate **in-memory request queue** (`pending`). |
| FR-11  | At most **one** request MUST execute simultaneously on each backend (in-flight ≤ 1). See INV-1. |
| FR-12  | When selecting the next request, the worker MUST prefer a request for the server's current model; if no such request exists — take the oldest from the queue (FIFO). See INV-2, INV-3. |
| FR-13  | The proxy MUST update the server's `current_model` with the model of the last successfully processed (or current in-flight) request. |
| FR-14  | Queue and worker state — **in-memory only**. Queue loss on daemon restart is acceptable (see NFR-3, INV-7). |

### 3.3. Routing

| ID     | Requirement |
|--------|-------------|
| FR-15  | The default routing strategy is `model_affinity_least_busy`. Algorithm: (1) server where `current_model == requested_model`; (2) server with the shortest `pending` queue; (3) tiebreak by server name in alphabetical order. |
| FR-16  | The proxy MUST support switching strategy via `routing.strategy` (`model_affinity_least_busy`, `least_busy`, `round_robin`, `deferred_model_then_capable`, `preserve_model_coverage`, `fair_share_round_robin`). For `deferred_model_then_capable` no server is assigned at accept time: the request is placed in a shared queue and assigned only when a server becomes free; selection priority — (1) first request with `model == current_model` on a compatible server, (2) first compatible FIFO request. For `preserve_model_coverage` — same cold distribution by `priority`, but when selecting the next job on a freeing server: (1) if HEAD of the queue is under `current_model` — take HEAD; (2) if HEAD is for a different model and another healthy server has `current_model` matching — take HEAD (swap is safe); (3) if this server is the sole holder of `current_model` — search for a job under `current_model` in the queue; if none exists, take HEAD (swap is unavoidable). For `fair_share_round_robin` — same base semantics as `deferred_model_then_capable`, but with starvation protection: when `scheduler.max_consecutive_per_model > 0` and the server has executed N consecutive jobs for the same model, the worker MUST forcibly pick the next FIFO job with a different model (if such a compatible job exists in the queue); if there is no other compatible model, behavior degrades to ordinary drain. Implementation of `round_robin` and pure `least_busy` — acceptable in MVP but not critical (see v0.2.0). |
| FR-17  | The proxy MUST exclude `unhealthy` backends from the candidate list until they recover. |

### 3.4. Retry & Failover

| ID     | Requirement |
|--------|-------------|
| FR-18  | On backend error (5xx, network reset, timeout) the proxy MUST retry the request with exponential backoff: `initial_backoff_ms`, `2x`, `4x`, …, capped at `max_backoff_ms`. Backoff applies between attempts regardless of which server handles the next attempt. |
| FR-19  | The total number of attempts for a single request MUST NOT exceed `retry.max_attempts` (default 3, **including the first**). See INV-5. |
| FR-20  | After a failed attempt on server X the next attempt MUST go to any other healthy server with this model (rolling exclusion size 1: only X is excluded, and only for the one next attempt — after that X is available again). If X is the only compatible healthy server, the exclusion is ignored and the attempt goes to X again (single-server degradation). There is no separate `failover` setting — this behavior is always active. |
| FR-21  | If the error occurred **before the first SSE chunk was sent to the client**, retry is permitted. If at least one chunk has already been sent — retry and server switching are PROHIBITED; the proxy terminates the SSE stream and marks the request as `failed`. See INV-6. |
| FR-22  | The proxy MUST mark a server `unhealthy` after `discovery.unhealthy_after_failed_polls` consecutive failed discovery polls (default 3). |
| FR-23  | Backend 4xx errors (except 429) are **not** retried — they are forwarded to the client as-is. |

### 3.5. Model Discovery

| ID     | Requirement |
|--------|-------------|
| FR-24  | The proxy MUST periodically (`discovery.interval_seconds`, default 30) poll `GET <backend>/v1/models` on each server and update `ModelMap`. |
| FR-25  | If `backends[].models` explicitly specifies a non-empty list — discovery for that server is NOT performed; the specified list is used. |
| FR-26  | A server unreachable for `unhealthy_after_failed_polls` consecutive cycles is marked `unhealthy`. After one successful poll — back to `healthy`. |
| FR-27  | On startup the daemon MUST complete the first discovery cycle before accepting HTTP requests (or accept requests and return `503` until the first cycle completes — implementation choice, but behavior must be documented). |

### 3.6. Request History (SQLite)

| ID     | Requirement |
|--------|-------------|
| FR-28  | Each request MUST be recorded in the SQLite `requests` table with the schema from `ARCHITECTURE.md` §9. |
| FR-29  | The `status` field MUST take values `queued`, `running`, `completed`, `failed` (see state diagram §5.1). |
| FR-30  | A request record is created on receipt (`queued`) and updated on status transitions; the final update writes `queue_wait_ms`, `duration_ms` (`server_proc_ms`), `input_tokens`, `output_tokens`, `error`. |
| FR-31  | The proxy MUST periodically (on startup + once a day) delete records older than `storage.history_retention_days` (default 30). |
| FR-32  | A record MUST contain `client_name` but MUST NOT contain the key itself or the request/response body (metadata and error only). |

### 3.7. TUI / IPC

| ID     | Requirement |
|--------|-------------|
| FR-33  | The daemon MUST expose the WebSocket endpoint `GET /admin/stream`, protected by `auth.admin_key`. |
| FR-34  | The server MUST send a `state_snapshot` (full snapshot of servers, queues, and the last N records) immediately on client connection. Incremental `state_diff` push is out of scope for v0.9.3 (see FUTURE.md). |
| FR-35  | Log-line push (`log_line` messages) is out of scope for v0.9.3. The TUI uses F5 / `request_snapshot` to refresh the view (see FR-51). |
| FR-36  | The TUI MUST display a request table with automatic hiding of `completed`/`failed` records after `tui.show_completed_minutes` (default 30). Records remain in SQLite. |
| FR-37  | The TUI MUST support hotkeys: `F1` — help overlay, `F5` — refresh snapshot (sends `request_snapshot` via WebSocket), `F10` / `q` — quit, `/` — table search, `Tab` — cycle focus, `↑`/`↓` — navigation in active pane, `Enter` — select/confirm, `Esc` — close overlay/modal. |
| FR-38  | The TUI MUST work correctly in Windows Terminal, cmd.exe, PowerShell, and standard Linux terminals (xterm-256color). |
| FR-39  | The TUI MUST display timestamps (`Queued`/`Started`/`Completed at` columns in the table, log timestamps) in the OS local timezone. The daemon stores/transmits time in UTC; conversion happens on the TUI side. |
| FR-40  | The TUI MUST display each server in the HeaderBar on a separate line (multi-line). Header height grows proportionally to the number of servers; the log pane shrinks proportionally. |
| FR-41  | The daemon MUST collect all observations from completed requests per `(server_name, model, endpoint)` key in memory and compute the linear model parameters `loaded·t_load + b·k_in + c·k_out ≈ t_all` using **ridge regression** (`(X^T X + λI)θ = X^T y`, λ = 1e-4) — 3×3 normal equations; 2×2 fallback if no reload observations or the matrix is degenerate. Minimum for a valid estimate — 3 observations (`perfMinSamples = 3`). Endpoint became part of the key in v0.10.0: `/v1/chat/completions`, `/v1/embeddings`, etc. are tracked separately so the regression isn't distorted by mixing request types. Fields `perf_ok`, `t_load_ms`, `tok_in_per_sec`, `tok_out_per_sec`, `r_squared`, `fit_quality` are published in `ServerState`; per-(model,endpoint) breakdown (with `t_load_ci` / `k_in_ci` / `k_out_ci` — half-widths of 95% confidence intervals introduced in v0.10.0) is published in `ServerState.per_model_stats[]`. The TUI MUST show header-level metrics in the HeaderBar server line in the format `t_load · ↓tok_in/s · ↑tok_out/s`; if `perf_ok = false` or `current_model` is empty — the metrics line is empty. For the server-detail Info pane, the TUI MUST list rows per `(model, endpoint)` and display R² + CI per coefficient; rows with `fit_quality = "degraded"` (R² < 0.70) MUST be visually highlighted. Fields `tokens_per_sec` and `ttft_ms` are **removed**. |

### 3.8. CLI / Launch

| ID     | Requirement |
|--------|-------------|
| FR-39  | The proxy MUST provide the `proxylm` CLI with subcommands: `serve`, `tui`, `config init`, `config validate`, `service install|uninstall|start|stop|status`, `version`. |
| FR-40  | `proxylm serve` MUST accept `--config PATH` (default — `config.yaml` alongside the binary), `--host`, `--port`. CLI arguments override YAML. If the default config is absent — the daemon MUST create it from the embedded template and continue starting. |
| FR-41  | `proxylm config init` MUST create `config.example.yaml` in the current directory (or per `--out`). |
| FR-42  | `proxylm config validate` MUST parse and validate the config via the typed model; on error — print a clear message and exit with code ≥ 1. |
| FR-43  | `proxylm tui --connect ws://host:port --token <admin_key>` MUST connect to a running daemon. |

### 3.9. Performance Metrics

| ID     | Requirement |
|--------|-------------|
| FR-44  | The daemon MUST record the model-switch event (`model_reloaded`) at every dispatch: the flag is `true` if `server.CurrentModel` at the time the job is taken differs from `job.Model`. The flag MUST be stored in `RequestRecord.ModelReloaded`, written to the `model_reloaded` column of the `requests` table (INTEGER NOT NULL DEFAULT 0), and published in `RequestState.model_reloaded` via IPC. |
| FR-45  | The TUI MUST display the **RM** (Reload Model) column in the request table — between the `Server` and `Queued` columns. Value: glyph `✓` if `model_reloaded = true`, `—` otherwise. |
| FR-46  | The TUI MUST support three named panes: `paneHeader`, `paneRequests`, `paneInfo`. `Tab` cycles focus: `paneHeader → paneRequests → paneInfo → paneHeader`. In `paneHeader`, `↑`/`↓` select a server (marker `▸`, border `StyleBorderActive`); `Enter` shows server details in `paneInfo` (the lower-right info panel, not a modal). In `paneRequests`, `↑`/`↓` navigate the request list; `Enter` shows request details in `paneInfo`. Mouse wheel in the header area MUST also change the selected server. |
| FR-47  | The pending-row glyph in the request table MUST be the single-cell character `…` (U+2026) instead of the emoji `⏳` (U+23F3, 2 cells) — for correct column alignment in terminals where character width equals code-point count. |
| FR-48  | The TUI MUST reconnect automatically on WebSocket disconnection using infinite exponential backoff (1 s, 2 s, 4 s, …, capped at 30 s). The title bar MUST show `connecting…` on the initial attempt, `reconnecting…` on subsequent attempts, and `live` when connected. Exit is only via `q` / `F10` / Ctrl-C. |
| FR-49  | On daemon startup, a single synchronous health-check poll (`GET /v1/models`) MUST be performed for every backend server, regardless of `discovery.enabled`. Servers with an explicit non-empty `backends[].models` list are considered healthy immediately without a poll. |
| FR-50  | Every `/v1/*` response MUST include the header `X-Request-Id` (UUIDv4). If the incoming client request already carries a valid `X-Request-Id` header, that value is reused; otherwise a new UUIDv4 is generated. The header is injected by a middleware before the handler executes. |
| FR-51  | To request a fresh state snapshot the TUI MUST send `{"type": "request_snapshot", "time": "<RFC3339>"}` via WebSocket. The daemon MUST respond with the same `state_snapshot` envelope as on initial connect. |

---

## 4. Non-Functional Requirements (NFR)

| ID     | Category           | Requirement |
|--------|--------------------|-------------|
| NFR-1  | Performance        | Proxy processing (accept, route, enqueue, excluding backend time) MUST sustain **≥ 100 RPS** on a single core with minimal backend load (benchmark with a mock backend responding instantly). |
| NFR-2  | Performance        | Streaming proxying MUST forward SSE chunks to the client with latency ≤ 50 ms relative to receipt from the backend (on a single host). |
| NFR-3  | Reliability        | Loss of in-memory queue contents on daemon restart is acceptable. At that moment the client receives a connection error (it retries on its side). Queue persistence is out of scope. |
| NFR-4  | Reliability        | The daemon MUST handle SIGINT/SIGTERM (on Linux) and Ctrl+C (on Windows): refuse new requests, wait for current in-flight requests to complete up to `shutdown_grace_seconds` (default 30 s), then force-terminate. On Windows/Linux/macOS, the installed service correctly responds to the service stop signal (see `kardianos/service` integration). |
| NFR-5  | Portability        | Support Windows 10/11, Linux (kernel ≥ 5.x, x86_64 / arm64), macOS (10.15+, x86_64 / arm64). Go ≥ 1.22. Build — **single binary with no runtime and no CGO**, cross-compiled via `GOOS=... GOARCH=... go build`. |
| NFR-6  | Maintainability    | Logs in JSON format (`log/slog`) with fields `ts`, `level`, `logger`, `event`, `request_id` (where applicable), `client`, `server`, `model`. |
| NFR-7  | Maintainability    | Package version is set in one place — via `-ldflags "-X main.version=<ver>"` at build time; CLI `proxylm version` prints it. |
| NFR-8  | Security           | API keys and the admin key are passed only via `Authorization: Bearer`. Keys MUST NOT be written to logs, TUI, or the database — only `client_name`. |
| NFR-9  | Security           | The config file containing keys is read with the current user's permissions by default; permission recommendations are in the README (outside SRS). |
| NFR-10 | API compatibility  | Requests and responses conform to OpenAI API v1 for mandatory fields. Extra fields are proxied without modification (FR-9). |
| NFR-11 | Testability        | Unit-test coverage of `internal/core/scheduler.go`, `internal/core/router.go`, `internal/core/retry.go` ≥ 80% lines (`go test -cover`). |
| NFR-12 | Documentation      | All public endpoints are described in `docs/API.md`; the `/admin/stream` message format — there as well. |

---

## 5. Data Structures and Lifecycle

### 5.1. RequestRecord

Fields (minimum):

| Field           | Type             | Description                                              |
|-----------------|------------------|----------------------------------------------------------|
| `request_id`    | UUID             | generated on receipt                                     |
| `client_name`   | str              | key name from `auth.api_keys`                            |
| `model`         | str              | value from the request body                              |
| `server`        | str \| None      | assigned by the router                                   |
| `status`        | enum             | `queued` / `running` / `completed` / `failed`            |
| `received_at`   | datetime         | moment of entry into the HTTP handler                    |
| `started_at`    | datetime \| None | moment of dispatch to backend (first attempt)            |
| `first_chunk_at` | datetime \| None | moment of first SSE chunk (for `stream=true`)           |
| `completed_at`  | datetime \| None | moment of finalization                                   |
| `queue_wait_ms` | int \| None      | `started_at − received_at` in ms                         |
| `duration_ms`   | int \| None      | `completed_at − started_at` in ms                        |
| `server_proc_ms` | int \| None     | alias/duplicate of `duration_ms` for UI LLM metric      |
| `ttft_ms`       | int \| None      | `first_chunk_at − started_at` in ms for stream          |
| `input_tokens`  | int \| None      | from response `usage` or fallback                        |
| `output_tokens` | int \| None      | from response `usage` / SSE count                        |
| `attempts`      | int              | total attempts across all servers                        |
| `error`         | str \| None      | message when `failed`                                    |
| `stream`        | bool             | streaming request flag                                   |
| `model_reloaded` | bool           | `true` if dispatch triggered a model switch on the server |

#### `RequestRecord` State Diagram

```
                    +--------+
                    |  new   |   (internal transition after auth/validation)
                    +---+----+
                        |
                        v
      +--------+   pick_next   +---------+   ok    +-----------+
      | queued |-------------->| running |-------->| completed |
      +---+----+               +---+-----+         +-----------+
          ^                        |
          |  retry (another server, | error & attempts left
          |  if available; else same)
          +------------------------+
                                   |
                                   | error & total attempts == max_attempts
                                   v
                              +--------+
                              | failed |
                              +--------+
```

Allowed transitions:
- `new → queued` (after successful authorization and model validation)
- `queued → running` (worker picks up the request)
- `running → completed` (backend returned a complete response; for streaming — after `[DONE]` or clean SSE close from the backend)
- `running → queued` (retry: backoff between attempts; implementation may keep the record in `running` with an `attempts` counter — acceptable if documented)
- `running → failed` (total `retry.max_attempts` exhausted)
- `running → running` (rolling exclusion: server change with `attempts` increment)

Terminal states: `completed`, `failed`. Records remain in SQLite until `history_retention_days`.

### 5.2. ServerInfo

| Field                 | Type            | Description                                       |
|-----------------------|-----------------|---------------------------------------------------|
| `name`                | str             | from config                                       |
| `url`                 | str             | base URL                                          |
| `type`                | enum            | `openai` (only this value in MVP)                 |
| `healthy`             | bool            | flag                                              |
| `current_model`       | str \| None     | model of the last processed / in-flight request   |
| `pending`             | []Request       | in-memory queue (slice under mutex)               |
| `in_flight`           | Request \| None | active request (≤ 1)                              |
| `failed_polls_streak` | int             | counter of consecutive failed discovery polls     |
| `models`              | set[str]        | known models (from discovery or config)           |

Lifecycle: created on daemon startup from config. `healthy` toggles on discovery misses and on consecutive failed requests (policy — implementation choice; minimum: discovery is sufficient).

### 5.3. ModelMap

`map[server_name string]map[model_id string]struct{}`. Updated by the discovery cycle or from explicit `backends[].models` config. Used by the router and the `GET /v1/models` endpoint (aggregation).

---

## 6. Scheduler Invariants

These properties MUST always hold. Test cases for them are a required part of unit tests (`internal/core/scheduler_test.go`).

### INV-1. At most 1 in-flight request per server at any time

**Test 1.1.**
- **Given:** server `srv1`, queue of 5 requests with different models.
- **When:** the worker processes them sequentially.
- **Then:** at any point in time `srv1.in_flight is None` or exactly one Request object; never two.

### INV-2. Drain current model before switching

If `pending` contains a request for model M and `current_model == M` (or the current in-flight uses M), then the next request processed MUST be **one of the requests for M**, not for a different model.

**Test 2.1.**
- **Given:** `current_model = "qwen2.5:14b"`, queue: `[A_qwen, B_llama, C_qwen, D_llama]` in arrival order.
- **When:** the worker calls `pick_next_request` four times in a row.
- **Then:** selection order — `A_qwen → C_qwen → B_llama → D_llama` (all qwen first, then FIFO for the rest).

**Test 2.2.**
- **Given:** `current_model = None`, queue: `[A_qwen]`. Concurrently, `B_qwen` arrives 1 ms later, `C_llama` 2 ms later.
- **When:** the worker takes `A_qwen` (in-flight); while `A` executes, the queue becomes `[B_qwen, C_llama]`.
- **Then:** after `A`, `B_qwen` is selected, then `C_llama`. No model switch between `A` and `B`.

### INV-3. FIFO within the same model on the same server

**Test 3.1.**
- **Given:** queue `[X1_M, X2_M, X3_M]` (all for model M).
- **When:** the worker processes three requests.
- **Then:** completion order matches arrival order (`X1 → X2 → X3`).

### INV-4. Completed only after a full response

A request is marked `completed` exclusively after receiving the complete response from the backend. For streaming — after receiving `data: [DONE]` or clean SSE connection close by the backend. An error mid-stream → `failed` (see INV-6).

**Test 4.1.**
- **Given:** mock backend started streaming `data: {chunk1}`, then closed the connection **before** `[DONE]`.
- **When:** the proxy detects the disconnection.
- **Then:** the record has `status == "failed"`, `error` contains a description of the disconnection. The client has received the already-sent chunks + SSE termination with an error marker (see API.md §1.6).

### INV-5. Attempt counter does not exceed the limit, and two consecutive attempts go to different servers

(a) Total number of attempts for a single request ≤ `retry.max_attempts`.

(b) If two consecutive attempts have |compatible healthy servers| ≥ 2, they MUST execute on different servers. This is "rolling exclusion size 1": after a failure on X the next attempt does not go to X. One attempt later, X is available again.

(c) If there is only one compatible server — all attempts go to it (single-server degradation). This is not a violation of (b): the candidate set is empty after exclusion, so the exclusion is lifted.

**Test 5.1.**
- **Given:** `max_attempts = 3`, two healthy servers with model M (`srv1`, `srv2`). `srv1` always returns 502, `srv2` — 200.
- **When:** client sends one request.
- **Then:** attempt 1 on `srv1` fails → attempt 2 on `srv2` (rolling exclusion) returns 200. `RequestRecord.attempts == 2`.

**Test 5.2.**
- **Given:** `max_attempts = 4`, two servers, both return 502.
- **When:** request is processed.
- **Then:** attempts alternate `srv1 → srv2 → srv1 → srv2` (or starting from `srv2`, depending on the router). No two consecutive attempts on the same server. After exhaustion — `502/503`, `status = failed`.

**Test 5.3 (single-server degradation).**
- **Given:** `max_attempts = 3`, one healthy server with model M, always 502.
- **When:** request is processed.
- **Then:** 3 consecutive attempts on this single server; errors returned to client. (b) does not apply since |healthy| = 1.

### INV-6. No retry after the first SSE chunk

If the proxy has already sent the client ≥ 1 SSE chunk and then receives an error from the backend — retry and server switching are PROHIBITED; the stream is terminated, record `failed`.

**Test 6.1.**
- **Given:** mock backend sent 2 SSE chunks, then dropped the connection.
- **When:** the proxy handles the disconnection.
- **Then:** an SSE chunk with an error object and stream termination are sent to the client; no attempts on other servers were made; corresponding log entry is present.

### INV-7. Queue loss is only acceptable on restart

During normal operation (without a process restart) no request from `pending` must be "forgotten" by the scheduler: it is either processed or marked `failed`.

**Test 7.1.**
- **Given:** queue of 100 requests; mock backend responds instantly.
- **When:** all requests pass through the worker.
- **Then:** exactly 100 records in SQLite in terminal states; none in `queued`/`running`.

### INV-8. Routing uses only healthy servers

**Test 8.1.**
- **Given:** two servers; `srv1` is marked `unhealthy`. Both advertise model M.
- **When:** client sends a request for M.
- **Then:** request is queued to `srv2`. If `srv2` is also unhealthy — `503 Service Unavailable`.

---

## 7. Acceptance Criteria (v0.9.3 baseline)

The baseline is considered complete when **all** items below are satisfied:

| ID    | Criterion | Verification method |
|-------|-----------|---------------------|
| AC-1  | `proxylm config init` creates a valid `config.example.yaml`. | CLI + diff against reference |
| AC-2  | `proxylm config validate` rejects invalid config with non-zero exit code and a clear message. | CLI + 3 negative cases |
| AC-3  | `proxylm serve` starts in < 5 s and accepts HTTP requests. | manual |
| AC-4  | `POST /v1/chat/completions` (non-streaming) with a valid key and existing model returns 200 with an OpenAI-compatible body. | integration test |
| AC-5  | `POST /v1/chat/completions` with `stream: true` delivers SSE chunks in real time; final `[DONE]` is present. | integration test |
| AC-6  | `GET /v1/models` aggregates models from all healthy servers. | integration test |
| AC-7  | Without `Authorization` header or with an invalid key — `401`. | integration test |
| AC-8  | Request for a non-existent model — `404` with body `{"error": {"code": "model_not_found", ...}}`. | integration test |
| AC-9  | Drain scenario: 10 requests for model A + 5 for model B, two servers with both models — model A on the selected server is not switched until its sub-queue is empty. Verified by logs and history. | integration test |
| AC-10 | Rolling failover: `srv1` returns 502 on first attempt → second attempt goes to `srv2` without consuming all `max_attempts` on `srv1`. | integration test |
| AC-11 | Streaming: error after first chunk does not trigger retry; record `failed`. | integration test |
| AC-12 | TUI connects via WebSocket with admin key and shows a live request and server table. | manual |
| AC-13 | Completed requests disappear from TUI table after `tui.show_completed_minutes` but remain in SQLite. | manual + SQL query |
| AC-14 | History is cleaned up per `history_retention_days` (test with mocked time). | unit test |
| AC-15 | `Ctrl+C` on the daemon terminates the process within `shutdown_grace_seconds`; in-flight requests either complete or the client receives an error. | manual |
| AC-16 | NFR-1: benchmark shows ≥ 100 RPS proxy processing (mock backend). | bench script |
| AC-17 | First `proxylm serve` launch without an existing `config.yaml` alongside the binary creates it from the embedded template and continues startup. | manual |
| AC-18 | `proxylm service install` registers the service on Windows (Service Manager) and Linux (systemd unit); `service start/stop` manages its lifecycle. | manual |
| AC-19 | Build `GOOS=linux GOARCH=arm64 go build` on a Windows host successfully produces a binary without CGO toolchain. | manual |
| AC-20 | After at least 3 completed requests to one model on one server, `ServerState.perf_ok = true`; `tok_out_per_sec > 0`; TUI shows the metric in the header line. | integration test |
| AC-21 | RM column in TUI table: a row with `model_reloaded=true` shows `✓`, a row without reload shows `—`. | manual + SQL query |
| AC-22 | `Tab` in TUI cycles focus Header → Requests → Info → Header; in `paneHeader` `↑`/`↓` change the selected server; `Enter` shows server details in `paneInfo`; in `paneRequests` `Enter` shows request details in `paneInfo`. | manual |
| AC-23 | Request table: columns are aligned in the terminal without shifting when pending rows are present (glyph `…` occupies exactly one cell). | manual |
| AC-24 | Field `model_reloaded` is present in the SQLite `requests` table (verified by `.schema requests`); value `1` for requests with model switch, `0` for others. | SQL query |
| AC-25 | TUI does not exit on WebSocket disconnection; after daemon restart (≤ 30 s) the TUI automatically shows live data again without any user action. | manual |
| AC-26 | With `discovery.enabled=false` and all backends having explicit `models` lists, the daemon accepts `/v1/chat/completions` immediately after startup (no 503 indefinitely). | integration test |
| AC-27 | Response to `POST /v1/chat/completions` includes the `X-Request-Id` header with a UUIDv4 value. | integration test |
| AC-28 | Pressing `F5` in the TUI causes a `request_snapshot` WebSocket frame to be sent and a `state_snapshot` to be received within the next tick. | manual |

---

## 8. Out of Scope (deferred to FUTURE)

The following are explicitly **not** implemented in the v0.9.3 baseline:

- Web UI (HTML interface).
- Prometheus metrics / `GET /metrics`.
- Native Ollama API endpoints (`/api/generate`, `/api/chat`) — OpenAI shim only.
- In-memory queue persistence across restarts.
- Per-client quotas and priorities.
- Rate limiting (`429` responses; code 429 is reserved for the future).
- Tools / function calling support beyond simple passthrough of request/response fields.
- Embedding rate limiting / special-form batch API.
- TLS encryption on the proxy side (a reverse proxy nginx/caddy in front of ProxyLM.GO is recommended).
- Cluster mode (multiple ProxyLM.GO instances with shared state).
- mTLS / OAuth2 authentication.
- Request cost tracking in monetary units.
- Canary deployment / hot config reload (requires restart).

---

## 9. Risks and Assumptions

### 9.1. Assumptions

| ID    | Assumption | If violated |
|-------|------------|-------------|
| A-1   | Backend servers are reachable at daemon startup (or become reachable within ≤ N discovery cycles). | Requests will receive 503 until recovery. |
| A-2   | LM Studio in server mode holds **one** active model at a time; model switch occurs via the first request with a different `model`. | The scheduler algorithm remains valid; INV-2 continues to hold. |
| A-3   | The Ollama OpenAI shim (`/v1/*`) correctly responds to `/v1/models` and `/v1/chat/completions`. | Native `/api/*` support is out of scope; users must use the OpenAI shim. |
| A-4   | The network between the proxy and backend is stable; significant packet loss is treated as a server failure. | Failover should compensate. |
| A-5   | Clients retry requests on 5xx from their side (since the proxy will have exhausted its own attempts). | Some requests will fail at the client when all backends are down. |
| A-6   | `usage` in the final SSE chunk from the backend is present **often enough** for token statistics to be useful. | If absent — `input_tokens`/`output_tokens` remain `NULL` (see U-1 in §9.3); fallback token counting tracked in [`docs/FUTURE.md`](./FUTURE.md). |

### 9.2. Risks

| ID    | Risk | Mitigation |
|-------|------|------------|
| R-1   | Starvation of model B under an infinite stream of model A requests (see ARCHITECTURE §3). | Documented as a deliberate trade-off; possible mitigation tracked in [`docs/FUTURE.md`](./FUTURE.md) (item §8). |
| R-2   | LM Studio takes a long time to load a large model into VRAM → request timeout. | `backends[].timeout_seconds` (default 600) + recommendation in README. |
| R-3   | Mismatch between discovery model list and reality (model deleted on host between polls). | Request will get 404 from backend → standard error path + next discovery cycle corrects ModelMap. |
| R-4   | Concurrent access to `current_model` between the worker and the router. | Worker updates `current_model` under the server's `sync.Mutex`; router reads the value under the same mutex (or `atomic.Pointer[string]`); eventual consistency is acceptable for heuristics. |
| R-5   | Queue size is unbounded — DoS from a client. | Documented as a deliberate trade-off; mitigation tracked in [`docs/FUTURE.md`](./FUTURE.md). |
| R-6   | Key leakage into logs during debugging. | Masking in `internal/api/auth.go`; covered by unit test. |
| R-7   | `modernc.org/sqlite` is slower than the CGO variant under high history write load. | Async history writing (via channel), batch inserts; further mitigation tracked in [`docs/FUTURE.md`](./FUTURE.md) (item §9). |

### 9.3. Closed Questions (Confirmed Decisions)

All questions previously marked "requires clarification" have been confirmed and are considered final for MVP.

- **U-1.** Token counting when `usage` is absent in the backend response — leave `NULL`. Fallback counting tracked in [`docs/FUTURE.md`](./FUTURE.md).
- **U-2.** `GET /v1/models` returns models **from healthy servers only**.
- **U-3.** `proxylm serve` when all backends are unavailable at startup — **starts** and returns `503` for requests until discovery finds at least one healthy server.
- **U-4.** On mid-stream SSE disconnection — send `event: error` with `data: {"error": {"code": "stream_aborted", "message": "..."}}`, then `data: [DONE]` (see API.md §1.6).
- **U-5.** Exactly **one** admin key is supported (`auth.admin_key`).
- **U-6.** On `503` the proxy returns the header `Retry-After: 1` (second).
- **U-7.** Config and database reside **alongside the binary** (portable). If `config.yaml` is absent, the daemon creates it from the embedded template and continues startup. Path can be overridden with `--config`.

Additionally confirmed regarding scheduler behavior:

- **K-1 (current_model semantics).** **Eventual consistency** is used: `current_model` on the server is updated *after* the in-flight request completes successfully. The router reads the value without locks (`atomic.Pointer[string]` or snapshot under RLock). The window between "request taken by worker" and "`current_model` updated" is acceptable — this is a heuristic for the router, not a strict invariant. Risk R-4 is accepted.

---

## 10. Versioning and Roadmap

### Current baseline: v0.9.3 (first public release)

Fully implements:
- FR-1 … FR-51 (HTTP API, scheduler, routing, retry, discovery, history, TUI/IPC, CLI, performance metrics, auto-reconnect, initial healthcheck, X-Request-Id middleware, F5 protocol)
- NFR-1 … NFR-12
- INV-1 … INV-8
- AC-1 … AC-28

### Future work

Roadmap items, ideas, and deferred mitigations are tracked **exclusively** in [`docs/FUTURE.md`](./FUTURE.md). No version-numbered roadmap is maintained inside this SRS — see `CLAUDE.md` (FUTURE-RULE) for how items move from FUTURE to a real release.

Versioning follows SemVer; see `CLAUDE.md` (section «Версионирование (SemVer)») for the bump policy and how the `MAJOR.MINOR.PATCH` components are chosen by Claude Code at commit time.
