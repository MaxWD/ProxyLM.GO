# ProxyLM.GO

[![CI](https://github.com/MaxWD/ProxyLM.GO/actions/workflows/ci.yml/badge.svg)](https://github.com/MaxWD/ProxyLM.GO/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/MaxWD/ProxyLM.GO?include_prereleases)](https://github.com/MaxWD/ProxyLM.GO/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/MaxWD/ProxyLM.GO)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

OpenAI-compatible HTTP proxy for local LLM servers (LM Studio, Ollama) with model-aware queueing, retry/failover, SSE streaming, and a Bubble Tea TUI. Single portable binary, no CGO.

**[На русском](README.ru.md)** · English

---

## Overview

ProxyLM.GO sits between your applications and one or more local (or remote) OpenAI-compatible LLM servers. To the client it looks like a standard OpenAI API endpoint; behind the scenes it manages routing, queuing, and failover across multiple backends.

The primary design goal is to eliminate redundant model swaps. Each LLM occupies significant VRAM; when multiple clients request different models in an interleaved pattern, a server without a proxy spends seconds to minutes unloading and reloading models on every request. ProxyLM.GO collects incoming requests into per-server queues and **drains all pending requests for the currently loaded model before switching** — the model loads once and processes its entire backlog. Requests for the same model across multiple capable servers are distributed in parallel to keep GPU utilization high.

## Features

- **Model-affinity queue** — per-server worker drains all queued requests for the current model before switching; prevents redundant model swaps (INV-1..INV-3)
- **OpenAI-compatible API** — `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/models`, `/healthz`
- **Multiple backends** — route across any number of OpenAI-compatible servers (LM Studio, Ollama, or cloud); configurable priority per backend
- **Auto-discovery** — polls each backend's `/v1/models` at a configurable interval; marks unhealthy servers after N consecutive failures
- **Retry and failover** — exponential backoff with rolling server exclusion; failover to another healthy backend after local retries (INV-5)
- **SSE streaming** — transparent chunk-by-chunk proxying; no buffering, no retry after the first chunk is sent to the client (INV-6)
- **Bearer authentication** — named API keys; client name appears in logs and history, the key itself does not
- **Request history in SQLite** — pure-Go, no CGO (`modernc.org/sqlite`); configurable retention
- **Bubble Tea TUI** — live request table, server health status, log stream; connects to the daemon over WebSocket
- **System service** — install as Windows Service, systemd unit, or launchd job with one command
- **Portable** — config and database live next to the binary; no installation required

## Screenshots

![ProxyLM.GO TUI](docs/img/sh.png)

A text version of the same layout (for code search and offline reading):

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
F1 Help   F5 Refresh   / Filter   Tab Header/Requests/Info   Click — select   q/F10 Quit
```

**Column widths** are sized for the typical case:

- `Model` (27 chars) — fits canonical names such as `qwen2.5-coder-14b-instruct` or `llama-3.1-8b-instruct-q4_k_m` without truncating the quantization suffix.
- `Server` (10 chars) — includes a 2-char `✗ ` prefix for failed-attempt servers, leaving 8 chars for the name.
- `Status` (12 chars) — covers the longest `✗ completed` plus one space of slack.
- `Tokens` (11 chars) — `NNN→NNNN` format; in-progress streaming shows `…` instead of the output count.
- `RM` (2 chars) — single-character "model reloaded" marker (`✓` / `—`) plus separator.

## Quick Start

### 1. Download

Download the pre-built binary for your platform from [Releases](https://github.com/MaxWD/ProxyLM.GO/releases):

| Platform      | Archive                             |
|---------------|-------------------------------------|
| Linux x86-64  | `proxylm_linux_x86_64.tar.gz`       |
| Linux ARM64   | `proxylm_linux_arm64.tar.gz`        |
| macOS x86-64  | `proxylm_macos_x86_64.tar.gz`       |
| macOS ARM64   | `proxylm_macos_arm64.tar.gz`        |
| Windows x86-64| `proxylm_windows_x86_64.zip`        |

Extract the archive. No runtime or interpreter is required.

> Note: `go install github.com/MaxWD/ProxyLM.GO@latest` does not work — the Go module path is a local name (`proxylm`), not the GitHub URL. Installation is via the pre-built binary or by building from source.

### 2. Run the daemon

```sh
./proxylm serve
```

On first run the daemon writes `config.yaml` and `proxylm.db` next to the binary from the embedded template. Edit `config.yaml` before the next start.

### 3. Configure backends

Open `config.yaml` and adjust the `backends` section:

```yaml
backends:
  - name: lm-studio
    url: http://127.0.0.1:1234   # LM Studio default
    type: openai
    timeout_seconds: 600

  - name: ollama
    url: http://127.0.0.1:11434  # Ollama default
    type: openai                 # Ollama exposes an OpenAI-compatible /v1/* shim
    timeout_seconds: 600
```

Change the placeholder keys in `auth.api_keys` and `auth.admin_key`, then restart the daemon.

### 4. Connect the TUI

```sh
./proxylm tui --connect ws://localhost:8080 --token <admin_key>
```

### 5. Send a request

```sh
curl -H "Authorization: Bearer sk-proxy-replace-me-aaaaa" \
     -H "Content-Type: application/json" \
     -d '{"model":"qwen2.5:14b","messages":[{"role":"user","content":"hi"}]}' \
     http://localhost:8080/v1/chat/completions
```

More examples (streaming, embeddings, `/v1/models`) — see [docs/API.md](docs/API.md) §4.

## Configuration

Full annotated example: [`config.example.yaml`](config.example.yaml).

| Section             | Purpose                                                                                   |
|---------------------|-------------------------------------------------------------------------------------------|
| `proxy`             | `host`, `port`, `log_level` (debug / info / warning / error)                              |
| `auth.api_keys`     | Named Bearer keys for client services                                                     |
| `auth.admin_key`    | Separate key for TUI and `/admin/*` endpoints                                             |
| `routing.strategy`  | `model_affinity_least_busy` (default), `least_busy`, `round_robin`, `deferred_model_then_capable`, `preserve_model_coverage` |
| `retry`             | `max_attempts`, `initial_backoff_ms`, `max_backoff_ms`; rolling server exclusion (size 1) |
| `discovery`         | `enabled`, `interval_seconds`, `unhealthy_after_failed_polls`                             |
| `storage`           | `database_path`, `history_retention_days`, `vacuum_on_start`                             |
| `tui`               | `show_completed_minutes` — how long completed requests stay visible in the table          |
| `compat`            | `response_format_mode`: `passthrough` / `normalize_json_object` / `strict_reject`        |
| `backends`          | List of servers: `name`, `url`, `priority`, `type`, `timeout_seconds`, `api_key`, `models` |

CLI flags `--host` / `--port` on the `serve` command override YAML values.

The `compat.response_format_mode` setting is useful for mixed backend pools:
- `passthrough` — forward `response_format` as-is (default).
- `normalize_json_object` — rewrite `response_format.type=json_object` to `json_schema` before the upstream call.
- `strict_reject` — return HTTP 400 at the proxy if `type` is not `json_schema` or `text`.

## Architecture Overview

```
  clients (service-a, service-b, ...)
            |  HTTP / OpenAI-compatible format
            v
   +----------------------------------+
   | ProxyLM.GO daemon                |        +-----------+
   |  AuthN -> Router -> per-server   |------->| srv1 (LM) |
   |           queues + workers       |        +-----------+
   |           (drain current model   |        +-----------+
   |            fully before switch)  |------->| srv2 (Ol) |
   |  Discovery / SQLite / IPC        |        +-----------+
   +----------------+-----------------+
                    |  WebSocket /admin/stream
                    v
              TUI (Bubble Tea)
```

The scheduler enforces **model affinity**: a server's worker pops requests for the currently loaded model first. Only when that sub-queue is empty does it switch to the next model. This is the core invariant (INV-2) that eliminates redundant VRAM swaps.

Full design details: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## TUI

The TUI is a separate process that connects to the running daemon over WebSocket and receives a real-time stream of request events and log lines.

```sh
./proxylm tui --connect ws://localhost:8080 --token <admin_key>
```

Hotkeys: `F1` help overlay, `F5` refresh snapshot (sends `request_snapshot` via WebSocket), `/` search, `Tab` cycle panes (Header / Requests / Info), `F10` or `q` quit.

If the WebSocket connection drops, the TUI reconnects automatically with exponential backoff (1 s → 30 s cap). The title bar shows `connecting…` / `reconnecting…` / `live`.

Completed requests are hidden from the table after `tui.show_completed_minutes` (default 30) but remain in SQLite.

On Windows `cmd.exe` Unicode glyphs may not render correctly. Enable ASCII fallback:

```bat
set PROXYLM_NO_UNICODE=1
proxylm.exe tui --connect ws://localhost:8080 --token <admin_key>
```

## Build from Source

Requires Go 1.25.10 or later. No CGO.

```sh
git clone https://github.com/MaxWD/ProxyLM.GO.git
cd ProxyLM.GO
go build -ldflags "-s -w -X main.version=dev" -o bin/proxylm .
```

On Windows:

```powershell
.\scripts\build.ps1
```

Cross-compilation (single static binary per target, no CGO):

```sh
GOOS=linux   GOARCH=amd64 go build -o bin/proxylm-linux-amd64   .
GOOS=linux   GOARCH=arm64 go build -o bin/proxylm-linux-arm64   .
GOOS=darwin  GOARCH=arm64 go build -o bin/proxylm-darwin-arm64  .
GOOS=windows GOARCH=amd64 go build -o bin/proxylm-windows-amd64.exe .
```

Or all targets at once:

```powershell
.\scripts\build-all.ps1
```

Run tests:

```sh
go test ./...
go test -cover ./internal/core/...
gofmt -l .
go vet ./...
golangci-lint run
```

## Run as a Service

ProxyLM.GO can register itself with the system service manager via a single CLI:

```sh
proxylm service install    # Windows Service / systemd unit / launchd job
proxylm service start
proxylm service status
proxylm service stop
proxylm service uninstall
```

Backed by [`github.com/kardianos/service`](https://github.com/kardianos/service):

- **Windows** — Service Control Manager; the service appears in `services.msc` after `install`.
- **Linux** — systemd unit written to `/etc/systemd/system/proxylm.service`; requires root for `install`/`uninstall`.
- **macOS** — launchd plist written to `~/Library/LaunchAgents/`.

The service working directory is the directory containing the binary; config and database are resolved relative to it.

On Linux/macOS set `config.yaml` permissions to `0600` and ensure it is owned by the user running the service.

## Documentation

| Document | Contents |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | System design, scheduler algorithm, retry/failover, streaming, IPC, database schema, code layout |
| [docs/SRS.md](docs/SRS.md) | Software Requirements Specification: FR/NFR, invariants, acceptance criteria, out-of-scope |
| [docs/API.md](docs/API.md) | API contract: OpenAI v1 endpoints, admin/IPC WebSocket, backend call format |
| [docs/AGENTS.md](docs/AGENTS.md) | Contributor roles and document ownership map |

## Contributing

Contributions are welcome. Please read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request.

## Security

To report a security vulnerability, see [SECURITY.md](SECURITY.md).

## License

MIT — see [LICENSE](LICENSE).

## Built With

- [Bubble Tea](https://github.com/charmbracelet/bubbletea) — TUI framework
- [Lip Gloss](https://github.com/charmbracelet/lipgloss) — terminal styling
- [chi](https://github.com/go-chi/chi) — HTTP router
- [coder/websocket](https://github.com/coder/websocket) — WebSocket (no CGO)
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) — pure-Go SQLite
- [cobra](https://github.com/spf13/cobra) — CLI framework
- [kardianos/service](https://github.com/kardianos/service) — cross-platform service manager
