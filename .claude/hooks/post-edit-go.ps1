# PostToolUse hook для ProxyLM.GO
# Срабатывает после Edit/Write Claude Code; если изменён *.go файл — запускает gofmt + go vet.
# Если Go не установлен — тихо пропускает (граceful no-op).
#
# Claude Code передаёт JSON-payload через stdin:
#   { "tool_input": { "file_path": "..." }, ... }

$ErrorActionPreference = 'SilentlyContinue'
$ProgressPreference    = 'SilentlyContinue'

# 1. Прочитать payload
$raw = [Console]::In.ReadToEnd()
if (-not $raw) { exit 0 }

try {
    $payload = $raw | ConvertFrom-Json
} catch {
    exit 0
}

$path = $payload.tool_input.file_path
if (-not $path) { exit 0 }

# 2. Только Go-файлы
if ($path -notmatch '\.go$') { exit 0 }

# 3. Только файлы внутри проекта ProxyLM.GO
$projectRoot = 'C:\!prg\ProxyLM.GO'
try {
    $abs = (Resolve-Path -Path $path -ErrorAction Stop).Path
} catch {
    exit 0
}
if (-not $abs.StartsWith($projectRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
    exit 0
}

# 4. Проверить наличие Go в PATH; если нет — тихо выйти
$goCmd = Get-Command go -ErrorAction SilentlyContinue
if (-not $goCmd) {
    # Go не установлен — пропускаем
    exit 0
}

# 5. gofmt -w на изменённый файл
try {
    & gofmt -w $abs 2>&1 | Out-Null
} catch {}

# 6. go vet ./... на весь модуль (вывод только если ошибки)
Push-Location $projectRoot
try {
    $vetOut = & go vet ./... 2>&1
    if ($LASTEXITCODE -ne 0) {
        Write-Output "[post-edit-go] go vet found issues:"
        Write-Output $vetOut
    }
} catch {} finally {
    Pop-Location
}

exit 0
