# ProxyLM.GO — API Specification

Версия документа: 0.7.0
Связанные документы: [`ARCHITECTURE.md`](./ARCHITECTURE.md), [`SRS.md`](./SRS.md)

Документ описывает три группы API:

1. **Внешний API** — OpenAI-совместимый, для клиентских сервисов.
2. **Admin / IPC API** — WebSocket-канал для TUI и любых административных клиентов.
3. **Внутренние backend-вызовы** — что и как прокси шлёт на LM Studio / Ollama.

Вся передача — UTF-8 JSON, если не указано иное. Все timestamps — ISO 8601 в UTC, если не указано иное.

---

## 1. Внешний API (OpenAI-совместимый)

Базовый префикс: `/v1`. Хост и порт — из `proxy.host` / `proxy.port` (default `0.0.0.0:8080`).

### 1.1. Общие требования

- Все эндпоинты под `/v1/*` требуют заголовок `Authorization: Bearer <api_key>`. Ключ ищется в `auth.api_keys[].key`. При несовпадении — `401`.
- Заголовок `Content-Type: application/json` обязателен для POST-запросов.
- Прокси добавляет в логи и историю поле `client_name`, соответствующее `auth.api_keys[].name`. Сам ключ нигде не логируется.
- Неизвестные поля тела запроса передаются на бэкенд без модификации (FR-9).
- На все ответы прокси добавляет заголовок `X-Request-Id: <uuid>` (тот же, что в `RequestRecord.request_id`).

### 1.2. `POST /v1/chat/completions`

#### Запрос

| Параметр | HTTP-уровень |
|----------|--------------|
| Метод    | `POST`       |
| Путь     | `/v1/chat/completions` |
| Заголовки | `Authorization: Bearer <key>`, `Content-Type: application/json`, `Accept: application/json` (для non-streaming) или `Accept: text/event-stream` (для streaming) |

JSON-схема тела (важные поля):

| Поле           | Тип                | Обяз. | Описание |
|----------------|--------------------|-------|----------|
| `model`        | string             | да    | Идентификатор модели (например, `qwen2.5:14b`) |
| `messages`     | array of message   | да    | Массив `{role: "system" \| "user" \| "assistant" \| "tool", content: string \| array}` |
| `stream`       | boolean            | нет   | `true` → SSE-ответ. Default `false`. |
| `max_tokens`   | integer            | нет   | Лимит сгенерированных токенов |
| `temperature`  | number (0..2)      | нет   | |
| `top_p`        | number (0..1)      | нет   | |
| `stop`         | string \| string[] | нет   | Стоп-последовательности |
| `presence_penalty`  | number        | нет   | passthrough |
| `frequency_penalty` | number        | нет   | passthrough |
| `tools`        | array              | нет   | passthrough (поддержка функций — на стороне бэкенда) |
| `tool_choice`  | string \| object   | нет   | passthrough |
| `response_format` | object          | нет   | passthrough или compat-нормализация (см. ниже) |
| `seed`         | integer            | нет   | passthrough |
| `user`         | string             | нет   | passthrough |

Пример:

```json
{
  "model": "qwen2.5:14b",
  "messages": [
    {"role": "system", "content": "Ты — ассистент."},
    {"role": "user", "content": "Привет."}
  ],
  "max_tokens": 256,
  "temperature": 0.7,
  "stream": false
}
```

#### Ответ (non-streaming)

Код `200 OK`. Заголовки: `Content-Type: application/json`, `X-Request-Id: <uuid>`.

```json
{
  "id": "chatcmpl-<id>",
  "object": "chat.completion",
  "created": 1730000000,
  "model": "qwen2.5:14b",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "Привет! Чем помочь?"},
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

Прокси возвращает тело бэкенда с минимальной нормализацией (поле `model` — то же, что в запросе клиента).

#### Ответ (streaming, `stream: true`)

Код `200 OK`. Заголовки: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Request-Id: <uuid>`, `Transfer-Encoding: chunked`.

Тело — последовательность SSE-чанков. Формат:

```
data: {"id":"chatcmpl-<id>","object":"chat.completion.chunk","created":1730000000,"model":"qwen2.5:14b","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-<id>","object":"chat.completion.chunk","created":1730000000,"model":"qwen2.5:14b","choices":[{"index":0,"delta":{"content":"Прив"},"finish_reason":null}]}

data: {"id":"chatcmpl-<id>","object":"chat.completion.chunk","created":1730000000,"model":"qwen2.5:14b","choices":[{"index":0,"delta":{"content":"ет!"},"finish_reason":null}]}

data: {"id":"chatcmpl-<id>","object":"chat.completion.chunk","created":1730000000,"model":"qwen2.5:14b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":23,"completion_tokens":7,"total_tokens":30}}

data: [DONE]

```

Каждое сообщение завершается двумя `\n`. Финальный маркер — литерал `data: [DONE]`. Прокси:

- Не буферизует тело — пересылает чанки по мере получения (через `http.ResponseWriter` + `http.Flusher`).
- Считает `output_tokens` по числу `delta.content` (или берёт `usage` из последнего чанка).
- При получении ошибки **до первого** проксированного чанка — может ретраить (FR-21).
- При получении ошибки **после первого** проксированного чанка — ретрай запрещён, см. §1.6 (stream_aborted).

#### Совместимость `response_format` (`compat.response_format_mode`)

- `passthrough` (default): поле уходит на backend без изменений.
- `normalize_json_object`: если `response_format.type == "json_object"`, прокси отправляет upstream-совместимый `response_format` c `type: "json_schema"` и минимальной permissive-схемой объекта.
- `strict_reject`: если `response_format.type` задан и не равен `json_schema` или `text`, прокси возвращает `400 invalid_request` до постановки в очередь.

### 1.3. `POST /v1/completions`

Контракт идентичен `POST /v1/chat/completions`, но тело запроса содержит `prompt: string | string[]` вместо `messages`. Поля `stream`, `max_tokens`, `temperature`, `top_p`, `stop` поддерживаются. Ответ — OpenAI-формат `text_completion` / `text_completion.chunk`.

### 1.4. `POST /v1/embeddings`

#### Запрос

```json
{
  "model": "nomic-embed-text",
  "input": "строка или массив строк"
}
```

| Поле       | Тип                  | Обяз. |
|------------|----------------------|-------|
| `model`    | string               | да    |
| `input`    | string \| string[]   | да    |
| `encoding_format` | string (`float` \| `base64`) | нет |
| `dimensions` | integer            | нет   |
| `user`     | string               | нет   |

#### Ответ

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

Streaming для embeddings не поддерживается.

### 1.5. `GET /v1/models`

#### Запрос

```
GET /v1/models HTTP/1.1
Authorization: Bearer <key>
```

#### Ответ

```json
{
  "object": "list",
  "data": [
    {"id": "qwen2.5:14b",  "object": "model", "owned_by": "proxylm", "served_by": ["srv1"]},
    {"id": "llama3.1:8b",  "object": "model", "owned_by": "proxylm", "served_by": ["srv1", "srv2"]}
  ]
}
```

Поле `served_by` — расширение OpenAI-схемы; клиент может его игнорировать. Список агрегируется по `ModelMap` healthy-серверов (см. SRS, FR-7, U-2).

### 1.6. `GET /healthz`

#### Запрос

```
GET /healthz HTTP/1.1
```

Без заголовка `Authorization`.

#### Ответ

Код `200 OK`. Тело:

```json
{"status": "ok", "version": "0.1.0", "uptime_seconds": 12345}
```

Если daemon в режиме graceful shutdown — `503` c телом `{"status": "shutting_down"}`.

### 1.7. Коды ошибок и формат тела ошибки

Тело ошибки — OpenAI-совместимое:

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

Поля `type` и `code` — машинно-читаемые. `param` присутствует, если ошибка касается конкретного поля запроса.

| HTTP | `code`                  | Когда                                                                  | Ретраить клиенту? |
|------|-------------------------|-------------------------------------------------------------------------|-------------------|
| 400  | `invalid_request`        | Невалидное тело JSON, отсутствует обязательное поле                    | нет               |
| 401  | `unauthorized`           | Нет/неверный `Authorization: Bearer`                                    | нет               |
| 404  | `model_not_found`        | Модель отсутствует на всех healthy-серверах                            | нет               |
| 408  | `request_timeout`        | Истёк `backends[].timeout_seconds`                                      | да                |
| 429  | `rate_limited`           | **Зарезервировано**, в MVP не возвращается                              | да                |
| 500  | `internal_error`         | Внутренняя ошибка прокси (баг)                                          | да                |
| 502  | `backend_error`          | Бэкенд вернул не-2xx, попытки исчерпаны, failover не помог              | да                |
| 503  | `service_unavailable`    | Все серверы с этой моделью `unhealthy`; daemon в shutdown'е             | да (с `Retry-After`) |
| 504  | `backend_timeout`        | Совокупный таймаут с учётом ретраев превышен                            | да                |

При 503 прокси добавляет `Retry-After: 1` (секунды).

#### Streaming-ошибка после первого чанка (`stream_aborted`)

Если прокси уже отправил клиенту хотя бы один SSE-чанк и далее получил ошибку от бэкенда — он **не** меняет HTTP-код (он уже 200). Вместо этого:

```
data: {"error":{"message":"Backend connection lost mid-stream","type":"backend_error","code":"stream_aborted"}}

data: [DONE]

```

Клиент должен обнаружить наличие `error` в SSE-чанке. Запись в истории — `failed`.

---

## 2. Admin / IPC API

### 2.1. `GET /admin/stream` — WebSocket

#### Подключение

```
GET /admin/stream HTTP/1.1
Host: <host>:<port>
Upgrade: websocket
Connection: Upgrade
Authorization: Bearer <admin_key>
Sec-WebSocket-Version: 13
Sec-WebSocket-Key: ...
```

`<admin_key>` — `auth.admin_key`. Несовпадение → `401`, без апгрейда.

`/admin/stream` доступен на том же listener'е, что и `/v1/*` (`proxy.host:proxy.port`).
Отдельный listener для IPC не предусмотрен.

### 2.2. Сообщения сервер → клиент

Все сообщения — JSON-фреймы (`text` opcode), в формате `{"type": "<тип>", ...}`.

#### `state_snapshot`

Отправляется один раз сразу после подключения. Содержит полное состояние.

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

##### Поля `ServerState` (v0.7.0)

| Поле | Тип | Описание |
|------|-----|----------|
| `name` | string | имя сервера из конфига |
| `url` | string | базовый URL |
| `healthy` | bool | статус по discovery |
| `current_model` | string \| `""` | загруженная / in-flight модель |
| `queue_size` | int | число запросов в `pending` |
| `models` | string[] | известные модели сервера |
| `perf_samples` | int | число наблюдений в регрессии для `current_model` |
| `perf_ok` | bool | `true` если регрессия рассчитана (≥ 3 наблюдений, `X^T X` не сингулярна) |
| `t_load_ms` | float64 | оценка времени загрузки модели (мс); 0 если нет reload-точек или `perf_ok=false` |
| `tok_in_per_sec` | float64 | пропускная способность по prompt-токенам (tok/s); 0 если нет данных |
| `tok_out_per_sec` | float64 | пропускная способность по completion-токенам (tok/s); 0 если нет данных |
| `per_model_stats` | `ModelStats[]` | статистика по всем моделям сервера, sorted by `samples DESC` |

> Поля `tokens_per_sec` и `ttft_ms` **удалены в v0.7.0**. Замена: `tok_out_per_sec` и `t_load_ms` из регрессии.

##### Структура `ModelStats`

| Поле | Тип | Описание |
|------|-----|----------|
| `model` | string | идентификатор модели |
| `samples` | int | общее число наблюдений для пары (server, model) |
| `loaded` | int | из них — с флагом model_reload (смена модели перед запросом) |
| `ok` | bool | регрессия валидна |
| `t_load_ms` | float64 | оценка времени загрузки (мс); 0 если нет данных |
| `tok_in_per_sec` | float64 | пропускная способность prompt-токенов (tok/s) |
| `tok_out_per_sec` | float64 | пропускная способность completion-токенов (tok/s) |

##### Поля `RequestState`

Дополнительное поле, добавленное в v0.7.0:

| Поле | Тип | Описание |
|------|-----|----------|
| `model_reloaded` | bool | `true` если в начале dispatch этого запроса `current_model` сервера отличалась от запрошенной модели (т.е. произошла смена модели) |

#### `state_diff`

Отправляется по факту изменений. Содержит частичные обновления.

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

`requests_removed` содержит массив `request_id`, которые клиент должен убрать из таблицы (например, после `tui.show_completed_minutes`).
`requests_upserted` использует тот же формат полей запроса, что и `state_snapshot.requests`; допускаются частичные апдейты.

#### `log_line`

Push строки лога.

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

### 2.3. Сообщения клиент → серверу

#### `subscribe`

Опциональная подписка на подмножество событий. Если не отправлена — клиент получает всё.

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

Ответ: `{"type": "pong", "timestamp": "..."}`. Сервер сам шлёт `ping` каждые 30 с при бездействии.

### 2.4. Закрытие соединения

| Код | Причина |
|------|---------|
| 1000 | Штатное закрытие (клиент послал close) |
| 1008 | Нарушение протокола / неверный JSON |
| 1011 | Внутренняя ошибка сервера |
| 4401 | Auth failure при подключении (вместе с HTTP 401 до апгрейда) |

---

## 3. Внутренние backend-вызовы

### 3.1. Общие правила

- Прокси использует `*http.Client` (`net/http`, stdlib) с `Timeout = backends[].timeout_seconds` (default 600 секунд). Стриминговые ответы читаются через `resp.Body` (`io.ReadCloser`) по строкам (`bufio.Scanner` или `bufio.Reader.ReadBytes('\n')`).
- Базовый URL — `backends[].url`.
- Если `backends[].api_key` не `null`, добавляется `Authorization: Bearer <api_key>` (свой для каждого бэкенда). Этот заголовок **не имеет отношения** к ключу клиента прокси.
- Заголовок `User-Agent: proxylm/<version>` (где `<version>` берётся из `runtime/debug.BuildInfo` или `-ldflags -X main.version=...`).
- На каждый бэкенд создаётся отдельный `*http.Client` с настроенным `*http.Transport` (keep-alive, `MaxIdleConnsPerHost`, общий таймаут).

### 3.2. Используемые пути

| Назначение                | Метод | Путь                       | Тело                                    |
|---------------------------|-------|----------------------------|------------------------------------------|
| Discovery                 | GET   | `/v1/models`               | —                                        |
| Chat completion (regular) | POST  | `/v1/chat/completions`     | тело клиента; `response_format` может быть нормализован по `compat.response_format_mode` |
| Chat completion (stream)  | POST  | `/v1/chat/completions`     | тело + `stream: true`; `response_format` может быть нормализован; ответ через `resp.Body` (chunked reading) |
| Text completion           | POST  | `/v1/completions`          | passthrough                              |
| Embeddings                | POST  | `/v1/embeddings`           | passthrough                              |

### 3.3. LM Studio / Ollama специфика

- LM Studio: OpenAI-совместимый API на `http://<host>:1234/v1/*`.
- Ollama: OpenAI-shim на `http://<host>:11434/v1/*` (нативные `/api/*` — out of scope MVP).
- Поведение `Ollama` при `model: "..."`, которой нет: 404 — мапится в стандартный путь ошибки прокси.

### 3.4. Обработка ответов

- 2xx — отдаём клиенту (с возможной нормализацией поля `model`).
- 4xx (кроме 429) — пробрасываем клиенту без ретрая (FR-23).
- 5xx, network reset, timeout — ретрай по политике (FR-18 ÷ FR-20).

---

## 4. Примеры curl

Задаём переменные окружения:

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
    "messages": [{"role": "user", "content": "Привет."}],
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
    "messages": [{"role": "user", "content": "Расскажи анекдот."}],
    "stream": true
  }'
```

`-N` отключает буферизацию curl — иначе SSE-чанки будут видны не сразу.

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
  -d '{"model": "nomic-embed-text", "input": "Текст для эмбеддинга"}'
```

### 4.5. Список моделей

```bash
curl -s "$PROXY/v1/models" -H "Authorization: Bearer $KEY"
```

### 4.6. Health-check

```bash
curl -s "$PROXY/healthz"
```

### 4.7. Подключение к `/admin/stream` (через `websocat`)

```bash
websocat -H="Authorization: Bearer $ADMIN" "ws://localhost:8080/admin/stream"
```

После подключения сразу придёт `state_snapshot`, далее — `state_diff` и `log_line` по мере событий.
