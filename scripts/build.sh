#!/usr/bin/env bash
# Сборка ProxyLM.GO под текущую платформу.
# Запуск: ./scripts/build.sh

set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

version="$(git describe --tags --always 2>/dev/null || echo dev)"

mkdir -p bin

case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*) exe="proxylm.exe" ;;
    *)                     exe="proxylm"     ;;
esac

output="bin/$exe"

CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=$version" \
    -o "$output" \
    .

echo "Built: $output ($version)"
