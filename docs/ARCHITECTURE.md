# ProxyLM.GO — Архитектура

## 1. Назначение

Прокси-сервер на Go перед локальными LLM-серверами (LM Studio, Ollama). Главная задача — **сериализовать запросы по моделям**, чтобы избежать постоянной выгрузки/загрузки моделей в VRAM.

Основные свойства:
- OpenAI-совместимый API на входе (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/models`)
- Поддержка streaming (SSE)
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
│ service-a, service-b, ...                                │
└──────────────────────┬───────────────────────────────────┘
                       │ HTTP, OpenAI-совместимый формат
                       ▼
┌──────────────────────────────────────────────────────────┐
│ ProxyLM.GO Daemon  (net/http + chi + goroutines)         │
│                                                          │
│  HTTP API ─► AuthN ─► Router ─► PerServerQueue           │
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
│                  retry + failover                        │
│                           │                              │
│  Discovery ──► ModelMap   │   SQLite history             │
│                           │   (modernc.org/sqlite)       │
│                           │                              │
│  IPC server (WebSocket) ◀─┴─►  TUI client (Bubble Tea)   │
└─────────────────────┬────────────────────────────────────┘
                      │ HTTP (OpenAI/Ollama API)
                      ▼
┌──────────────────────────────────────────────────────────┐
│ Backend LLM серверы                                      │
│ srv1 (LM Studio), srv2 (Ollama), ...                     │
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

Это и есть `model_affinity_least_busy` — стратегия по умолчанию. Реализация — чистая функция от `[]*ServerInfo` и `model`, не блокирует воркеры; читает `CurrentModel` через `atomic.Pointer[string].Load()`.

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

## 7. Discovery

- Раз в `discovery.interval_seconds` (default 30s) опрашиваем `/v1/models` каждого сервера.
- Один общий `time.Ticker`, в цикле — fan-out goroutines (по одной на сервер), результат собирается в `ModelMap: map[string]map[string]struct{}`.
- Используется роутером.
- При недоступности сервера N циклов подряд → флаг `unhealthy.Store(false)`.
- Discovery-цикл получает `context.Context`, корректно завершается при shutdown.

## 8. TUI ↔ Daemon (IPC)

Daemon поднимает дополнительный WebSocket-эндпоинт (`/admin/stream`) на основном HTTP-порту (либо отдельном порту, см. конфиг).

WebSocket-библиотека: `github.com/coder/websocket` (минималистичная, идиоматичная для Go 1.21+, без зависимостей).

TUI (Bubble Tea) подключается через тот же бинарник в режиме `proxylm tui --connect ...`, получает JSON-сообщения двух видов:
- `state`: снапшот + дифф (запросы, серверы, статистика).
- `log`: строка лога.

Аутентификация: тот же Bearer-механизм, но используется выделенный admin-ключ.

Publisher на стороне daemon — отдельная goroutine с входящим `chan Event`; core-модули (scheduler, router, retry) шлют события неблокирующе (с защитой от backpressure: drop при переполнении буфера, в лог — `event_drop`).

## 9. БД (SQLite)

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

## 10. Конфиг

См. `config.example.yaml`. Секции: `proxy`, `auth`, `routing`, `retry`, `discovery`, `storage`, `tui`, `compat`, `backends`.

Загрузка: `gopkg.in/yaml.v3` → типизированная Go-структура → ручная валидация (порт > 0, непустые имена ключей, валидные URL'ы бэкендов). При отсутствии файла рядом с бинарником — daemon создаёт его из встроенного шаблона (`//go:embed config.example.yaml`) и логирует предупреждение.

Путь поиска конфига:
1. `--config <path>` — явное переопределение.
2. `<dir(executable)>/config.yaml` — рядом с бинарником (default, portable).

БД: `storage.database_path` (по умолчанию — `./proxylm.db` относительно бинарника).

## 11. Команды CLI

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

## 12. Структура кода

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
│   │       └── openai.go         # клиент к OpenAI-совместимым (LM Studio, Ollama)
│   ├── api/
│   │   ├── server.go             # net/http + chi, lifecycle, graceful shutdown
│   │   ├── auth.go               # middleware Bearer
│   │   ├── routes_openai.go      # /v1/*
│   │   ├── routes_admin.go       # /admin/stream (WebSocket)
│   │   ├── routes_health.go      # /healthz
│   │   └── streaming.go          # SSE-проксирование + token counting
│   ├── storage/
│   │   ├── db.go                 # подключение, миграции (//go:embed migrations/*.sql)
│   │   ├── history.go            # запись/чтение requests (async writer)
│   │   └── migrations/
│   │       ├── 0001_init.sql
│   │       └── 0002_model_reloaded.sql  # ALTER TABLE requests ADD COLUMN model_reloaded INTEGER NOT NULL DEFAULT 0
│   ├── ipc/
│   │   ├── messages.go           # типы JSON-сообщений (state_snapshot/diff/log_line/...)
│   │   ├── server.go             # publisher на стороне daemon
│   │   └── client.go             # WebSocket-клиент (используется TUI)
│   ├── tui/
│   │   ├── app.go                # Bubble Tea Model/Update/View
│   │   ├── widgets.go            # HeaderBar, RequestTable, LogPane (lipgloss + bubbles)
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

## 13. Стек

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

## 14. ASCII-мокап TUI

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
├─ Log ────────────────────────────────────────────────────────────────────────────────────────┤
│ 14:02:11 INFO  api      accepted req#0043 client=service-b model=llama3.1:8b               │
│ 14:02:11 INFO  router   chose srv2 (model loaded, queue=0)                                  │
│ 14:02:11 INFO  srv2     model swap: idle → llama3.1:8b                                      │
│ 14:01:08 INFO  srv1     completed req#0042 6.3s 82 tok                                      │
│ 14:01:02 INFO  api      accepted req#0042 client=service-a model=qwen2.5:14b                │
└──────────────────────────────────────────────────────────────────────────────────────────────┘
   Tab Header/Requests/Log  F5 Refresh  F10 Quit
```

Изменения относительно v0.1.0:

- Колонка **RM** (Reload Model) между `Server` и `Queued`: `✓` если запрос был диспатчирован со сменой модели (`model_reloaded = true`), `—` иначе.
- Глиф очереди `…` (U+2026, один символ-cell) вместо `⏳` (emoji wide-character, 2 ячейки) — исправляет сдвиг колонок в терминалах, которые рисуют emoji в две ячейки ширины.
- Шапка сервера показывает метрики регрессии: `t_load · ↓tok_in/s · ↑tok_out/s`. Если `PerfOK=false` или модель не загружена — строка пустая; если нет reload-наблюдений — `t_load` заменяется на `—`.
- Активный сервер в шапке маркируется `▸`.

Bubble Tea-архитектура: `Model` хранит снапшот `[]ServerView`, `[]RequestRow`, кольцевой буфер логов. `Update(msg)` обрабатывает три источника:
1. WebSocket-сообщения (`state_snapshot` / `state_diff` / `log_line`) — через `tea.Cmd` с горутиной-читателем.
2. Tick для периодических задач (TUI auto-hide completed по `tui.show_completed_minutes`).
3. Ключевые события (`Tab`, `F5`, `F10`, `q`, `/`, `↑`, `↓`, `Enter`).

`View()` рендерит весь TUI через `lipgloss`-стили (border, foreground, padding).

### Интерактивная шапка (paneHeader)

Начиная с v0.7.0, TUI поддерживает три именованных панели: `paneHeader`, `paneRequests`, `paneLog`. `Tab` циклически переключает фокус: Header → Requests → Log → Header.

Когда активна `paneHeader`:
- `↑` / `↓` выбирают сервер в шапке; выбранный получает маркер `▸` и яркую рамку (`StyleBorderActive`).
- `Enter` открывает server-detail modal с таблицей per-model статистики: `Model | Reqs | Load | t_load | ↓tok/s | ↑tok/s`.
- Mouse wheel в области шапки также меняет выбранный сервер.

Modal закрывается на `Esc` / повторный `Enter` / `q`.

## 15. Сборка и распространение

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

## 16. Метрики производительности (регрессия)

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
| Минимум `perfMinSamples = 3` наблюдений; хотя бы одно с `loaded=1`; 3×3 не сингулярна | 3×3 | `PerfStats{OK: true, TLoadMs, KInMsTok, KOutMsTok}` |
| ≥ 3 наблюдений; все с `loaded=0` или 3×3 сингулярна | 2×2 (fallback без `t_load`) | `OK: true`, `TLoadMs = 0` |
| < 3 наблюдений | — | `OK: false` |

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

В IPC `ServerState` перф-поля (см. `API.md` §2.2) заполняются через `buildSnapshot`: `tok_in_per_sec = 1000 / KInMsTok` (нули и отрицательные → 0), аналогично `tok_out_per_sec`.

## 17. Нерешённые вопросы / отложено на будущее

- Метрики Prometheus (`/metrics`).
- Web UI вместо/помимо TUI.
- Поддержка native Ollama API endpoints (`/api/generate`).
- Приоритизация по клиенту (квоты).
- Persistence очереди при рестарте (отдельно обсуждалось — намеренно не делаем).
- Опциональная сборка с CGO-SQLite (`mattn/go-sqlite3`) под build tag — для high-throughput инсталляций.
- Авто-update механизм (скачивание новых релизов с GitHub).
