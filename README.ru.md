# ProxyLM.GO

[![CI](https://github.com/MaxWD/ProxyLM.GO/actions/workflows/ci.yml/badge.svg)](https://github.com/MaxWD/ProxyLM.GO/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/MaxWD/ProxyLM.GO?include_prereleases)](https://github.com/MaxWD/ProxyLM.GO/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/MaxWD/ProxyLM.GO)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Мультипротокольный LLM-прокси (OpenAI + Anthropic API) перед любым LLM-бэкендом — локальным (LM Studio, Ollama, vLLM, llama.cpp) или удалённым (OpenRouter, Groq, Together AI, OpenAI, Anthropic). Model-aware очередь, кросс-протокольная трансляция, retry/failover, SSE-streaming, консольный TUI и браузерный дашборд. Один portable-бинарник, без CGO.

[English](README.md) · **На русском**

---

## Обзор

ProxyLM.GO встаёт между вашими приложениями и одним или несколькими LLM-серверами, представляясь клиенту обычным OpenAI- или Anthropic-совместимым эндпоинтом, а за кулисами занимается маршрутизацией, очередями и failover'ом между бэкендами. Главная задача — устранить лишние переключения моделей: каждая LLM занимает значительный объём VRAM, и сервер, обслуживающий несколько моделей вперемешку, может тратить секунды-минуты на перезагрузку при каждом запросе. ProxyLM.GO ставит входящие запросы в очередь по серверам и **обслуживает все запросы к уже загруженной модели, прежде чем переключиться** — модель загружается один раз и обрабатывает весь свой бэклог, а запросы к одной и той же модели на нескольких подходящих серверах распределяются параллельно, чтобы держать загрузку GPU высокой.

## Возможности

- **Model-affinity очередь** — воркер каждого сервера обслуживает все запросы в очереди для текущей модели, прежде чем переключиться; предотвращает лишние swap'ы модели (INV-1..INV-3)
- **OpenAI + Anthropic API** — `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/messages`, `/v1/models`, `/healthz`
- **Кросс-протокольная трансляция** — клиенты OpenAI SDK могут обращаться к Anthropic-бэкендам и наоборот, автоматически (см. ниже)
- **Несколько бэкендов** — маршрутизация на любое число серверов; приоритет на бэкенд настраивается (предпочесть локальный сервер облаку, когда оба умеют модель)
- **Авто-discovery** — периодически опрашивает `/v1/models` каждого бэкенда; помечает unhealthy после N неудачных попыток
- **Retry и failover** — экспоненциальный backoff + rolling exclusion упавшего сервера; failover на другой healthy-бэкенд после локальных ретраев (INV-5)
- **SSE streaming** — прозрачное побайтовое проксирование; без буферизации, без retry после отправки первого чанка клиенту (INV-6)
- **Двойная аутентификация** — принимает и `Authorization: Bearer` (OpenAI-стиль), и `x-api-key` (Anthropic-стиль); поимённые API-ключи, имя клиента в логах/истории, но не сам ключ
- **История запросов в SQLite** — pure-Go, без CGO (`modernc.org/sqlite`); настраиваемый retention
- **Bubble Tea TUI + браузерный дашборд** — таблица запросов в реальном времени, статус серверов и лог-стрим, из консольного клиента (`proxylm tui`) или браузера (`proxylm web`)
- **Системная служба** — установка как Windows Service, systemd unit или launchd job одной командой
- **Portable** — конфиг и БД лежат рядом с бинарником; без установки

## Быстрый старт

### 1. Получить бинарник

Скачать готовый архив для своей платформы из [Releases](https://github.com/MaxWD/ProxyLM.GO/releases) и распаковать — рантайм или интерпретатор не нужны:

| Платформа      | Архив                          |
|----------------|----------------------------------|
| Linux x86-64   | `proxylm_linux_x86_64.tar.gz`   |
| Linux ARM64    | `proxylm_linux_arm64.tar.gz`    |
| macOS x86-64   | `proxylm_macos_x86_64.tar.gz`   |
| macOS ARM64    | `proxylm_macos_arm64.tar.gz`    |
| Windows x86-64 | `proxylm_windows_x86_64.zip`    |

Либо собрать самостоятельно — см. [Сборка из исходников](#сборка-из-исходников).

> Внимание: `go install github.com/MaxWD/ProxyLM.GO@latest` не работает — путь Go-модуля локальный (`proxylm`), а не GitHub URL.

### 2. Запустить daemon

```sh
./proxylm serve
```

При первом запуске рядом с бинарником появятся `config.yaml` и `proxylm.db`, созданные из встроенного шаблона.

### 3. Настроить бэкенды

Открыть `config.yaml` и подправить `backends`:

```yaml
backends:
  - name: lm-studio
    url: http://127.0.0.1:1234   # порт LM Studio по умолчанию
    timeout_seconds: 600
    priority: 100                # меньше число = выше предпочтение среди свободных серверов

  - name: ollama
    url: http://127.0.0.1:11434  # порт Ollama по умолчанию (OpenAI-совместимый /v1/* shim)
    timeout_seconds: 600
    priority: 200

  # Anthropic Claude API — укажи type: anthropic для нативного Anthropic-протокола
  # - name: anthropic-cloud
  #   url: https://api.anthropic.com
  #   type: anthropic
  #   api_key: sk-ant-api03-...
  #   priority: 900               # большое число = используется только когда локальные не справляются
```

`type` выбирает wire-протокол: `openai` (по умолчанию — LM Studio, Ollama, vLLM, OpenRouter и т. д.) или `anthropic`. Затем замени placeholder-ключи в `auth.api_keys` и `auth.admin_key` и перезапусти daemon.

### 4. Отправить запрос

```sh
curl -H "Authorization: Bearer sk-proxy-replace-me-aaaaa" \
     -H "Content-Type: application/json" \
     -d '{"model":"qwen2.5:14b","messages":[{"role":"user","content":"hi"}]}' \
     http://localhost:8080/v1/chat/completions
```

Либо через Anthropic Messages API — работает на том же daemon'е независимо от протокола бэкенда:

```sh
curl -H "x-api-key: sk-proxy-replace-me-aaaaa" \
     -H "Content-Type: application/json" \
     -H "anthropic-version: 2023-06-01" \
     -d '{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}' \
     http://localhost:8080/v1/messages
```

Больше примеров (streaming, embeddings, `/v1/models`) — в [docs/API.ru.md](docs/API.ru.md) §4.

## Команды

`proxylm` — единый бинарник; каждый режим — подкоманда.

| Команда | Описание | Типичный вызов |
|---|---|---|
| `serve` | Запустить daemon (HTTP-прокси + IPC WebSocket) | `proxylm serve` |
| `tui` | Подключить консольный TUI к запущенному daemon'у | `proxylm tui --connect ws://localhost:8080 --token <admin_key>` |
| `web` | Открыть браузерный дашборд | `proxylm web --connect ws://localhost:8080 --token <admin_key>` |
| `config init` | Создать `config.yaml` из встроенного шаблона | `proxylm config init` |
| `config validate` | Проверить `config.yaml` | `proxylm config validate` |
| `service install` | Зарегистрировать `proxylm` как системную службу | `proxylm service install` |
| `service uninstall` | Удалить регистрацию службы | `proxylm service uninstall` |
| `service start` / `stop` | Запустить / остановить службу | `proxylm service start` |
| `service status` | Показать статус службы | `proxylm service status` |
| `version` | Показать версию, OS/arch, версию Go | `proxylm version` |

Флаги, о которых стоит знать:

- **`serve`** — `--config <path>` (по умолчанию: `config.yaml` рядом с бинарником).
- **`tui`** — `--connect <ws-url>` (по умолчанию `ws://localhost:8080`), `--token <admin_key>` (обязателен).
- **`web`** — `--connect <url>` (`ws://`/`wss://`/`http://`/`https://`, по умолчанию `ws://localhost:8080`), `--token <admin_key>` (опционален — включает автоподключение), `--listen <host:port>` (по умолчанию `127.0.0.1:8081`), `--no-open` (не открывать браузер автоматически).

## Конфигурация

Полный пример с комментариями — [`config.example.yaml`](config.example.yaml).

| Секция              | Назначение                                                                                |
|---------------------|---------------------------------------------------------------------------------------------|
| `proxy`             | `host`, `port`, `log_level` (debug / info / warning / error)                              |
| `auth.api_keys`     | Поимённые Bearer-ключи для клиентских сервисов                                            |
| `auth.admin_key`    | Отдельный ключ для `tui`, `web` и `/admin/*` эндпоинтов                                    |
| `routing.strategy`  | `model_affinity_least_busy` (по умолчанию), `least_busy`, `round_robin`, `deferred_model_then_capable`, `preserve_model_coverage` |
| `retry`             | `max_attempts`, `initial_backoff_ms`, `max_backoff_ms`; rolling server exclusion (размер 1) |
| `discovery`         | `enabled`, `interval_seconds`, `unhealthy_after_failed_polls`                              |
| `storage`           | `database_path`, `history_retention_days`, `vacuum_on_start`                              |
| `tui`               | `show_completed_minutes` — сколько завершённые запросы остаются видны в таблице            |
| `compat`            | `response_format_mode`: `passthrough` / `normalize_json_object` / `strict_reject`         |
| `backends`          | Список серверов: `name`, `url`, `priority`, `type` (`openai`/`anthropic`), `timeout_seconds`, `api_key`, `models` |

CLI-флаги `--host` / `--port` у `serve` переопределяют значения из YAML.

## Кросс-протокольная трансляция

Прокси принимает и OpenAI-стиль API (`/v1/chat/completions` и т. д.), и Anthropic Messages API (`/v1/messages`) на одном порту, а каждый бэкенд независимо объявляет свой протокол через `type: openai` / `type: anthropic`. Все четыре комбинации клиент/бэкенд работают прозрачно — клиент OpenAI SDK может быть направлен на Anthropic-бэкенд и наоборот — с автоматической трансляцией тел запросов/ответов и SSE-streaming'а. Подробности: [docs/ARCHITECTURE.ru.md](docs/ARCHITECTURE.ru.md) §7-8, [docs/API.ru.md](docs/API.ru.md) §1.5.

## TUI

![ProxyLM.GO TUI](docs/img/sh.png)

```sh
./proxylm tui --connect ws://localhost:8080 --token <admin_key>
```

Хоткеи: `F1` — help, `F5` — refresh снапшота, `/` — поиск, `Tab` — переключение панелей (Header / Requests / Info), `F10` или `q` — выход. Автоматически переподключается при разрыве (экспоненциальный backoff, 1 с → cap 30 с).

В `cmd.exe` под Windows юникод-глифы могут рендериться некорректно; установи `PROXYLM_NO_UNICODE=1` для ASCII-fallback.

## Web UI

```sh
./proxylm web --connect ws://localhost:8080 --token <admin_key>
```

Поднимает небольшой локальный HTTP-сервер и открывает браузер по умолчанию (если не указан `--no-open`). Это строго **read-only** зеркало TUI — та же стойка серверов, таблица запросов и детальные панели, живые данные через тот же WebSocket `/admin/stream` — без единого элемента, меняющего состояние daemon'а, и с автопереподключением при разрыве. Сам daemon не отдаёт браузерный UI; `proxylm web` — отдельный локальный клиент, по аналогии с `proxylm tui`.

## Сборка из исходников

Требуется Go 1.25.12 или новее. CGO не требуется.

```sh
git clone https://github.com/MaxWD/ProxyLM.GO.git
cd ProxyLM.GO
go build -ldflags "-s -w -X main.version=dev" -o bin/proxylm .
```

Под Windows: `.\scripts\build.ps1`. Кросс-компиляция всех целей одной командой: `.\scripts\build-all.ps1`, либо по отдельности:

```sh
GOOS=linux   GOARCH=amd64 go build -o bin/proxylm-linux-amd64   .
GOOS=darwin  GOARCH=arm64 go build -o bin/proxylm-darwin-arm64  .
GOOS=windows GOARCH=amd64 go build -o bin/proxylm-windows-amd64.exe .
```

Тесты и линт:

```sh
go test ./...
go test -cover ./internal/core/...
gofmt -l .
go vet ./...
golangci-lint run
```

## Запуск как службы

```sh
proxylm service install    # Windows Service / systemd unit / launchd job
proxylm service start
proxylm service status
proxylm service stop
proxylm service uninstall
```

Под капотом — [`github.com/kardianos/service`](https://github.com/kardianos/service): Windows Service Control Manager, systemd unit в `/etc/systemd/system/proxylm.service` (нужны root-права для install/uninstall) или launchd plist в `~/Library/LaunchAgents/`. Рабочий каталог службы — каталог бинарника; конфиг и БД резолвятся относительно него. На Linux/macOS выставь `config.yaml` права `0600`.

## Документация

| Документ | Содержание |
|---|---|
| [docs/ARCHITECTURE.ru.md](docs/ARCHITECTURE.ru.md) | Архитектура системы, алгоритм планировщика, retry/failover, streaming, IPC, схема БД, структура кода |
| [docs/SRS.ru.md](docs/SRS.ru.md) | ТЗ: FR/NFR, инварианты, acceptance-критерии, out-of-scope |
| [docs/API.ru.md](docs/API.ru.md) | API-контракт: эндпоинты OpenAI/Anthropic, admin/IPC WebSocket, формат backend-вызовов |
| [docs/AGENTS.ru.md](docs/AGENTS.ru.md) | Роли участников и карта владельцев документов |

## Участие в разработке

Приветствуются pull request'ы. Ознакомьтесь с [CONTRIBUTING.md](CONTRIBUTING.md) перед открытием PR.

## Безопасность

Для сообщения об уязвимости см. [SECURITY.md](SECURITY.md).

## Лицензия

MIT — см. [LICENSE](LICENSE).

## Built With

- [Bubble Tea](https://github.com/charmbracelet/bubbletea) — TUI-фреймворк
- [Lip Gloss](https://github.com/charmbracelet/lipgloss) — стилизация терминала
- [chi](https://github.com/go-chi/chi) — HTTP-роутер
- [coder/websocket](https://github.com/coder/websocket) — WebSocket (без CGO)
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) — pure-Go SQLite
- [cobra](https://github.com/spf13/cobra) — CLI-фреймворк
- [kardianos/service](https://github.com/kardianos/service) — кроссплатформенный менеджер служб
