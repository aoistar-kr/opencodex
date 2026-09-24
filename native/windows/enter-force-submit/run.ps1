#Requires -Version 5.1
<#
.SYNOPSIS
  Builds if needed and starts the Enter force-submit helper.
.DESCRIPTION
  The helper needs no elevation. It installs a low-level keyboard hook, polls UI
  Automation for the Codex composer state, and writes to the OpenCodex
  force-submit named pipe. It never starts, stops, or patches the Codex app.

  Use -Probe to dump the live UI Automation state instead of running the helper,
  and -DryRun to detect and log without swallowing Enter.
#>
[CmdletBinding()]
param(
  [string]$Bridge,
  [int]$StatusInterval,
  [string[]]$Process,
  [int]$Interval,
  [int]$Seconds,
  [string]$LogPath,
  [string[]]$LimitPattern,
  [switch]$DryRun,
  [switch]$Probe,
  [switch]$ProbeEnter,
  [int]$Debounce,
  [switch]$VerboseLog,
  [switch]$Foreground,
  [switch]$Force
)

$ErrorActionPreference = "Stop"

$root = $PSScriptRoot
$executable = Join-Path $root "bin\enter-force-submit.exe"
$invariant = [System.Globalization.CultureInfo]::InvariantCulture

& (Join-Path $root "build.ps1") -Force:$Force
if (-not (Test-Path -LiteralPath $executable)) {
  throw "Build did not produce $executable"
}

$arguments = New-Object System.Collections.Generic.List[string]
if ($Bridge) { $arguments.Add("--bridge"); $arguments.Add($Bridge) }
if ($StatusInterval -gt 0) { $arguments.Add("--status-interval"); $arguments.Add($StatusInterval.ToString($invariant)) }
if ($Process) { $arguments.Add("--process"); $arguments.Add(($Process -join ";")) }
if ($Interval -gt 0) { $arguments.Add("--interval"); $arguments.Add($Interval.ToString($invariant)) }
if ($Seconds -gt 0) { $arguments.Add("--seconds"); $arguments.Add($Seconds.ToString($invariant)) }
if ($LogPath) { $arguments.Add("--log"); $arguments.Add($LogPath) }
if ($LimitPattern) { $arguments.Add("--limit-pattern"); $arguments.Add(($LimitPattern -join ";")) }
if ($DryRun) { $arguments.Add("--dry-run") }
if ($Probe) { $arguments.Add("--probe") }
if ($ProbeEnter) { $arguments.Add("--probe-enter") }
if ($Debounce -gt 0) { $arguments.Add("--debounce"); $arguments.Add($Debounce.ToString($invariant)) }
if ($VerboseLog) { $arguments.Add("--verbose") }

if ($Probe -or $ProbeEnter -or $Foreground) {
  & $executable @arguments
  exit $LASTEXITCODE
}

$quoted = foreach ($argument in $arguments) {
  if ($argument -match '[\s"]') { '"' + ($argument -replace '"', '\"') + '"' } else { $argument }
}

Start-Process -FilePath $executable -ArgumentList ($quoted -join " ") -WindowStyle Hidden

$resolvedLog = if ($LogPath) { $LogPath } else { Join-Path $env:LOCALAPPDATA "OpenCodex\enter-force-submit\helper.log" }
Write-Host "enter-force-submit started (hidden). log: $resolvedLog"
Write-Host "stop with: Stop-Process -Name enter-force-submit"
