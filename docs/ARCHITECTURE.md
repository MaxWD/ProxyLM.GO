# ProxyLM.GO — Architecture

## 1. Purpose

A Go proxy server in front of local and remote LLM servers (LM Studio, Ollama, Anthropic, OpenAI, etc.). The primary goal is to **serialize requests by model**, preventing constant model eviction and loading into VRAM.

Key properties:
- OpenAI-compatible API on the ingress (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/models`)
- Anthropic Messages API on the ingress (`/v1/messages`) — clients using Anthropic SDK connect without code changes
- **Cross-protocol translation:** all four client/backend combinations are handled transparently (OpenAI↔OpenAI, OpenAI↔Anthropic, Anthropic↔OpenAI, Anthropic↔Anthropic)
- **Dual auth:** `Authorization: Bearer` and `x-api-key` headers are both accepted
- Streaming support (SSE — OpenAI `data:` format and Anthropic `event:`/`data:` format)
- Multiple backend servers; knows which models are where (auto-discovery via `/v1/models`)
- Routing: model affinity + least-busy
- Retry + failover to another server with the same model
- API-key authentication
- btop-style console TUI (separate client to the daemon)
- Embedded, read-only Web UI (`/ui/*`) mirroring the TUI dashboard for browsers on the LAN — see §19
- Request history in SQLite; performance regression observations are persisted too (see §10, §17)
- **Single portable binary:** the same executable file acts as daemon, TUI client, and service installer. Cross-compiles to any OS without CGO.

## 2. System Diagram

```
┌──────────────────────────────────────────────────────────┐
│ Clients (internal services)                              │
│ service-a (OpenAI SDK), service-b (Anthropic SDK), ...   │
└──────────────────────┬───────────────────────────────────┘
                       │ HTTP (OpenAI /v1/* or Anthropic /v1/messages)
                       ▼
┌──────────────────────────────────────────────────────────┐
│ ProxyLM.GO Daemon  (net/http + chi + goroutines)         │
│                                                          │
│  HTTP API ─► AuthN ─► Translate? ─► Router ─► Queue     │
│  (OpenAI+Anthropic)  (Bearer/x-api-key)  (model)        │
│                          │                               │
│                          │ selection: which server for   │
│                          │   this (model)                │
│                          ▼                               │
│                  ┌──────────────────┐                    │
│                  │ Worker per server│  ⇐ key rule:       │
│                  │ "drain current   │     "while there   │
│                  │  model fully,    │     are requests   │
│                  │  then switch"    │     for model X —  │
│                  └────────┬─────────┘     don't swap it" │
│                           │                              │
│                  Backend client (*http.Client)           │
│                  (openai.go or anthropic.go)             │
│                  retry + failover                        │
│                           │                              │
│  Discovery ──► ModelMap   │   SQLite history             │
│                           │   (modernc.org/sqlite)       │
│                           │                              │
│  IPC server (WebSocket) ◀─┴─►  TUI client (Bubble Tea)   │
└─────────────────────┬────────────────────────────────────┘
                      │ HTTP (OpenAI /v1/* or Anthropic /v1/messages)
                      ▼
┌──────────────────────────────────────────────────────────┐
│ Backend LLM servers                                      │
│ srv1 (LM Studio/OpenAI), srv2 (Ollama), srv3 (Anthropic) │
└──────────────────────────────────────────────────────────┘
```

## 3. Scheduler Algorithm (Core)

Each backend server has its own worker goroutine. At any moment exactly **one** request executes on a server — this rule eliminates model eviction races.

```go
// Per-server state (simplified)
type Server struct {
    Name         string
    URL          string
    CurrentModel atomic.Pointer[string] // model of the last served / in-flight request
    mu           sync.Mutex
    pending      []*Request              // FIFO under mu
    notify       chan struct{}           // buffered 1, signal "new item arrived"
    inFlight     *Request                // at most one, under mu
    healthy      atomic.Bool
    // ...
}

// Server worker: one goroutine per server
func (s *Server) workerLoop(ctx context.Context, dispatch DispatchFn) {
    for {
        next := s.pickNextRequest()
        if next == nil {
            select {
            case <-ctx.Done():
                return
            case <-s.notify: // woken up: enqueue or health-change
                continue
            }
        }
        s.mu.Lock()
        s.inFlight = next
        s.mu.Unlock()

        dispatch(ctx, next)            // blocks until completion (or error + retries)

        cm := next.Model
        s.CurrentModel.Store(&cm)      // eventual consistency for the router
        s.mu.Lock()
        s.inFlight = nil
        s.mu.Unlock()
    }
}

// pickNextRequest — drain current model → FIFO
func (s *Server) pickNextRequest() *Request {
    s.mu.Lock()
    defer s.mu.Unlock()

    cm := s.CurrentModel.Load()
    if cm != nil {
        for i, r := range s.pending {
            if r.Model == *cm {
                s.pending = append(s.pending[:i], s.pending[i+1:]...)
                return r
            }
        }
    }
    if len(s.pending) > 0 {
        r := s.pending[0]
        s.pending = s.pending[1:]
        return r
    }
    return nil
}

// Enqueue wakes the worker non-blocking
func (s *Server) Enqueue(r *Request) {
    s.mu.Lock()
    s.pending = append(s.pending, r)
    s.mu.Unlock()
    select {
    case s.notify <- struct{}{}:
    default: // channel capacity 1 — signal already present, no duplication needed
    }
}
```

**Algorithm properties:**
- Requests for the **current** model are always served first, even if they arrive while the server is busy. → The model is **not evicted** between them.
- When the queue for the current model is empty, the next FIFO request is taken — it triggers a model switch.
- One in-flight per server. Requests on different servers proceed in parallel (one goroutine per server + separate `*http.Client` for each).
- Starvation of model B is possible only if the request stream for model A is truly infinite — **this trade-off is intentional**. Optionally, `max_consecutive_requests_per_model` can be added later.

The `sync.Cond` alternative is intentionally not used: a `notify` channel with capacity 1 + `select { case <-ctx.Done(); case <-s.notify }` is an idiomatic Go pattern that correctly handles cancellation via `context.Context`.

## 4. Routing (Router)

When a new request arrives with model M:

```
candidates = filter(servers, healthy && M ∈ models_of(server))
if candidates is empty:
  → 404 Model not found
sort candidates by:
  1) prefer server where M == current_model (avoid swap)
  2) prefer server with the shortest pending queue (least-busy)
  3) tiebreak: server name (stability)
choose candidates[0]
```

This is `model_affinity_least_busy` — the default push-strategy. Implementation is a pure function of `[]*ServerInfo` and `model`; it does not block workers and reads `CurrentModel` via `atomic.Pointer[string].Load()`.

### Pull strategies (shared `JobPool`)

For `deferred_model_then_capable`, `preserve_model_coverage`, and `fair_share_round_robin` no server is assigned at accept time. Instead the scheduler keeps a shared `JobPool` (`internal/core/pool.go`); released workers pull the next compatible Job themselves via `PopFor` / `PopForCoverage` / `PopForFairShare`. This redistributes load proportionally to backend speed when several servers hold the same model.

`fair_share_round_robin` (added in v0.10.0) extends `deferred_model_then_capable` with **starvation protection**. The scheduler tracks `ConsecutiveModelCount` on each `ServerInfo` (under `s.mu`, updated in `dispatch`). When `scheduler.max_consecutive_per_model > 0` and the count has reached the limit, `PopForFairShare` scans the pool for a Job under a model different from `current_model` and dispatches it. If no other compatible model is queued, it falls back to ordinary FIFO drain — the worker is never stuck idle while compatible work exists. The cost is one extra model reload every N requests, which is honestly reflected in the perf regression (`t_load × loaded=1`).

## 5. Retry + Failover

On backend error:
- Transient errors (5xx, timeout, network reset) → retry with exponential backoff.
- **Rolling exclusion size 1:** after a failure on server X the next attempt goes to any other healthy server with this model; X is excluded ONLY for the one next attempt (after that it is available again). If X is the only compatible server, the exclusion is ignored and the attempt goes to X again.
- Overall cap — `retry.max_attempts` (default 3) attempts per request **regardless of how they are distributed across servers** (see INV-5).
- There is no separate `failover` setting; this behavior is always active.
- A server that accumulates consecutive failures is marked `unhealthy` (via discovery), excluded from routing, and periodically health-checked.

Backoff: `time.Sleep(d)` inside the worker is acceptable (the worker is a separate goroutine, it blocks nothing but its own queue); for cancellation — `select { case <-time.After(d); case <-ctx.Done() }`.

Streaming nuance: if the error arrives **before the first SSE chunk** — retry is safe. If part of the response has already been sent to the client — retry is impossible (the response degrades to a specific error to the client).

## 6. Streaming

- Client: `POST /v1/chat/completions` with `stream: true`.
- The proxy opens a streaming connection to the backend via the standard `http.Client.Do(req)`; the response is read from `resp.Body` (`io.ReadCloser`).
- Line-by-line reading via `bufio.Reader.ReadBytes('\n')`; SSE frame parsing (prefix `data: `, delimiter — blank line).
- Writing to the client: `http.ResponseWriter.Write(...)` + `w.(http.Flusher).Flush()` after each chunk — no buffering.
- In parallel the proxy counts `output_tokens` (from `delta.content` or from the `usage` field of the final chunk) and sends events to the IPC publisher for the TUI.
- `input_tokens`: taken from the last chunk with `usage` (LM Studio/Ollama OpenAI shim returns usage in the final `[DONE]` chunk); fallback tokenization library — deferred to v0.2 (U-1).

## 7. Cross-Protocol Translation

In v0.11.0 ProxyLM.GO became multi-protocol. The translation layer sits between the HTTP handler and the scheduler worker, and between the worker and the backend client.

### Translation matrix

| Client endpoint        | Backend `type` | Action |
|------------------------|----------------|--------|
| `/v1/chat/completions` | `openai`       | Passthrough — no transformation |
| `/v1/chat/completions` | `anthropic`    | Request: OpenAI→Anthropic; Response: Anthropic→OpenAI |
| `/v1/messages`         | `openai`       | Request: Anthropic→OpenAI; Response: OpenAI→Anthropic |
| `/v1/messages`         | `anthropic`    | Passthrough — no transformation |

`/v1/completions` and `/v1/embeddings` are never routed to `type: anthropic` backends (returns `400`).

### Package `internal/api/translate/`

Three files implement the translation logic:

- `request.go` — converts request structs between OpenAI `ChatCompletionRequest` and Anthropic `MessagesRequest`. Key mappings: `messages[].role`, system prompt extraction/injection, `max_tokens`, `stop`/`stop_sequences`, tool format differences.
- `response.go` — converts non-streaming response structs. Handles content blocks (`content[].type = "text"`), `stop_reason`/`finish_reason`, `usage` field renaming.
- `stream.go` — defines the `StreamTranslator` interface used by streaming handlers. Stateful: tracks `message_start` → `content_block_*` → `message_stop` event sequence.

### Streaming translation (`internal/api/streaming_translate.go`)

For streaming paths requiring translation, the proxy uses `streaming_translate.go` instead of the plain `streaming.go`. It wraps the SSE line reader and applies the `StreamTranslator` to convert each event before forwarding to the client. The stateful translator is necessary because Anthropic SSE is fundamentally different from OpenAI SSE:

- **OpenAI SSE:** `data: <json-chunk>\n\n`, ended with `data: [DONE]\n\n`.
- **Anthropic SSE:** `event: <type>\ndata: <json>\n\n` — named events; multiple event types per message.

Translation rules for `Anthropic→OpenAI` direction: accumulate `content_block_delta` text into a synthetic `choices[0].delta.content` chunk; map `message_start.usage.input_tokens` → first chunk `usage.prompt_tokens`; map `message_delta.usage.output_tokens` → last chunk `usage`; emit `data: [DONE]` on `message_stop`.

Translation rules for `OpenAI→Anthropic` direction: wrap each `choices[0].delta.content` into a `content_block_delta` event; synthesize `message_start` before the first chunk; emit `message_stop` after `data: [DONE]`.

### Anthropic Backend client (`internal/core/backends/anthropic.go`)

Implements the `Backend` interface for backends with `type: anthropic`. Sends requests to `<url>/v1/messages` using the Anthropic Messages API wire format. Authentication: sets both `Authorization: Bearer <api_key>` and `x-api-key: <api_key>` for maximum compatibility with Anthropic-compatible services. The INV-1..INV-8 scheduler invariants are unchanged — the `anthropic.go` backend participates in the same per-server queue and worker loop as `openai.go`.

## 8. Discovery

- **Initial healthcheck:** on daemon startup, before accepting HTTP requests, a single synchronous poll of `GET /v1/models` is performed for every backend server regardless of `discovery.enabled`. Servers with an explicit non-empty `backends[].models` list are marked healthy immediately without a poll.
- Every `discovery.interval_seconds` (default 30 s), poll `/v1/models` on each server.
- One shared `time.Ticker`, then fan-out goroutines (one per server) in a loop; results collected into `ModelMap: map[string]map[string]struct{}`.
- Used by the router.
- If a server is unreachable for N consecutive cycles → `unhealthy.Store(false)`.
- The discovery loop receives a `context.Context` and shuts down cleanly on shutdown.
- **Loaded-model probe (v0.13.0):** in the same poll cycle, backends that implement the optional `backends.LoadedModelsProber` interface are additionally queried for the models *currently resident in memory*. The probe is type-driven (`backends[].type`): Ollama → `GET /api/ps`, LM Studio → `GET /api/v1/models` (models with a non-empty `loaded_instances`), llama.cpp → `GET /models` (filter `status==loaded`). Plain `openai` and `anthropic` backends don't implement it and are skipped. The result is stored on `ServerInfo` via `SetLoadedModels` (atomic) and published as `ServerState.loaded_models` / `loaded_models_probed`. A probe failure is non-fatal — it never flips `healthy` and leaves the previous snapshot intact.
- **Strict `/v1/models` shape validation (v0.14.0):** `parseModelsBody` (`internal/core/backends/backend.go`), shared by `openai.go` and `anthropic.go`, accepts two shapes: `{"data": [...]}` where every entry has a non-empty string `id` (`{"data": []}` is valid — zero models, not an error), or a legacy top-level string array (including `[]`). Any other 2xx body — objects in `data` missing `id`, HTML, unrelated JSON — returns an error wrapping the sentinel `backends.ErrModelsShape`. `Discovery` (`initial healthcheck` and `pollOne`) treats this distinctly from a network/HTTP error: the server is marked `unhealthy` and the WARN log names `backends[].url` as the likely misconfiguration (a user pointed the proxy at a non-OpenAI-compatible endpoint — Anthropic `/v1/messages`, Azure `api-version`, raw Ollama `/api/generate`, etc.). A healthy 2xx response with zero models (`{"data": []}`) is logged at WARN ("server healthy but reports zero models") but the server stays `healthy`. For servers with a non-empty explicit `backends[].models` list, a shape error during a **periodic** poll is treated as a successful liveness check (`failed_polls` resets to 0, server marked `healthy`) — the endpoint responded, and the explicit list is already the source of truth for routing; this leniency does not apply to network-level errors, and the initial healthcheck for explicit-models servers is unchanged (still skips the poll entirely, per FR-49).

## 9. TUI ↔ Daemon (IPC)

The daemon exposes an additional WebSocket endpoint (`/admin/stream`) on the main HTTP port (or a separate port, see config).

WebSocket library: `github.com/coder/websocket` (minimalist, idiomatic for Go 1.21+, no external dependencies).

The TUI (Bubble Tea) connects via the same binary in `proxylm tui --connect ...` mode. On connect the daemon immediately sends a `state_snapshot` envelope (type `state_snapshot`, field `time`, field `payload` containing `servers` and `requests` arrays). The TUI may request a fresh snapshot at any time by sending `{"type": "request_snapshot", "time": "<RFC3339>"}` — this is the F5 refresh mechanism.

**TUI auto-reconnect:** on WebSocket disconnection the TUI client performs infinite exponential backoff (1 s, 2 s, …, cap 30 s). The title shows `connecting…` / `reconnecting…` / `live`. Exit only via `q` / `F10` / Ctrl-C.

Authentication: the same Bearer mechanism, but using a dedicated admin key. Since v0.14.0 a second, browser-oriented auth channel exists for the same endpoint — see §19.

The publisher on the daemon side is a separate goroutine with an incoming `chan Event`; core modules (scheduler, router, retry) send events non-blocking (with backpressure protection: drop on buffer overflow, log `event_drop`).

## 10. Database (SQLite)

Driver: **`modernc.org/sqlite`** — pure-Go, no CGO. Access via the standard `database/sql` package (driver registers via `import _ "modernc.org/sqlite"`).

Migrations — `*.sql` files under `internal/storage/migrations/`, embedded in the binary via `//go:embed migrations/*.sql`. Applied sequentially on first run and on subsequent starts (no-op if version matches).

```sql
-- 0001_init.sql
CREATE TABLE IF NOT EXISTS requests (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id      TEXT UNIQUE NOT NULL,        -- UUID
  client_name     TEXT,                         -- from API key
  model           TEXT NOT NULL,
  server          TEXT,
  status          TEXT NOT NULL,                -- queued/running/completed/failed
  received_at     TIMESTAMP NOT NULL,
  started_at      TIMESTAMP,
  first_chunk_at  TIMESTAMP,
  completed_at    TIMESTAMP,
  queue_wait_ms   INTEGER,
  duration_ms     INTEGER,
  ttft_ms         INTEGER,
  input_tokens    INTEGER,
  output_tokens   INTEGER,
  attempts        INTEGER DEFAULT 0,
  error           TEXT,
  stream          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_received_at ON requests(received_at);
CREATE INDEX IF NOT EXISTS idx_status      ON requests(status);

CREATE TABLE IF NOT EXISTS schema_version (
  version INTEGER PRIMARY KEY,
  applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- 0002_model_reloaded.sql (migration v0.7.0)
ALTER TABLE requests ADD COLUMN model_reloaded INTEGER NOT NULL DEFAULT 0;
-- Backfill = 0: for old rows the reload fact is unknown — safe default.

-- 0004_perf_observations.sql (migration v0.14.0)
CREATE TABLE perf_observations (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  server     TEXT    NOT NULL,
  model      TEXT    NOT NULL,
  endpoint   TEXT    NOT NULL,
  in_tokens  INTEGER NOT NULL,
  out_tokens INTEGER NOT NULL,
  total_ms   INTEGER NOT NULL,
  loaded     INTEGER NOT NULL,
  created_at TEXT    NOT NULL
);
CREATE INDEX idx_perf_obs_key ON perf_observations(server, model, endpoint, id);
-- id doubles as FIFO order for ring-buffer trimming within a (server, model,
-- endpoint) key; created_at (RFC3339Nano UTC) is forensics-only and is never
-- read back into PerfTracker.
```

Queue — **in-memory only** (restart = clients receive an error and retry on their own).

History writing — asynchronous: a writer goroutine reads from `chan HistoryEvent`, performs batch inserts (or single inserts with PRAGMA `synchronous = NORMAL`).

### Perf statistics persistence (v0.14.0)

Prior to v0.14.0, `PerfTracker` observations lived only in memory and were lost on every restart (see §17). `internal/storage/perf.go` (`PerfStore`) now persists them, always on, with no config flag. Lifecycle, wired in `cmd/serve.go`:

1. **Startup trim.** `PerfStore.Trim(ctx, core.PerfMaxObservations)` deletes rows beyond the in-memory ring-buffer cap (1000 per key) for each `(server, model, endpoint)` — guards against a table grown larger under a previous build that used a bigger cap.
2. **Bulk restore.** `PerfStore.LoadAll(ctx)` reads all rows grouped by key, ordered `id ASC` (oldest→newest); `PerfTracker.Load` bulk-restores them into memory. `Load` never invokes the sink — restoring persisted data must not immediately re-queue it for persistence.
3. **Sink wiring.** Only *after* the bulk restore is `PerfTracker.SetSink(perfStore.Enqueue)` called, then `go perfStore.Run(ctx)` starts the single writer goroutine. Ordering matters: wiring the sink before `Load` would write the restored points straight back to the table they came from.
4. **Hot-path write.** On every subsequent accepted `Record` (i.e. one that passed validation), the sink fires *outside* `PerfTracker`'s mutex and performs a non-blocking send on `chan perfRow` (capacity 1024). A full channel drops the observation with a Debug log — this is best-effort persistence and must never add latency or backpressure to the scheduler.
5. **Single writer.** `PerfStore.Run` is the only goroutine that writes to `perf_observations`; it drains the channel and inserts one row at a time using a detached `context.Background()` + 5 s timeout per insert (so an in-flight write survives daemon shutdown — the same pattern as `persistRecord` in `internal/api/routes_openai.go`).
6. **Ongoing trim.** `runMaintenance` (renamed from `runRetention` in v0.14.0) runs `PerfStore.Trim` every hour **unconditionally** — independent of whether `storage.history_retention_days > 0` — alongside the existing history-retention sweep.

## 11. Configuration

See `config.example.yaml`. Sections: `proxy`, `auth`, `routing`, `retry`, `discovery`, `storage`, `tui`, `compat`, `backends`.

Loading: `gopkg.in/yaml.v3` → typed Go struct → manual validation (port > 0, non-empty key names, valid backend URLs). If the file is not found alongside the binary — the daemon creates it from the embedded template (`//go:embed config.example.yaml`) and logs a warning.

Config search path:
1. `--config <path>` — explicit override.
2. `<dir(executable)>/config.yaml` — alongside the binary (default, portable).

Database: `storage.database_path` (default — `./proxylm.db` relative to the binary).

## 12. CLI Commands

```
proxylm serve   [--config config.yaml] [--host ...] [--port ...]
proxylm tui     [--connect ws://host:port] [--token ...] [--config ...]
proxylm config  init                                # generates config.example.yaml
proxylm config  validate [--config config.yaml]     # validates config
proxylm service install   [--config config.yaml]    # registers with Service Manager / systemd / launchd
proxylm service uninstall
proxylm service start
proxylm service stop
proxylm service status
proxylm version
```

All commands are implemented via `spf13/cobra`. `service *` uses `github.com/kardianos/service` — a unified API for Windows Service, systemd, launchd, OpenRC, SysV.

## 13. Code Structure

```
ProxyLM.GO/
├── go.mod
├── go.sum
├── main.go                       # cobra root + version embedded via -ldflags
├── README.md
├── CLAUDE.md
├── config.example.yaml
├── cmd/                          # cobra commands (thin wrappers)
│   ├── root.go
│   ├── serve.go
│   ├── tui.go
│   ├── config.go
│   ├── service.go
│   └── version.go
├── internal/
│   ├── config/                   # YAML parsing + validation + auto-generation
│   ├── logging/                  # log/slog setup (JSON handler, level)
│   ├── core/
│   │   ├── models.go             # RequestRecord, ServerInfo, ModelInfo, statuses
│   │   ├── scheduler.go          # per-server worker (goroutine), drain-then-switch; sets JobResult.ModelReloaded
│   │   ├── router.go             # server selection for (model)
│   │   ├── retry.go              # backoff + failover policy
│   │   ├── discovery.go          # periodic poll /v1/models
│   │   ├── perf.go               # PerfTracker: linear regression (server, model) → PerfStats/ModelSummary
│   │   └── backends/
│   │       ├── backend.go        # Backend interface
│   │       ├── openai.go         # client to OpenAI-compatible servers (LM Studio, Ollama, etc.)
│   │       └── anthropic.go      # client to Anthropic-compatible servers (type: anthropic)
│   ├── api/
│   │   ├── server.go             # net/http + chi, lifecycle, graceful shutdown; mounts /ui/* (webui.Handler) outside auth
│   │   ├── auth.go               # Bearer + x-api-key middleware (dual auth); admin Sec-WebSocket-Protocol token channel (v0.14.0)
│   │   ├── routes_openai.go      # /v1/chat/completions, /v1/completions, /v1/embeddings, /v1/models
│   │   ├── routes_anthropic.go   # /v1/messages handler (Anthropic Messages API)
│   │   ├── routes_admin.go       # /admin/stream (WebSocket)
│   │   ├── routes_health.go      # /healthz
│   │   ├── streaming.go          # SSE proxying + token counting (OpenAI protocol)
│   │   ├── streaming_translate.go # SSE proxying with cross-protocol translation
│   │   └── translate/
│   │       ├── request.go        # OpenAI↔Anthropic request struct translation
│   │       ├── response.go       # OpenAI↔Anthropic non-streaming response translation
│   │       └── stream.go         # StreamTranslator interface + stateful event translation
│   ├── storage/
│   │   ├── db.go                 # connection, migrations (//go:embed migrations/*.sql)
│   │   ├── history.go            # write/read requests (async writer)
│   │   ├── perf.go               # PerfStore: async persistence of PerfTracker observations (v0.14.0, see §10)
│   │   └── migrations/
│   │       ├── 0001_init.sql
│   │       ├── 0002_model_reloaded.sql  # ALTER TABLE requests ADD COLUMN model_reloaded INTEGER NOT NULL DEFAULT 0
│   │       └── 0004_perf_observations.sql # perf_observations table (v0.14.0)
│   ├── ipc/
│   │   ├── messages.go           # JSON message types (Envelope, state_snapshot, request_snapshot, hello, ...)
│   │   ├── server.go             # publisher on the daemon side; negotiates "proxylm-admin" subprotocol
│   │   └── client.go             # WebSocket client (used by TUI)
│   ├── tui/
│   │   ├── app.go                # Bubble Tea Model/Update/View
│   │   ├── widgets.go            # HeaderBar, RequestTable, InfoPane (lipgloss + bubbles)
│   │   ├── styles.go             # lipgloss styles
│   │   └── keys.go               # hotkeys (F5, F10, q, /)
│   ├── webui/                    # embedded read-only Web UI (v0.14.0, see §19)
│   │   ├── webui.go              # //go:embed static; http.Handler with forced Cache-Control: no-cache
│   │   └── static/               # index.html, app.js, style.css — vanilla JS, no build step
│   └── service/
│       └── service.go            # kardianos/service integration
├── scripts/
│   ├── build.ps1                 # build for Windows
│   ├── build.sh                  # build for Linux/macOS
│   └── build-all.ps1             # cross-compile all targets
└── test/
    └── integration/
        └── api_e2e_test.go       # mock backends via httptest.Server
```

Package tests live alongside their code (Go convention: `scheduler.go` + `scheduler_test.go`).

## 14. Stack

| Layer          | Library                                       |
|----------------|-----------------------------------------------|
| Language       | Go 1.25+ (minimum dictated by `modernc.org/sqlite`) |
| HTTP server    | `net/http` (stdlib, Go 1.22 mux) + `github.com/go-chi/chi/v5` |
| HTTP client    | `net/http` (stdlib) + per-backend `*http.Client` |
| WebSocket      | `github.com/coder/websocket`                  |
| TUI            | `github.com/charmbracelet/bubbletea` + `lipgloss` + `bubbles` |
| SQLite         | `modernc.org/sqlite` (pure-Go, no CGO)        |
| Config (YAML)  | `gopkg.in/yaml.v3`                            |
| CLI            | `github.com/spf13/cobra`                      |
| Service install| `github.com/kardianos/service`                |
| UUID           | `github.com/google/uuid`                      |
| Logging        | `log/slog` (stdlib, JSON handler)             |
| Tests          | stdlib `testing` + table-driven; integration via `net/http/httptest` |
| Lint           | `gofmt`, `go vet`, `golangci-lint`            |

Go ≥ 1.25 is effectively required by the compiler (minimum dictated by `modernc.org/sqlite` ≥ 1.50). The code itself uses no language features beyond Go 1.22.

Dependencies are minimized: all core HTTP server/client code is stdlib. Third-party libraries are used only where stdlib is inconvenient or absent (WebSocket, TUI, SQLite, CLI, YAML).

## 15. TUI ASCII Mockup

```
┌─ ProxyLM.GO v0.7.0 ──────────────────────────────────────────────────────────────────────────┐
│▸srv1 ●(qwen2.5:14b)  4200ms · ↓0 tok/s · ↑38 tok/s  Q:2                                    │
│ srv2 ●(idle)                                          Q:0                                    │
│ srv3 ✗(down)                                          Q:0                                    │
├──────────────────────────────────────────────────────────────────────────────────────────────┤
│  ID    State   Recv'd      Model           Server  RM  Queued     Time   I/O tok    Status   │
│  0042  ✓ done  14:01:02    qwen2.5:14b     srv1    —   14:01:08    6.3s  312/82      OK      │
│  0043  ▶ run   14:02:11    llama3.1:8b     srv2    ✓   —           —     400/—       …       │
│  0044  … q     14:02:15    llama3.1:8b     srv2*   —   —           —     —/—         …       │
│  0045  … q     14:02:20    qwen2.5:14b     srv1*   —   —           —     —/—         …       │
│  0040  ✗ fail  13:55:40    qwen2.5:14b     srv1    ✓   13:55:55   15.0s  —/—         ERR(2)  │
│                                                                                              │
├─ Info ────────────────────────────────────────────────────────────────────────────────────────┤
│ ID           0e9c...    Created    14:01:02   Queue wait  120ms                              │
│ Client       service-a  Started   14:01:02   Prompt tok  312                                │
│ Model        qwen2.5:14b           Completed 14:01:08   Output tok  82                      │
│ Server       srv1       Status    completed (1/3)       RM  —                               │
└──────────────────────────────────────────────────────────────────────────────────────────────┘
   F1 Help  Tab panes  F5 Refresh  m Models  t Tail  / Filter  q/F10 Quit
```

Changes relative to v0.1.0:

- Column **RM** (Reload Model) between `Server` and `Queued`: `✓` if the request was dispatched with a model switch (`model_reloaded = true`), `—` otherwise.
- Queue glyph `…` (U+2026, single cell character) instead of `⏳` (emoji wide character, 2 cells) — fixes column shifting in terminals that render emoji as two cells wide.
- Server header shows regression metrics: `t_load · ↓tok_in/s · ↑tok_out/s`. If `PerfOK=false` or model is not loaded — the metrics line is empty. The `t_load` field distinguishes three cases (v0.12.0): `—` (no reload ever observed — genuinely unknown), `1.4s*` (estimate based on fewer than 3 reload samples — low confidence), and `1.4s` (≥ 3 samples — confident).
- In the server-detail Info pane (bottom panel), the per-model table renders `tok/s` as `value±margin`; only the `±margin` part (the 95% CI error bound) is tinted (teal) so the point estimate stays readable while the uncertainty stands out (v0.12.1). Header-row metrics in the server list keep the neutral dim color.
- **Models overlay** (hotkey `m`, v0.12.0): a centered overlay listing all discovered models on the selected server, with the active model marked `▶`. Closes on `m` / `Esc` / `q`.
- Active server in the header is marked with `▸`.
- **Distinct per-server colors** (v0.13.0): each server chip (and the request table's `Server` column) is colored by the server's index in the priority-sorted list against a 12-color palette, replacing the former name-hash that collided into repeats at 3–4 servers.
- **Pulsing in-flight lamp** (v0.13.0): a healthy server actively processing a request shows a `●` whose brightness *pulses* (~640 ms cycle); an idle server shows a steady `●`. Driven by a ~160 ms animation tick that runs only while at least one server is in-flight.
- **Servers sorted by priority** (v0.13.0): ascending by `ServerState.priority` (lower = higher preference, on top), tiebreak by name — deterministic order, independent of config order.
- **Loaded-model indicator** (v0.13.0): the server-detail Info pane shows `In memory: …` (models actually resident per the native probe), `n/a` (backend doesn't support the probe), or `— (none)`. When the proxy's `current_model` is no longer resident (idle-unloaded) the chip marks it with `⏏`.
- **Generation tail** (hotkey `t`, v0.13.0): for an in-flight streaming request the request Info pane can show the last ~160 generated characters (`last_tokens`). Hidden by default (response content — privacy); kept in memory only.

Bubble Tea architecture: `Model` holds a snapshot of `[]ServerView`, `[]RequestRow`, and detail state. `Update(msg)` handles three sources:
1. WebSocket messages (`state_snapshot`) — via `tea.Cmd` with a reader goroutine.
2. Tick for periodic tasks (TUI auto-hide of completed records per `tui.show_completed_minutes`).
3. Key events (`F1`, `F5`, `Tab`, `F10`, `q`, `/`, `↑`, `↓`, `Enter`, `Esc`).

`View()` renders the entire TUI via `lipgloss` styles (border, foreground, padding).

### Three panes

The TUI supports three named panes: `paneHeader`, `paneRequests`, `paneInfo`. `Tab` cycles focus: Header → Requests → Info → Header.

When `paneHeader` is active:
- `↑` / `↓` select a server in the header; the selected one gets the `▸` marker and a bright border (`StyleBorderActive`).
- `Enter` shows server details (per-model statistics) in `paneInfo`.
- Mouse wheel in the header area also changes the selected server.

When `paneRequests` is active:
- `↑` / `↓` navigate the request list.
- `Enter` shows request details in `paneInfo`.

`paneInfo` is the lower-right information panel — it is not a modal overlay.

`F1` opens a help overlay listing all hotkeys. `Esc` / `F1` / `q` close the overlay.

## 16. Build and Distribution

**One-liner build:**
```bash
go build -ldflags "-s -w -X main.version=$(git describe --tags --always)" -o bin/proxylm .
```

**Cross-compilation:**
```bash
GOOS=windows GOARCH=amd64 go build -o bin/proxylm-windows-amd64.exe .
GOOS=linux   GOARCH=amd64 go build -o bin/proxylm-linux-amd64 .
GOOS=linux   GOARCH=arm64 go build -o bin/proxylm-linux-arm64 .
GOOS=darwin  GOARCH=amd64 go build -o bin/proxylm-darwin-amd64 .
GOOS=darwin  GOARCH=arm64 go build -o bin/proxylm-darwin-arm64 .
```

Since the SQLite driver is pure-Go, **CGO_ENABLED=0** is acceptable (and preferred for static compilation).

## 17. Performance Metrics (Regression)

The module `internal/core/perf.go` estimates server performance per `(server_name, model)` pair using linear regression. Observations live in memory (`[]perfObservation`) for the fast regression path; since v0.14.0 they are additionally persisted to SQLite and restored on startup (see §10, "Perf statistics persistence") — a daemon restart no longer resets the regression to zero samples.

### Model

For each completed request an observation is recorded:

```
(t_all, k_in, k_out, loaded)
```

- `t_all` — total request execution time on the server (ms, field `duration_ms`).
- `k_in` — number of prompt tokens (`input_tokens`).
- `k_out` — number of completion tokens (`output_tokens`).
- `loaded` ∈ {0, 1} — 1 if at the start of dispatch a model switch occurred (`model_reloaded = true`).

Linear model:

```
loaded · t_load  +  b · k_in  +  c · k_out  ≈  t_all
```

Parameters θ = (t_load, b, c) minimize Σ(t_all − fit)². Solved via normal equations X^T X · θ = X^T y, Cramer's method.

### Three modes

| Condition | System size | Result |
|-----------|-------------|--------|
| ≥ `perfMinSamples = 3` observations; at least one `loaded=1`; ≥ 2 clean `loaded=0` | **two-stage** (v0.12.0) | `OK: true`, `TLoadMs` from reload residuals |
| ≥ 3 observations; at least one `loaded=1`; < 2 clean `loaded=0` | 3×3 NNLS (joint, fallback) | `OK: true`, `TLoadMs, KInMsTok, KOutMsTok` |
| ≥ 3 observations; all with `loaded=0` or 3×3 singular | 2×2 (fallback without `t_load`) | `OK: true`, `TLoadMs = 0` |
| < 3 observations | — | `OK: false` |

### Two-stage `t_load` estimation (v0.12.0)

INV-2 keeps a model loaded until its queue drains, so reloads are rare: a steady-state server typically yields a **single** `loaded=1` observation among hundreds of `loaded=0`. A joint 3-variable NNLS determines `t_load` from essentially one residual — high variance — and frequently collapses it to `0` on noise, which surfaced in the TUI as `—` even though load time always physically exists.

When ≥ 2 clean `loaded=0` observations are present, `fitRegression` instead:

1. Fits the token coefficients `k_in`, `k_out` from the clean `loaded=0` points (abundant and uncontaminated by load time) via 2-variable NNLS.
2. Estimates `t_load` as the clamped (≥ 0) mean of the residuals `t_all − k_in·in − k_out·out` over the `loaded=1` observations.

This recovers a stable `t_load` from a single reload. The TUI marks the estimate's confidence via `ServerState.t_load_loaded` (number of reload observations): `< 3` → trailing `*`.

### Public types

```go
type PerfStats struct {
    Samples    int
    Loaded     int     // number of reload observations
    TLoadMs    float64 // estimated t_load (0 if no data)
    KInMsTok   float64 // ms/tok for prompt
    KOutMsTok  float64 // ms/tok for completion
    OK         bool
}

type ModelSummary struct {
    Model string
    Stats PerfStats
}
```

Method `ServerSummary(server string) []ModelSummary` returns all models for a server, sorted by `Samples DESC` — used for the server-detail modal in TUI.

In IPC `ServerState`, perf fields (see `API.md` §2.2) are populated via `buildSnapshot`: `tok_in_per_sec = 1000 / KInMsTok` (zeros and negatives → 0), similarly for `tok_out_per_sec`.

## 19. Embedded Web UI

`internal/webui` embeds a small static frontend (`index.html`, `app.js`, `style.css`) via `//go:embed static` and serves it through `webui.Handler()`, mounted at `/ui/*` in `internal/api/server.go` (`GET /ui` redirects to `/ui/`). The handler wraps `http.FileServerFS` with a `cacheControlWriter` that forces `Cache-Control: no-cache` on every response — the binary (and the embedded frontend baked into it) can be replaced by a rebuild at any time, so a browser must not keep `index.html`/`app.js` cached across restarts.

`/ui/*` sits **outside** both the `ClientAuth` and `AdminAuth` middleware groups: the static assets by themselves carry no secrets and no live data. All live state is fetched by the page's own WebSocket connection to `/admin/stream`, authenticated independently (see below). The threat model is identical to the TUI connecting over `ws://` on a LAN: whoever can reach the HTTP port can load the page, but only whoever holds `auth.admin_key` can open the WebSocket and see live state — the static page carries no secrets by design.

### Browser auth channel for `/admin/stream`

The browser's native `WebSocket` API cannot set arbitrary request headers at handshake time (no `Authorization`), so the frontend cannot reuse the TUI's Bearer-token path directly. Instead, `internal/api/auth.go` (`extractAdminKey`) also accepts the key via `Sec-WebSocket-Protocol`, in an entry of the form:

```
Sec-WebSocket-Protocol: proxylm-admin, proxylm-token.<base64url-no-padding(admin_key)>
```

`proxylm-admin` is the "real" subprotocol negotiated by the server (`internal/ipc/server.go`, `websocket.AcceptOptions.Subprotocols`); `proxylm-token.<...>` is never selected as the negotiated subprotocol — it exists purely as a side channel to smuggle the key, relying on RFC 6455's "client offers a list, server picks one or none" subprotocol negotiation. `extractAdminKeyFromSubprotocol` scans every `Sec-WebSocket-Protocol` header value for a `proxylm-token.` prefix and base64url-decodes (no padding) the remainder. If both `Authorization: Bearer` and the subprotocol token are present on the same request, `Authorization` wins. The admin key is never logged in either path. The TUI client is unaffected — it continues to authenticate exclusively via `Authorization: Bearer` and never offers subprotocols.

### Frontend

`internal/webui/static/` is a read-only dashboard mirroring the TUI: a server rack sorted by `priority` (health lamp, current model, queue depth, `SLOW` flag, perf metric), a request table honoring `tui.show_completed_minutes` for TTL-based hiding of completed/failed rows, and detail panes for the selected server (including the per-model perf table with ±CI) or request (including the generation tail). The admin key is entered once through a small auth form and cached in the browser's `localStorage`; the page reconnects to `/admin/stream` automatically with exponential backoff on disconnection, mirroring the TUI's own reconnect logic (FR-48). There is no server-rendering or build step — plain HTML/CSS/JS, matching the project's minimal-dependencies philosophy (§14).

## 20. Open Questions / Deferred

- Prometheus metrics (`/metrics`).
- Native Ollama API endpoints (`/api/generate`).
- Per-client prioritization (quotas).
- Queue persistence across restarts (intentionally deferred).
- Optional CGO-SQLite build (`mattn/go-sqlite3`) under a build tag — for high-throughput installations.
- Auto-update mechanism (downloading new releases from GitHub).
