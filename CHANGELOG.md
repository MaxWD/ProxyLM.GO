# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.9.5] - 2026-05-16

### Fixed

- **ipc/client**: HTTP response body was leaked on a failed WebSocket handshake. `coder/websocket.Dial` returns the `*http.Response` alongside the connection; it is now drained and closed unconditionally when non-nil.
- **core/router\_test**: replaced `err.(*ErrNoServer)` type assertion with `errors.As` so the test remains correct if the router ever wraps the sentinel error.
- **core/perf**: removed an ineffectual write (`bestRSS = r`) in the all-zero branch of `solveNNLS3` where `bestRSS` is not read afterward.

### Changed

- **ci**: `TestScheduler_Deferred_DrainBeforeSwap` was flaky under `-race` on Linux runners because three concurrent `Submit()` goroutines could push jobs in non-deterministic order. Replaced the approach with a hold-pattern: `j1` signals `j1Running` before blocking on a release channel; the test waits for the signal, guaranteeing the server is in-flight before `j2`/`j3` are submitted sequentially. No `time.Sleep` used.
- **golangci-lint**: migrated `.golangci.yml` from v1 to v2 format (`version: "2"`, `linters.default: none`, `linters.settings`, `linters.exclusions.rules`, new `formatters` section). Added targeted exclusion rules for idiomatic patterns (`defer x.Close()` error, `staticcheck` QF quick-fix hints, `contextcheck` on intentional `context.Background()` uses in history persistence and graceful shutdown). Dropped `gosimple` (merged into `staticcheck` in v2) and the `nilerr` linter (two flagged call-sites are intentional fallbacks with source comments).

## [0.9.4] - 2026-05-16

### Security

- Bumped `go` directive in `go.mod` from `1.25.0` to `1.25.10`, closing four stdlib CVEs that `govulncheck` flagged against the stale minimum:
  - **GO-2026-4971**: panic in `net.Dial`/`LookupPort` on Windows when a NUL byte appears in the address (fixed in `net@go1.25.10`).
  - **GO-2026-4947**: unexpected work during chain building in `crypto/x509` (fixed in `crypto/x509@go1.25.9`).
  - **GO-2026-4946**: inefficient policy validation in `crypto/x509` (fixed in `crypto/x509@go1.25.9`).
  - **GO-2026-4918**: infinite loop in HTTP/2 transport when given a malformed `SETTINGS_MAX_FRAME_SIZE` (fixed in `net/http@go1.25.10`).

### Fixed

- **ci**: added `defaults.run.shell: bash` to all workflow jobs. The Windows runner was using PowerShell, which split `-coverprofile=coverage.out` into two arguments; Go then tried to load `.out` as a package and failed. Forcing Bash via Git for Windows makes the command line consistent across all three OS runners.
- **ci**: upgraded `golangci-lint-action` from `v6` to `v7` with `install-mode: goinstall`. The prebuilt binary in v6 shipped its own Go 1.24 runtime, which caused `typecheck` failures on every package declaring `go 1.25` in `go.mod`. Building the linter from source with `goinstall` uses the same Go version installed by `setup-go`.

## [0.9.3] - 2026-05-16

Initial public release. See [README.md](README.md) for project description, quick start, and configuration reference.

[Unreleased]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.9.5...HEAD
[0.9.5]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.9.4...v0.9.5
[0.9.4]: https://github.com/MaxWD/ProxyLM.GO/compare/v0.9.3...v0.9.4
[0.9.3]: https://github.com/MaxWD/ProxyLM.GO/releases/tag/v0.9.3
