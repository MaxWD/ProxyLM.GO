# Сборка ProxyLM.GO под текущую платформу (Windows / Linux / macOS — определяется средой Go).
# Запуск: .\scripts\build.ps1

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    $version = 'dev'
    try {
        $describe = & git describe --tags --always 2>$null
        if ($LASTEXITCODE -eq 0 -and $describe) { $version = $describe }
    } catch { }

    $bin = Join-Path $root 'bin'
    New-Item -ItemType Directory -Force -Path $bin | Out-Null

    $exeName = if ($IsWindows -or -not (Test-Path variable:IsWindows)) { 'proxylm.exe' } else { 'proxylm' }
    $output  = Join-Path $bin $exeName

    $env:CGO_ENABLED = '0'
    & go build -trimpath -ldflags "-s -w -X main.version=$version" -o $output .
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }

    Write-Output "Built: $output ($version)"
} finally {
    Pop-Location
}
