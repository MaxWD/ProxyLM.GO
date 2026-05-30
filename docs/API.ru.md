# ProxyLM.GO — API Specification

Версия документа: 0.11.0
Связанные документы: [`ARCHITECTURE.ru.md`](./ARCHITECTURE.ru.md), [`SRS.ru.md`](./SRS.ru.md)

Документ описывает три группы API:

1. **Внешний API** — OpenAI-совместимый, для клиентских сервисов.
2. **Admin / IPC API** — WebSocket-канал для TUI и любых административных клиентов.
3. **Внутренние backend-вызовы** — что и как прокси шлёт на LM Studio / Ollama.

Вся передача — UTF-8 JSON, если не указано иное. Все timestamps — ISO 8601 в UTC, если не указано иное.

---

## 1. Внешний API (OpenAI-совместимый)

Базовый префикс: `/v1`. Хост и порт — из `proxy.host` / `proxy.port` (default `0.0.0.0:8080`).

### 1.1. Общие требования

- Все эндпоинты под `/v1/*` требуют аутентификацию. Ключ ищется в `auth.api_keys[].key`. При несовпадении — `401`. Принимаются два стиля заголовков (в порядке приоритета):
  1. `Authorization: Bearer <api_key>` — стандартный стиль OpenAI.
  2. `x-api-key: <api_key>` — стиль Anthropic SDK.
  Если оба заголовка присутствуют, приоритет у `Authorization`. Поддержка обоих стилей позволяет Anthropic SDK-клиентам аутентифицироваться так же, как к настоящему Anthropic API.
- Заголовок `Content-Type: application/json` обязателен для POST-запросов.
- Прокси добавляет в логи и историю поле `client_name`, соответствующее `auth.api_keys[].name`. Сам ключ нигде не логируется.
- Неизвестные поля тела запроса передаются на бэкенд без модификации (FR-9).
- Прокси добавляет заголовок `X-Request-Id: <uuid>` ко всем ответам `/v1/*`. Middleware генерирует UUIDv4, если клиент не передал заголовок; если клиент прислал валидный `X-Request-Id` — он переиспользуется. Значение записывается в `w.Header()` до вызова хендлера.

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
- При получении ошибки **после первого** проксированного чанка — ретрай запрещён, см. §1.8 (stream_aborted).

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

### 1.5. `POST /v1/messages` (Anthropic Messages API)

#### Запрос

| Параметр | HTTP-уровень |
|----------|--------------|
| Метод    | `POST`       |
| Путь     | `/v1/messages` |
| Заголовки | `Authorization: Bearer <key>` или `x-api-key: <key>`, `Content-Type: application/json` |

JSON-схема тела (важные поля):

| Поле           | Тип                | Обяз. | Описание |
|----------------|--------------------|-------|----------|
| `model`        | string             | да    | Идентификатор модели (например, `claude-3-5-sonnet-20241022`) |
| `max_tokens`   | integer            | да    | Максимальное количество токенов для генерации |
| `messages`     | array of message   | да    | Массив `{role: "user" \| "assistant", content: string \| array}` |
| `system`       | string             | нет   | Системный промпт (верхнеуровневое поле, не внутри `messages`) |
| `stream`       | boolean            | нет   | `true` → SSE-ответ. Default `false`. |
| `temperature`  | number (0..1)      | нет   | passthrough |
| `top_p`        | number (0..1)      | нет   | passthrough |
| `top_k`        | integer            | нет   | passthrough |
| `stop_sequences` | string[]         | нет   | passthrough |
| `tools`        | array              | нет   | passthrough (формат инструментов Anthropic) |
| `metadata`     | object             | нет   | passthrough |
| `thinking`     | object             | нет   | Конфигурация extended thinking; passthrough только на пути Anthropic→Anthropic |

Пример:

```json
{
  "model": "claude-3-5-sonnet-20241022",
  "max_tokens": 256,
  "system": "Ты — полезный ассистент.",
  "messages": [
    {"role": "user", "content": "Привет."}
  ]
}
```

#### Ответ (non-streaming)

Код `200 OK`. Заголовки: `Content-Type: application/json`, `X-Request-Id: <uuid>`.

```json
{
  "id": "msg_01XFDUDYJgAACzvnptvVoYEL",
  "type": "message",
  "role": "assistant",
  "content": [
    {"type": "text", "text": "Привет! Чем могу помочь?"}
  ],
  "model": "claude-3-5-sonnet-20241022",
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": {
    "input_tokens": 12,
    "output_tokens": 8
  }
}
```

#### Ответ (streaming, `stream: true`)

Код `200 OK`. Заголовки: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Request-Id: <uuid>`, `Transfer-Encoding: chunked`.

Тело — последовательность именованных SSE-событий. **Формат SSE у Anthropic отличается от OpenAI**: перед строкой `data:` идёт строка `event:`.

```
event: message_start
data: {"type":"message_start","message":{"id":"msg_01...","type":"message","role":"assistant","content":[],"model":"claude-3-5-sonnet-20241022","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Привет"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"!"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":8}}

event: message_stop
data: {"type":"message_stop"}

```

Каждое событие разделяется пустой строкой. Поток завершается событием `event: message_stop`.

#### Кросс-протокольная трансляция

Прокси прозрачно транслирует между форматами OpenAI и Anthropic на основе **клиентского эндпоинта** и поля **`type` бэкенда**. Полная матрица совместимости — в FR-56. Трансляция охватывает поля запроса, поля ответа и SSE-события.

| Клиентский эндпоинт | Тип бэкенда | Трансляция |
|---------------------|-------------|------------|
| `/v1/chat/completions` | `openai` | passthrough |
| `/v1/chat/completions` | `anthropic` | запрос OpenAI→Anthropic; ответ Anthropic→OpenAI |
| `/v1/messages` | `openai` | запрос Anthropic→OpenAI; ответ OpenAI→Anthropic |
| `/v1/messages` | `anthropic` | passthrough |

#### Формат ошибок Anthropic

Ошибки от Anthropic-бэкендов используют другой конверт:

```json
{"type": "error", "error": {"type": "invalid_request_error", "message": "..."}}
```

При трансляции для OpenAI-клиента прокси конвертирует это в стандартный формат ошибки OpenAI (см. §1.8).

#### Ограничения

- `/v1/completions` и `/v1/embeddings` не транслируются на Anthropic-бэкенды — возвращается `400 invalid_request`, если бэкенд имеет `type: anthropic`.
- Extended thinking (`thinking`, `budget_tokens`) пробрасывается только при Anthropic-клиент → Anthropic-бэкенд. При трансляции на OpenAI-бэкенд эти поля молча отбрасываются.

### 1.6. `GET /v1/models`

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

### 1.7. `GET /healthz`

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

### 1.8. Коды ошибок и формат тела ошибки

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

Все сообщения — JSON-фреймы (`text` opcode). Каждый фрейм — `Envelope`:

```json
{"type": "<тип>", "time": "<RFC3339>", "payload": { ... }}
```

#### `state_snapshot`

Отправляется один раз сразу после подключения, а также в ответ на фрейм `request_snapshot` от клиента. Содержит полное состояние.

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
        "t_load_loaded": 3,
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

##### Поля `ServerState`

| Поле | Тип | Описание |
|------|-----|----------|
| `name` | string | имя сервера из конфига |
| `url` | string | базовый URL |
| `healthy` | bool | статус по discovery |
| `in_flight` | bool | `true` если на сервере выполняется запрос |
| `current_model` | string | загруженная / in-flight модель; пустая строка если нет |
| `queue_depth` | int | число запросов в `pending` |
| `models` | string[] | известные модели сервера |
| `perf_samples` | int | число наблюдений в регрессии для `current_model`; опускается если 0 |
| `perf_ok` | bool | `true` если регрессия рассчитана (≥ 3 наблюдений, `X^T X` не сингулярна); опускается если false |
| `t_load_ms` | float64 | оценка времени загрузки (мс); опускается если 0 |
| `t_load_loaded` | int | число reload-наблюдений (`loaded=1`), на которых построена header-оценка `t_load_ms`; TUI помечает `< 3` как низкоуверенную (завершающая `*`), а `0` как `—` (неизвестно); опускается если 0 (v0.12.0) |
| `tok_in_per_sec` | float64 | пропускная способность по prompt-токенам (tok/s); опускается если 0 |
| `tok_out_per_sec` | float64 | пропускная способность по completion-токенам (tok/s); опускается если 0 |
| `r_squared` | float64 | коэффициент детерминации R² header-регрессии, [0,1]; опускается если 0 (нет fit'а) |
| `fit_quality` | string | словесная оценка: `"good"` (R² ≥ 0.70), `"degraded"` (<0.70), `""` если нет fit'а |
| `slow` | bool | последний завершённый запрос занял ≥ 2× от расчётного по регрессии; опускается если false |
| `failure_count` | int64 | суммарное число упавших попыток на этом сервере с момента старта daemon; опускается если 0 |
| `per_model_stats` | `ModelStats[]` | статистика per-(model, endpoint), отсортированная по `samples DESC`; опускается если пусто |

##### Структура `ModelStats`

Отдельный бакет на каждую пару `(model, endpoint)` сервера. Endpoint стал частью ключа регрессии в v0.10.0, чтобы запросы с принципиально разными профилями tokens/ms (например, `/v1/chat/completions` vs `/v1/embeddings`) не искажали fit.

| Поле | Тип | Описание |
|------|-----|----------|
| `model` | string | идентификатор модели |
| `endpoint` | string | путь запроса (напр. `/v1/chat/completions`); опускается если пусто (legacy-данные до миграции v0.10.0) |
| `samples` | int | общее число наблюдений для ключа (server, model, endpoint) |
| `loaded` | int | из них — с флагом model_reload (смена модели перед запросом) |
| `ok` | bool | регрессия валидна |
| `t_load_ms` | float64 | оценка времени загрузки (мс); опускается если 0 |
| `tok_in_per_sec` | float64 | пропускная способность prompt-токенов (tok/s); опускается если 0 |
| `tok_out_per_sec` | float64 | пропускная способность completion-токенов (tok/s); опускается если 0 |
| `r_squared` | float64 | коэффициент детерминации R², [0,1]; опускается если 0 |
| `fit_quality` | string | `"good"` / `"degraded"` / `""` (нет fit'а) |
| `t_load_ci` | float64 | half-width 95% доверительного интервала для `t_load_ms`; опускается если 0 (мало данных или коэффициент зафиксирован NNLS на нуле) |
| `k_in_ci` | float64 | half-width 95% CI коэффициента входных токенов `k_in_ms_tok` (публикуемая величина — `tok_in_per_sec = 1000/k_in`; CI выражен в `ms/tok`, клиент может пересчитать) |
| `k_out_ci` | float64 | half-width 95% CI коэффициента выходных токенов `k_out_ms_tok` |

##### Поля `RequestState`

| Поле | Тип | Описание |
|------|-----|----------|
| `id` | string | UUID запроса |
| `client_name` | string | имя из `auth.api_keys` |
| `model` | string | модель из тела запроса |
| `endpoint` | string | путь запроса (напр. `/v1/chat/completions`) |
| `stream` | bool | `true` если streaming-запрос |
| `server_name` | string | имя назначенного сервера |
| `status` | string | `queued` / `running` / `completed` / `failed` |
| `http_status` | int | HTTP-код ответа от бэкенда (0 если ещё не диспатчено) |
| `prompt_tokens` | int | токенов в prompt |
| `output_tokens` | int | сгенерированных токенов |
| `created_at` | string (RFC3339) | момент входа в HTTP-хендлер |
| `started_at` | string (RFC3339) | момент первого dispatch; опускается если ещё не диспатчено |
| `completed_at` | string (RFC3339) | момент финализации; опускается если ещё не завершено |
| `attempt` | int | 1-based номер последней (или текущей) попытки; 0 если ещё не диспатчено |
| `max_attempts` | int | настроенный `retry.max_attempts` |
| `queue_wait_ms` | int64 | миллисекунды между `created_at` и `started_at`; 0 если ещё в очереди |
| `model_reloaded` | bool | `true` если при dispatch произошла смена модели; опускается если false |
| `last_failed_server` | string | имя сервера, на котором упала предыдущая попытка; пустая строка если ещё не было неудач; опускается если пусто |
| `error_message` | string | последнее сообщение об ошибке для `failed`-запросов; опускается если пусто |

### 2.3. Сообщения клиент → серверу

#### `request_snapshot`

TUI отправляет этот фрейм, чтобы потребовать у daemon'а актуальный snapshot. Используется при нажатии `F5`.

```json
{"type": "request_snapshot", "time": "2026-05-09T14:03:00Z"}
```

Payload не требуется. Daemon отвечает обычным `state_snapshot` (тем же путём, что и при connect).

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

**OpenAI-протокол бэкенды (`type: openai` или `type: ollama`):**

| Назначение                | Метод | Путь                       | Тело                                    |
|---------------------------|-------|----------------------------|------------------------------------------|
| Discovery                 | GET   | `/v1/models`               | —                                        |
| Chat completion (regular) | POST  | `/v1/chat/completions`     | тело клиента или транслированное из Anthropic; `response_format` может быть нормализован по `compat.response_format_mode` |
| Chat completion (stream)  | POST  | `/v1/chat/completions`     | тело + `stream: true`; ответ через `resp.Body` (chunked reading) |
| Text completion           | POST  | `/v1/completions`          | passthrough (только для OpenAI-клиентов; не маршрутизируется на Anthropic-бэкенды) |
| Embeddings                | POST  | `/v1/embeddings`           | passthrough (только для OpenAI-клиентов; не маршрутизируется на Anthropic-бэкенды) |

**Anthropic-протокол бэкенды (`type: anthropic`):**

| Назначение                   | Метод | Путь          | Тело                                                                 |
|------------------------------|-------|---------------|----------------------------------------------------------------------|
| Messages (regular)           | POST  | `/v1/messages` | тело клиента или транслированное из OpenAI `chat/completions` формата |
| Messages (stream)            | POST  | `/v1/messages` | тело + `stream: true`; ответ использует формат SSE-событий Anthropic |

### 3.3. Совместимость бэкендов

ProxyLM.GO **не привязан к конкретному бэкенду**: подключается любой сервер, отдающий OpenAI-совместимый `/v1/*` API. Прокси прозрачен — он пробрасывает JSON-тело и заголовки клиента (кроме `Authorization`, который подставляется из `backends[].api_key`) и читает ответ потоково.

Протестированные / типичные бэкенды:

| Бэкенд                          | URL по умолчанию                      | Аутентификация       | Примечания                                                                              |
|---------------------------------|---------------------------------------|----------------------|------------------------------------------------------------------------------------------|
| **LM Studio** (локально)        | `http://<host>:1234`                  | опц. Bearer          | Одна модель в памяти; срабатывает INV-2 model affinity.                                  |
| **Ollama** (локально)           | `http://<host>:11434`                 | без auth по умолчанию | OpenAI-совместимый shim на `/v1/*` (нативный `/api/*` — out of scope, см. SRS §8). Одно-модельная семантика; `keep_alive` управляется переменной окружения `OLLAMA_KEEP_ALIVE` на стороне Ollama, не через прокси. При отсутствии модели возвращает `404` — мапится в стандартный путь ошибки. |
| **vLLM** (локально/кластер)     | `http://<host>:8000`                  | опц. Bearer          | Одна модель; полный OpenAI surface (включая `logprobs`).                                  |
| **llama.cpp** (`./server`)      | `http://<host>:8080`                  | опц. Bearer          | Одна модель; минимальный OpenAI shim.                                                    |
| **OpenAI** (облако)             | `https://api.openai.com`              | Bearer (`sk-...`)    | Multi-model; INV-2 model affinity для cloud — лишний overhead, но не ломает. Платный — рекомендуется `priority` побольше, чтобы облако работало fallback'ом. |
| **OpenRouter** (облако)         | `https://openrouter.ai/api`           | Bearer (`sk-or-v1-`) | Multi-model (сотни моделей в `/v1/models`); см. *Известные ограничения* ниже.            |
| **Groq / Together / Fireworks** (облако) | provider-specific          | Bearer               | Multi-model; семантика как у OpenRouter.                                                 |
| **Anthropic** (облако)       | `https://api.anthropic.com`           | Bearer или `x-api-key` | Требуется `type: anthropic`. Discovery не выполняется (у Anthropic нет `/v1/models`); `backends[].models` укажите явно. |

Пример конфигурации для Anthropic-бэкенда:

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

Конфигурация для OpenAI-протокол бэкендов:

```yaml
- name: <человекочитаемое-имя>
  url: <base-url-без-/v1>
  api_key: <токен-или-null>
  timeout_seconds: <секунды>
  priority: <число, меньше = выше приоритет>
```

Поле `backends[].type` управляет wire-протоколом при взаимодействии с данным бэкендом. Допустимые значения: `openai` (по умолчанию, включает `ollama`), `anthropic`. При `type: anthropic` прокси отправляет на бэкенд запросы Anthropic Messages API и транслирует запросы/ответы клиента при необходимости.

**Известные ограничения cloud-бэкендов в v0.11.x:**

- Retry-политика (FR-18..FR-20) пока не парсит заголовок `Retry-After`; при массовых 429 от облака может усилить rate-limit. Запланировано в [`docs/FUTURE.md`](./FUTURE.md), пункт *429-aware retry*.
- Discovery опрашивает `/v1/models` каждые `discovery.interval_seconds` для **всех** бэкендов; OpenRouter (~300 моделей) забьёт таблицу прокси. Обход: укажите `models:` явно в конфиге бэкенда, чтобы ограничить набор обнаруживаемых моделей.
- Model-affinity (INV-2) сериализует запросы по модели на одном сервере. Для multi-model cloud это no-op overhead, не польза; per-backend `serialize_by_model` в плане (см. FUTURE.md).

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

### 4.7. Anthropic Messages API (non-streaming)

```bash
curl -s "$PROXY/v1/messages" \
  -H "x-api-key: $KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 64,
    "system": "Ты — полезный ассистент.",
    "messages": [{"role": "user", "content": "Привет."}]
  }'
```

`x-api-key` и `Authorization: Bearer` принимаются оба.

### 4.8. Anthropic Messages API (streaming)

```bash
curl -N "$PROXY/v1/messages" \
  -H "x-api-key: $KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 64,
    "messages": [{"role": "user", "content": "Расскажи анекдот."}],
    "stream": true
  }'
```

### 4.9. Подключение к `/admin/stream` (через `websocat`)

```bash
websocat -H="Authorization: Bearer $ADMIN" "ws://localhost:8080/admin/stream"
```

После подключения сразу придёт `state_snapshot`. Нажми `F5` в TUI (или отправь `{"type":"request_snapshot","time":"..."}`) для получения свежего снапшота в любой момент.
