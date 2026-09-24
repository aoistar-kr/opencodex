#Requires -Version 5.1
<#
.SYNOPSIS
  Points the Codex desktop app at the OpenCodex app-server bridge through the
  supported CODEX_CLI_PATH override, records the value it displaced so the
  bridge can chain to it, and undoes all of it on request.

.DESCRIPTION
  The desktop app resolves which Codex CLI to run as its local app-server from,
  in this order:

    1. the per-host codex_cli_command (remote/SSH hosts only, not the local host)
    2. the CODEX_CLI_PATH process environment variable
    3. its bundled core under resources\app.asar

  The env mechanism is the supported one. There is no desktop settings key for
  this path: the [desktop] table and the app's setting registry carry no
  codexCliPath, and the bundle has no .env loader. The app reads
  process.env.CODEX_CLI_PATH at launch, so the persisted store is the per-user
  environment variable in HKCU\Environment, which Explorer hands to the next
  shell:AppsFolder\...!App launch. Writing it needs no elevation and touches
  neither app.asar nor WindowsApps.

  The variable is read once per launch, so a change takes effect the next time
  the app starts (one app restart). This script never stops, kills, or relaunches
  the app.

  The app spawns the resolved path as (verified against the live process tree and
  the installed bundle):

    <CODEX_CLI_PATH> -c features.code_mode_host=true app-server --analytics-default-enabled [-c ...]

  and spawns it through a cross-spawn-compatible wrapper (the bundled
  comspec/windowsVerbatimArguments path), so either an .exe or a .cmd works.
  A path outside WindowsApps is spawned as-is; only a path under
  Program Files\WindowsApps is relocated to a writable copy first.

  Chaining contract. One variable holds one value, so installing the bridge
  displaces whatever CODEX_CLI_PATH already named. Install copies that displaced
  value into the user variable OCX_CODEX_DOWNSTREAM_CLI, and the bridge is
  expected to read OCX_CODEX_DOWNSTREAM_CLI as the downstream CLI it spawns
  (falling back to its own default when it is unset). Install writes
  OCX_CODEX_DOWNSTREAM_CLI only when the displaced value is non-empty and is not
  already the bridge path; when the value is empty or already the bridge, that
  variable is left exactly as it was.

  Install is atomic. Recording starts before the first mutation, and any failure
  after that restores both variables and the state file to exactly what they
  were before the attempt, then rethrows.

  On this machine CODEX_CLI_PATH is already in use: it points at the Codex Web
  GPT desktop proxy, and ChatGPT.exe is already spawning that proxy as its
  app-server. Install records it so Rollback restores it exactly.

  -Mode Install   records both variables, chains the displaced value, then writes
                  the bridge path.
  -Mode Rollback  restores both variables exactly as recorded, then drops state.
  -Mode Status    reports both variables and the recorded state without writing.
                  The keyboard helper path is resolved as argument, then recorded
                  value, then default, and its presence is reported, so Status
                  shows the helper's real path once it has been reported.
#>
[CmdletBinding()]
param(
  [ValidateSet("Status", "Install", "Rollback")]
  [string]$Mode = "Status",

  # Path the desktop app should spawn as its Codex CLI. The app-server bridge
  # owns this file; this script only persists and records the path.
  [string]$WrapperPath,

  # Keyboard-helper executable reserved for the usage-limit Enter fallback. The
  # helper is out of scope here; the path is only recorded for it to consume.
  # Leave empty to use the recorded value from an earlier Install, or the default.
  [string]$KeyHelperPath,

  # State file holding the previous values for rollback.
  [string]$StatePath,

  # Install even when the bridge file does not exist yet.
  [switch]$Force
)

$ErrorActionPreference = "Stop"

# Names the desktop app reads, and the name the bridge reads for chaining.
$CliPathVariable = "CODEX_CLI_PATH"
$DownstreamVariable = "OCX_CODEX_DOWNSTREAM_CLI"

# Layout the bridge and the keyboard helper are expected to follow. Override any
# of them on the command line when the real artefacts land elsewhere.
$OcxLocalRoot = Join-Path $env:LOCALAPPDATA "OpenCodex"
$BridgeDirName = "codex-appserver-bridge"
$BridgeFileName = "ocx-codex-appserver-bridge.exe"
$KeyHelperDirName = "enter-force-submit"
$KeyHelperFileName = "enter-force-submit.exe"
$StateFileName = "cli-path-state.json"

$BridgeDir = Join-Path $OcxLocalRoot $BridgeDirName
$DefaultWrapperPath = Join-Path $BridgeDir $BridgeFileName
$DefaultKeyHelperPath = Join-Path (Join-Path $OcxLocalRoot $KeyHelperDirName) $KeyHelperFileName
if ([string]::IsNullOrWhiteSpace($WrapperPath)) { $WrapperPath = $DefaultWrapperPath }
if ([string]::IsNullOrWhiteSpace($StatePath)) { $StatePath = Join-Path $BridgeDir $StateFileName }

function Get-UserVariable {
  param([string]$Name)
  return [Environment]::GetEnvironmentVariable($Name, [EnvironmentVariableTarget]::User)
}

# A $null value deletes the variable; an empty value reads back as unset too.
function Set-UserVariable {
  param([string]$Name, $Value)
  [Environment]::SetEnvironmentVariable($Name, $Value, [EnvironmentVariableTarget]::User)
}

function Write-CliPathState {
  param([hashtable]$State)
  $directory = Split-Path -Parent $StatePath
  if (-not [string]::IsNullOrWhiteSpace($directory)) {
    New-Item -ItemType Directory -Path $directory -Force | Out-Null
  }
  $temporary = "$StatePath.tmp-$PID"
  ($State | ConvertTo-Json) | Set-Content -LiteralPath $temporary -Encoding UTF8
  Move-Item -LiteralPath $temporary -Destination $StatePath -Force
}

function Read-CliPathState {
  if (-not (Test-Path -LiteralPath $StatePath)) { return $null }
  return (Get-Content -Raw -LiteralPath $StatePath | ConvertFrom-Json)
}

function Format-Value {
  param($Value)
  if ([string]::IsNullOrEmpty([string]$Value)) { return "(unset)" }
  return [string]$Value
}

function Get-RecordedProperty {
  param($State, [string]$Name)
  if (-not $State) { return $null }
  if ($null -eq $State.PSObject.Properties[$Name]) { return $null }
  return [string]$State.$Name
}

if ($Mode -eq "Status") {
  $state = Read-CliPathState
  $recordedHelper = Get-RecordedProperty -State $state -Name "keyHelperPath"
  if (-not [string]::IsNullOrWhiteSpace($KeyHelperPath)) {
    $helperPath = $KeyHelperPath; $helperSource = "argument"
  } elseif (-not [string]::IsNullOrWhiteSpace($recordedHelper)) {
    $helperPath = $recordedHelper; $helperSource = "recorded"
  } else {
    $helperPath = $DefaultKeyHelperPath; $helperSource = "default"
  }
  $helperPresence = if (Test-Path -LiteralPath $helperPath) { "exists" } else { "missing" }
  Write-Host "$CliPathVariable (user) : $(Format-Value (Get-UserVariable $CliPathVariable))"
  Write-Host "$DownstreamVariable (user) : $(Format-Value (Get-UserVariable $DownstreamVariable))"
  Write-Host "Bridge path            : $WrapperPath"
  Write-Host "Key helper path        : $helperPath  ($helperSource, $helperPresence)"
  Write-Host "State file             : $StatePath"
  if ($state) {
    Write-Host "Recorded $CliPathVariable : $(Format-Value $state.previousValue)"
    Write-Host "Recorded $DownstreamVariable : $(Format-Value $state.downstreamPreviousValue)"
    Write-Host "Recorded bridge path   : $($state.wrapperPath)"
    Write-Host "Recorded at           : $($state.installedAt)"
  } else {
    Write-Host "Recorded state         : (no state file)"
  }
  if ((Get-UserVariable $CliPathVariable) -eq $WrapperPath) {
    Write-Host "Pointed at the bridge. Restart the Codex app to apply it."
  }
  exit 0
}

if ($Mode -eq "Install") {
  if (-not $Force -and -not (Test-Path -LiteralPath $WrapperPath -PathType Leaf)) {
    throw "Bridge not found at $WrapperPath. Build it first, pass -WrapperPath, or use -Force."
  }
  $effectiveKeyHelperPath = if ([string]::IsNullOrWhiteSpace($KeyHelperPath)) { $DefaultKeyHelperPath } else { $KeyHelperPath }
  $previousCli = Get-UserVariable $CliPathVariable
  $previousDownstream = Get-UserVariable $DownstreamVariable
  if ($previousCli -eq $WrapperPath -and (Read-CliPathState)) {
    Write-Host "$CliPathVariable already points at the bridge and the previous values are recorded; nothing to change."
    exit 0
  }
  $cliHadValue = -not [string]::IsNullOrEmpty($previousCli)
  $downstreamHadValue = -not [string]::IsNullOrEmpty($previousDownstream)
  $chainDownstream = $cliHadValue -and ($previousCli -ne $WrapperPath)
  $priorStateText = if (Test-Path -LiteralPath $StatePath) { Get-Content -Raw -LiteralPath $StatePath } else { $null }

  # Everything below mutates. Any failure restores both variables and the state
  # file to exactly what they were before this attempt, then rethrows.
  try {
    Write-CliPathState -State @{
      variable = $CliPathVariable
      downstreamVariable = $DownstreamVariable
      wrapperPath = $WrapperPath
      keyHelperPath = $effectiveKeyHelperPath
      previousHadValue = $cliHadValue
      previousValue = $(if ($cliHadValue) { $previousCli } else { $null })
      downstreamHadValue = $downstreamHadValue
      downstreamPreviousValue = $(if ($downstreamHadValue) { $previousDownstream } else { $null })
      downstreamWritten = $chainDownstream
      installedAt = [DateTimeOffset]::Now.ToString("o")
    }
    if ($chainDownstream) {
      Set-UserVariable -Name $DownstreamVariable -Value $previousCli
      $writtenDownstream = Get-UserVariable $DownstreamVariable
      if ($writtenDownstream -ne $previousCli) { throw "Failed to chain $DownstreamVariable; read back '$writtenDownstream'." }
    }
    Set-UserVariable -Name $CliPathVariable -Value $WrapperPath
    $writtenCli = Get-UserVariable $CliPathVariable
    if ($writtenCli -ne $WrapperPath) { throw "Failed to persist $CliPathVariable; read back '$writtenCli'." }
  } catch {
    $failure = $_
    try {
      Set-UserVariable -Name $CliPathVariable -Value $(if ($cliHadValue) { $previousCli } else { $null })
      Set-UserVariable -Name $DownstreamVariable -Value $(if ($downstreamHadValue) { $previousDownstream } else { $null })
    } catch { Write-Warning "Install rollback could not restore the environment variables: $($_.Exception.Message)" }
    try {
      if ($null -eq $priorStateText) { Remove-Item -LiteralPath $StatePath -Force -ErrorAction SilentlyContinue }
      else { Set-Content -LiteralPath $StatePath -Value $priorStateText -Encoding UTF8 -NoNewline }
    } catch { Write-Warning "Install rollback could not restore $($StatePath): $($_.Exception.Message)" }
    throw $failure
  }

  Write-Host "$CliPathVariable set to $WrapperPath."
  if ($chainDownstream) {
    Write-Host "$DownstreamVariable set to the displaced value: $previousCli."
  } else {
    Write-Host "$DownstreamVariable left unchanged (displaced value was empty or already the bridge)."
  }
  Write-Host "Previous values recorded at $StatePath."
  Write-Host "Restart the Codex app to apply it. Undo with: -Mode Rollback"
  exit 0
}

if ($Mode -eq "Rollback") {
  $state = Read-CliPathState
  if (-not $state) { throw "No state file at $StatePath; nothing to roll back." }
  $restoreCli = $(if ($state.previousHadValue) { [string]$state.previousValue } else { $null })
  Set-UserVariable -Name $CliPathVariable -Value $restoreCli
  $writtenCli = Get-UserVariable $CliPathVariable
  if ($state.previousHadValue) {
    if ($writtenCli -ne [string]$state.previousValue) { throw "Rollback did not restore '$($state.previousValue)'; read back '$writtenCli'." }
    Write-Host "$CliPathVariable restored to $writtenCli."
  } else {
    if (-not [string]::IsNullOrEmpty($writtenCli)) { throw "Rollback did not remove $CliPathVariable; read back '$writtenCli'." }
    Write-Host "$CliPathVariable removed (it was unset before the install)."
  }
  # A state file written before chaining existed has no downstream fields; leave
  # that variable strictly alone rather than guessing a value to delete.
  if ($null -eq $state.PSObject.Properties["downstreamVariable"]) {
    Write-Host "$DownstreamVariable left unchanged (state file predates the chaining contract)."
  } else {
    $restoreDownstream = $(if ($state.downstreamHadValue) { [string]$state.downstreamPreviousValue } else { $null })
    Set-UserVariable -Name $DownstreamVariable -Value $restoreDownstream
    $writtenDownstream = Get-UserVariable $DownstreamVariable
    if ($state.downstreamHadValue) {
      if ($writtenDownstream -ne [string]$state.downstreamPreviousValue) { throw "Rollback did not restore $DownstreamVariable to '$($state.downstreamPreviousValue)'; read back '$writtenDownstream'." }
      Write-Host "$DownstreamVariable restored to $writtenDownstream."
    } else {
      if (-not [string]::IsNullOrEmpty($writtenDownstream)) { throw "Rollback did not remove $DownstreamVariable; read back '$writtenDownstream'." }
      Write-Host "$DownstreamVariable removed (it was unset before the install)."
    }
  }
  Remove-Item -LiteralPath $StatePath -Force
  Write-Host "Restart the Codex app to apply the restored value."
  exit 0
}
