# ProxyLM.GO — API Specification

Document version: 0.11.0
Related documents: [`ARCHITECTURE.md`](./ARCHITECTURE.md), [`SRS.md`](./SRS.md)

This document describes three API groups:

1. **External API** — OpenAI-compatible, for client services.
2. **Admin / IPC API** — WebSocket channel for TUI and any administrative clients.
3. **Internal backend calls** — what and how the proxy sends to LM Studio / Ollama.

All data transfer is UTF-8 JSON unless otherwise noted. All timestamps are ISO 8601 in UTC unless otherwise noted.

---

## 1. External API (OpenAI-compatible)

Base prefix: `/v1`. Host and port — from `proxy.host` / `proxy.port` (default `0.0.0.0:8080`).

### 1.1. General requirements

- All endpoints under `/v1/*` require authentication. The key is looked up in `auth.api_keys[].key`. On mismatch — `401`. Two header styles are accepted (in priority order):
  1. `Authorization: Bearer <api_key>` — standard OpenAI style.
  2. `x-api-key: <api_key>` — Anthropic SDK style.
  If both headers are present, `Authorization` takes precedence. This dual-auth support allows Anthropic SDK clients to authenticate the same way they would to the real Anthropic API.
- The header `Content-Type: application/json` is required for POST requests.
- The proxy adds the field `client_name`, corresponding to `auth.api_keys[].name`, to logs and history. The key itself is never logged.
- Unknown request body fields are forwarded to the backend without modification (FR-9).
- The proxy adds the header `X-Request-Id: <uuid>` to all `/v1/*` responses. A middleware generates a UUIDv4 if the client did not supply the header; if the client sends a valid `X-Request-Id` it is reused. The value is set in `w.Header()` before the handler runs.

### 1.2. `POST /v1/chat/completions`

#### Request

| Parameter | HTTP level |
|-----------|------------|
| Method    | `POST`     |
| Path      | `/v1/chat/completions` |
| Headers   | `Authorization: Bearer <key>`, `Content-Type: application/json`, `Accept: application/json` (for non-streaming) or `Accept: text/event-stream` (for streaming) |

JSON body schema (key fields):

| Field          | Type               | Req. | Description |
|----------------|--------------------|------|-------------|
| `model`        | string             | yes  | Model identifier (e.g., `qwen2.5:14b`) |
| `messages`     | array of message   | yes  | Array of `{role: "system" \| "user" \| "assistant" \| "tool", content: string \| array}` |
| `stream`       | boolean            | no   | `true` → SSE response. Default `false`. |
| `max_tokens`   | integer            | no   | Generated token limit |
| `temperature`  | number (0..2)      | no   | |
| `top_p`        | number (0..1)      | no   | |
| `stop`         | string \| string[] | no   | Stop sequences |
| `presence_penalty`  | number        | no   | passthrough |
| `frequency_penalty` | number        | no   | passthrough |
| `tools`        | array              | no   | passthrough (function support — on the backend side) |
| `tool_choice`  | string \| object   | no   | passthrough |
| `response_format` | object          | no   | passthrough or compat normalization (see below) |
| `seed`         | integer            | no   | passthrough |
| `user`         | string             | no   | passthrough |

Example:

```json
{
  "model": "qwen2.5:14b",
  "messages": [
    {"role": "system", "content": "You are an assistant."},
    {"role": "user", "content": "Hello."}
  ],
  "max_tokens": 256,
  "temperature": 0.7,
  "stream": false
}
```

#### Response (non-streaming)

Code `200 OK`. Headers: `Content-Type: application/json`, `X-Request-Id: <uuid>`.

```json
{
  "id": "chatcmpl-<id>",
  "object": "chat.completion",
  "created": 1730000000,
  "model": "qwen2.5:14b",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "Hello! How can I help?"},
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 23,
    "completion_tokens": 7,
    "total_tokens": 30
  }
}
```

The proxy returns the backend body with minimal normalization (the `model` field matches what the client sent in the request).

#### Response (streaming, `stream: true`)

Code `200 OK`. Headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Request-Id: <uuid>`, `Transfer-Encoding: chunked`.

Body — a sequence of SSE chunks. Format:

```
data: {"id":"chatcmpl-<id>","object":"chat.completion.chunk","created":1730000000,"model":"qwen2.5:14b","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-<id>","object":"chat.completion.chunk","created":1730000000,"model":"qwen2.5:14b","choices":[{"index":0,"delta":{"content":"Hell"},"finish_reason":null}]}

data: {"id":"chatcmpl-<id>","object":"chat.completion.chunk","created":1730000000,"model":"qwen2.5:14b","choices":[{"index":0,"delta":{"content":"o!"},"finish_reason":null}]}

data: {"id":"chatcmpl-<id>","object":"chat.completion.chunk","created":1730000000,"model":"qwen2.5:14b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":23,"completion_tokens":7,"total_tokens":30}}

data: [DONE]

```

Each message ends with two `\n`. The final marker is the literal `data: [DONE]`. The proxy:

- Does not buffer the body — forwards chunks as they arrive (via `http.ResponseWriter` + `http.Flusher`).
- Counts `output_tokens` by the number of `delta.content` chunks (or takes `usage` from the last chunk).
- On error **before the first** proxied chunk — may retry (FR-21).
- On error **after the first** proxied chunk — retry is forbidden, see §1.8 (stream_aborted).

#### `response_format` compatibility (`compat.response_format_mode`)

- `passthrough` (default): the field is forwarded to the backend unchanged.
- `normalize_json_object`: if `response_format.type == "json_object"`, the proxy sends an upstream-compatible `response_format` with `type: "json_schema"` and a minimal permissive object schema.
- `strict_reject`: if `response_format.type` is set and is not `json_schema` or `text`, the proxy returns `400 invalid_request` before queuing.

### 1.3. `POST /v1/completions`

The contract is identical to `POST /v1/chat/completions`, but the request body contains `prompt: string | string[]` instead of `messages`. Fields `stream`, `max_tokens`, `temperature`, `top_p`, `stop` are supported. Response — OpenAI format `text_completion` / `text_completion.chunk`.

### 1.4. `POST /v1/embeddings`

#### Request

```json
{
  "model": "nomic-embed-text",
  "input": "string or array of strings"
}
```

| Field       | Type                 | Req. |
|-------------|----------------------|------|
| `model`     | string               | yes  |
| `input`     | string \| string[]   | yes  |
| `encoding_format` | string (`float` \| `base64`) | no |
| `dimensions` | integer             | no   |
| `user`      | string               | no   |

#### Response

```json
{
  "object": "list",
  "data": [
    {"object": "embedding", "index": 0, "embedding": [0.012, -0.043, ...]}
  ],
  "model": "nomic-embed-text",
  "usage": {"prompt_tokens": 5, "total_tokens": 5}
}
```

Streaming for embeddings is not supported.

### 1.5. `POST /v1/messages` (Anthropic Messages API)

#### Request

| Parameter | HTTP level |
|-----------|------------|
| Method    | `POST`     |
| Path      | `/v1/messages` |
| Headers   | `Authorization: Bearer <key>` or `x-api-key: <key>`, `Content-Type: application/json` |

JSON body schema (key fields):

| Field          | Type               | Req. | Description |
|----------------|--------------------|------|-------------|
| `model`        | string             | yes  | Model identifier (e.g., `claude-3-5-sonnet-20241022`) |
| `max_tokens`   | integer            | yes  | Maximum number of tokens to generate |
| `messages`     | array of message   | yes  | Array of `{role: "user" \| "assistant", content: string \| array}` |
| `system`       | string             | no   | System prompt (top-level field, not inside `messages`) |
| `stream`       | boolean            | no   | `true` → SSE response. Default `false`. |
| `temperature`  | number (0..1)      | no   | passthrough |
| `top_p`        | number (0..1)      | no   | passthrough |
| `top_k`        | integer            | no   | passthrough |
| `stop_sequences` | string[]         | no   | passthrough |
| `tools`        | array              | no   | passthrough (Anthropic tool format) |
| `metadata`     | object             | no   | passthrough |
| `thinking`     | object             | no   | Extended thinking config; passthrough only on Anthropic→Anthropic path |

Example:

```json
{
  "model": "claude-3-5-sonnet-20241022",
  "max_tokens": 256,
  "system": "You are a helpful assistant.",
  "messages": [
    {"role": "user", "content": "Hello."}
  ]
}
```

#### Response (non-streaming)

Code `200 OK`. Headers: `Content-Type: application/json`, `X-Request-Id: <uuid>`.

```json
{
  "id": "msg_01XFDUDYJgAACzvnptvVoYEL",
  "type": "message",
  "role": "assistant",
  "content": [
    {"type": "text", "text": "Hello! How can I help you today?"}
  ],
  "model": "claude-3-5-sonnet-20241022",
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": {
    "input_tokens": 12,
    "output_tokens": 10
  }
}
```

#### Response (streaming, `stream: true`)

Code `200 OK`. Headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Request-Id: <uuid>`, `Transfer-Encoding: chunked`.

Body — a sequence of named SSE events. **Anthropic SSE format differs from OpenAI**: each event has an `event:` line before the `data:` line.

```
event: message_start
data: {"type":"message_start","message":{"id":"msg_01...","type":"message","role":"assistant","content":[],"model":"claude-3-5-sonnet-20241022","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"!"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":10}}

event: message_stop
data: {"type":"message_stop"}

```

Each event is separated by a blank line. The stream ends with `event: message_stop`.

#### Cross-protocol translation

The proxy transparently translates between OpenAI and Anthropic wire formats based on the **client endpoint** and **backend `type`** field. See FR-56 for the full compatibility matrix. Translation covers request fields, response fields, and streaming events.

| Client endpoint | Backend type | Translation |
|-----------------|--------------|-------------|
| `/v1/chat/completions` | `openai` | passthrough |
| `/v1/chat/completions` | `anthropic` | OpenAI→Anthropic request; Anthropic→OpenAI response |
| `/v1/messages` | `openai` | Anthropic→OpenAI request; OpenAI→Anthropic response |
| `/v1/messages` | `anthropic` | passthrough |

#### Anthropic error format

Errors from Anthropic backends use a different envelope:

```json
{"type": "error", "error": {"type": "invalid_request_error", "message": "..."}}
```

When translating for an OpenAI client, the proxy converts this to the standard OpenAI error format (see §1.7).

#### Limitations

- `/v1/completions` and `/v1/embeddings` are not translated to Anthropic backends — a `400 invalid_request` is returned if the backend has `type: anthropic`.
- Extended thinking (`thinking`, `budget_tokens`) passes through only on Anthropic client → Anthropic backend. On translation to an OpenAI backend, these fields are silently dropped.

### 1.6. `GET /v1/models`

#### Request

```
GET /v1/models HTTP/1.1
Authorization: Bearer <key>
```

#### Response

```json
{
  "object": "list",
  "data": [
    {"id": "qwen2.5:14b",  "object": "model", "owned_by": "proxylm", "served_by": ["srv1"]},
    {"id": "llama3.1:8b",  "object": "model", "owned_by": "proxylm", "served_by": ["srv1", "srv2"]}
  ]
}
```

The `served_by` field is an extension of the OpenAI schema; clients may ignore it. The list is aggregated from the `ModelMap` of healthy servers (see SRS, FR-7, U-2).

### 1.7. `GET /healthz`

#### Request

```
GET /healthz HTTP/1.1
```

No `Authorization` header required.

#### Response

Code `200 OK`. Body:

```json
{"status": "ok", "version": "0.1.0", "uptime_seconds": 12345}
```

If the daemon is in graceful shutdown mode — `503` with body `{"status": "shutting_down"}`.

### 1.8. Error codes and error body format

Error body — OpenAI-compatible:

```json
{
  "error": {
    "message": "Human-readable description.",
    "type": "invalid_request_error",
    "code": "model_not_found",
    "param": "model"
  }
}
```

Fields `type` and `code` are machine-readable. `param` is present when the error relates to a specific request field.

| HTTP | `code`                  | When                                                                   | Client retry? |
|------|-------------------------|------------------------------------------------------------------------|---------------|
| 400  | `invalid_request`        | Invalid JSON body, missing required field                             | no            |
| 401  | `unauthorized`           | Missing/invalid `Authorization: Bearer`                               | no            |
| 404  | `model_not_found`        | Model absent from all healthy servers                                 | no            |
| 408  | `request_timeout`        | `backends[].timeout_seconds` exceeded                                 | yes           |
| 429  | `rate_limited`           | **Reserved**, not returned in MVP                                     | yes           |
| 500  | `internal_error`         | Internal proxy error (bug)                                            | yes           |
| 502  | `backend_error`          | Backend returned non-2xx, retries exhausted, failover did not help    | yes           |
| 503  | `service_unavailable`    | All servers with this model are `unhealthy`; daemon is shutting down  | yes (with `Retry-After`) |
| 504  | `backend_timeout`        | Cumulative timeout including retries exceeded                         | yes           |

On 503 the proxy adds `Retry-After: 1` (seconds).

#### Streaming error after first chunk (`stream_aborted`)

If the proxy has already sent the client at least one SSE chunk and then receives an error from the backend — it does **not** change the HTTP status code (already 200). Instead:

```
data: {"error":{"message":"Backend connection lost mid-stream","type":"backend_error","code":"stream_aborted"}}

data: [DONE]

```

The client must detect the presence of `error` in an SSE chunk. History record — `failed`.

---

## 2. Admin / IPC API

### 2.1. `GET /admin/stream` — WebSocket

#### Connection

```
GET /admin/stream HTTP/1.1
Host: <host>:<port>
Upgrade: websocket
Connection: Upgrade
Authorization: Bearer <admin_key>
Sec-WebSocket-Version: 13
Sec-WebSocket-Key: ...
```

`<admin_key>` — `auth.admin_key`. Mismatch → `401`, no upgrade.

`/admin/stream` is available on the same listener as `/v1/*` (`proxy.host:proxy.port`).
No separate listener for IPC is provided.

### 2.2. Server → client messages

All messages are JSON frames (`text` opcode). Each frame is an `Envelope`:

```json
{"type": "<type>", "time": "<RFC3339>", "payload": { ... }}
```

#### `state_snapshot`

Sent once immediately after connection, and again in response to a `request_snapshot` frame from the client. Contains the full state.

```json
{
  "type": "state_snapshot",
  "time": "2026-05-09T14:01:02Z",
  "payload": {
    "servers": [
      {
        "name": "srv1",
        "url": "http://127.0.0.1:1234",
        "healthy": true,
        "in_flight": true,
        "current_model": "qwen2.5:14b",
        "queue_depth": 2,
        "models": ["qwen2.5:14b", "llama3.1:8b"],
        "perf_samples": 12,
        "perf_ok": true,
        "t_load_ms": 4200.0,
        "tok_in_per_sec": 0.0,
        "tok_out_per_sec": 38.5,
        "r_squared": 0.94,
        "fit_quality": "good",
        "slow": false,
        "failure_count": 0,
        "per_model_stats": [
          {
            "model": "qwen2.5:14b",
            "endpoint": "/v1/chat/completions",
            "samples": 12,
            "loaded": 3,
            "ok": true,
            "t_load_ms": 4200.0,
            "tok_in_per_sec": 0.0,
            "tok_out_per_sec": 38.5,
            "r_squared": 0.94,
            "fit_quality": "good",
            "t_load_ci": 180.0,
            "k_out_ci": 1.2
          },
          {
            "model": "llama3.1:8b",
            "endpoint": "/v1/chat/completions",
            "samples": 5,
            "loaded": 1,
            "ok": true,
            "t_load_ms": 1100.0,
            "tok_in_per_sec": 0.0,
            "tok_out_per_sec": 72.1,
            "r_squared": 0.62,
            "fit_quality": "degraded",
            "t_load_ci": 950.0,
            "k_out_ci": 4.7
          }
        ]
      }
    ],
    "requests": [
      {
        "id": "0e9c...",
        "client_name": "service-a",
        "model": "qwen2.5:14b",
        "endpoint": "/v1/chat/completions",
        "stream": false,
        "server_name": "srv1",
        "status": "completed",
        "http_status": 200,
        "prompt_tokens": 312,
        "output_tokens": 82,
        "created_at": "2026-05-09T14:01:02Z",
        "started_at": "2026-05-09T14:01:02Z",
        "completed_at": "2026-05-09T14:01:08Z",
        "attempt": 1,
        "max_attempts": 3,
        "queue_wait_ms": 120,
        "model_reloaded": false,
        "last_failed_server": ""
      }
    ]
  }
}
```

##### `ServerState` fields

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | server name from config |
| `url` | string | base URL |
| `healthy` | bool | status from discovery |
| `in_flight` | bool | `true` when a request is currently executing on this server |
| `current_model` | string | loaded / in-flight model; empty string if none |
| `queue_depth` | int | number of requests in `pending` |
| `models` | string[] | known models of the server |
| `perf_samples` | int | number of observations in the regression for `current_model`; omitted if 0 |
| `perf_ok` | bool | `true` if regression is computed (≥ 3 observations, `X^T X` is non-singular); omitted if false |
| `t_load_ms` | float64 | estimated model load time (ms); omitted if 0 |
| `tok_in_per_sec` | float64 | prompt-token throughput (tok/s); omitted if 0 |
| `tok_out_per_sec` | float64 | completion-token throughput (tok/s); omitted if 0 |
| `r_squared` | float64 | coefficient of determination R² of header-level regression, [0,1]; omitted if 0 (no fit) |
| `fit_quality` | string | qualitative grade: `"good"` (R² ≥ 0.70), `"degraded"` (<0.70), `""` if no fit |
| `slow` | bool | last completed request took ≥ 2× the regression estimate; omitted if false |
| `failure_count` | int64 | total failed attempts on this server since daemon start; omitted if 0 |
| `per_model_stats` | `ModelStats[]` | per-(model, endpoint) statistics, sorted by `samples DESC`; omitted if empty |

##### `ModelStats` structure

Tracks a single `(model, endpoint)` bucket on this server. Endpoint became part of the regression key in v0.10.0 so that request types with fundamentally different token/time profiles (e.g. `/v1/chat/completions` vs `/v1/embeddings`) don't distort the fit.

| Field | Type | Description |
|-------|------|-------------|
| `model` | string | model identifier |
| `endpoint` | string | request path (e.g. `/v1/chat/completions`); omitted if empty (legacy data prior to v0.10.0 migration) |
| `samples` | int | total number of observations for the (server, model, endpoint) key |
| `loaded` | int | of those — with the model_reload flag (model switch before request) |
| `ok` | bool | regression is valid |
| `t_load_ms` | float64 | estimated load time (ms); omitted if 0 |
| `tok_in_per_sec` | float64 | prompt-token throughput (tok/s); omitted if 0 |
| `tok_out_per_sec` | float64 | completion-token throughput (tok/s); omitted if 0 |
| `r_squared` | float64 | coefficient of determination R², [0,1]; omitted if 0 |
| `fit_quality` | string | `"good"` / `"degraded"` / `""` (no fit) |
| `t_load_ci` | float64 | half-width of 95% confidence interval for `t_load_ms`; omitted if 0 (insufficient data or coefficient pinned at 0 by NNLS) |
| `k_in_ci` | float64 | half-width of 95% CI for the input-token coefficient `k_in_ms_tok` (note: the published value is `tok_in_per_sec = 1000/k_in`; the CI is in `ms/tok` units, the client may convert) |
| `k_out_ci` | float64 | half-width of 95% CI for the output-token coefficient `k_out_ms_tok` |

##### `RequestState` fields

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | request UUID |
| `client_name` | string | name from `auth.api_keys` |
| `model` | string | model from the request body |
| `endpoint` | string | request path (e.g. `/v1/chat/completions`) |
| `stream` | bool | `true` if streaming request |
| `server_name` | string | assigned server name |
| `status` | string | `queued` / `running` / `completed` / `failed` |
| `http_status` | int | HTTP status code from the backend (0 if not yet dispatched) |
| `prompt_tokens` | int | tokens in the prompt |
| `output_tokens` | int | generated tokens |
| `created_at` | string (RFC3339) | moment of entry into the HTTP handler |
| `started_at` | string (RFC3339) | moment of first dispatch; omitted if not yet dispatched |
| `completed_at` | string (RFC3339) | moment of finalization; omitted if not yet completed |
| `attempt` | int | 1-based index of the last (or current) attempt; 0 if not yet dispatched |
| `max_attempts` | int | configured `retry.max_attempts` |
| `queue_wait_ms` | int64 | milliseconds between `created_at` and `started_at`; 0 if still queued |
| `model_reloaded` | bool | `true` if a model switch occurred at dispatch time; omitted if false |
| `last_failed_server` | string | name of the server where the previous attempt failed; empty if no failed attempt yet; omitted if empty |
| `error_message` | string | last error message for `failed` requests; omitted if empty |

### 2.3. Client → server messages

#### `request_snapshot`

The TUI sends this frame to request an immediate snapshot from the daemon. Used when the user presses `F5`.

```json
{"type": "request_snapshot", "time": "2026-05-09T14:03:00Z"}
```

No `payload` is required. The daemon responds with a regular `state_snapshot` message (same as on connect).

### 2.4. Connection close

| Code | Reason |
|------|--------|
| 1000 | Normal close (client sent close) |
| 1008 | Protocol violation / invalid JSON |
| 1011 | Internal server error |
| 4401 | Auth failure during connection (along with HTTP 401 before upgrade) |

---

## 3. Internal Backend Calls

### 3.1. General rules

- The proxy uses `*http.Client` (`net/http`, stdlib) with `Timeout = backends[].timeout_seconds` (default 600 seconds). Streaming responses are read via `resp.Body` (`io.ReadCloser`) line-by-line (`bufio.Scanner` or `bufio.Reader.ReadBytes('\n')`).
- Base URL — `backends[].url`.
- If `backends[].api_key` is not `null`, `Authorization: Bearer <api_key>` is added (individual per backend). This header has **no relation** to the proxy client's key.
- Header `User-Agent: proxylm/<version>` (where `<version>` comes from `runtime/debug.BuildInfo` or `-ldflags -X main.version=...`).
- A separate `*http.Client` with a configured `*http.Transport` (keep-alive, `MaxIdleConnsPerHost`, overall timeout) is created for each backend.

### 3.2. Paths used

**OpenAI-protocol backends (`type: openai` or `type: ollama`):**

| Purpose                   | Method | Path                       | Body                                    |
|---------------------------|--------|----------------------------|-----------------------------------------|
| Discovery                 | GET    | `/v1/models`               | —                                       |
| Chat completion (regular) | POST   | `/v1/chat/completions`     | client body or translated from Anthropic; `response_format` may be normalized per `compat.response_format_mode` |
| Chat completion (stream)  | POST   | `/v1/chat/completions`     | body + `stream: true`; response via `resp.Body` (chunked reading) |
| Text completion           | POST   | `/v1/completions`          | passthrough (OpenAI clients only; not routed to Anthropic backends) |
| Embeddings                | POST   | `/v1/embeddings`           | passthrough (OpenAI clients only; not routed to Anthropic backends) |

**Anthropic-protocol backends (`type: anthropic`):**

| Purpose                      | Method | Path              | Body                                                                 |
|------------------------------|--------|-------------------|----------------------------------------------------------------------|
| Messages (regular)           | POST   | `/v1/messages`    | client body or translated from OpenAI `chat/completions` format      |
| Messages (stream)            | POST   | `/v1/messages`    | body + `stream: true`; response uses Anthropic SSE event format      |

### 3.3. Backend compatibility notes

ProxyLM.GO is **backend-agnostic**: any server exposing an OpenAI-compatible `/v1/*` API works without per-backend code. The proxy is transparent — it forwards the JSON body and headers (minus `Authorization`, which is injected from `backends[].api_key`) and reads the response stream-by-stream.

Tested / commonly-used backends:

| Backend                      | Default URL                          | Authentication        | Notes                                                                                    |
|------------------------------|--------------------------------------|-----------------------|------------------------------------------------------------------------------------------|
| **LM Studio** (local)        | `http://<host>:1234`                 | optional Bearer       | Single-model server (one model loaded at a time). Triggers INV-2 model affinity.         |
| **Ollama** (local)           | `http://<host>:11434`                | none by default       | OpenAI-compatible shim at `/v1/*` (native `/api/*` is out of scope — see SRS §8). Single-model semantics; `keep_alive` controlled via `OLLAMA_KEEP_ALIVE` env on the Ollama side, not via this proxy. Returns `404` when `model` is unknown — mapped to standard proxy error. |
| **vLLM** (local/cluster)     | `http://<host>:8000`                 | optional Bearer       | Single-model server. Full OpenAI surface (incl. `logprobs`).                              |
| **llama.cpp** (`./server`)   | `http://<host>:8080`                 | optional Bearer       | Single-model. Minimal OpenAI shim.                                                       |
| **OpenAI** (cloud)           | `https://api.openai.com`             | Bearer (`sk-...`)     | Multi-model; INV-2 model affinity does not provide benefit but is not harmful. Cost-bearing — use `priority` to keep cloud as fallback. |
| **OpenRouter** (cloud)       | `https://openrouter.ai/api`          | Bearer (`sk-or-v1-`)  | Multi-model (hundreds of models in `/v1/models`); see *Known limitations* below.         |
| **Groq / Together / Fireworks** (cloud) | provider-specific          | Bearer                | Multi-model; same semantics as OpenRouter.                                               |
| **Anthropic** (cloud)        | `https://api.anthropic.com`          | Bearer or `x-api-key` | `type: anthropic` required. Discovery is not performed (Anthropic has no `/v1/models` endpoint); set `backends[].models` explicitly. |

Configuration example for an Anthropic backend:

```yaml
- name: anthropic-claude
  url: https://api.anthropic.com
  type: anthropic
  api_key: sk-ant-...
  timeout_seconds: 600
  priority: 10
  models:
    - claude-3-5-sonnet-20241022
    - claude-3-haiku-20240307
```

Configuration for OpenAI-protocol backends:

```yaml
- name: <descriptive-name>
  url: <base-url-without-/v1>
  api_key: <token-or-null>
  timeout_seconds: <seconds>
  priority: <int, lower = preferred>
```

The `backends[].type` field controls the wire protocol used when communicating with this backend. Accepted values: `openai` (default, also covers `ollama`), `anthropic`. When `type: anthropic`, the proxy sends Anthropic Messages API requests to the backend and translates client requests/responses as needed.

**Known limitations for cloud backends in v0.11.x:**

- The retry policy (FR-18..FR-20) does not yet parse `Retry-After` headers; under heavy 429 from a cloud provider it can amplify rate limits. Track via [`docs/FUTURE.md`](./FUTURE.md) item *429-aware retry*.
- Discovery polls `/v1/models` every `discovery.interval_seconds` for **all** backends; an OpenRouter backend (~300 models) populates the proxy's model table with all of them. Recommended workaround: set `models:` explicitly per backend to restrict discovery to the models you actually use.
- Model-affinity routing (INV-2) sequences requests by model on each server. For multi-model cloud backends this is a no-op overhead rather than a benefit; a per-backend `serialize_by_model` flag is on the roadmap (see FUTURE.md).

### 3.4. Response handling

- 2xx — forward to client (with possible `model` field normalization).
- 4xx (except 429) — pass through to client without retry (FR-23).
- 5xx, network reset, timeout — retry per policy (FR-18 ÷ FR-20).

---

## 4. curl Examples

Set environment variables:

```bash
export PROXY=http://localhost:8080
export KEY=sk-proxy-replace-me-aaaaa
export ADMIN=sk-admin-replace-me
```

### 4.1. Chat completion (non-streaming)

```bash
curl -s "$PROXY/v1/chat/completions" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2.5:14b",
    "messages": [{"role": "user", "content": "Hello."}],
    "max_tokens": 64
  }'
```

### 4.2. Chat completion (streaming)

```bash
curl -N "$PROXY/v1/chat/completions" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d '{
    "model": "qwen2.5:14b",
    "messages": [{"role": "user", "content": "Tell me a joke."}],
    "stream": true
  }'
```

`-N` disables curl buffering — otherwise SSE chunks will not appear in real time.

### 4.3. Completions

```bash
curl -s "$PROXY/v1/completions" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model": "qwen2.5:14b", "prompt": "Once upon a time", "max_tokens": 32}'
```

### 4.4. Embeddings

```bash
curl -s "$PROXY/v1/embeddings" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model": "nomic-embed-text", "input": "Text to embed"}'
```

### 4.5. List models

```bash
curl -s "$PROXY/v1/models" -H "Authorization: Bearer $KEY"
```

### 4.6. Health check

```bash
curl -s "$PROXY/healthz"
```

### 4.7. Anthropic Messages API (non-streaming)

```bash
curl -s "$PROXY/v1/messages" \
  -H "x-api-key: $KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 64,
    "system": "You are a helpful assistant.",
    "messages": [{"role": "user", "content": "Hello."}]
  }'
```

`x-api-key` and `Authorization: Bearer` are both accepted.

### 4.8. Anthropic Messages API (streaming)

```bash
curl -N "$PROXY/v1/messages" \
  -H "x-api-key: $KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 64,
    "messages": [{"role": "user", "content": "Tell me a joke."}],
    "stream": true
  }'
```

### 4.9. Connect to `/admin/stream` (via `websocat`)

```bash
websocat -H="Authorization: Bearer $ADMIN" "ws://localhost:8080/admin/stream"
```

After connecting, `state_snapshot` arrives immediately. Press `F5` in the TUI (or send `{"type":"request_snapshot","time":"..."}`) to request a fresh snapshot at any time.
