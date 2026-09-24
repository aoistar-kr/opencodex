#Requires -Version 5.1
<#
.SYNOPSIS
  Builds the Codex app-server bridge and installs it into the per-user OpenCodex
  state directory.
.DESCRIPTION
  Compiles the single self-contained Windows executable and copies it to
  %LOCALAPPDATA%\OpenCodex\codex-appserver-bridge. Activating the bridge -
  recording the downstream CLI and repointing CODEX_CLI_PATH - is left to the
  installer, which owns those environment variables.
.EXAMPLE
  powershell -ExecutionPolicy Bypass -File build.ps1
  powershell -ExecutionPolicy Bypass -File build.ps1 -NoInstall
#>
[CmdletBinding()]
param(
  [switch]$NoInstall
)

$ErrorActionPreference = "Stop"
$sourceDirectory = Split-Path -Parent $PSCommandPath
$stateDirectory = if ($env:LOCALAPPDATA) {
  Join-Path $env:LOCALAPPDATA "OpenCodex\codex-appserver-bridge"
} else {
  Join-Path $env:TEMP "OpenCodex\codex-appserver-bridge"
}
$binary = Join-Path $stateDirectory "ocx-codex-appserver-bridge.exe"
$buildTarget = if ($NoInstall) { Join-Path $sourceDirectory "ocx-codex-appserver-bridge.exe" } else { $binary }

New-Item -ItemType Directory -Path (Split-Path -Parent $buildTarget) -Force | Out-Null

Push-Location $sourceDirectory
try {
  $env:CGO_ENABLED = "0"
  & go build -trimpath -ldflags "-s -w" -o $buildTarget .
  if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE." }
} finally {
  Pop-Location
}

Write-Host "Built $buildTarget"
if ($NoInstall) { exit 0 }

Write-Host ""
Write-Host "Activation is installer-owned. In order:"
Write-Host ('  1. record the CLI the app uses today:  setx OCX_CODEX_DOWNSTREAM_CLI "<value of CODEX_CLI_PATH>"')
Write-Host ('  2. point the app at the bridge:        setx CODEX_CLI_PATH "{0}"' -f $binary)
Write-Host "  3. restart the Codex app once."
Write-Host ""
Write-Host "Verify once the app is running:"
Write-Host ('  & "{0}" --ocx-status' -f $binary)
