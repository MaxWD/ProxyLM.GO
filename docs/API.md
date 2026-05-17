# ProxyLM.GO — API Specification

Document version: 0.7.0
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

- All endpoints under `/v1/*` require the header `Authorization: Bearer <api_key>`. The key is looked up in `auth.api_keys[].key`. On mismatch — `401`.
- The header `Content-Type: application/json` is required for POST requests.
- The proxy adds the field `client_name`, corresponding to `auth.api_keys[].name`, to logs and history. The key itself is never logged.
- Unknown request body fields are forwarded to the backend without modification (FR-9).
- The proxy adds the header `X-Request-Id: <uuid>` to all responses (the same value as in `RequestRecord.request_id`).

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
- On error **after the first** proxied chunk — retry is forbidden, see §1.6 (stream_aborted).

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

### 1.5. `GET /v1/models`

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

### 1.6. `GET /healthz`

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

### 1.7. Error codes and error body format

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

All messages are JSON frames (`text` opcode), in the format `{"type": "<type>", ...}`.

#### `state_snapshot`

Sent once immediately after connection. Contains the full state.

```json
{
  "type": "state_snapshot",
  "timestamp": "2026-05-09T14:01:02Z",
  "version": "0.7.0",
  "servers": [
    {
      "name": "srv1",
      "url": "http://127.0.0.1:1234",
      "healthy": true,
      "current_model": "qwen2.5:14b",
      "queue_size": 2,
      "models": ["qwen2.5:14b", "llama3.1:8b"],
      "perf_samples": 12,
      "perf_ok": true,
      "t_load_ms": 4200.0,
      "tok_in_per_sec": 0.0,
      "tok_out_per_sec": 38.5,
      "per_model_stats": [
        {
          "model": "qwen2.5:14b",
          "samples": 12,
          "loaded": 3,
          "ok": true,
          "t_load_ms": 4200.0,
          "tok_in_per_sec": 0.0,
          "tok_out_per_sec": 38.5
        },
        {
          "model": "llama3.1:8b",
          "samples": 5,
          "loaded": 1,
          "ok": true,
          "t_load_ms": 1100.0,
          "tok_in_per_sec": 0.0,
          "tok_out_per_sec": 72.1
        }
      ]
    }
  ],
  "requests": [
    {
      "request_id": "0e9c...",
      "client_name": "service-a",
      "model": "qwen2.5:14b",
      "server": "srv1",
      "status": "completed",
      "received_at": "2026-05-09T14:01:02Z",
      "started_at": "2026-05-09T14:01:02Z",
      "first_chunk_at": null,
      "completed_at": "2026-05-09T14:01:08Z",
      "queue_wait_ms": 120,
      "duration_ms": 6300,
      "server_proc_ms": 6300,
      "ttft_ms": null,
      "input_tokens": 312,
      "output_tokens": 82,
      "attempts": 1,
      "error": null,
      "stream": false,
      "model_reloaded": false
    }
  ],
  "stats": {
    "queued": 0,
    "running": 1,
    "completed_30m": 17,
    "failed_30m": 1
  }
}
```

##### `ServerState` fields (v0.7.0)

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | server name from config |
| `url` | string | base URL |
| `healthy` | bool | status from discovery |
| `current_model` | string \| `""` | loaded / in-flight model |
| `queue_size` | int | number of requests in `pending` |
| `models` | string[] | known models of the server |
| `perf_samples` | int | number of observations in the regression for `current_model` |
| `perf_ok` | bool | `true` if regression is computed (≥ 3 observations, `X^T X` is non-singular) |
| `t_load_ms` | float64 | estimated model load time (ms); 0 if no reload observations or `perf_ok=false` |
| `tok_in_per_sec` | float64 | prompt-token throughput (tok/s); 0 if no data |
| `tok_out_per_sec` | float64 | completion-token throughput (tok/s); 0 if no data |
| `per_model_stats` | `ModelStats[]` | statistics for all models of the server, sorted by `samples DESC` |

> Fields `tokens_per_sec` and `ttft_ms` **removed in v0.7.0**. Replaced by: `tok_out_per_sec` and `t_load_ms` from regression.

##### `ModelStats` structure

| Field | Type | Description |
|-------|------|-------------|
| `model` | string | model identifier |
| `samples` | int | total number of observations for the (server, model) pair |
| `loaded` | int | of those — with the model_reload flag (model switch before request) |
| `ok` | bool | regression is valid |
| `t_load_ms` | float64 | estimated load time (ms); 0 if no data |
| `tok_in_per_sec` | float64 | prompt-token throughput (tok/s) |
| `tok_out_per_sec` | float64 | completion-token throughput (tok/s) |

##### `RequestState` fields

Additional field added in v0.7.0:

| Field | Type | Description |
|-------|------|-------------|
| `model_reloaded` | bool | `true` if at the start of dispatching this request `current_model` on the server differed from the requested model (i.e., a model switch occurred) |

#### `state_diff`

Sent when changes occur. Contains partial updates.

```json
{
  "type": "state_diff",
  "timestamp": "2026-05-09T14:02:11Z",
  "servers": [
    {"name": "srv1", "current_model": "llama3.1:8b", "queue_size": 1}
  ],
  "requests_upserted": [
    {
      "request_id": "1a2b...",
      "client_name": "service-b",
      "model": "llama3.1:8b",
      "server": "srv1",
      "status": "running",
      "received_at": "2026-05-09T14:02:11Z",
      "started_at": "2026-05-09T14:02:11Z",
      "queue_wait_ms": 95,
      "duration_ms": null,
      "server_proc_ms": null,
      "input_tokens": null,
      "output_tokens": null,
      "error": null,
      "attempts": 1,
      "model_reloaded": true
    }
  ],
  "requests_removed": []
}
```

`requests_removed` contains an array of `request_id` values the client should remove from the table (e.g., after `tui.show_completed_minutes`).
`requests_upserted` uses the same request field format as `state_snapshot.requests`; partial updates are allowed.

#### `log_line`

Push of a single log line.

```json
{
  "type": "log_line",
  "timestamp": "2026-05-09T14:02:11Z",
  "level": "INFO",
  "logger": "router",
  "event": "chose srv2 (model loaded, queue=0)",
  "request_id": "1a2b...",
  "context": {"model": "llama3.1:8b", "client": "service-b"}
}
```

### 2.3. Client → server messages

#### `subscribe`

Optional subscription to a subset of events. If not sent — the client receives everything.

```json
{
  "type": "subscribe",
  "channels": ["state", "log"],
  "filter": {"levels": ["INFO", "WARNING", "ERROR"]}
}
```

#### `ping`

```json
{"type": "ping", "timestamp": "2026-05-09T14:03:00Z"}
```

Response: `{"type": "pong", "timestamp": "..."}`. The server itself sends `ping` every 30 s when idle.

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

| Purpose                   | Method | Path                       | Body                                    |
|---------------------------|--------|----------------------------|-----------------------------------------|
| Discovery                 | GET    | `/v1/models`               | —                                       |
| Chat completion (regular) | POST   | `/v1/chat/completions`     | client body; `response_format` may be normalized per `compat.response_format_mode` |
| Chat completion (stream)  | POST   | `/v1/chat/completions`     | body + `stream: true`; `response_format` may be normalized; response via `resp.Body` (chunked reading) |
| Text completion           | POST   | `/v1/completions`          | passthrough                             |
| Embeddings                | POST   | `/v1/embeddings`           | passthrough                             |

### 3.3. LM Studio / Ollama specifics

- LM Studio: OpenAI-compatible API at `http://<host>:1234/v1/*`.
- Ollama: OpenAI shim at `http://<host>:11434/v1/*` (native `/api/*` — out of scope for MVP).
- Ollama behavior when `model: "..."` does not exist: 404 — mapped to the standard proxy error path.

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

### 4.7. Connect to `/admin/stream` (via `websocat`)

```bash
websocat -H="Authorization: Bearer $ADMIN" "ws://localhost:8080/admin/stream"
```

After connecting, `state_snapshot` arrives immediately, followed by `state_diff` and `log_line` as events occur.
