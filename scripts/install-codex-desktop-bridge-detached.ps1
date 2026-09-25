#Requires -Version 5.1
<#
.SYNOPSIS
  Detached, hidden one-shot that rebuilds the repo bridge and Enter helper,
  installs them through scripts/apply-codex-desktop-bridge.ps1, restarts the
  Codex desktop app, and records a full status snapshot.

.DESCRIPTION
  Foreground (default) mode only detaches the worker and returns immediately, so
  the caller's turn can end without losing the build/install work.

  Worker mode (-Worker):

    1. waits DelaySeconds so the caller's turn can finish
    2. snapshots the pre-install state (helper/bridge PIDs and file hashes)
    3. stops the installed helper so the rebuilt helper is the one that runs
    4. runs apply-codex-desktop-bridge.ps1 with -SkipSubmitTest, so it builds and
       installs only the repo-built bridge and helper, activates CODEX_CLI_PATH
       and starts its own detached worker that restarts the Codex app once. The
       legacy bridge-CLI five-turn injection is always skipped here, so a live
       submit probe can never be run twice against the same install.
    5. waits for install-status.json to reach a terminal stage
    6. in a finally block that runs on success, failure and exception alike,
       stops any helper started straight from the repo build and starts the
       installed helper through the installer's own auto-start VBS when none is
       running, then records the installed-helper process count
    7. writes install-restart-status.json and install-restart-status.txt with exit
       codes, installed hashes, process PIDs/paths, the bridge status reply
       (including its usage section) and the helper convergence record

  Only the installed bridge/helper binaries under %LOCALAPPDATA%\OpenCodex, the
  helper auto-start value, and the two CLI environment variables the installer
  already owns are touched. Nothing in the opencodex-mux source tree changes.

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File scripts\install-codex-desktop-bridge-detached.ps1
#>
[CmdletBinding()]
param(
  [switch]$Worker,
  [string]$RunId,
  [ValidateRange(0, 300)][int]$DelaySeconds = 10,
  [ValidateRange(60, 7200)][int]$StatusWaitSeconds = 2400
)

$ErrorActionPreference = "Stop"

$RepoRoot = Split-Path -Parent $PSScriptRoot
$ApplyPath = Join-Path $PSScriptRoot "apply-codex-desktop-bridge.ps1"

$OcxLocalRoot = Join-Path $env:LOCALAPPDATA "OpenCodex"
$BridgeDir = Join-Path $OcxLocalRoot "codex-appserver-bridge"
$BridgeExe = Join-Path $BridgeDir "ocx-codex-appserver-bridge.exe"
$ApplyStatusPath = Join-Path $BridgeDir "install-status.json"
$HelperDir = Join-Path $OcxLocalRoot "enter-force-submit"
$HelperExe = Join-Path $HelperDir "enter-force-submit.exe"
$HelperVbs = Join-Path $HelperDir "enter-force-submit.vbs"
$RepoHelperExe = Join-Path $RepoRoot "native\windows\enter-force-submit\bin\enter-force-submit.exe"

$StatusPath = Join-Path $BridgeDir "install-restart-status.json"
$TextStatusPath = Join-Path $BridgeDir "install-restart-status.txt"
$LogPath = Join-Path $BridgeDir "install-restart-log.txt"

$HelperProcessName = "enter-force-submit.exe"
$BridgeProcessName = "ocx-codex-appserver-bridge.exe"
$DownstreamProcessName = "codex-webgpt-proxy.exe"
$RealCliProcessName = "codex.exe"

function Get-PreferredShell {
  $shell = Get-Command pwsh.exe -ErrorAction SilentlyContinue
  if (-not $shell) { $shell = Get-Command powershell.exe -ErrorAction Stop }
  return $shell.Source
}

if (-not $Worker) {
  New-Item -ItemType Directory -Path $BridgeDir -Force | Out-Null
  if ([string]::IsNullOrWhiteSpace($RunId)) { $RunId = [DateTimeOffset]::Now.ToString("yyyyMMdd-HHmmss") }
  $commandLine = '"' + (Get-PreferredShell) + '" -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File "' + $PSCommandPath +
    '" -Worker -RunId ' + $RunId + ' -DelaySeconds ' + $DelaySeconds +
    ' -StatusWaitSeconds ' + $StatusWaitSeconds
  # Win32_Process.Create reparents the worker to WmiPrvSE.exe, so stopping the
  # Codex process tree cannot stop the worker mid-install.
  $created = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = $commandLine }
  if ($created.ReturnValue -ne 0) {
    throw "detached install worker could not be created (Win32_Process.Create returned $($created.ReturnValue))"
  }
  Write-Host ("Detached install worker started (PID {0})." -f $created.ProcessId)
  Write-Host "Run id: $RunId"
  Write-Host "Status: $StatusPath"
  Write-Host "Log:    $LogPath"
  exit 0
}

if ([string]::IsNullOrWhiteSpace($RunId)) { throw "Worker mode requires -RunId." }

New-Item -ItemType Directory -Path $BridgeDir -Force | Out-Null

function Write-Report {
  param([string]$Line)
  Add-Content -LiteralPath $LogPath -Value ((Get-Date -Format "yyyy-MM-dd HH:mm:ss") + "  " + $Line) -Encoding UTF8
  Write-Host $Line
}

function Get-UserVariable {
  param([string]$Name)
  return [Environment]::GetEnvironmentVariable($Name, [EnvironmentVariableTarget]::User)
}

function Get-FileSha256 {
  param([string]$Path)
  if (-not (Test-Path -LiteralPath $Path)) { return $null }
  return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash
}

function Get-ProcSnapshot {
  param([string]$Name)
  return @(Get-CimInstance Win32_Process -Filter ("Name='" + $Name + "'") | ForEach-Object {
    [ordered]@{ pid = [int]$_.ProcessId; path = [string]$_.ExecutablePath; parentPid = [int]$_.ParentProcessId }
  })
}

function Get-HelperProcesses {
  # installed = the helper running from %LOCALAPPDATA%\OpenCodex, not from the
  # repo build directory a test harness may have launched.
  return @(Get-ProcSnapshot $HelperProcessName | ForEach-Object {
    $path = [string]$_.path
    [ordered]@{ pid = $_.pid; path = $path; installed = ($path -and $path.StartsWith($HelperDir, [StringComparison]::OrdinalIgnoreCase)) }
  })
}

function Get-ChainSnapshot {
  # bridge -> downstream CLI -> real codex
  $snapshot = @(Get-CimInstance Win32_Process)
  $result = [ordered]@{ ok = $false; bridgePid = 0; bridgePath = $null; downstreamPid = 0; realCliPid = 0 }
  foreach ($bridge in @($snapshot | Where-Object { $_.Name -eq $BridgeProcessName })) {
    foreach ($downstream in @($snapshot | Where-Object { $_.ParentProcessId -eq $bridge.ProcessId -and $_.Name -eq $DownstreamProcessName })) {
      foreach ($real in @($snapshot | Where-Object { $_.ParentProcessId -eq $downstream.ProcessId -and $_.Name -eq $RealCliProcessName })) {
        $result.ok = $true
        $result.bridgePid = [int]$bridge.ProcessId
        $result.bridgePath = [string]$bridge.ExecutablePath
        $result.downstreamPid = [int]$downstream.ProcessId
        $result.realCliPid = [int]$real.ProcessId
        return $result
      }
    }
  }
  return $result
}

function Invoke-BridgeClient {
  param([string]$Argument)
  $output = & $BridgeExe $Argument 2>$null
  $code = $LASTEXITCODE
  $parsed = $null
  try { $parsed = ($output | Out-String).Trim() | ConvertFrom-Json } catch { $parsed = $null }
  return [pscustomobject]@{ argument = $Argument; exitCode = [int]$code; reply = $parsed; raw = (($output | Out-String).Trim()) }
}

function Stop-InstalledHelper {
  $stopped = @()
  foreach ($helper in @(Get-HelperProcesses | Where-Object { $_.installed })) {
    Stop-Process -Id ([int]$helper.pid) -ErrorAction SilentlyContinue
    $stopped += [int]$helper.pid
  }
  for ($attempt = 0; $attempt -lt 20; $attempt++) {
    if (@(Get-HelperProcesses | Where-Object { $_.installed }).Count -eq 0) { break }
    Start-Sleep -Milliseconds 500
  }
  return $stopped
}

function Stop-SourceHelper {
  # A helper started straight from the repo build must not shadow or duplicate the
  # installed one.
  $stopped = @()
  foreach ($helper in @(Get-HelperProcesses | Where-Object { -not $_.installed })) {
    Stop-Process -Id ([int]$helper.pid) -ErrorAction SilentlyContinue
    $stopped += [int]$helper.pid
  }
  return $stopped
}

function Start-InstalledHelperIfNeeded {
  # The installer owns the hidden auto-start VBS; use it so the helper starts the
  # same way the Run key starts it.
  if (@(Get-HelperProcesses | Where-Object { $_.installed }).Count -ge 1) { return $false }
  if (Test-Path -LiteralPath $HelperVbs) {
    & (Join-Path $env:WINDIR "System32\wscript.exe") //B //NoLogo $HelperVbs | Out-Null
  } elseif (Test-Path -LiteralPath $HelperExe) {
    Start-Process -FilePath $HelperExe -WindowStyle Hidden
  } else {
    return $false
  }
  for ($attempt = 0; $attempt -lt 20; $attempt++) {
    Start-Sleep -Milliseconds 500
    if (@(Get-HelperProcesses | Where-Object { $_.installed }).Count -ge 1) { break }
  }
  return $true
}

function Get-HelperConvergence {
  $helpers = @(Get-HelperProcesses)
  $installed = @($helpers | Where-Object { $_.installed })
  return [ordered]@{
    recordedAt = [DateTimeOffset]::Now.ToString("o")
    helperProcessCount = $helpers.Count
    installedHelperProcessCount = $installed.Count
    installedHelperPaths = @($installed | ForEach-Object { $_.path })
    helperPaths = @($helpers | ForEach-Object { $_.path })
    installedHelperIsExactlyOne = (($installed.Count -eq 1) -and ($helpers.Count -eq 1))
  }
}

function Write-RestartStatus {
  param([hashtable]$Extra)
  $payload = [ordered]@{
    runId = $RunId
    workerPid = $PID
    repoRoot = $RepoRoot
    applyScript = $ApplyPath
    applyStatusPath = $ApplyStatusPath
    logPath = $LogPath
    updatedAt = [DateTimeOffset]::Now.ToString("o")
  }
  foreach ($key in $Extra.Keys) { $payload[$key] = $Extra[$key] }
  $json = $payload | ConvertTo-Json -Depth 10
  $temporaryPath = "$StatusPath.tmp-$PID"
  Set-Content -LiteralPath $temporaryPath -Value $json -Encoding UTF8
  Move-Item -LiteralPath $temporaryPath -Destination $StatusPath -Force
  return $json
}

$report = [ordered]@{}
$exitCode = 1
$finalStage = "worker"
$finalState = "failed"
$finalMessage = "worker did not reach a terminal state"

try {
  Write-Report "worker $PID starting for run $RunId (delay ${DelaySeconds}s)"
  Write-RestartStatus @{ stage = "delay"; state = "running"; message = "waiting ${DelaySeconds}s before touching the app" }
  Start-Sleep -Seconds $DelaySeconds

  if (-not (Test-Path -LiteralPath $ApplyPath)) { throw "apply script is missing: $ApplyPath" }

  $pre = [ordered]@{
    recordedAt = [DateTimeOffset]::Now.ToString("o")
    helperProcesses = @(Get-HelperProcesses)
    bridgeProcesses = @(Get-ProcSnapshot $BridgeProcessName)
    installedBridgeSha256 = Get-FileSha256 $BridgeExe
    installedHelperSha256 = Get-FileSha256 $HelperExe
    userCliPath = Get-UserVariable "CODEX_CLI_PATH"
    userDownstreamCli = Get-UserVariable "OCX_CODEX_DOWNSTREAM_CLI"
  }
  Write-Report ("pre: helper=" + $pre.helperProcesses.Count + " bridge=" + $pre.bridgeProcesses.Count)
  Write-RestartStatus @{ stage = "preflight"; state = "running"; message = "pre-install snapshot recorded"; preflight = $pre }

  $stoppedHelperPids = Stop-InstalledHelper
  Write-Report ("stopped installed helper pids: " + (($stoppedHelperPids) -join ", "))

  Write-RestartStatus @{ stage = "apply"; state = "running"; message = "building and installing the repo bridge/helper" }
  # -SkipSubmitTest is always present: the apply script's legacy bridge-CLI
  # five-turn injection must never run on top of a real install.
  $applyArguments = @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $ApplyPath, "-RunId", $RunId, "-SkipSubmitTest")
  & (Get-PreferredShell) @applyArguments | ForEach-Object { Write-Report ("apply: " + $_) }
  $applyExitCode = $LASTEXITCODE
  Write-Report "apply foreground exit code: $applyExitCode"

  $terminalStages = @("verify", "submit-test", "rollback", "precheck", "activate")
  $deadline = (Get-Date).AddSeconds($StatusWaitSeconds)
  $applyStatus = $null
  if ($applyExitCode -eq 0) {
    while ((Get-Date) -lt $deadline) {
      if (Test-Path -LiteralPath $ApplyStatusPath) {
        try { $candidate = Get-Content -Raw -LiteralPath $ApplyStatusPath | ConvertFrom-Json } catch { $candidate = $null }
        if ($candidate -and $candidate.runId -eq $RunId -and $candidate.state -in @("succeeded", "failed") -and $candidate.stage -in $terminalStages) {
          if (-not ($candidate.stage -eq "activate" -and $candidate.state -eq "succeeded")) { $applyStatus = $candidate; break }
        }
      }
      Start-Sleep -Seconds 5
    }
  }
  if ($applyStatus) { Write-Report ("apply worker terminal: " + $applyStatus.stage + "/" + $applyStatus.state + " - " + $applyStatus.message) }
  else { Write-Report "apply worker terminal status was not observed in time." }

  $bridgeStatus = Invoke-BridgeClient "--ocx-status"
  $bridgeUsage = Invoke-BridgeClient "--ocx-usage"
  Write-Report ("bridge status exit code: " + $bridgeStatus.exitCode + "; connected: " + [string]$bridgeStatus.reply.connected + "; usage limitState: " + [string]$bridgeStatus.reply.usage.limitState)

  $helperProcessesAtCompletion = @(Get-HelperProcesses)
  $repoHelperSha256 = Get-FileSha256 $RepoHelperExe
  $installedHelperSha256 = Get-FileSha256 $HelperExe
  $installedBridgeSha256 = Get-FileSha256 $BridgeExe

  $report = [ordered]@{
    recordedAt = [DateTimeOffset]::Now.ToString("o")
    applyExitCode = $applyExitCode
    applySkippedSubmitTest = $true
    applyTerminalStage = if ($applyStatus) { $applyStatus.stage } else { $null }
    applyTerminalState = if ($applyStatus) { $applyStatus.state } else { $null }
    applyTerminalMessage = if ($applyStatus) { $applyStatus.message } else { $null }
    installedBridgePath = $BridgeExe
    installedBridgeSha256 = $installedBridgeSha256
    installedHelperPath = $HelperExe
    installedHelperSha256 = $installedHelperSha256
    repoHelperSha256 = $repoHelperSha256
    helperMatchesRepoBuild = ($repoHelperSha256 -and $installedHelperSha256 -and $repoHelperSha256 -eq $installedHelperSha256)
    stoppedHelperPidsAtStart = $stoppedHelperPids
    helperProcessesAtCompletion = $helperProcessesAtCompletion
    bridgeProcesses = @(Get-ProcSnapshot $BridgeProcessName)
    chain = Get-ChainSnapshot
    userCliPath = Get-UserVariable "CODEX_CLI_PATH"
    userDownstreamCli = Get-UserVariable "OCX_CODEX_DOWNSTREAM_CLI"
    bridgeStatusExitCode = $bridgeStatus.exitCode
    bridgeStatusRaw = $bridgeStatus.raw
    bridgeUsageExitCode = $bridgeUsage.exitCode
    bridgeUsageRaw = $bridgeUsage.raw
  }

  $bridgeOk = ($bridgeStatus.exitCode -eq 0) -and ($bridgeStatus.reply -ne $null) -and ([bool]$bridgeStatus.reply.connected)
  $helperOk = ($helperProcessesAtCompletion.Count -eq 1)
  $chainOk = [bool]$report.chain.ok
  $succeeded = ($applyExitCode -eq 0) -and ($applyStatus -and $applyStatus.state -eq "succeeded") -and $bridgeOk -and $helperOk -and $chainOk
  $exitCode = if ($succeeded) { 0 } else { 1 }
  $finalStage = "complete"
  $finalState = if ($succeeded) { "succeeded" } else { "failed" }
  $finalMessage = if ($succeeded) {
    "installed bridge and helper are live; bridge reports connected, helper count is 1, and the app chain bridge -> downstream -> codex is up."
  } else {
    "install finished with applyExitCode=$applyExitCode, bridgeOk=$bridgeOk, helperOk=$helperOk, chainOk=$chainOk."
  }
  Write-Report $finalMessage
} catch {
  $failure = $_.Exception.Message
  Write-Report "worker failed: $failure"
  $report.error = $failure
  $finalStage = "worker"
  $finalState = "failed"
  $finalMessage = $failure
  $exitCode = 1
} finally {
  # Helper convergence runs on every outcome, including a failed or thrown
  # install: the repo-built helper is stopped and the installed helper is started
  # through the installer's own auto-start VBS when none is running.
  try {
    $stoppedSourceHelperPids = Stop-SourceHelper
    $installedHelperStarted = Start-InstalledHelperIfNeeded
    $convergence = Get-HelperConvergence
    Write-Report ("helper convergence: installed=" + $convergence.installedHelperProcessCount + " total=" + $convergence.helperProcessCount + " exactlyOne=" + $convergence.installedHelperIsExactlyOne)
    $report.stoppedSourceHelperPids = $stoppedSourceHelperPids
    $report.installedHelperStartedByWorker = $installedHelperStarted
    $report.helperConvergence = $convergence
  } catch {
    Write-Report ("helper convergence failed: " + $_.Exception.Message)
    $report.helperConvergenceError = $_.Exception.Message
  }
  $json = Write-RestartStatus @{ stage = $finalStage; state = $finalState; message = $finalMessage; result = $report }
  Set-Content -LiteralPath $TextStatusPath -Value $json -Encoding UTF8
}
exit $exitCode
