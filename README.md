# ProxyLM.GO

[![CI](https://github.com/MaxWD/ProxyLM.GO/actions/workflows/ci.yml/badge.svg)](https://github.com/MaxWD/ProxyLM.GO/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/MaxWD/ProxyLM.GO?include_prereleases)](https://github.com/MaxWD/ProxyLM.GO/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/MaxWD/ProxyLM.GO)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Multi-protocol LLM proxy (OpenAI + Anthropic API) for any LLM backend — local (LM Studio, Ollama, vLLM, llama.cpp) or remote (OpenRouter, Groq, Together AI, OpenAI, Anthropic). Model-aware queueing, cross-protocol translation, retry/failover, SSE streaming, a console TUI, and a browser dashboard. Single portable binary, no CGO.

**[На русском](README.ru.md)** · English

---

## Overview

ProxyLM.GO sits between your applications and one or more LLM servers, presenting a standard OpenAI or Anthropic API endpoint while managing routing, queuing, and failover across multiple backends behind the scenes. Its primary design goal is to eliminate redundant model swaps: each LLM occupies significant VRAM, and a server juggling several models on demand can spend seconds to minutes reloading on every request. ProxyLM.GO queues incoming requests per server and **drains all pending requests for the currently loaded model before switching** — the model loads once and processes its entire backlog, while requests for the same model across multiple capable servers are distributed in parallel to keep GPU utilization high.

## Features

- **Model-affinity queue** — per-server worker drains all queued requests for the current model before switching; prevents redundant model swaps (INV-1..INV-3)
- **OpenAI + Anthropic API** — `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/messages`, `/v1/models`, `/healthz`
- **Cross-protocol translation** — OpenAI SDK clients can reach Anthropic backends and vice versa, automatically (see below)
- **Multiple backends** — route across any number of servers; configurable priority per backend (prefer local over cloud when both can serve a model)
- **Auto-discovery** — polls each backend's `/v1/models` at a configurable interval; marks unhealthy servers after N consecutive failures
- **Retry and failover** — exponential backoff with rolling server exclusion; failover to another healthy backend after local retries (INV-5)
- **SSE streaming** — transparent chunk-by-chunk proxying; no buffering, no retry after the first chunk is sent to the client (INV-6)
- **Dual authentication** — accepts both `Authorization: Bearer` (OpenAI-style) and `x-api-key` (Anthropic-style); named API keys, client name in logs/history, never the key itself
- **Request history in SQLite** — pure-Go, no CGO (`modernc.org/sqlite`); configurable retention
- **Bubble Tea TUI + browser dashboard** — live request table, server health, and log stream, from a console client (`proxylm tui`) or a browser (`proxylm web`)
- **System service** — install as Windows Service, systemd unit, or launchd job with one command
- **Portable** — config and database live next to the binary; no installation required

## Screenshots

![ProxyLM.GO TUI](docs/img/sh.png)

## Quick Start

### 1. Get the binary

Download the pre-built archive for your platform from [Releases](https://github.com/MaxWD/ProxyLM.GO/releases) and extract it — no runtime or interpreter required:

| Platform      | Archive                       |
|---------------|--------------------------------|
| Linux x86-64  | `proxylm_linux_x86_64.tar.gz`  |
| Linux ARM64   | `proxylm_linux_arm64.tar.gz`   |
| macOS x86-64  | `proxylm_macos_x86_64.tar.gz`  |
| macOS ARM64   | `proxylm_macos_arm64.tar.gz`   |
| Windows x86-64| `proxylm_windows_x86_64.zip`   |

Or build it yourself — see [Build from Source](#build-from-source).

> Note: `go install github.com/MaxWD/ProxyLM.GO@latest` does not work — the Go module path is a local name (`proxylm`), not the GitHub URL.

### 2. Run the daemon

```sh
./proxylm serve
```

On first run this writes `config.yaml` and `proxylm.db` next to the binary from the embedded template.

### 3. Configure backends

Open `config.yaml` and adjust `backends`:

```yaml
backends:
  - name: lm-studio
    url: http://127.0.0.1:1234   # LM Studio default port
    timeout_seconds: 600
    priority: 100                # lower number = higher preference among free servers

  - name: ollama
    url: http://127.0.0.1:11434  # Ollama default port (OpenAI-compatible /v1/* shim)
    timeout_seconds: 600
    priority: 200

  # Anthropic Claude API — set type: anthropic for the native Anthropic protocol
  # - name: anthropic-cloud
  #   url: https://api.anthropic.com
  #   type: anthropic
  #   api_key: sk-ant-api03-...
  #   priority: 900               # high number = used only when locals can't serve
```

`type` selects the wire protocol: `openai` (default — LM Studio, Ollama, vLLM, OpenRouter, etc.) or `anthropic`. Then replace the placeholder keys in `auth.api_keys` and `auth.admin_key` and restart the daemon.

### 4. Send a request

```sh
curl -H "Authorization: Bearer sk-proxy-replace-me-aaaaa" \
     -H "Content-Type: application/json" \
     -d '{"model":"qwen2.5:14b","messages":[{"role":"user","content":"hi"}]}' \
     http://localhost:8080/v1/chat/completions
```

Or via the Anthropic Messages API — works against the same daemon regardless of the backend's own protocol:

```sh
curl -H "x-api-key: sk-proxy-replace-me-aaaaa" \
     -H "Content-Type: application/json" \
     -H "anthropic-version: 2023-06-01" \
     -d '{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}' \
     http://localhost:8080/v1/messages
```

More examples (streaming, embeddings, `/v1/models`) — see [docs/API.md](docs/API.md) §4.

## Commands

`proxylm` is a single binary; every mode is a subcommand.

| Command | Description | Typical invocation |
|---|---|---|
| `serve` | Run the daemon (HTTP proxy + IPC WebSocket) | `proxylm serve` |
| `tui` | Connect the console TUI to a running daemon | `proxylm tui --connect ws://localhost:8080 --token <admin_key>` |
| `web` | Open the browser dashboard | `proxylm web --connect ws://localhost:8080 --token <admin_key>` |
| `config init` | Write `config.yaml` from the embedded template | `proxylm config init` |
| `config validate` | Validate `config.yaml` | `proxylm config validate` |
| `service install` | Register `proxylm` as a system service | `proxylm service install` |
| `service uninstall` | Remove the service registration | `proxylm service uninstall` |
| `service start` / `stop` | Start / stop the service | `proxylm service start` |
| `service status` | Show the service status | `proxylm service status` |
| `version` | Print version, OS/arch, Go version | `proxylm version` |

Flags worth knowing:

- **`serve`** — `--config <path>` (default: `config.yaml` next to the binary).
- **`tui`** — `--connect <ws-url>` (default `ws://localhost:8080`), `--token <admin_key>` (required).
- **`web`** — `--connect <url>` (`ws://`/`wss://`/`http://`/`https://`, default `ws://localhost:8080`), `--token <admin_key>` (optional — enables auto-connect), `--listen <host:port>` (default `127.0.0.1:8081`), `--no-open` (don't launch the browser automatically).

## Configuration

Full annotated example: [`config.example.yaml`](config.example.yaml).

| Section             | Purpose                                                                                   |
|---------------------|-------------------------------------------------------------------------------------------|
| `proxy`             | `host`, `port`, `log_level` (debug / info / warning / error)                              |
| `auth.api_keys`     | Named Bearer keys for client services                                                     |
| `auth.admin_key`    | Separate key for `tui`, `web`, and `/admin/*` endpoints                                   |
| `routing.strategy`  | `model_affinity_least_busy` (default), `least_busy`, `round_robin`, `deferred_model_then_capable`, `preserve_model_coverage` |
| `retry`             | `max_attempts`, `initial_backoff_ms`, `max_backoff_ms`; rolling server exclusion (size 1) |
| `discovery`         | `enabled`, `interval_seconds`, `unhealthy_after_failed_polls`                             |
| `storage`           | `database_path`, `history_retention_days`, `vacuum_on_start`                             |
| `tui`               | `show_completed_minutes` — how long completed requests stay visible in the table          |
| `compat`            | `response_format_mode`: `passthrough` / `normalize_json_object` / `strict_reject`        |
| `backends`          | List of servers: `name`, `url`, `priority`, `type` (`openai`/`anthropic`), `timeout_seconds`, `api_key`, `models` |

CLI flags `--host` / `--port` on `serve` override YAML values.

## Cross-Protocol Translation

The proxy accepts both the OpenAI-style API (`/v1/chat/completions`, `/v1/messages` counterpart aside) and the Anthropic Messages API (`/v1/messages`) on the same port, and each backend independently declares its own protocol via `type: openai` / `type: anthropic`. All four client/backend combinations work transparently — an OpenAI SDK client can be routed to an Anthropic backend and vice versa — with request/response bodies and SSE streaming translated automatically. Details: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) §7-8, [docs/API.md](docs/API.md) §1.5.

## TUI

```sh
./proxylm tui --connect ws://localhost:8080 --token <admin_key>
```

Hotkeys: `F1` help, `F5` refresh snapshot, `/` search, `Tab` cycle panes (Header / Requests / Info), `F10` or `q` quit. Reconnects automatically on disconnection (exponential backoff, 1 s → 30 s cap).

On Windows `cmd.exe`, Unicode glyphs may not render; set `PROXYLM_NO_UNICODE=1` for an ASCII fallback.

## Web UI

```sh
./proxylm web --connect ws://localhost:8080 --token <admin_key>
```

Runs a small local HTTP server and opens the default browser (unless `--no-open`). It is a **strictly read-only** mirror of the TUI — the same server rack, request table, and detail panes, live over the same `/admin/stream` WebSocket — with no controls that change daemon state, and it reconnects automatically on disconnection. The daemon itself serves no browser UI; `proxylm web` is a separate local client, analogous to `proxylm tui`.

## Build from Source

Requires Go 1.25.12 or later. No CGO.

```sh
git clone https://github.com/MaxWD/ProxyLM.GO.git
cd ProxyLM.GO
go build -ldflags "-s -w -X main.version=dev" -o bin/proxylm .
```

On Windows: `.\scripts\build.ps1`. Cross-compile all targets at once: `.\scripts\build-all.ps1`, or individually:

```sh
GOOS=linux   GOARCH=amd64 go build -o bin/proxylm-linux-amd64   .
GOOS=darwin  GOARCH=arm64 go build -o bin/proxylm-darwin-arm64  .
GOOS=windows GOARCH=amd64 go build -o bin/proxylm-windows-amd64.exe .
```

Run tests and lint:

```sh
go test ./...
go test -cover ./internal/core/...
gofmt -l .
go vet ./...
golangci-lint run
```

## Run as a Service

```sh
proxylm service install    # Windows Service / systemd unit / launchd job
proxylm service start
proxylm service status
proxylm service stop
proxylm service uninstall
```

Backed by [`github.com/kardianos/service`](https://github.com/kardianos/service): Windows Service Control Manager, a systemd unit at `/etc/systemd/system/proxylm.service` (root required for install/uninstall), or a launchd plist under `~/Library/LaunchAgents/`. The service's working directory is the directory containing the binary; config and database resolve relative to it. On Linux/macOS, set `config.yaml` permissions to `0600`.

## Documentation

| Document | Contents |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | System design, scheduler algorithm, retry/failover, streaming, IPC, database schema, code layout |
| [docs/SRS.md](docs/SRS.md) | Software Requirements Specification: FR/NFR, invariants, acceptance criteria, out-of-scope |
| [docs/API.md](docs/API.md) | API contract: OpenAI/Anthropic endpoints, admin/IPC WebSocket, backend call format |
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
