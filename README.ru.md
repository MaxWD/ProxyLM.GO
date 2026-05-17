# ProxyLM.GO

ProxyLM.GO — это посредник между вашими приложениями и сервисами больших языковых моделей (LLM). Для приложения он выглядит как обычный OpenAI-совместимый сервер, но за его спиной может стоять любое количество фактических серверов — как локальные (LM Studio, Ollama), так и облачные. Главное, чтобы сервер поддерживал OpenAI API. ProxyLM.GO сам решает, к какому из них адресовать каждый запрос.

Главная задача — **выжать максимум из имеющейся инфраструктуры LLM-серверов**: не дать каждому серверу тратить время на постоянное переключение моделей и одновременно равномерно нагружать все доступные серверы, чтобы результат приходил к пользователю как можно быстрее. Каждая LLM-модель занимает много памяти видеокарты; если на одном сервере доступно несколько моделей и приложения дёргают их вперемешку, серверу приходится выгружать одну модель и подгружать другую буквально на каждом запросе — это занимает секунды-минуты, и всё это время пользователи ждут. ProxyLM.GO собирает входящие запросы в очередь и **группирует их по модели**: сначала все запросы к модели A, потом все к модели B — модель загружается один раз и обрабатывает всю очередь, прежде чем её сменят. Если ту же модель умеют несколько серверов, запросы распределяются между ними параллельно и каждый новый запрос уходит к наименее загруженному — простаивающих машин не остаётся, и одна и та же ферма GPU отдаёт результаты заметно быстрее.

Поставляется одним исполняемым файлом, который работает фоновой службой и имеет встроенный консольный интерфейс мониторинга. Кросс-компилируется под Windows, Linux и macOS.

## Возможности

- OpenAI-совместимый API: `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/models`, `/healthz`
- **Model-affinity queue**: запросы для текущей модели сервера обслуживаются до конца, прежде чем модель будет переключена
- Несколько backend-серверов, авто-discovery моделей через `/v1/models`, failover на резервный сервер
- Streaming (SSE) с прозрачным проксированием чанков
- Bearer-аутентификация по поименованным API-ключам (имя клиента попадает в логи и историю; ключ — нет)
- История запросов в SQLite, retention 30 дней (настраивается)
- btop-подобный TUI на Bubble Tea — отдельный процесс через WebSocket к daemon'у
- Установка как Windows Service / systemd unit / launchd job одной командой (`proxylm service install`)
- Portable: конфиг и БД лежат рядом с бинарником

## Архитектура

```
  клиенты (service-a, service-b, ...)
            │  HTTP / OpenAI-совместимый формат
            ▼
   ┌─────────────────────────────────┐
   │ ProxyLM.GO daemon               │
   │  AuthN ─► Router ─► per-server  │       ┌───────────┐
   │            queues + workers ────┼──────►│ srv1 (LM) │
   │              (drain current     │       └───────────┘
   │               model fully)      │       ┌───────────┐
   │  Discovery / SQLite / IPC ──────┼──────►│ srv2 (Ol) │
   │                                 │       └───────────┘
   └────────────────┬────────────────┘
                    │ WebSocket /admin/stream
                    ▼
              TUI (Bubble Tea)
```

Подробности — в [`docs/ARCHITECTURE.ru.md`](./docs/ARCHITECTURE.ru.md).

## Установка

### Готовый бинарник

Скачать соответствующий артефакт для своей платформы (`proxylm-<os>-<arch>[.exe]`) и положить в произвольную папку. Никакие рантаймы / интерпретаторы не нужны.

### Сборка из исходников

Требуется Go 1.22+:

```
git clone <repo-url> proxylm.go
cd proxylm.go
go build -ldflags "-s -w -X main.version=dev" -o bin/proxylm .
```

Или скрипт:

```
# Windows
.\scripts\build.ps1

# Linux / macOS
./scripts/build.sh
```

### Кросс-компиляция (без CGO, один файл на каждую цель)

```
GOOS=windows GOARCH=amd64 go build -o bin/proxylm-windows-amd64.exe .
GOOS=linux   GOARCH=amd64 go build -o bin/proxylm-linux-amd64    .
GOOS=linux   GOARCH=arm64 go build -o bin/proxylm-linux-arm64    .
GOOS=darwin  GOARCH=arm64 go build -o bin/proxylm-darwin-arm64   .
```

Или одной командой:

```
.\scripts\build-all.ps1
```

## Быстрый старт

1. Положить бинарник `proxylm` (или `proxylm.exe`) в произвольную папку.

2. Запустить daemon:

   ```
   ./proxylm serve
   ```

   При первом запуске рядом с бинарником появятся `config.yaml` (из встроенного шаблона) и `proxylm.db` (SQLite). Откорректировать `config.yaml`: подправить секцию `backends:` под свои IP/порты, поменять API-ключи в `auth.api_keys` и `auth.admin_key`. Перезапустить.

3. В отдельном терминале — TUI:

   ```
   ./proxylm tui --connect ws://localhost:8080 --token <admin_key>
   ```

Пример запроса:

```
curl -H "Authorization: Bearer sk-proxy-replace-me-aaaaa" \
     -H "Content-Type: application/json" \
     -d '{"model":"qwen2.5:14b","messages":[{"role":"user","content":"hi"}]}' \
     http://localhost:8080/v1/chat/completions
```

Больше примеров (streaming, embeddings, `/v1/models`) — в [`docs/API.ru.md`](./docs/API.ru.md) §4.

## TUI

![ProxyLM.GO TUI](docs/img/sh.png)

Текстовая версия того же layout (для поиска по коду и offline-чтения):

```
ProxyLM.GO vX.Y.Z                                                          2026-05-17 14:32:07
╭──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────╮
│ lmstudio   ● qwen2.5-coder-14b-instruct   850ms · ↓12.3 tok/s · ↑51.8 tok/s                                              │
│ ollama     ● llama-3.1-8b-instruct-q4_k_m                                                                                │
│ backup     ✗ idle                                                                                                        │
│ Queued: 2   Running: 1   Done/30m: 4   Failed: 1   Servers: 2/3 healthy                                                  │
╰──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────╯
╭──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────╮
│ Requests                                                                                                                 │
│   #    Client    Model                        Server     Status       RM Queued   Started  Elapsed I→O tok               │
│ ▶ a3f2 webclient qwen2.5-coder-14b-instruct   lmstudio   ▶ running    —  14:31:50 14:31:52 15.2s   512→…                 │
│   7c1e apitest   llama-3.1-8b-instruct-q4_k_m ollama     … queued     —  14:31:55 —        —      —→—                    │
│   d09b botuser   qwen2.5-coder-14b-instruct   lmstudio   … queued     —  14:32:01 —        —      —→—                    │
│   55ab cli-app   gemma-2-9b-it-q4_k_m         lmstudio   ✓ completed  ✓  14:01:10 14:01:11 8.4s    256→1024              │
│   f1e0 tester    mistral-7b-instruct-v0.3     ✗ backup   ✗ failed     —  14:15:22 14:15:23 2.1s    128→—                 │
╰──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────╯
╭──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────╮
│ Info — Request a3f2                                                                                                      │
│ ID           8fa3...a3f2     Created      2026-05-17 14:31:50                                                            │
│ Client       webclient       Started      2026-05-17 14:31:52                                                            │
│ Model        qwen2.5-coder-14b-instruct    Completed    —                                                                │
│ Endpoint     /v1/chat/completions          Queue wait   120ms                                                            │
│ Stream       yes             Prompt tok   512                                                                            │
│ Server       lmstudio        Output tok   …                                                                              │
│ Status       running (1/2)   RM           —                                                                              │
╰──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────╯
F1 Help   F5 Refresh   / Filter   Tab Header/Requests/Info   Click — выбор   q/F10 Quit
```

**Ширины колонок** подобраны для типичного случая:

- `Model` (27 символов) — вмещает канонические имена вроде `qwen2.5-coder-14b-instruct` или `llama-3.1-8b-instruct-q4_k_m` без обрезки квантизационного суффикса.
- `Server` (10 символов) — включает 2-символьный префикс `✗ ` для упавших серверов, оставляя 8 символов на имя.
- `Status` (12 символов) — покрывает самую длинную строку `✗ completed` плюс один пробел запаса.
- `Tokens` (11 символов) — формат `NNN→NNNN`; во время streaming output показывается `…` вместо числа.
- `RM` (2 символа) — однобитовая отметка «модель грузилась для задачи» (`✓` / `—`) плюс разделитель.

Хоткеи: `F1` — справка, `F5` — переподключение / refresh снапшота, `/` — фильтр по таблице, `Tab` — переключение Header/Requests/Info, клик мышкой — выбор строки, `F10` или `q` — выход.

Завершённые запросы скрываются из таблицы через `tui.show_completed_minutes` (по умолчанию 30 минут) — но остаются в SQLite.

В `cmd.exe` юникод-глифы (`●`, `✓`, `▶`, `…`, `↓`, `↑`, `→`) могут рендериться некорректно. Включи ASCII-fallback:

```
set PROXYLM_NO_UNICODE=1
proxylm.exe tui --connect ws://localhost:8080 --token <admin_key>
```

## Конфигурация

Полный пример с комментариями — [`config.example.yaml`](./config.example.yaml). Ключевые секции:

| Секция            | Назначение                                                         |
|-------------------|--------------------------------------------------------------------|
| `proxy`           | `host`, `port`, `log_level`                                         |
| `auth.api_keys`   | список Bearer-ключей с `name`/`key`                                 |
| `auth.admin_key`  | отдельный ключ для TUI и `/admin/*` эндпоинтов                      |
| `routing.strategy`| `model_affinity_least_busy` (default), `least_busy`, `round_robin`, `deferred_model_then_capable` |
| `retry`           | `max_attempts`, `initial_backoff_ms`, `max_backoff_ms` (rolling exclusion: упавший сервер пропускается ровно на 1 след. попытку) |
| `discovery`       | `interval_seconds`, `unhealthy_after_failed_polls`                  |
| `storage`         | `database_path`, `history_retention_days`                           |
| `tui`             | `show_completed_minutes`                                            |
| `compat`          | `response_format_mode` (`passthrough`, `normalize_json_object`, `strict_reject`) |
| `backends`        | список серверов: `name`, `url`, `priority`, `type`, `timeout_seconds`, `models` |

CLI-флаги `--host` / `--port` у `serve` переопределяют YAML.

Для mixed-пула OpenAI-совместимых бэкендов полезен `compat.response_format_mode`:
- `passthrough` — прокси не меняет `response_format`.
- `normalize_json_object` — конвертирует `response_format.type=json_object` в `json_schema` перед upstream-вызовом.
- `strict_reject` — возвращает ранний `400` на прокси, если `response_format.type` не `json_schema|text`.

## Запуск как сервис

ProxyLM.GO поддерживает регистрацию в системном Service Manager через единую CLI:

```
proxylm service install     # регистрирует службу под Windows / systemd / launchd
proxylm service start
proxylm service status
proxylm service stop
proxylm service uninstall
```

Под капотом — `github.com/kardianos/service`, который определяет ОС и работает соответственно:
- **Windows:** Service Control Manager (`sc.exe`-эквивалент). После `install` служба видна в `services.msc`.
- **Linux:** systemd unit в `/etc/systemd/system/proxylm.service` (нужны root-права для `install`/`uninstall`).
- **macOS:** launchd plist в `~/Library/LaunchAgents/`.

Имя службы — `proxylm`. Рабочий каталог — каталог бинарника; конфиг и БД лежат там же.

Конфиг с ключами должен иметь права `0600` (Linux/macOS) и принадлежать пользователю, под которым работает служба.

## Разработка

Установка зависимостей:

```
go mod download
```

Тесты и линт:

```
go test ./...
go test -cover ./internal/core/...
gofmt -l .
go vet ./...
golangci-lint run
```

Структура каталогов и обзор модулей — [`docs/ARCHITECTURE.ru.md`](./docs/ARCHITECTURE.ru.md) §12.

## Документация

- Архитектура: [`docs/ARCHITECTURE.ru.md`](./docs/ARCHITECTURE.ru.md)
- Полное ТЗ (FR / NFR / инварианты / acceptance-критерии): [`docs/SRS.ru.md`](./docs/SRS.ru.md)
- API-контракт (OpenAI и admin/IPC): [`docs/API.ru.md`](./docs/API.ru.md)
- Роли и владельцы документов: [`docs/AGENTS.ru.md`](./docs/AGENTS.ru.md)
- Идеи и задачи на будущее (парковка, не roadmap): [`docs/FUTURE.ru.md`](./docs/FUTURE.ru.md)

## Лицензия

MIT.

## Статус

Pre-1.0 alpha. Архитектурный скелет и документация готовы; реализация ядра в работе. Репозиторий приватный — issue tracker и канал обратной связи согласовываются отдельно.
