# ProxyLM.GO — Future Ideas and Tasks

> **FUTURE-RULE.** This file is an idea parking lot, not a work plan. **Nothing here is implemented automatically**: neither Claude Code, nor sub-agents, nor the tech-writer should treat items here as a signal to act. Any implementation requires an **explicit user request** ("implement item N" or equivalent). See also the FUTURE-RULE section in `CLAUDE.md`.
>
> **What the tech-writer is allowed to do:** at each significant release or on request, run a **sweep over every parked item** (not only those obviously implemented in the current release). For each item:
> - grep the repo for the field names, config keys, or function names it mentions;
> - if **fully implemented** — delete the block (description survives in CHANGELOG / SRS / ARCHITECTURE — keeping a stub here would duplicate it and rot);
> - if **partially implemented** — keep the block but prepend `**Уже сделано в vX.Y.Z:**` listing what is already done, and rewrite "Problem"/"Solution" for the remaining scope; delete instead if the remainder lost its motivation;
> - if **not implemented** — leave as-is, optionally refresh wording if the architecture has shifted.
>
> After the sweep — renumber survivors sequentially (1..N, no gaps), update any external cross-references, and append new ideas with the next number in the standard format (name, problem, solution, priority, optionally risks/constraints).
>
> Content is fully read-only for all other roles.

This document records features that did not make it into current releases but arise from accumulated development experience. Each item contains a brief rationale and an indicative priority.

---

## 1. In-memory queue persistence (priority: medium)

**Problem:** when the daemon restarts, all requests in `pending` are lost — clients receive a connection error and must retry on their side. In production installations this can cause significant losses.

**Solution:** when started with `--durable-queue`, persist `pending` in SQLite and restore on the next start. Requests in `running` status at restore time are moved back to `queued` (retry). Implementation requires idempotency guarantees on the client side or an explicit `client_supports_retry` flag.

**Constraint:** queue persistence is intentionally deferred to v0.2+ (U-3 in SRS.md).

---

## 2. Authenticated WS multiplexing with event filtering (priority: medium)

**Problem:** the current `/admin/stream` delivers all events to all connected clients. With multiple simultaneous TUI sessions, backpressure and event drops are possible.

**Solution:** extend the `subscribe` protocol (already partially described in API.md §2.3): the client specifies concrete servers, log levels, and a time range. The server filters the stream before sending, reducing traffic. Add a separate `multiplexer` layer with a per-connection buffer.

---

## 3. Optional CGO SQLite build (priority: low)

**Problem:** `modernc.org/sqlite` (pure-Go) is noticeably slower than `mattn/go-sqlite3` (CGO) under high history throughput.

**Solution:** build tag `sqlite_cgo` switches to `mattn/go-sqlite3` instead of `modernc`; default remains pure-Go. Relevant for high-throughput installations (R-7 in SRS.md §9.2).

---

## 4. Model aliasing / fallback mapping (priority: low)

**Problem:** when a client requests a model not present on any healthy server, the proxy returns `ErrNoServer` / 503. Some clients work with an entire model family (e.g., "any 20B+ instruct") and do not want a 404 when a specific model name is not deployed.

**Solution:** config parameter at the `compat.model_aliases` level (or a separate section) with rules of the form `from: <model> → to: [<candidate1>, <candidate2>, …]`. When a request arrives for a model absent from all healthy servers, the proxy substitutes it with the first available candidate from the list and forwards; both model names (`requested_model` vs `actual_model`) are recorded in logs and history.

**Constraint:** currently (v0.9.3 baseline) the absence of a model returns `ErrNoServer` / 503 — this is a deliberate choice until aliasing is implemented.

---

## 5. 429-aware retry with `Retry-After` and jitter (priority: high)

**Problem:** the retry policy (FR-18..FR-20) does not currently distinguish between 5xx, network errors, and 429 rate-limit responses. For cloud backends (OpenRouter, Groq, Together, OpenAI) this can amplify rate limits — failover to a second cloud backend multiplies the 429 storm rather than waiting it out.

**Solution:**
- classify upstream errors: 4xx (except 429) → no retry; 429 / 503 → respect `Retry-After` (seconds or HTTP-date), capped by `retry.max_backoff_ms`; 5xx / network → exponential backoff as today;
- add jitter (e.g., `±20%`) to backoff intervals to avoid synchronized retry waves;
- optional per-backend circuit breaker: after N consecutive 429s within a window, treat the backend as `unhealthy` for a cool-down period.

**Risks:** small change in retry semantics — existing clients should not regress because today's behavior is "retry on any error", and the new policy is strictly less aggressive on 429.

---

## 6. Per-backend `serialize_by_model` flag (priority: medium)

**Problem:** model-affinity routing (INV-2) serializes requests by model on each server to prevent VRAM swaps. For single-model backends (LM Studio, Ollama, vLLM, llama.cpp) this is essential. For multi-model cloud backends (OpenRouter, Groq, Together — all models available simultaneously, no VRAM swap), it is pure overhead: requests for different models that could be served in parallel are queued sequentially.

**Solution:** per-backend config flag `serialize_by_model: auto | true | false` (default `auto`):
- `true` — keep current INV-2 behavior (single-model backend);
- `false` — disable per-model serialization for this backend; requests dispatched as soon as the worker is free, regardless of model;
- `auto` — heuristic: if `/v1/models` returns ≤ 2 models, behave as `true`; if ≥ 10, as `false`; otherwise log a hint and default to `true`.

**Risks:** complicates the scheduler's worker loop; needs careful test coverage so single-model behavior is unaffected.

---

## 7. Shadow streaming for the generation tail (priority: low/medium)

**Problem:** the "generation tail" feature (hotkey `t`, introduced in v0.13.0) shows the last generated tokens only for requests that the **client** sent in streaming mode (`"stream": true`). When a client sends `stream: false`, the proxy takes the non-streaming path (`runNonStream`): it receives the entire response from the backend as a single chunk at the end, with no intermediate chunks — and the TUI displays `non-streaming request — no live tail`. This means the observability of live generation depends on client behaviour, not on backend capability. Local servers (LM Studio, Ollama, llama.cpp) fully support streaming regardless of how the client asks.

**Solution:** optionally (config flag, default off), for requests that arrive as non-streaming, the proxy executes "shadow streaming": it forwards the request to the backend with `stream: true`, feeds incoming SSE chunks to `core.LiveTail` as in the current streaming path, but returns to the **client** a single assembled non-streaming JSON response at the end — not a chunked stream. The tail then works for any request regardless of client behaviour.

**Risks/constraints:**
- Behavioural change to the response path; must be strictly opt-in via config.
- Requires correct reassembly of the full response from deltas: accumulating `content`, `tool_calls` / `function_call` fragments, aggregating `usage` (prompt/completion tokens), reconciling `finish_reason` / `stop_reason`.
- Cross-protocol translation (OpenAI ↔ Anthropic) — both SSE formats must be handled.
- Client cancellation mid-stream must abort the backend connection without leaking goroutines.
- INV-6 interaction: INV-6 prohibits retry/failover after the first SSE chunk has been sent to the client. In shadow mode no chunks are forwarded to the client, so retry and failover remain permitted up to the final response assembly — this must be explicitly documented and enforced in the implementation.
- Embeddings are not streamed by backends and are out of scope.
