# ProxyLM.GO — контекст для Claude Code

Мультипротокольный LLM-прокси на Go (OpenAI + Anthropic API) перед локальными и облачными LLM с model-aware queueing и TUI. Один portable-бинарник: и daemon, и TUI-клиент, и инсталлятор службы. Кросс-протокольная трансляция: клиент OpenAI SDK → Anthropic-бэкенд и наоборот.

## Стек

Go 1.25+ (минимум диктует `modernc.org/sqlite`), `net/http` + `chi`, `coder/websocket`, `bubbletea`/`lipgloss`/`bubbles`, `modernc.org/sqlite` (pure-Go, без CGO), `gopkg.in/yaml.v3`, `spf13/cobra`, `kardianos/service`, `log/slog`.

## Запуск, тесты, линт

- Build:  `go build -ldflags "-s -w -X main.version=dev" -o bin/proxylm.exe .`
- Daemon: `go run . serve`
- TUI:    `go run . tui --connect ws://localhost:8080 --token <admin_key>`
- Тесты:  `go test ./...`
- Cover:  `go test -cover ./internal/core/...`
- Линт:   `gofmt -l . && go vet ./... && golangci-lint run`

## Ключевые инварианты ядра (полная формулировка — `docs/SRS.md` §6)

- INV-1: на одном бэкенде ≤ 1 in-flight запроса.
- INV-2: пока в очереди есть запросы для текущей модели сервера — переключение модели запрещено.
- INV-3: FIFO внутри одной модели на одном сервере.
- INV-4: `completed` ставится только после полного ответа (для stream — после `[DONE]`).
- INV-5: попыток ≤ `retry.max_attempts` на исходном сервере + по 1 на каждый failover-сервер.
- INV-6: после первого SSE-чанка клиенту — ретрай и failover ЗАПРЕЩЕНЫ.
- INV-7: ни один запрос из `pending` не «теряется» во время штатной работы.
- INV-8: роутер использует только healthy-серверы.

## Структура каталогов

```
ProxyLM.GO/
├── go.mod
├── main.go                       # cobra root, версия через -ldflags
├── config.example.yaml           # шаблон, embedded в бинарник
├── cmd/                          # cobra-команды (serve, tui, config, service, version)
├── internal/
│   ├── config/                   # YAML + валидация + autogen
│   ├── logging/                  # log/slog setup
│   ├── core/                     # scheduler, router, retry, discovery, backends/
│   ├── api/                      # net/http + chi: /v1/*, /admin/stream, /healthz, streaming
│   ├── storage/                  # database/sql + modernc.org/sqlite + migrations//go:embed
│   ├── ipc/                      # WS publisher (server.go) + client (для TUI)
│   ├── tui/                      # Bubble Tea Model/Update/View
│   └── service/                  # kardianos/service install/lifecycle
├── scripts/                      # build.{ps1,sh}, build-all.ps1
├── docs/                         # ARCHITECTURE, SRS, API, AGENTS
└── test/integration/             # e2e через httptest.Server
```

## Конвенции

- Все публичные функции с I/O принимают `context.Context` первым параметром.
- Логи — `log/slog` JSON-handler; стандартные ключи: `request_id`, `client`, `server`, `model`.
- Конкуренция: горутины + каналы; для очередей сервера — `slice` под `sync.Mutex` (для индексного сканирования drain-current-model) + `chan struct{}` ёмкости 1 для пробуждения воркера.
- `current_model` сервера — `atomic.Pointer[string]`; читается роутером без блокировок (eventual consistency).
- HTTP-клиент — один `*http.Client` per backend с настроенным `*http.Transport` (timeouts, pooling).
- БД — только `database/sql` поверх `modernc.org/sqlite`. Миграции — `*.sql` embedded через `//go:embed migrations/*.sql`.
- Конфиг и БД лежат **рядом с бинарником** (portable). При отсутствии `config.yaml` daemon создаёт его из встроенного шаблона.
- Sub-команды CLI — через `spf13/cobra`; service install — через `github.com/kardianos/service`.
- Тесты: table-driven (`tests := []struct{...}` + `t.Run`), `t.Helper()` в утилитах, никаких `time.Sleep` для синхронизации.

## Что НЕ делать

- Не использовать CGO. SQLite — только `modernc.org/sqlite`.
- Не использовать тяжёлые HTTP-фреймворки (`gin`, `fiber`, `echo`) — `net/http` + `chi` достаточно.
- Не блокировать HTTP-handler синхронным I/O в обход воркера (планировщик — единственная точка диспатча к бэкенду).
- Не логировать API-ключи / admin-ключ. В логи / БД / TUI идёт только `client_name`.
- Не реализовывать Web UI, Prometheus, native Ollama API, rate limiting, persistence очереди — это v0.2+.
- Не ретраить запрос после отправки клиенту первого SSE-чанка (INV-6).
- Не использовать `panic` для штатных ошибок — только явный `error` через возврат.
- Не использовать `time.Sleep` в тестах планировщика — синхронизация через каналы / `sync.WaitGroup`.

## Версионирование (SemVer)

Версия — три числа `MAJOR.MINOR.PATCH`:

- **MAJOR** — серьёзные изменения, полная или частичная несовместимость с предыдущими версиями (изменение public API / wire-протокола, удаление команд, миграции БД без обратной совместимости).
- **MINOR** — добавлен новый функционал, не противоречащий предыдущему (новая команда, новая стратегия роутера, новый эндпоинт, новое поле конфига с дефолтом).
- **PATCH** — мелкие исправления: фиксы багов, косметические правки, документация, рефакторинг без изменения поведения.

Хранилище версий — git-тег вида `vMAJOR.MINOR.PATCH` на коммите релиза. Сам бинарник получает версию из `git describe --tags --always` через `-ldflags "-X main.version=..."` в build-скриптах.

### Как Claude Code назначает версию при коммите

Перед каждым коммитом, который содержит изменения кода (не только docs/scripts), Claude:

1. Читает последний git-тег: `git describe --tags --abbrev=0` (если тегов нет — берёт `v0.0.0`).
2. Анализирует diff и сообщения с момента этого тега, выбирает компонент:
   - есть несовместимое изменение → bump MAJOR, сбросить MINOR и PATCH в 0;
   - иначе есть новый функционал → bump MINOR, сбросить PATCH в 0;
   - иначе → bump PATCH.
3. **Обновляет `CHANGELOG.md`** — добавляет секцию `## [<new-version>] - <date>` с подразделами Added / Changed / Fixed / Removed (по применимости). Обновляет compare-ссылки внизу файла. Это делается **в том же коммите**, что и код, или отдельным коммитом в той же ветке — но **до создания PR**.
4. После успешного `git commit` создаёт тег: `git tag v<new-version>` на этом коммите.
5. В commit message включает строку `Version: v<new-version>` (для читаемости истории).

Если изменения только в `docs/`, `scripts/`, `.claude/`, `*.md`, `.gitignore` — версия НЕ инкрементируется и тег НЕ ставится.

При неоднозначности (например, рефакторинг плюс новый флаг) — спросить пользователя, какой компонент инкрементировать.

## Git workflow на public-репозитории (PROTECTED-MAIN-RULE)

`origin/main` на `MaxWD/ProxyLM.GO` **защищён** branch protection. Прямой `git push origin main` отклоняется (`GH006: Protected branch update failed`). Это применяется ко **всем** агентам и main-agent'у одинаково.

**Стандартный цикл изменения (любая правка кода/docs):**

1. `git switch -c <type>/<short-slug>` — feature branch (type ∈ `feat`/`fix`/`docs`/`chore`/`refactor`/`test`).
2. Коммит(ы) с conventional-commits message.
3. `git push -u origin <branch>` — push feature branch.
4. Создать PR через web UI (`https://github.com/MaxWD/ProxyLM.GO/pull/new/<branch>`); gh CLI не установлен у пользователя.
5. Дождаться зелёного CI (5 status checks).
6. Squash-merge через web UI (linear-history включён).
7. Локально: `git switch main && git pull origin main && git branch -d <branch>`.

**Если коммиты случайно ушли в локальный `main`** — спасать через перенос на ветку:
```
git switch -c <type>/<slug>          # ветка из текущего HEAD с коммитами
git branch -f main origin/main       # local main → origin/main
git push -u origin <branch>          # затем PR
```

**Релизные теги** (`v*.*.*`) push'аются **напрямую** — branch protection не блокирует push тегов. Тег триггерит `release.yml` (GoReleaser) → GitHub Release.

**Команды без явного запроса пользователя НЕ выполняются:** `git push`, `git tag` на remote, `gh repo create`, `gh release create`, force-push, переписывание истории. См. также §«Executing actions with care» в системном промпте.

Полная процедура — в `.claude/agents/github-publisher.md` (раздел «Workflow при защищённом main»).

## docs/FUTURE.md — парковка идей (FUTURE-RULE)

`docs/FUTURE.md` — это **список идей и задач на будущее**, накопленный за время разработки. Из этого файла **ничего не реализуется без явного запроса пользователя**:

- Claude Code (и любой sub-agent) **НЕ** должен брать пункты оттуда как guidance к действию, даже если они выглядят релевантными текущей задаче.
- Упоминание FUTURE.md как источника обоснования («раз есть в FUTURE, значит можно сделать») — недопустимо.
- Реализовать пункт можно только после прямой формулировки пользователем («сделай пункт N из FUTURE.md» или эквивалент).

### Что **можно** и нужно делать с FUTURE.md (только tech-writer)

1. **Sweep всех пунктов при каждой ревизии.** При каждом значимом релизе (или по запросу) tech-writer проходит **весь список FUTURE сверху вниз** и для **каждого** пункта grep'ом по репозиторию проверяет упомянутые в нём имена полей, ключей конфига, функций, build-тегов, миграций. Это страховка от ситуации, когда разработчик закрыл фичу без ссылки на FUTURE — пункт остаётся «висеть», хотя парковать его уже нет смысла. Возможные исходы для каждого пункта:
   - **Полностью реализован** → блок удаляется целиком. Stub'ы вида «DONE in vX.Y.Z» **не оставляются** — описание реализации живёт в `CHANGELOG.md`, `docs/SRS.md`, `docs/ARCHITECTURE.md` и docstring'ах; дублирование в FUTURE.md только создаёт устаревающую копию.
   - **Частично реализован** → блок **не удаляется**, а переформулируется: в начало блока добавляется строка `**Уже сделано в vX.Y.Z:**` с перечнем того, что закрыто; разделы «Проблема» и «Решение» переписываются под оставшийся объём (что ещё не покрыто). Если оставшаяся часть утратила смысл (была единственная мотивация) — блок удаляется так же, как при полной реализации.
   - **Не реализован** → оставить как есть; при необходимости уточнить актуальность (например, если изменилась архитектура и решение нужно пересмотреть).
2. **Перенумерация.** После удалений оставшиеся пункты получают **последовательные номера 1..N** (без дырок). Делается синхронно в `FUTURE.md` и `FUTURE.ru.md` в одном коммите.
3. **Внешние ссылки.** Если на удаляемый или перенумерованный пункт ссылается другой документ (`SRS.md`, `ARCHITECTURE.md`, `API.md`, записи CHANGELOG прошлых релизов, комментарии в коде), такие ссылки обновляются: либо на «реализовано в vX.Y.Z, см. FR-такой-то», либо на новый номер.
4. **Добавление новых идей.** При появлении новой идеи tech-writer проверяет, что она не дублирует существующий пункт, добавляет блок в конец списка с новым номером и поддерживает единый формат: название, проблема, решение, приоритет, опционально риски/ограничения.

Для всех остальных ролей содержимое FUTURE.md — read-only.

## Двуязычная документация (BILINGUAL-RULE)

Пользовательская и архитектурная документация ведётся **на двух языках** — английском и русском — как **равнозначные** версии. Английская — основная (для GitHub-аудитории), русская — параллельная.

**Парные файлы:**

| Английский (главный)    | Русский (параллельный)      | Владелец синхронизации |
|-------------------------|------------------------------|------------------------|
| `README.md`             | `README.ru.md`               | go-devops-cli (контент) + tech-writer (перевод) |
| `docs/ARCHITECTURE.md`  | `docs/ARCHITECTURE.ru.md`    | tech-writer |
| `docs/SRS.md`           | `docs/SRS.ru.md`             | tech-writer |
| `docs/API.md`           | `docs/API.ru.md`             | tech-writer |
| `docs/AGENTS.md`        | `docs/AGENTS.ru.md`          | tech-writer |
| `docs/FUTURE.md`        | `docs/FUTURE.ru.md`          | tech-writer |

**Правила синхронизации:**

1. **При любом смысловом изменении одной языковой версии — обновить вторую в том же коммите.** Это касается: новых разделов, изменения формулировок требований (FR/NFR/INV/AC), новых API-эндпоинтов, обновления команд CLI, изменения архитектурной схемы, добавления/удаления полей конфига. Косметические правки одного языка (опечатки, грамматика) — допустимо без правки другого.
2. **Структура (нумерация разделов, ID требований, имена FR/NFR/INV/AC) — идентичная** в обеих версиях. Cross-reference `§3.2 / INV-5 / FR-12` указывает на одно и то же место в обоих файлах.
3. **Ссылки между docs/** в EN-файлах указывают на EN (`docs/SRS.md`), в RU-файлах — на RU (`docs/SRS.ru.md`). Кросс-языковых ссылок внутри docs/ нет.
4. **Корневые ссылки** (`LICENSE`, `CLAUDE.md`, `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `CHANGELOG.md`) — одни и те же в обеих версиях (эти файлы только на английском, без RU-вариантов).
5. **README.md ↔ README.ru.md:** наверху каждого — switcher-ссылка на другой язык (`**[На русском](README.ru.md)** · English` / `English · **На русском**`).
6. **CLAUDE.md** — **только русский**, без EN-варианта. Это внутренний контекст для AI-ассистента, не публичная документация.
7. **При несогласованности** (одна версия обновлена, другая отстаёт) — tech-writer выравнивает их в ближайшем коммите. Если рассинхронизация обнаружена в коде-ревью PR'а — блокирует merge до выравнивания.

**Что НЕ переводится:**
- `CLAUDE.md` (internal AI context — только RU).
- `LICENSE` (юридический текст — только EN, стандартный MIT).
- `.github/**` (CI/templates на EN).
- `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, `CHANGELOG.md` (только EN — стандарт для open-source). Допустима русская секция в конце `CONTRIBUTING.md` (она там уже есть).
- Code comments — пишутся на языке которым удобнее автору (mix EN/RU допустим), но для public API doc-comments предпочтителен EN.

## Ссылки

- Архитектура: `docs/ARCHITECTURE.md` (EN, основная) / `docs/ARCHITECTURE.ru.md` (RU, параллельная)
- Полное ТЗ: `docs/SRS.md` / `docs/SRS.ru.md`
- API-контракт: `docs/API.md` / `docs/API.ru.md`
- Роли исполнителей: `docs/AGENTS.md` / `docs/AGENTS.ru.md`
- Идеи на будущее (read-only для исполнителей): `docs/FUTURE.md` / `docs/FUTURE.ru.md`
