# ProxyLM.GO — Future Ideas and Tasks

> **FUTURE-RULE.** This file is an idea parking lot, not a work plan. **Nothing here is implemented automatically**: neither Claude Code, nor sub-agents, nor the tech-writer should treat items here as a signal to act. Any implementation requires an **explicit user request** ("implement item N" or equivalent). See also the FUTURE-RULE section in `CLAUDE.md`.
>
> **What the tech-writer is allowed to do:** at each significant release or on request, revise this file — remove already-implemented items, add new ideas that emerged during work, maintain a consistent format (name, problem, solution, priority, optionally risks/constraints). Content is fully read-only for all other roles.

This document records features that did not make it into current releases but arise from accumulated development experience. Each item contains a brief rationale and an indicative priority.

---

## 1. Persistence of perf statistics (priority: high)

**Problem:** `PerfTracker` keeps all observations `(server, model)` in RAM. When the daemon restarts, the history is wiped; regression restarts from scratch — the first 2–3 requests yield no metrics.

**Solution:** persist `[]perfObservation` in SQLite (new table `perf_observations`), load on startup. Limit storage size (e.g., 1000 observations per pair, FIFO).

**Risks:** small overhead per completed request; migration 0003 required.

---

## 2. Ridge regression / regularization (priority: medium) — DONE in v0.10.0

Implemented as `(X^T X + λI)θ = X^T y` with λ = 1e-4 across all 1/2/3-var solvers, plus R² and 95% confidence intervals published in `PerfStats` (`RSquared`, `TLoadCI`, `KInCI`, `KOutCI`, `FitQuality ∈ {"good","degraded",""}`). The TUI highlights degraded rows in the server-detail Info pane. See `internal/core/perf.go`.

---

## 3. Per-endpoint statistics (priority: low) — DONE in v0.10.0

The regression key is now `(server, model, endpoint)`. Endpoint is propagated from `RequestRecord.Endpoint` into `core.Job.Endpoint` → `Scheduler.recordPerf` → `PerfTracker.Record`. `ServerSummary` returns one `ModelSummary` per `(model, endpoint)` bucket; `ipc.ModelStats` exposes the new `Endpoint` field. The TUI server-detail table shows a `Endpoint` column.

---

## 4. In-memory queue persistence (priority: medium)

**Problem:** when the daemon restarts, all requests in `pending` are lost — clients receive a connection error and must retry on their side. In production installations this can cause significant losses.

**Solution:** when started with `--durable-queue`, persist `pending` in SQLite and restore on the next start. Requests in `running` status at restore time are moved back to `queued` (retry). Implementation requires idempotency guarantees on the client side or an explicit `client_supports_retry` flag.

**Constraint:** queue persistence is intentionally deferred to v0.2+ (U-3 in SRS.md).

---

## 5. Web UI (priority: low)

**Problem:** TUI requires a terminal; when monitoring remotely via a browser, TUI is inconvenient.

**Solution:** a minimalist Web UI on the same WebSocket (`/admin/stream`), with no frontend dependencies (vanilla JS + EventSource or WebSocket). Table view of requests and server header bar — analogous to TUI.

**Constraint:** explicitly out of scope for MVP (SRS §8).

---

## 6. Authenticated WS multiplexing with event filtering (priority: medium)

**Problem:** the current `/admin/stream` delivers all events to all connected clients. With multiple simultaneous TUI sessions, backpressure and event drops are possible.

**Solution:** extend the `subscribe` protocol (already partially described in API.md §2.3): the client specifies concrete servers, log levels, and a time range. The server filters the stream before sending, reducing traffic. Add a separate `multiplexer` layer with a per-connection buffer.

---

## 7. Confidence interval and R² in server-detail modal (priority: low) — DONE in v0.10.0

R² and 95% CI half-widths are computed in `fillFitQuality` using σ̂² · diag((X^T X + λI)^-1) and published as `t_load_ci`, `k_in_ci`, `k_out_ci` in `ModelStats`. The TUI server-detail Info pane renders `↓tok/s` and `↑tok/s` columns as `38.5±5.1` when CI is available; the `R²` column shows the coefficient; degraded rows (R² < 0.70) are flash-highlighted.

---

## 8. max_consecutive_requests_per_model — starvation protection (priority: medium) — DONE in v0.10.0

Implemented as a **new routing strategy** `fair_share_round_robin` (instead of altering existing strategies). When `scheduler.max_consecutive_per_model > 0` and the server reaches the limit, `pool.PopForFairShare` forcibly picks the next FIFO Job for a different model. Falls back to ordinary drain if no other compatible model is queued. See `internal/core/pool.go`, `internal/core/scheduler.go`, and the strategy section in `docs/ARCHITECTURE.md`.

---

## 9. Optional CGO SQLite build (priority: low)

**Problem:** `modernc.org/sqlite` (pure-Go) is noticeably slower than `mattn/go-sqlite3` (CGO) under high history throughput.

**Solution:** build tag `sqlite_cgo` switches to `mattn/go-sqlite3` instead of `modernc`; default remains pure-Go. Relevant for high-throughput installations (R-7 in SRS.md §9.2).

---

## 10. Model aliasing / fallback mapping (priority: low)

**Problem:** when a client requests a model not present on any healthy server, the proxy returns `ErrNoServer` / 503. Some clients work with an entire model family (e.g., "any 20B+ instruct") and do not want a 404 when a specific model name is not deployed.

**Solution:** config parameter at the `compat.model_aliases` level (or a separate section) with rules of the form `from: <model> → to: [<candidate1>, <candidate2>, …]`. When a request arrives for a model absent from all healthy servers, the proxy substitutes it with the first available candidate from the list and forwards; both model names (`requested_model` vs `actual_model`) are recorded in logs and history.

**Constraint:** currently (v0.9.3 baseline) the absence of a model returns `ErrNoServer` / 503 — this is a deliberate choice until aliasing is implemented.

---

## 11. Preflight `/v1/models` shape validation (priority: high)

**Problem:** the proxy is backend-agnostic and accepts any URL in `backends[].url`. If a user accidentally points it at a non-OpenAI endpoint (Anthropic `/v1/messages`, Azure with `api-version`, raw Ollama `/api/generate`), the misconfiguration surfaces only on the first client request as a 404/500 with no diagnostic guidance.

**Solution:** at startup, after the initial healthcheck, issue a single `GET /v1/models` and validate the response shape (`data: []` with objects having `id: string`). If the shape is wrong, mark the server `unhealthy` and emit a WARN log explaining that the endpoint is not OpenAI-compatible. The proxy stays up — the rest of the configured servers continue to work.

**Risks:** adds one extra HTTP call per backend at startup; trivial cost.

---

## 12. 429-aware retry with `Retry-After` and jitter (priority: high)

**Problem:** the retry policy (FR-18..FR-20) does not currently distinguish between 5xx, network errors, and 429 rate-limit responses. For cloud backends (OpenRouter, Groq, Together, OpenAI) this can amplify rate limits — failover to a second cloud backend multiplies the 429 storm rather than waiting it out.

**Solution:**
- classify upstream errors: 4xx (except 429) → no retry; 429 / 503 → respect `Retry-After` (seconds or HTTP-date), capped by `retry.max_backoff_ms`; 5xx / network → exponential backoff as today;
- add jitter (e.g., `±20%`) to backoff intervals to avoid synchronized retry waves;
- optional per-backend circuit breaker: after N consecutive 429s within a window, treat the backend as `unhealthy` for a cool-down period.

**Risks:** small change in retry semantics — existing clients should not regress because today's behavior is "retry on any error", and the new policy is strictly less aggressive on 429.

---

## 13. Per-backend `serialize_by_model` flag (priority: medium)

**Problem:** model-affinity routing (INV-2) serializes requests by model on each server to prevent VRAM swaps. For single-model backends (LM Studio, Ollama, vLLM, llama.cpp) this is essential. For multi-model cloud backends (OpenRouter, Groq, Together — all models available simultaneously, no VRAM swap), it is pure overhead: requests for different models that could be served in parallel are queued sequentially.

**Solution:** per-backend config flag `serialize_by_model: auto | true | false` (default `auto`):
- `true` — keep current INV-2 behavior (single-model backend);
- `false` — disable per-model serialization for this backend; requests dispatched as soon as the worker is free, regardless of model;
- `auto` — heuristic: if `/v1/models` returns ≤ 2 models, behave as `true`; if ≥ 10, as `false`; otherwise log a hint and default to `true`.

**Risks:** complicates the scheduler's worker loop; needs careful test coverage so single-model behavior is unaffected.
