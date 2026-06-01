# ProxyLM.GO — Архитектура

## 1. Назначение

Прокси-сервер на Go перед локальными и удалёнными LLM-серверами (LM Studio, Ollama, Anthropic, OpenAI и т. д.). Главная задача — **сериализовать запросы по моделям**, чтобы избежать постоянной выгрузки/загрузки моделей в VRAM.

Основные свойства:
- OpenAI-совместимый API на входе (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/models`)
- Anthropic Messages API на входе (`/v1/messages`) — Anthropic SDK-клиенты подключаются без изменения кода
- **Кросс-протокольная трансляция:** все четыре комбинации клиент/бэкенд обрабатываются прозрачно (OpenAI↔OpenAI, OpenAI↔Anthropic, Anthropic↔OpenAI, Anthropic↔Anthropic)
- **Двойная аутентификация:** принимаются оба стиля `Authorization: Bearer` и `x-api-key`
- Поддержка streaming (SSE — формат `data:` OpenAI и формат `event:`/`data:` Anthropic)
- Несколько бэкенд-серверов; знает какие модели где есть (авто-обнаружение через `/v1/models`)
- Маршрутизация: model affinity + least-busy
- Retry + failover на другой сервер с той же моделью
- Аутентификация по API-ключам
- Консольный TUI в стиле btop (отдельный клиент к daemon'у)
- История запросов в SQLite
- **Один portable-бинарник:** один и тот же исполняемый файл — и daemon, и TUI-клиент, и инсталлятор службы. Кросс-компиляция под любую OS без CGO.

## 2. Принципиальная схема

```
┌──────────────────────────────────────────────────────────┐
│ Клиенты (внутренние сервисы)                             │
│ service-a (OpenAI SDK), service-b (Anthropic SDK), ...   │
└──────────────────────┬───────────────────────────────────┘
                       │ HTTP (OpenAI /v1/* или Anthropic /v1/messages)
                       ▼
┌──────────────────────────────────────────────────────────┐
│ ProxyLM.GO Daemon  (net/http + chi + goroutines)         │
│                                                          │
│  HTTP API ─► AuthN ─► Translate? ─► Router ─► Queue     │
│  (OpenAI+Anthropic)  (Bearer/x-api-key)  (model)        │
│                          │                               │
│                          │ выбор: какой сервер для       │
│                          │   этой (model)                │
│                          ▼                               │
│                  ┌──────────────────┐                    │
│                  │ Worker per server│  ⇐ ключ:           │
│                  │ "drain current   │     "пока есть     │
│                  │  model fully,    │     запросы для    │
│                  │  then switch"    │     модели X — её  │
│                  └────────┬─────────┘     не отпускаем"  │
│                           │                              │
│                  Backend client (*http.Client)           │
│                  (openai.go или anthropic.go)            │
│                  retry + failover                        │
│                           │                              │
│  Discovery ──► ModelMap   │   SQLite history             │
│                           │   (modernc.org/sqlite)       │
│                           │                              │
│  IPC server (WebSocket) ◀─┴─►  TUI client (Bubble Tea)   │
└─────────────────────┬────────────────────────────────────┘
                      │ HTTP (OpenAI /v1/* или Anthropic /v1/messages)
                      ▼
┌──────────────────────────────────────────────────────────┐
│ Backend LLM серверы                                      │
│ srv1 (LM Studio/OpenAI), srv2 (Ollama), srv3 (Anthropic) │
└──────────────────────────────────────────────────────────┘
```

## 3. Алгоритм планировщика (ядро проекта)

Каждый бэкенд-сервер имеет свой воркер-goroutine. В каждый момент на сервере выполняется **один** запрос — это правило избавляет от гонок выгрузки моделей.

```go
// Per-server state (упрощённо)
type Server struct {
    Name         string
    URL          string
    CurrentModel atomic.Pointer[string] // модель последнего обслуженного / in-flight
    mu           sync.Mutex
    pending      []*Request              // FIFO под mu
    notify       chan struct{}           // буферизованный 1, сигнал «появилось новое»
    inFlight     *Request                // не больше одного, под mu
    healthy      atomic.Bool
    // ...
}

// Воркер сервера: одна goroutine на сервер
func (s *Server) workerLoop(ctx context.Context, dispatch DispatchFn) {
    for {
        next := s.pickNextRequest()
        if next == nil {
            select {
            case <-ctx.Done():
                return
            case <-s.notify: // разбудили: enqueue или health-change
                continue
            }
        }
        s.mu.Lock()
        s.inFlight = next
        s.mu.Unlock()

        dispatch(ctx, next)            // блокирует до завершения (или ошибки + ретраев)

        cm := next.Model
        s.CurrentModel.Store(&cm)      // eventual consistency для роутера
        s.mu.Lock()
        s.inFlight = nil
        s.mu.Unlock()
    }
}

// pickNextRequest — drain текущей модели → FIFO
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

// Enqueue будит воркер неблокирующе
func (s *Server) Enqueue(r *Request) {
    s.mu.Lock()
    s.pending = append(s.pending, r)
    s.mu.Unlock()
    select {
    case s.notify <- struct{}{}:
    default: // канал ёмкости 1 — сигнал уже есть, дублирование не нужно
    }
}
```

**Свойства алгоритма:**
- Запросы для **текущей** модели всегда обслуживаются первыми, даже если приходят, пока сервер занят. → Модель **не выгружается** между ними.
- Когда очередь для текущей модели пуста, берём следующий запрос FIFO — он триггерит переключение модели.
- Один in-flight на сервер. На разных серверах запросы идут параллельно (по одной goroutine на сервер + отдельный `*http.Client` на каждый).
- Голод модели B возможен лишь если поток запросов модели A ровно бесконечен — **по требованию это допустимо**. Опционально позже можно добавить лимит `max_consecutive_requests_per_model`.

Альтернатива sync.Cond не используется намеренно: канал `notify` с ёмкостью 1 + `select { case <-ctx.Done(); case <-s.notify }` — идиоматичный Go-паттерн, корректно обрабатывает cancellation через `context.Context`.

## 4. Маршрутизация (роутер)

Когда приходит новый запрос с моделью M:

```
candidates = filter(servers, healthy && M ∈ models_of(server))
if candidates is empty:
  → 404 Model not found
sort candidates by:
  1) prefer сервер, у которого M == current_model (избежать swap)
  2) prefer сервер с наименьшей длиной pending (least-busy)
  3) tiebreak: имя сервера (стабильность)
choose candidates[0]
```

Это и есть `model_affinity_least_busy` — push-стратегия по умолчанию. Реализация — чистая функция от `[]*ServerInfo` и `model`, не блокирует воркеры; читает `CurrentModel` через `atomic.Pointer[string].Load()`.

### Pull-стратегии (общий `JobPool`)

Для `deferred_model_then_capable`, `preserve_model_coverage` и `fair_share_round_robin` сервер не выбирается при приёме. Вместо этого планировщик держит общий `JobPool` (`internal/core/pool.go`); освобождающиеся воркеры сами тянут следующий совместимый Job через `PopFor` / `PopForCoverage` / `PopForFairShare`. Это перераспределяет нагрузку пропорционально скорости бэкендов, когда модель есть на нескольких серверах.

`fair_share_round_robin` (добавлено в v0.10.0) расширяет `deferred_model_then_capable` **защитой от голодания**. Планировщик ведёт счётчик `ConsecutiveModelCount` на каждом `ServerInfo` (под `s.mu`, обновляется в `dispatch`). При `scheduler.max_consecutive_per_model > 0` и достижении лимита `PopForFairShare` ищет в пуле Job под модель, отличную от `current_model`, и отдаёт его. Если других совместимых моделей в очереди нет — поведение деградирует к обычному FIFO-drain'у (воркер никогда не простаивает при наличии совместимой работы). Цена — лишняя загрузка модели каждые N запросов, что честно отражается в perf-регрессии (`t_load × loaded=1`).

## 5. Retry + Failover

При ошибке от бэкенда:
- Кратковременные ошибки (5xx, таймаут, network reset) → ретрай по экспоненциальному backoff.
- **Rolling exclusion size 1:** после неудачи на сервере X следующая попытка идёт на любой другой healthy-сервер с этой моделью; X исключается ТОЛЬКО на одну следующую попытку (через шаг он снова доступен). Если X — единственный совместимый, исключение игнорируется и попытка идёт снова на X.
- Общий cap — `retry.max_attempts` (default 3) попыток на запрос **независимо от их распределения по серверам** (см. INV-5).
- Отдельной настройки `failover` НЕТ; такое поведение работает всегда.
- Сервер, на котором подряд накопились отказы, помечается `unhealthy` (через discovery), исключается из роутинга и периодически проверяется health-check'ом.

Backoff: `time.Sleep(d)` внутри воркера допустим (воркер — отдельная goroutine, не блокирует ничего, кроме своей очереди); для cancellation — `select { case <-time.After(d); case <-ctx.Done() }`.

Streaming-нюанс: если ошибка приходит **до первого SSE-чанка** — ретрай безопасен. Если уже отправили часть ответа клиенту — ретрай невозможен (ответ деградирует в конкретную ошибку клиенту).

## 6. Streaming

- Клиент: `POST /v1/chat/completions` с `stream: true`.
- Прокси открывает streaming-соединение к бэкенду через обычный `http.Client.Do(req)`; ответ читается из `resp.Body` (`io.ReadCloser`).
- Чтение по строкам через `bufio.Reader.ReadBytes('\n')`; парсинг SSE-фреймов (префикс `data: `, разделитель — пустая строка).
- Запись клиенту: `http.ResponseWriter.Write(...)` + `w.(http.Flusher).Flush()` после каждого чанка — без буферизации.
- Параллельно прокси считает `output_tokens` (по `delta.content` или из `usage` финального чанка) и отправляет события в IPC-publisher для TUI.
- `input_tokens`: берём из последнего чанка с `usage` (LM Studio/Ollama OpenAI-shim возвращают usage в финальном `[DONE]`-чанке); fallback-подсчёт через библиотеку токенизации — отложен на v0.2 (U-1).

## 7. Кросс-протокольная трансляция

В v0.11.0 ProxyLM.GO стал мультипротокольным. Слой трансляции находится между HTTP-хендлером и воркером планировщика, а также между воркером и backend-клиентом.

### Матрица трансляций

| Клиентский эндпоинт    | `type` бэкенда | Действие |
|------------------------|----------------|----------|
| `/v1/chat/completions` | `openai`       | Passthrough — трансляция отсутствует |
| `/v1/chat/completions` | `anthropic`    | Запрос: OpenAI→Anthropic; Ответ: Anthropic→OpenAI |
| `/v1/messages`         | `openai`       | Запрос: Anthropic→OpenAI; Ответ: OpenAI→Anthropic |
| `/v1/messages`         | `anthropic`    | Passthrough — трансляция отсутствует |

`/v1/completions` и `/v1/embeddings` никогда не маршрутизируются на бэкенды `type: anthropic` (возвращается `400`).

### Пакет `internal/api/translate/`

Три файла реализуют логику трансляции:

- `request.go` — конвертирует структуры запросов между OpenAI `ChatCompletionRequest` и Anthropic `MessagesRequest`. Ключевые маппинги: `messages[].role`, извлечение/добавление system-промпта, `max_tokens`, `stop`/`stop_sequences`, различия форматов инструментов.
- `response.go` — конвертирует структуры не-streaming ответов. Обрабатывает content blocks (`content[].type = "text"`), `stop_reason`/`finish_reason`, переименование полей `usage`.
- `stream.go` — определяет интерфейс `StreamTranslator`, используемый streaming-хендлерами. Stateful: отслеживает последовательность событий `message_start` → `content_block_*` → `message_stop`.

### Streaming с трансляцией (`internal/api/streaming_translate.go`)

Для streaming-путей, требующих трансляции, прокси использует `streaming_translate.go` вместо обычного `streaming.go`. Он оборачивает построчный SSE-ридер и применяет `StreamTranslator` к каждому событию перед отправкой клиенту. Stateful-транслятор необходим, поскольку SSE Anthropic принципиально отличается от SSE OpenAI:

- **OpenAI SSE:** `data: <json-chunk>\n\n`, завершается `data: [DONE]\n\n`.
- **Anthropic SSE:** `event: <type>\ndata: <json>\n\n` — именованные события; несколько типов событий на сообщение.

Правила трансляции для направления `Anthropic→OpenAI`: накапливать текст `content_block_delta` в синтетический чанк `choices[0].delta.content`; маппить `message_start.usage.input_tokens` → `usage.prompt_tokens` первого чанка; маппить `message_delta.usage.output_tokens` → `usage` последнего чанка; эмитировать `data: [DONE]` при `message_stop`.

Правила трансляции для направления `OpenAI→Anthropic`: оборачивать каждый `choices[0].delta.content` в событие `content_block_delta`; синтезировать `message_start` перед первым чанком; эмитировать `message_stop` после `data: [DONE]`.

### Anthropic Backend client (`internal/core/backends/anthropic.go`)

Реализует интерфейс `Backend` для бэкендов с `type: anthropic`. Отправляет запросы на `<url>/v1/messages` используя wire-формат Anthropic Messages API. Аутентификация: устанавливает и `Authorization: Bearer <api_key>`, и `x-api-key: <api_key>` для максимальной совместимости с Anthropic-совместимыми сервисами. Инварианты планировщика INV-1..INV-8 не изменились — `anthropic.go`-бэкенд участвует в той же per-server очереди и воркер-цикле, что и `openai.go`.

## 8. Discovery

- **Initial healthcheck:** при старте daemon'а, до начала приёма HTTP-запросов, выполняется один синхронный poll `GET /v1/models` для каждого backend-сервера, независимо от `discovery.enabled`. Серверы с явно указанным непустым списком `backends[].models` помечаются healthy сразу без poll'а.
- Раз в `discovery.interval_seconds` (default 30s) опрашиваем `/v1/models` каждого сервера.
- Один общий `time.Ticker`, в цикле — fan-out goroutines (по одной на сервер), результат собирается в `ModelMap: map[string]map[string]struct{}`.
- Используется роутером.
- При недоступности сервера N циклов подряд → флаг `unhealthy.Store(false)`.
- Discovery-цикл получает `context.Context`, корректно завершается при shutdown.
- **Проба загруженных моделей (v0.13.0):** в том же цикле опроса бэкенды, реализующие опциональный интерфейс `backends.LoadedModelsProber`, дополнительно опрашиваются на предмет моделей, *реально находящихся в памяти*. Проба выбирается по типу (`backends[].type`): Ollama → `GET /api/ps`, LM Studio → `GET /api/v1/models` (модели с непустым `loaded_instances`), llama.cpp → `GET /models` (фильтр `status==loaded`). Обычные `openai` и `anthropic` бэкенды интерфейс не реализуют и пропускаются. Результат сохраняется в `ServerInfo` через `SetLoadedModels` (atomic) и публикуется как `ServerState.loaded_models` / `loaded_models_probed`. Ошибка пробы не фатальна — не влияет на `healthy` и не затирает прошлый снимок.

## 9. TUI ↔ Daemon (IPC)

Daemon поднимает дополнительный WebSocket-эндпоинт (`/admin/stream`) на основном HTTP-порту (либо отдельном порту, см. конфиг).

WebSocket-библиотека: `github.com/coder/websocket` (минималистичная, идиоматичная для Go 1.21+, без зависимостей).

TUI (Bubble Tea) подключается через тот же бинарник в режиме `proxylm tui --connect ...`. При подключении daemon немедленно отправляет `state_snapshot`-конверт (поле `type`, поле `time`, поле `payload` с массивами `servers` и `requests`). TUI может запросить свежий снапшот в любой момент, отправив `{"type": "request_snapshot", "time": "<RFC3339>"}` — это механизм F5-refresh.

**Авто-переподключение TUI:** при разрыве WS-соединения TUI-клиент выполняет бесконечный экспоненциальный backoff (1 с, 2 с, …, cap 30 с). В заголовке показывается `connecting…` / `reconnecting…` / `live`. Выход — только через `q` / `F10` / Ctrl-C.

Аутентификация: тот же Bearer-механизм, но используется выделенный admin-ключ.

Publisher на стороне daemon — отдельная goroutine с входящим `chan Event`; core-модули (scheduler, router, retry) шлют события неблокирующе (с защитой от backpressure: drop при переполнении буфера, в лог — `event_drop`).

## 10. БД (SQLite)

Драйвер: **`modernc.org/sqlite`** — pure-Go, без CGO. Доступ через стандартный пакет `database/sql` (драйвер регистрируется через `import _ "modernc.org/sqlite"`).

Миграции — `*.sql` файлы под `internal/storage/migrations/`, embedded в бинарник через `//go:embed migrations/*.sql`. Применяются последовательно при первом запуске и при последующих стартах (no-op, если версия совпадает).

```sql
-- 0001_init.sql
CREATE TABLE IF NOT EXISTS requests (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id      TEXT UNIQUE NOT NULL,        -- UUID
  client_name     TEXT,                         -- по API-ключу
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

-- 0002_model_reloaded.sql (миграция v0.7.0)
ALTER TABLE requests ADD COLUMN model_reloaded INTEGER NOT NULL DEFAULT 0;
-- Backfill = 0: для старых строк факт reload неизвестен — безопасный дефолт.
```

Очередь — **только in-memory** (рестарт = клиенты получат ошибку и сами повторят).

Запись истории — асинхронная: горутина-писатель читает из `chan HistoryEvent`, делает batch-инсерты (или одиночные с PRAGMA `synchronous = NORMAL`).

## 11. Конфиг

См. `config.example.yaml`. Секции: `proxy`, `auth`, `routing`, `retry`, `discovery`, `storage`, `tui`, `compat`, `backends`.

Загрузка: `gopkg.in/yaml.v3` → типизированная Go-структура → ручная валидация (порт > 0, непустые имена ключей, валидные URL'ы бэкендов). При отсутствии файла рядом с бинарником — daemon создаёт его из встроенного шаблона (`//go:embed config.example.yaml`) и логирует предупреждение.

Путь поиска конфига:
1. `--config <path>` — явное переопределение.
2. `<dir(executable)>/config.yaml` — рядом с бинарником (default, portable).

БД: `storage.database_path` (по умолчанию — `./proxylm.db` относительно бинарника).

## 12. Команды CLI

```
proxylm serve   [--config config.yaml] [--host ...] [--port ...]
proxylm tui     [--connect ws://host:port] [--token ...] [--config ...]
proxylm config  init                                # генерирует config.example.yaml
proxylm config  validate [--config config.yaml]     # проверяет конфиг
proxylm service install   [--config config.yaml]    # регистрирует в Service Manager / systemd / launchd
proxylm service uninstall
proxylm service start
proxylm service stop
proxylm service status
proxylm version
```

Все команды реализованы через `spf13/cobra`. `service *` использует `github.com/kardianos/service` — единый API под Windows Service, systemd, launchd, OpenRC, SysV.

## 13. Структура кода

```
ProxyLM.GO/
├── go.mod
├── go.sum
├── main.go                       # cobra root + version embedded via -ldflags
├── README.md
├── CLAUDE.md
├── config.example.yaml
├── cmd/                          # cobra-команды (тонкие обёртки)
│   ├── root.go
│   ├── serve.go
│   ├── tui.go
│   ├── config.go
│   ├── service.go
│   └── version.go
├── internal/
│   ├── config/                   # YAML парсинг + валидация + автогенерация
│   ├── logging/                  # log/slog setup (JSON handler, level)
│   ├── core/
│   │   ├── models.go             # RequestRecord, ServerInfo, ModelInfo, статусы
│   │   ├── scheduler.go          # per-server worker (goroutine), drain-then-switch; ставит JobResult.ModelReloaded
│   │   ├── router.go             # выбор сервера для (model)
│   │   ├── retry.go              # backoff + failover политика
│   │   ├── discovery.go          # периодический poll /v1/models
│   │   ├── perf.go               # PerfTracker: линейная регрессия (server, model) → PerfStats/ModelSummary
│   │   └── backends/
│   │       ├── backend.go        # интерфейс Backend
│   │       ├── openai.go         # клиент к OpenAI-совместимым (LM Studio, Ollama и т. д.)
│   │       └── anthropic.go      # клиент к Anthropic-совместимым (type: anthropic)
│   ├── api/
│   │   ├── server.go             # net/http + chi, lifecycle, graceful shutdown
│   │   ├── auth.go               # middleware Bearer + x-api-key (двойная аутентификация)
│   │   ├── routes_openai.go      # /v1/chat/completions, /v1/completions, /v1/embeddings, /v1/models
│   │   ├── routes_anthropic.go   # хендлер /v1/messages (Anthropic Messages API)
│   │   ├── routes_admin.go       # /admin/stream (WebSocket)
│   │   ├── routes_health.go      # /healthz
│   │   ├── streaming.go          # SSE-проксирование + token counting (протокол OpenAI)
│   │   ├── streaming_translate.go # SSE-проксирование с кросс-протокольной трансляцией
│   │   └── translate/
│   │       ├── request.go        # трансляция структур запросов OpenAI↔Anthropic
│   │       ├── response.go       # трансляция не-streaming ответов OpenAI↔Anthropic
│   │       └── stream.go         # интерфейс StreamTranslator + stateful трансляция событий
│   ├── storage/
│   │   ├── db.go                 # подключение, миграции (//go:embed migrations/*.sql)
│   │   ├── history.go            # запись/чтение requests (async writer)
│   │   └── migrations/
│   │       ├── 0001_init.sql
│   │       └── 0002_model_reloaded.sql  # ALTER TABLE requests ADD COLUMN model_reloaded INTEGER NOT NULL DEFAULT 0
│   ├── ipc/
│   │   ├── messages.go           # типы JSON-сообщений (Envelope, state_snapshot, request_snapshot, hello, ...)
│   │   ├── server.go             # publisher на стороне daemon
│   │   └── client.go             # WebSocket-клиент (используется TUI)
│   ├── tui/
│   │   ├── app.go                # Bubble Tea Model/Update/View
│   │   ├── widgets.go            # HeaderBar, RequestTable, InfoPane (lipgloss + bubbles)
│   │   ├── styles.go             # lipgloss-стили
│   │   └── keys.go               # хоткеи (F5, F10, q, /)
│   └── service/
│       └── service.go            # kardianos/service интеграция
├── scripts/
│   ├── build.ps1                 # сборка под Windows
│   ├── build.sh                  # сборка под Linux/macOS
│   └── build-all.ps1             # кросс-компиляция все цели
└── test/
    └── integration/
        └── api_e2e_test.go       # mock backends через httptest.Server
```

Тесты пакетов лежат рядом с кодом (Go convention: `scheduler.go` + `scheduler_test.go`).

## 14. Стек

| Слой           | Библиотека                                    |
|----------------|-----------------------------------------------|
| Язык           | Go 1.25+ (минимум диктует `modernc.org/sqlite`) |
| HTTP server    | `net/http` (stdlib, Go 1.22 mux) + `github.com/go-chi/chi/v5` |
| HTTP client    | `net/http` (stdlib) + per-backend `*http.Client` |
| WebSocket      | `github.com/coder/websocket`                  |
| TUI            | `github.com/charmbracelet/bubbletea` + `lipgloss` + `bubbles` |
| SQLite         | `modernc.org/sqlite` (pure-Go, без CGO)       |
| Конфиг (YAML)  | `gopkg.in/yaml.v3`                            |
| CLI            | `github.com/spf13/cobra`                      |
| Service install| `github.com/kardianos/service`                |
| UUID           | `github.com/google/uuid`                      |
| Логирование    | `log/slog` (stdlib, JSON handler)             |
| Тесты          | stdlib `testing` + table-driven; integration через `net/http/httptest` |
| Линт           | `gofmt`, `go vet`, `golangci-lint`            |

Go ≥ 1.25 фактически требуется к компилятору (минимум диктует `modernc.org/sqlite` ≥ 1.50). Языковые фичи кода не выходят за пределы Go 1.22.

Зависимости минимизированы: всё ядро HTTP-server/client — stdlib. Сторонние библиотеки — только там, где stdlib неудобен или отсутствует (WebSocket, TUI, SQLite, CLI, YAML).

## 15. ASCII-мокап TUI

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

Изменения относительно v0.1.0:

- Колонка **RM** (Reload Model) между `Server` и `Queued`: `✓` если запрос был диспатчирован со сменой модели (`model_reloaded = true`), `—` иначе.
- Глиф очереди `…` (U+2026, один символ-cell) вместо `⏳` (emoji wide-character, 2 ячейки) — исправляет сдвиг колонок в терминалах, которые рисуют emoji в две ячейки ширины.
- Шапка сервера показывает метрики регрессии: `t_load · ↓tok_in/s · ↑tok_out/s`. Если `PerfOK=false` или модель не загружена — строка пустая. Поле `t_load` различает три случая (v0.12.0): `—` (reload вообще не наблюдался — оценить нельзя), `1.4s*` (оценка по менее чем 3 reload-наблюдениям — низкая уверенность) и `1.4s` (≥ 3 наблюдений — надёжно).
- В нижней Info-панели (детали сервера) per-model таблица выводит `tok/s` как `значение±погрешность`; в отдельный цвет (teal) красится **только часть `±погрешность`** (граница 95% доверительного интервала) — чтобы точечная оценка оставалась читаемой, а неопределённость выделялась (v0.12.1). Метрики в строках списка серверов остаются нейтрального тусклого цвета.
- **Оверлей моделей** (хоткей `m`, v0.12.0): центрированный оверлей со списком всех обнаруженных моделей выбранного сервера; активная модель помечается `▶`. Закрывается по `m` / `Esc` / `q`.
- Активный сервер в шапке маркируется `▸`.
- **Различимые цвета серверов** (v0.13.0): каждый чип сервера (и колонка `Server` в таблице запросов) красится по индексу сервера в отсортированном по приоритету списке из 12-цветной палитры — вместо прежнего хэша имени, который давал повтор цвета уже на 3–4 серверах.
- **Пульсирующая лампа работы** (v0.13.0): работающий (in-flight) healthy-сервер показывает `●` с пульсирующей яркостью (цикл ~640 мс), простаивающий — ровный `●`. Управляется анимационным тиком ~160 мс, который работает только пока есть хотя бы один in-flight сервер.
- **Сортировка серверов по приоритету** (v0.13.0): по возрастанию `ServerState.priority` (меньше = выше предпочтение, наверху), tiebreak по имени — детерминированный порядок, независимый от порядка в конфиге.
- **Индикатор загруженной модели** (v0.13.0): нижняя Info-панель сервера показывает `In memory: …` (модели, реально находящиеся в памяти по данным нативной пробы), `n/a` (бэкенд пробу не поддерживает) или `— (none)`. Если `current_model` прокси уже не в памяти (выгрузилась по idle), чип помечает её `⏏`.
- **Хвост генерации** (хоткей `t`, v0.13.0): для выполняющегося streaming-запроса Info-панель запроса может показать последние ~160 сгенерированных символов (`last_tokens`). По умолчанию скрыт (контент ответа — приватность); хранится только в памяти.

Bubble Tea-архитектура: `Model` хранит снапшот `[]ServerView`, `[]RequestRow` и детальное состояние. `Update(msg)` обрабатывает три источника:
1. WebSocket-сообщения (`state_snapshot`) — через `tea.Cmd` с горутиной-читателем.
2. Tick для периодических задач (TUI auto-hide completed по `tui.show_completed_minutes`).
3. Ключевые события (`F1`, `F5`, `Tab`, `F10`, `q`, `/`, `↑`, `↓`, `Enter`, `Esc`).

`View()` рендерит весь TUI через `lipgloss`-стили (border, foreground, padding).

### Три панели

TUI поддерживает три именованных панели: `paneHeader`, `paneRequests`, `paneInfo`. `Tab` циклически переключает фокус: Header → Requests → Info → Header.

Когда активна `paneHeader`:
- `↑` / `↓` выбирают сервер в шапке; выбранный получает маркер `▸` и яркую рамку (`StyleBorderActive`).
- `Enter` показывает детали сервера (per-model статистику) в `paneInfo`.
- Mouse wheel в области шапки также меняет выбранный сервер.

Когда активна `paneRequests`:
- `↑` / `↓` навигируют по списку запросов.
- `Enter` показывает детали запроса в `paneInfo`.

`paneInfo` — нижняя правая информационная панель, не modal-overlay.

`F1` открывает help-overlay со списком хоткеев. `Esc` / `F1` / `q` закрывают overlay.

## 16. Сборка и распространение

**Однострочная сборка:**
```bash
go build -ldflags "-s -w -X main.version=$(git describe --tags --always)" -o bin/proxylm .
```

**Кросс-компиляция:**
```bash
GOOS=windows GOARCH=amd64 go build -o bin/proxylm-windows-amd64.exe .
GOOS=linux   GOARCH=amd64 go build -o bin/proxylm-linux-amd64 .
GOOS=linux   GOARCH=arm64 go build -o bin/proxylm-linux-arm64 .
GOOS=darwin  GOARCH=amd64 go build -o bin/proxylm-darwin-amd64 .
GOOS=darwin  GOARCH=arm64 go build -o bin/proxylm-darwin-arm64 .
```

Поскольку SQLite-драйвер — pure-Go, **CGO_ENABLED=0** допустимо (и предпочтительно для статической компиляции).

## 17. Метрики производительности (регрессия)

Модуль `internal/core/perf.go` оценивает производительность сервера по паре `(server_name, model)` методом линейной регрессии. Все наблюдения хранятся в памяти (`[]perfObservation`) — при рестарте daemon'а история обнуляется.

### Модель

Для каждого завершённого запроса записывается наблюдение:

```
(t_all, k_in, k_out, loaded)
```

- `t_all` — полное время выполнения запроса на сервере (мс, поле `duration_ms`).
- `k_in` — число prompt-токенов (`input_tokens`).
- `k_out` — число completion-токенов (`output_tokens`).
- `loaded` ∈ {0, 1} — 1 если в начале dispatch была смена модели (`model_reloaded = true`).

Линейная модель:

```
loaded · t_load  +  b · k_in  +  c · k_out  ≈  t_all
```

Параметры θ = (t_load, b, c) минимизируют Σ(t_all − fit)². Решение через нормальные уравнения X^T X · θ = X^T y, метод Крамера.

### Три режима

| Условие | Размер системы | Результат |
|---------|----------------|-----------|
| ≥ `perfMinSamples = 3` наблюдений; хотя бы одно `loaded=1`; ≥ 2 чистых `loaded=0` | **двухэтапная** (v0.12.0) | `OK: true`, `TLoadMs` из остатков reload |
| ≥ 3 наблюдений; хотя бы одно `loaded=1`; < 2 чистых `loaded=0` | 3×3 NNLS (совместная, fallback) | `OK: true`, `TLoadMs, KInMsTok, KOutMsTok` |
| ≥ 3 наблюдений; все с `loaded=0` или 3×3 сингулярна | 2×2 (fallback без `t_load`) | `OK: true`, `TLoadMs = 0` |
| < 3 наблюдений | — | `OK: false` |

### Двухэтапная оценка `t_load` (v0.12.0)

Из-за INV-2 (дренируем текущую модель до конца, прежде чем переключать) события reload редки: на установившемся профиле сервера обычно ровно **одно** наблюдение `loaded=1` на сотни `loaded=0`. Совместный 3-var NNLS определяет `t_load` фактически по одному остатку — дисперсия огромна — и при шуме регулярно «прибивает» его к `0`, из-за чего в TUI был `—`, хотя load-время физически всегда есть.

При наличии ≥ 2 чистых наблюдений `loaded=0` `fitRegression` вместо этого:

1. Оценивает токенные коэффициенты `k_in`, `k_out` по чистым точкам `loaded=0` (их много, и они не загрязнены load-временем) через 2-var NNLS.
2. Оценивает `t_load` как clamped (≥ 0) среднее остатков `t_all − k_in·in − k_out·out` по наблюдениям `loaded=1`.

Это даёт стабильную оценку `t_load` из единственного reload. TUI маркирует уверенность оценки через `ServerState.t_load_loaded` (число reload-наблюдений): `< 3` → завершающая `*`.

### Публичные типы

```go
type PerfStats struct {
    Samples    int
    Loaded     int     // число reload-наблюдений
    TLoadMs    float64 // оценка t_load (0 если нет данных)
    KInMsTok   float64 // мс/tok для prompt
    KOutMsTok  float64 // мс/tok для completion
    OK         bool
}

type ModelSummary struct {
    Model string
    Stats PerfStats
}
```

Метод `ServerSummary(server string) []ModelSummary` возвращает все модели сервера, отсортированные по `Samples DESC` — используется для server-detail modal в TUI.

В IPC `ServerState` перф-поля (см. `API.ru.md` §2.2) заполняются через `buildSnapshot`: `tok_in_per_sec = 1000 / KInMsTok` (нули и отрицательные → 0), аналогично `tok_out_per_sec`.

## 18. Нерешённые вопросы / отложено на будущее

- Метрики Prometheus (`/metrics`).
- Web UI вместо/помимо TUI.
- Поддержка native Ollama API endpoints (`/api/generate`).
- Приоритизация по клиенту (квоты).
- Persistence очереди при рестарте (отдельно обсуждалось — намеренно не делаем).
- Опциональная сборка с CGO-SQLite (`mattn/go-sqlite3`) под build tag — для high-throughput инсталляций.
- Авто-update механизм (скачивание новых релизов с GitHub).
