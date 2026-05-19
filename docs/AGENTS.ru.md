# Специализированные агенты для разработки ProxyLM.GO

Каждая роль ниже — это отдельный исполнитель (sub-agent), которому передаётся часть работы. Имя роли совпадает с именем файла в `.claude/agents/<role>.md` и используется как `owner` в task-листе.

## Карта владельцев документов и кода

| Файл / каталог              | Владелец              | Аудитория                    |
|-----------------------------|-----------------------|------------------------------|
| `docs/ARCHITECTURE.md` (EN, основной) / `docs/ARCHITECTURE.ru.md` (RU, параллельный) | tech-writer | внутренняя команда / агенты |
| `docs/SRS.md` (EN) / `docs/SRS.ru.md` (RU, этот в RU-варианте) | tech-writer | исполнители ТЗ           |
| `docs/API.md` (EN) / `docs/API.ru.md` (RU) | tech-writer | backend + интеграторы                       |
| `docs/AGENTS.md` (EN) / `docs/AGENTS.ru.md` (этот) | tech-writer | оркестрация ролей                   |
| `docs/FUTURE.md` (EN) / `docs/FUTURE.ru.md` (RU) | tech-writer | парковка идей (read-only для исполнителей) |
| `CLAUDE.md` (в корне, только RU) | tech-writer       | Claude Code (AI-ассистент)   |
| `README.md` (EN, основной) / `README.ru.md` (RU, параллельный) | go-devops-cli (контент) + tech-writer (sync переводов) | внешние пользователи |
| `config.example.yaml`       | go-backend-engineer   | пользователи                 |
| `go.mod` / `go.sum`         | go-devops-cli         | сборка / зависимости         |
| `main.go`, `cmd/*`          | go-devops-cli         | вход и CLI                   |
| `internal/core/*`           | go-backend-engineer   | ядро                         |
| `internal/api/*`            | go-backend-engineer   | HTTP-сервер                  |
| `internal/storage/*`        | go-backend-engineer   | SQLite                       |
| `internal/config/*`         | go-backend-engineer   | конфиг                       |
| `internal/logging/*`        | go-backend-engineer   | логирование                  |
| `internal/ipc/server.go`    | go-backend-engineer   | publisher                    |
| `internal/ipc/client.go`    | go-tui-engineer       | потребитель в TUI            |
| `internal/ipc/messages.go`  | tech-writer (схемы) / go-backend-engineer (типы) | оба используют |
| `internal/tui/*`            | go-tui-engineer       | TUI-приложение               |
| `internal/service/*`        | go-devops-cli         | service install / lifecycle  |
| `scripts/*`                 | go-devops-cli         | сборка / упаковка            |
| `test/integration/*`        | go-qa-tests           | e2e тесты                    |
| `*_test.go` (юнит)          | владелец пакета       | unit-тесты                   |
| `LICENSE`                   | github-publisher      | лицензионный текст (MIT)     |
| `SECURITY.md`               | github-publisher      | политика раскрытия CVE       |
| `CONTRIBUTING.md`           | github-publisher      | гайд для контрибьюторов      |
| `CODE_OF_CONDUCT.md`        | github-publisher      | Contributor Covenant         |
| `CHANGELOG.md`              | github-publisher (структура) / tech-writer (заполнение) | релиз-ноты |
| `.github/workflows/*`       | github-publisher      | CI/CD на GitHub Actions      |
| `.github/ISSUE_TEMPLATE/*`  | github-publisher      | формы багов / фич            |
| `.github/PULL_REQUEST_TEMPLATE.md` | github-publisher | чек-лист PR                  |
| `.github/dependabot.yml`    | github-publisher      | автообновление зависимостей  |
| `.github/FUNDING.yml`       | github-publisher      | GitHub Sponsors (опц.)       |
| `.goreleaser.yml` (опц.)    | go-devops-cli (сборка) / github-publisher (release pipeline) | релиз-пайплайн |

## 1. tech-writer (Архитектор / Технический писатель)

**Цель:** поддерживать `docs/ARCHITECTURE.ru.md`, `docs/SRS.ru.md`, `docs/API.ru.md`, `docs/AGENTS.ru.md` и `CLAUDE.md` в актуальном состоянии. Превращать архитектурные изменения в проверяемые требования и в инструкции для AI-ассистента.

**Ответственность:**
- Зафиксировать API-контракт (OpenAI v1, SSE-формат, `/admin/stream`-протокол).
- Описать структуры данных (Request, ServerState, ModelInfo) и их жизненный цикл.
- Описать инварианты планировщика и тест-кейсы для них (особенно: «модель не переключается, пока для неё есть запросы»).
- Список acceptance-критериев для MVP.
- Точно перечислить, что **не** входит в MVP.
- Поддерживать `CLAUDE.md` в корне: соглашения по коду, команды для запуска тестов/линта, точки входа, важные инварианты ядра, ссылки на ARCHITECTURE/SRS/API. Этот файл — контекст для AI-ассистента, не для людей; держать сфокусированным (без дублирования README), но без жёсткого ограничения по строкам.
- Поддерживать `docs/FUTURE.ru.md` (и его EN-пару `docs/FUTURE.md`) — «парковка идей». При каждом релизе/значимом изменении ревизировать список по §FUTURE-RULE в `CLAUDE.md`: (1) **удалять** блоки, попавшие в выпущенный код (описание сохраняется в CHANGELOG / SRS / ARCHITECTURE — stub в FUTURE не нужен), (2) перед удалением — grep'ом по репозиторию проверять имена полей / ключей конфига / функций из блока и удалять пункт, даже если функционал был реализован походя другой задачей, (3) **перенумеровывать** оставшиеся пункты последовательно (1..N, без дырок) синхронно в EN и RU версиях, (4) обновлять внешние ссылки на номер (`FUTURE.md #N`) — либо на «реализовано в vX.Y.Z», либо на новый номер, (5) новые идеи добавлять в конец списка со следующим номером в едином формате (название / проблема / решение / приоритет / опционально риски). Из FUTURE.ru.md **ничего не реализуется самим агентом или Claude Code** — пункты выполняются только по явному запросу пользователя.
- **Двуязычная синхронизация (BILINGUAL-RULE — см. CLAUDE.md):** держать каждую пару EN/RU в `docs/` согласованной (`docs/X.md` ↔ `docs/X.ru.md`). Любое смысловое изменение одной языковой версии — отразить во второй в том же коммите. Косметические правки (опечатки, грамматика) — допустимы в одной языковой версии. Структура (нумерация разделов, ID FR/NFR/INV/AC) идентична между парами. Cross-references в EN-файлах указывают на EN (`docs/SRS.md`); в RU-файлах — на RU (`docs/SRS.ru.md`).
- **Синхронизация переводов README:** tech-writer совместно с go-devops-cli отвечает за точность перевода `README.ru.md`. Контент (фичи, команды, инсталляция) обновляется в `README.md` go-devops-cli; tech-writer держит `README.ru.md` в синхронизации.

**Формат deliverable:** `docs/SRS.ru.md` + `docs/API.ru.md` + `docs/ARCHITECTURE.ru.md` + `docs/FUTURE.ru.md` + `CLAUDE.md`.

## 2. go-backend-engineer (Бэкенд / ядро)

**Цель:** реализовать ядро прокси на Go.

**Ответственность:**
- `internal/config/config.go` — Go-структуры конфига с `yaml` тегами + явная валидация.
- `internal/config/template.go` — встроенный шаблон (`//go:embed config.example.yaml`) + автогенерация.
- `internal/core/models.go` — типы `RequestRecord`, `ServerInfo`, `ModelInfo`, статусы.
- `internal/core/scheduler.go` — per-server worker goroutine, drain-current-then-switch.
- `internal/core/router.go` — выбор сервера для (model).
- `internal/core/retry.go` — политика retry + failover (с `context.Context` cancellation).
- `internal/core/discovery.go` — periodic poll `/v1/models` через `time.Ticker`.
- `internal/core/backends/openai.go` — клиент к OpenAI-совместимым серверам (LM Studio, Ollama).
- `internal/api/*` — `net/http` + `chi` приложение, авторизация, маршруты `/v1/*`, `/admin/stream`, graceful shutdown.
- `internal/api/streaming.go` — проксирование SSE (`http.Flusher`) + подсчёт токенов.
- `internal/storage/db.go` — `database/sql` поверх `modernc.org/sqlite`, миграции через `//go:embed`.
- `internal/storage/history.go` — async writer через канал, чистка по retention.
- `internal/logging/slog.go` — `log/slog` setup (JSON handler, уровни).
- `internal/ipc/server.go` — WebSocket publisher (`coder/websocket`).
- Юнит-тесты на scheduler/router/retry (table-driven, coverage ≥ 80%).

**Приоритет реализации:** сначала непотоковый путь end-to-end, затем streaming.

**Идиоматический Go:**
- `context.Context` первым параметром во всех публичных функциях с I/O.
- Возврат `error` явный, без panic в normal flow.
- Конкуренция: горутины + каналы для очередей; `sync.Mutex` + `atomic.*` для разделяемого состояния.
- Один `*http.Client` per backend с настроенным `*http.Transport`.

## 3. go-tui-engineer (TUI)

**Цель:** Bubble Tea-приложение с btop-подобным интерфейсом + IPC-клиент к daemon'у.

**Ответственность:**
- `internal/ipc/client.go` — WebSocket-клиент (`coder/websocket`), подписка на `state`/`log`, переподключение с backoff.
- `internal/tui/app.go` — Bubble Tea `Model` / `Update` / `View`. Источники сообщений: WS-фрейм, `tea.Tick` для auto-hide, key events.
- `internal/tui/widgets.go` — `HeaderBar`, `RequestTable` (с авто-удалением через `show_completed_minutes`), `LogPane`.
- `internal/tui/styles.go` — `lipgloss`-стили (border, foreground, padding, layout).
- `internal/tui/keys.go` — хоткеи: F5 refresh, F10 quit, q quit, / поиск.
- Корректная работа в Windows Terminal / cmd / PowerShell (ASCII-fallback через env `PROXYLM_NO_UNICODE=1`).

**Архитектурные ограничения:**
- TUI ↔ daemon — только через WebSocket; никакого прямого доступа к БД или ядру.
- Никакого побочного I/O в `Update()` — только через `tea.Cmd`.

## 4. go-devops-cli (CLI / упаковка / запуск)

**Цель:** один способ запуска под Windows + Linux + macOS, понятный README. **Владелец `README.md` и сборки.**

**Ответственность:**
- `main.go` — entry point, инициализация cobra root.
- `cmd/*.go` — `serve`, `tui`, `config init|validate`, `service install|uninstall|start|stop|status`, `version`. Используется `spf13/cobra`.
- `internal/service/service.go` — интеграция с `github.com/kardianos/service` (Windows Service / systemd / launchd).
- `scripts/build.ps1`, `scripts/build.sh`, `scripts/build-all.ps1` — сборка и кросс-компиляция (`GOOS`/`GOARCH`).
- `README.md` (в корне) — описание проекта, скачивание готового бинарника, быстрый старт (3 команды: положить → `./proxylm serve` → `./proxylm tui`), скриншот/мокап TUI, ссылка на `docs/ARCHITECTURE.ru.md` для деталей. Раздел про `service install` под Windows и Linux.
- `go.mod` / зависимости.
- Документация по упаковке релизов (опционально, GitHub Actions для авто-сборки артефактов).

## 5. github-publisher (Публикация / Release Manager)

**Цель:** подготовить и поддерживать проект для публикации на GitHub под `MaxWD/ProxyLM.GO` (public) по мировым open-source практикам.

**Ответственность:**
- Корневые open-source файлы: `LICENSE` (MIT, Copyright (c) Maxim Dolgushev), `SECURITY.md`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md` (Contributor Covenant 2.1), `CHANGELOG.md` (Keep a Changelog).
- Инфраструктура `.github/`: `workflows/ci.yml` (build+test+lint на Linux/Windows/macOS), `workflows/release.yml` (кросс-сборка на git tag v*, 5 целей: win-amd64, linux-amd64, linux-arm64, darwin-amd64, darwin-arm64), `workflows/govulncheck.yml`, `dependabot.yml`, `ISSUE_TEMPLATE/*`, `PULL_REQUEST_TEMPLATE.md`.
- Pre-publish secret/privacy аудит: grep-паттерны для API-ключей и токенов, проверка `.gitignore` (config.yaml, *.db, bin/, *.exe, .git/COMMIT_MSG_*.txt), аудит git-истории.
- Релизный pipeline: упаковка артефактов с README+LICENSE+config.example.yaml, SHA256 checksums, `gh release create` с body из CHANGELOG.
- Repository metadata: рекомендации по topics, description, social preview image.
- Координация: делегирует tech-writer'у английский README и заполнение CHANGELOG; go-devops-cli — GoReleaser/build-скрипты и go.mod path; go-qa-tests — coverage badge и pre-release smoke; backend/tui — аудит на hardcoded персональные данные.
- Список вещей, которые требует человеческого решения: создание репозитория, branch protection rules, GPG-подпись коммитов, topics, FUNDING.yml, версионная стратегия первого публичного релиза.

**Что НЕ делает:**
- Не выполняет `git push`, `gh release create`, `gh repo create` без явного запроса.
- Не переписывает git-историю (`filter-repo`) без явного решения пользователя.
- Не указывает Claude/Anthropic как copyright-holder в LICENSE (атрибуция Claude — только trailer `Co-Authored-By:` в коммитах).
- Не редактирует код в `internal/*`, `cmd/*`, `docs/*` напрямую — только через делегирование.

**Формат deliverable:** `LICENSE`, `SECURITY.md`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `CHANGELOG.md` (структура), `.github/**` (полностью), pre-publish аудит-отчёт, чек-лист действий для пользователя перед публикацией.

## 6. go-qa-tests (Тесты)

**Цель:** покрыть критичную логику и обеспечить regression-safety.

**Ответственность:**
- Юнит-тесты — рядом с пакетами (Go convention `*_test.go`):
  - `internal/core/scheduler_test.go` — инварианты планировщика (тест-кейсы из SRS, INV-1..INV-8).
  - `internal/core/router_test.go` — выбор сервера, edge cases (нет такой модели; все серверы down).
  - `internal/core/retry_test.go` — backoff + failover.
- `test/integration/api_e2e_test.go` — поднимаем `net/http.ServeMux` + mock-бэкенды через `httptest.Server`, гоняем сценарии:
  - 10 запросов к модели A + 5 к модели B → модель A никогда не выгружается, пока её очередь не опустеет.
  - Сервер падает посреди запроса → failover на резервный сервер.
  - Streaming-ответ (мокаем SSE через `httptest.Server` + `http.Flusher`).
- Покрытие core ≥ 80% строк (`go test -cover ./internal/core/...`).
- Бенчмарк NFR-1 (`go test -bench` на mock-бэкенде).

**Конвенции тестов:**
- Table-driven (`tests := []struct{name string; ...}{...}` + `t.Run(tt.name, ...)`).
- `t.Helper()` в утилитах.
- Никаких `time.Sleep` в тестах планировщика — использовать каналы синхронизации / `sync.WaitGroup`.

## Порядок работы

1. **tech-writer** делает ТЗ из архитектуры → согласование с пользователем.
2. Параллельно:
   - **go-backend-engineer** делает скелет ядра (config + storage + api без streaming + scheduler с непотоковым dispatch).
   - **go-tui-engineer** — скелет TUI с замоканым IPC.
   - **go-devops-cli** — `main.go`, cobra-команды, README, скрипты сборки.
3. Интеграция: TUI подключается к реальному daemon'у; streaming реализуется поверх готового non-streaming пути.
4. **go-qa-tests** догоняет тестами по мере появления модулей; integration-тесты — после интеграции.
5. Smoke-test на реальном LM Studio / Ollama (силами пользователя).
6. Кросс-компиляция и проверка артефактов под Windows/Linux/macOS.
