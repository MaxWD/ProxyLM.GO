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

## 2. Ridge regression / regularization (priority: medium)

**Problem:** with few observations, or when all `loaded=1` (or all `loaded=0`), the matrix `X^T X` may be nearly singular, leading to numerically unstable estimates. The current fallback is 2×2 without diagnostics.

**Solution:** add ridge regularization: `(X^T X + λI) · θ = X^T y`, where λ is a small constant (e.g., `1e-4`). Compute the R² quality metric and publish it in the TUI server-detail modal as a `degraded fit` indicator when R² is below threshold.

**User-visible effect:** the operator sees "fit: good / degraded" in the modal and understands estimate reliability.

---

## 3. Per-endpoint statistics (priority: low)

**Problem:** `PerfTracker` aggregates by `(server, model)` without accounting for request type. `POST /v1/chat/completions` and `POST /v1/embeddings` have fundamentally different token/time profiles; mixing distorts the regression.

**Solution:** add an `endpoint` dimension to the observation key: `(server, model, endpoint)`. A separate `ModelSummary` per endpoint; in the modal — tabs or separate rows.

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

## 7. Confidence interval and R² in server-detail modal (priority: low)

**Problem:** the server-detail modal shows only a point estimate for `t_load / tok/s`. The user does not know how reliable the estimate is (few data points, high variance).

**Solution:** compute R² (coefficient of determination) and a 95% confidence interval for each parameter based on residuals. Show in the modal next to the estimate: `38.5 tok/s [±5.1]  R²=0.94`.

---

## 8. max_consecutive_requests_per_model — starvation protection (priority: medium)

**Problem:** with a continuous stream of requests for model A, other models in the queue will not be served (starvation). This is currently a documented trade-off (R-1 in SRS.md §9.2).

**Solution:** config parameter `scheduler.max_consecutive_per_model` (int, default 0 = disabled): after N consecutive requests for model A the worker forcibly picks the next FIFO request, even if there are more requests for A pending.

---

## 9. Optional CGO SQLite build (priority: low)

**Problem:** `modernc.org/sqlite` (pure-Go) is noticeably slower than `mattn/go-sqlite3` (CGO) under high history throughput.

**Solution:** build tag `sqlite_cgo` switches to `mattn/go-sqlite3` instead of `modernc`; default remains pure-Go. Relevant for high-throughput installations (R-7 in SRS.md §9.2).

---

## 10. Model aliasing / fallback mapping (priority: low)

**Problem:** when a client requests a model not present on any healthy server, the proxy returns `ErrNoServer` / 503. Some clients work with an entire model family (e.g., "any 20B+ instruct") and do not want a 404 when a specific model name is not deployed.

**Solution:** config parameter at the `compat.model_aliases` level (or a separate section) with rules of the form `from: <model> → to: [<candidate1>, <candidate2>, …]`. When a request arrives for a model absent from all healthy servers, the proxy substitutes it with the first available candidate from the list and forwards; both model names (`requested_model` vs `actual_model`) are recorded in logs and history.

**Constraint:** currently (v0.9.3 baseline) the absence of a model returns `ErrNoServer` / 503 — this is a deliberate choice until aliasing is implemented.
