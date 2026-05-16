# Кросс-компиляция ProxyLM.GO под все поддерживаемые цели.
# Запуск: .\scripts\build-all.ps1
#
# CGO отключён (SQLite — pure-Go через modernc.org/sqlite), поэтому
# любая целевая платформа собирается без C-toolchain.

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

    $targets = @(
        @{ os = 'windows'; arch = 'amd64'; ext = '.exe' },
        @{ os = 'windows'; arch = 'arm64'; ext = '.exe' },
        @{ os = 'linux';   arch = 'amd64'; ext = ''     },
        @{ os = 'linux';   arch = 'arm64'; ext = ''     },
        @{ os = 'darwin';  arch = 'amd64'; ext = ''     },
        @{ os = 'darwin';  arch = 'arm64'; ext = ''     }
    )

    $env:CGO_ENABLED = '0'
    foreach ($t in $targets) {
        $env:GOOS   = $t.os
        $env:GOARCH = $t.arch
        $out = Join-Path $bin ("proxylm-{0}-{1}{2}" -f $t.os, $t.arch, $t.ext)

        Write-Output ("Building {0}/{1} -> {2}" -f $t.os, $t.arch, $out)
        & go build -trimpath -ldflags "-s -w -X main.version=$version" -o $out .
        if ($LASTEXITCODE -ne 0) { throw ("go build failed for {0}/{1}" -f $t.os, $t.arch) }
    }

    Write-Output ""
    Write-Output ("All targets built into {0} ({1})" -f $bin, $version)
    Get-ChildItem $bin | Select-Object Name, Length | Format-Table -AutoSize
} finally {
    Remove-Item Env:GOOS   -ErrorAction SilentlyContinue
    Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
    Pop-Location
}
