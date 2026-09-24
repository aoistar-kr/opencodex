#Requires -Version 5.1
<#
.SYNOPSIS
  Builds, installs and activates the OpenCodex Codex app-server bridge and the
  Enter force-submit helper, then restarts the Codex desktop app exactly once
  from a detached worker.

.DESCRIPTION
  Default (foreground) mode does only the safe, non-disruptive work:

    1. builds the bridge from native/codex-appserver-bridge and installs it to
       %LOCALAPPDATA%\OpenCodex\codex-appserver-bridge\ocx-codex-appserver-bridge.exe
    2. builds the helper from native/windows/enter-force-submit and installs it to
       %LOCALAPPDATA%\OpenCodex\enter-force-submit\enter-force-submit.exe
    3. registers the helper for user-logon auto-start through a per-user Run key,
       recording the prior value of that key first
    4. applies scripts/install-codex-desktop-cli-path.ps1, which records the CLI
       the app uses today (codex-webgpt-proxy.exe) as OCX_CODEX_DOWNSTREAM_CLI so
       the bridge chains through it, and points CODEX_CLI_PATH at the bridge

  Unless -NoRestart is given, it then starts the detached worker and returns.

  Worker mode (-Worker) is the only part that touches the running app. It is
  created through Win32_Process.Create, so its parent is WmiPrvSE.exe and not the
  Codex process tree: stopping that tree cannot stop the worker. The worker keeps
  running after the caller's turn dies and records every stage to
  install-status.json and install-log.txt, both under the bridge directory.

  Compensation contracts, in both directions:

    - Foreground: the region from the helper auto-start registration and the CLI
      activation through the detached worker launch is wrapped. Any failure there
      restores the installer state, restores the recorded auto-start, stops the
      helper, and never restarts the app.
    - Worker: pre-restart checks run before the app is touched. If they fail the
      worker rolls the already-activated state back and exits without restarting
      the app. Only after that does it stop the app once, relaunch it, and check
      the process tree (bridge -> codex-webgpt-proxy -> real codex), the bridge's
      own --ocx-status connected flag, and the helper process. A post-restart
      failure rolls everything back and relaunches the original app.

  The live force-submit test that follows a healthy restart is temporary test
  mode: it drives the bridge CLI directly and enables no persistent bypass. A
  turn counts as a success only when the turn completed AND the thread's own user
  item is exactly the prompt that was submitted AND every attempt stayed on one
  stable target thread. A turn that completed without the submitted item observed
  is recorded as a failure and is never counted. If the thread changes, the
  remaining attempts are abandoned as failed. UI rendering cannot be confirmed
  from here, so uiVisibility stays "unconfirmed" and the visible check is carried
  as pending rather than reported as success.

  -Remove undoes everything: stops the helper, restores the recorded auto-start
  state, and rolls the environment back through the installer.

  -NoRestart stops after step 4.
#>
[CmdletBinding()]
param(
  [switch]$Worker,
  [switch]$NoRestart,
  [switch]$Remove,
  [switch]$Force,
  [switch]$SkipSubmitTest,
  [string]$RunId,
  [int]$DetachDelaySeconds = 8,
  [int]$SubmitTestTarget = 5,
  # Kept for command-line compatibility. Live testing never exceeds Target:
  # the first turn is the gate and only its success unlocks the remainder.
  [int]$SubmitTestMaxAttempts = 5,
  [int]$SubmitTestDeadlineSeconds = 900,
  [int]$SubmitTestPerAttemptSeconds = 120,
  [int]$SubmitTestSettleSeconds = 15,

  # Read-only diagnostic: report what the completion detector sees for a token.
  [string]$ProbeEvidenceToken,
  [string]$ProbeEvidenceThreadId,
  [string]$ProbeEvidenceExpectedText,

  # Live-test-only guard. With -NoRollback the worker records a failure but never
  # undoes the already-activated install (environment, auto-start, helper), so a
  # transient post-restart health check cannot tear the feature down.
  [switch]$NoRollback
)

$ErrorActionPreference = "Stop"

$RepoRoot = Split-Path -Parent $PSScriptRoot
$InstallerPath = Join-Path $PSScriptRoot "install-codex-desktop-cli-path.ps1"
$BridgeSource = Join-Path $RepoRoot "native\codex-appserver-bridge"
$HelperSource = Join-Path $RepoRoot "native\windows\enter-force-submit"

$OcxLocalRoot = Join-Path $env:LOCALAPPDATA "OpenCodex"
$BridgeDir = Join-Path $OcxLocalRoot "codex-appserver-bridge"
$BridgeExe = Join-Path $BridgeDir "ocx-codex-appserver-bridge.exe"
$HelperDir = Join-Path $OcxLocalRoot "enter-force-submit"
$HelperExe = Join-Path $HelperDir "enter-force-submit.exe"
$HelperVbs = Join-Path $HelperDir "enter-force-submit.vbs"
$AutostartStatePath = Join-Path $HelperDir "autostart-state.json"
$InstallerStatePath = Join-Path $BridgeDir "cli-path-state.json"
$StatusPath = Join-Path $BridgeDir "install-status.json"
$LogPath = Join-Path $BridgeDir "install-log.txt"

# Helper auto-start lives in the per-user Run key, the same place this machine
# already starts the OpenCodex tray from, so no elevation is involved.
$AutostartKey = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run"
$AutostartName = "OpenCodexEnterForceSubmit"

$CliPathVariable = "CODEX_CLI_PATH"
$DownstreamVariable = "OCX_CODEX_DOWNSTREAM_CLI"
$BridgeProcessName = "ocx-codex-appserver-bridge.exe"
$DownstreamProcessName = "codex-webgpt-proxy.exe"
$HelperProcessName = "enter-force-submit.exe"
$RealCliProcessName = "codex.exe"

if ([string]::IsNullOrWhiteSpace($RunId)) { $RunId = [DateTimeOffset]::Now.ToString("yyyyMMdd-HHmmss") }
# A caller cannot turn the live probe into an unbounded retry loop.
$SubmitTestTarget = [Math]::Max(1, [Math]::Min(5, $SubmitTestTarget))

function Write-Log {
  param([string]$Message)
  $directory = Split-Path -Parent $LogPath
  if (-not (Test-Path -LiteralPath $directory)) { New-Item -ItemType Directory -Path $directory -Force | Out-Null }
  Add-Content -LiteralPath $LogPath -Value ((Get-Date -Format "yyyy-MM-dd HH:mm:ss") + "  " + $Message) -Encoding UTF8
}

function Write-Status {
  param([string]$Stage, [string]$State, [string]$Message, [hashtable]$Extra)
  $payload = [ordered]@{
    runId = $RunId
    stage = $Stage
    state = $State
    message = $Message
    workerPid = $PID
    bridgePath = $BridgeExe
    helperPath = $HelperExe
    updatedAt = [DateTimeOffset]::Now.ToString("o")
  }
  if ($Extra) { foreach ($key in $Extra.Keys) { $payload[$key] = $Extra[$key] } }
  $directory = Split-Path -Parent $StatusPath
  if (-not (Test-Path -LiteralPath $directory)) { New-Item -ItemType Directory -Path $directory -Force | Out-Null }
  $temporary = "$StatusPath.tmp-$PID"
  ($payload | ConvertTo-Json -Depth 8) | Set-Content -LiteralPath $temporary -Encoding UTF8
  Move-Item -LiteralPath $temporary -Destination $StatusPath -Force
  Write-Log ("[" + $Stage + "/" + $State + "] " + $Message)
}

function Get-UserVariable {
  param([string]$Name)
  return [Environment]::GetEnvironmentVariable($Name, [EnvironmentVariableTarget]::User)
}

function Get-RunValue {
  $item = Get-ItemProperty -Path $AutostartKey -Name $AutostartName -ErrorAction SilentlyContinue
  if (-not $item) { return $null }
  return [string]$item.$AutostartName
}

function Get-CodexPackage {
  Import-Module Appx -ErrorAction SilentlyContinue
  $package = Get-AppxPackage -Name OpenAI.Codex -ErrorAction SilentlyContinue
  if (-not $package) { $package = Get-AppxPackage -Name OpenAI.CodexBeta -ErrorAction SilentlyContinue }
  return $package
}

function Get-CodexAppProcesses {
  param([string]$Root)
  if ([string]::IsNullOrWhiteSpace($Root)) { return @() }
  return @(Get-CimInstance Win32_Process -Filter "Name='ChatGPT.exe'" | Where-Object {
    $_.ExecutablePath -and $_.ExecutablePath.StartsWith($Root, [StringComparison]::OrdinalIgnoreCase)
  })
}

function Test-BridgeClient {
  # Runs the bridge as its own client. Exit 2 means the binary works but no
  # bridge is serving yet, which is the expected state before the restart.
  $output = & $BridgeExe --ocx-status 2>$null
  $code = $LASTEXITCODE
  $parsed = $null
  try { $parsed = ($output | Out-String).Trim() | ConvertFrom-Json } catch { $parsed = $null }
  return [pscustomobject]@{ exitCode = $code; reply = $parsed }
}

function Get-ProcessChain {
  # The app spawns the bridge directly, the bridge spawns the CLI that
  # CODEX_CLI_PATH named before activation, and that CLI spawns the real codex.
  $snapshot = @(Get-CimInstance Win32_Process)
  foreach ($bridge in @($snapshot | Where-Object { $_.Name -eq $BridgeProcessName })) {
    $parent = $snapshot | Where-Object { $_.ProcessId -eq $bridge.ParentProcessId }
    foreach ($downstream in @($snapshot | Where-Object { $_.ParentProcessId -eq $bridge.ProcessId -and $_.Name -eq $DownstreamProcessName })) {
      foreach ($real in @($snapshot | Where-Object { $_.ParentProcessId -eq $downstream.ProcessId -and $_.Name -eq $RealCliProcessName })) {
        return [pscustomobject]@{
          ok = $true
          bridgePid = [int]$bridge.ProcessId
          bridgeParent = [string]$parent.Name
          downstreamPid = [int]$downstream.ProcessId
          realCliPid = [int]$real.ProcessId
        }
      }
    }
  }
  return [pscustomobject]@{ ok = $false; bridgePid = 0; bridgeParent = ""; downstreamPid = 0; realCliPid = 0 }
}

function Install-Autostart {
  $vbs = @(
    'Dim shell'
    'Set shell = CreateObject("WScript.Shell")'
    'shell.Run """' + $HelperExe + '""", 0, False'
  ) -join [Environment]::NewLine
  Set-Content -LiteralPath $HelperVbs -Value $vbs -Encoding ASCII
  $data = '"' + (Join-Path $env:WINDIR "System32\wscript.exe") + '" //B //NoLogo "' + $HelperVbs + '"'
  # The first run records what was there; a re-run must not overwrite the true
  # original with our own value.
  if (-not (Test-Path -LiteralPath $AutostartStatePath)) {
    $previous = Get-RunValue
    $hadValue = -not [string]::IsNullOrEmpty($previous)
    @{
      name = $AutostartName
      hadValue = $hadValue
      previousValue = $(if ($hadValue) { $previous } else { $null })
      vbsPath = $HelperVbs
      recordedAt = [DateTimeOffset]::Now.ToString("o")
    } | ConvertTo-Json | Set-Content -LiteralPath $AutostartStatePath -Encoding UTF8
  }
  if (-not (Test-Path -LiteralPath $AutostartKey)) { New-Item -Path $AutostartKey -Force | Out-Null }
  New-ItemProperty -Path $AutostartKey -Name $AutostartName -Value $data -PropertyType String -Force | Out-Null
  return $data
}

function Remove-RunValue {
  Remove-Item -Path (Join-Path $AutostartKey $AutostartName) -Force -ErrorAction SilentlyContinue
  # Remove-ItemProperty was observed to leave the value behind once; verify and
  # fall back to reg.exe rather than trusting a silent removal.
  if ($null -ne (Get-RunValue)) {
    & (Join-Path $env:SystemRoot "System32\reg.exe") DELETE "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /V $AutostartName /F | Out-Null
  }
}

function Restore-Autostart {
  if (Test-Path -LiteralPath $AutostartStatePath) {
    $state = Get-Content -Raw -LiteralPath $AutostartStatePath | ConvertFrom-Json
    if ($state.hadValue) {
      New-ItemProperty -Path $AutostartKey -Name $AutostartName -Value ([string]$state.previousValue) -PropertyType String -Force | Out-Null
    } else {
      Remove-RunValue
    }
    Remove-Item -LiteralPath $AutostartStatePath -Force -ErrorAction SilentlyContinue
  }
  Remove-Item -LiteralPath $HelperVbs -Force -ErrorAction SilentlyContinue
}

function Start-Helper {
  if (Get-Process -Name ([IO.Path]::GetFileNameWithoutExtension($HelperProcessName)) -ErrorAction SilentlyContinue) {
    return $false
  }
  Start-Process -FilePath $HelperExe -WindowStyle Hidden
  return $true
}

function Stop-Helper {
  Get-Process -Name ([IO.Path]::GetFileNameWithoutExtension($HelperProcessName)) -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
}

function Build-Bridge {
  & (Join-Path $BridgeSource "build.ps1")
  if ($LASTEXITCODE -ne 0) { throw "bridge build failed with exit code $LASTEXITCODE" }
  if (-not (Test-Path -LiteralPath $BridgeExe)) { throw "bridge build did not produce $BridgeExe" }
}

function Install-Helper {
  & (Join-Path $HelperSource "build.ps1") -Force:$Force
  if ($LASTEXITCODE -ne 0) { throw "helper build failed with exit code $LASTEXITCODE" }
  $built = Join-Path $HelperSource "bin\enter-force-submit.exe"
  if (-not (Test-Path -LiteralPath $built)) { throw "helper build did not produce $built" }
  New-Item -ItemType Directory -Path $HelperDir -Force | Out-Null
  if ((Resolve-Path -LiteralPath $built).Path -ne (Join-Path $HelperDir "enter-force-submit.exe")) {
    Copy-Item -LiteralPath $built -Destination $HelperExe -Force
  }
}

function Stop-CodexApp {
  param([string]$Root)
  $targets = @(Get-CimInstance Win32_Process | Where-Object {
    $_.ExecutablePath -and $_.ExecutablePath.StartsWith($Root, [StringComparison]::OrdinalIgnoreCase)
  })
  if ($targets.Count -eq 0) { return }
  $ids = @{}
  foreach ($target in $targets) { $ids[[uint32]$target.ProcessId] = $true }
  # The loop variable must not be named $Root: PowerShell variable names are
  # case-insensitive, so it would bind to the [string] parameter, be coerced to a
  # string, and make $candidate.ProcessId null.
  $treeRoots = @($targets | Where-Object { -not $ids.ContainsKey([uint32]$_.ParentProcessId) })
  foreach ($candidate in $treeRoots) {
    $process = Get-Process -Id $candidate.ProcessId -ErrorAction SilentlyContinue
    if ($process -and $process.MainWindowHandle -ne 0) { [void]$process.CloseMainWindow() }
  }
  Start-Sleep -Seconds 3
  # /T is safe here: this worker's parent is WmiPrvSE.exe, never the app tree.
  foreach ($candidate in $treeRoots) {
    if (Get-Process -Id $candidate.ProcessId -ErrorAction SilentlyContinue) {
      & (Join-Path $env:SystemRoot "System32\taskkill.exe") /PID $candidate.ProcessId /T /F | Out-Null
    }
  }
  for ($attempt = 0; $attempt -lt 20; $attempt++) {
    if (@(Get-CimInstance Win32_Process | Where-Object {
      $_.ExecutablePath -and $_.ExecutablePath.StartsWith($Root, [StringComparison]::OrdinalIgnoreCase)
    }).Count -eq 0) { return }
    Start-Sleep -Milliseconds 500
  }
  throw "Codex package processes survived shutdown."
}

function Start-CodexApp {
  param([string]$Aumid, [string]$CliPath, [string]$Downstream)
  # The relaunched app inherits this process environment. The user environment
  # block is already correct, and building it here as well removes any dependence
  # on whether the launcher refreshed.
  $env:CODEX_CLI_PATH = $CliPath
  if ([string]::IsNullOrWhiteSpace($Downstream)) {
    Remove-Item Env:OCX_CODEX_DOWNSTREAM_CLI -ErrorAction SilentlyContinue
  } else {
    $env:OCX_CODEX_DOWNSTREAM_CLI = $Downstream
  }
  Start-Process "shell:AppsFolder\$Aumid"
}

function Test-PreRestart {
  $problems = @()
  if (-not (Test-Path -LiteralPath $BridgeExe)) {
    $problems += "bridge executable is missing: $BridgeExe"
  } else {
    try {
      $probe = Test-BridgeClient
      if (-not ($probe.exitCode -in @(0, 1, 2))) { $problems += "bridge client returned exit code $($probe.exitCode)" }
    } catch {
      $problems += "bridge client could not run: $($_.Exception.Message)"
    }
  }
  if (-not (Test-Path -LiteralPath $HelperExe)) { $problems += "helper executable is missing: $HelperExe" }
  if (-not (Test-Path -LiteralPath $InstallerStatePath)) { $problems += "installer state is missing: $InstallerStatePath" }
  $userCli = Get-UserVariable $CliPathVariable
  if ($userCli -ne $BridgeExe) { $problems += "user $CliPathVariable is '$userCli', expected '$BridgeExe'" }
  $downstream = Get-UserVariable $DownstreamVariable
  if ([string]::IsNullOrWhiteSpace($downstream)) { $problems += "user $DownstreamVariable is empty" }
  $package = Get-CodexPackage
  if (-not $package -or -not $package.InstallLocation) {
    $problems += "the Codex desktop MSIX package was not found"
  } elseif (@(Get-CodexAppProcesses -Root $package.InstallLocation).Count -eq 0) {
    $problems += "the Codex desktop app is not running"
  }
  return $problems
}

function Test-PostRestart {
  param([string]$Root, [int]$TimeoutSeconds = 120)
  $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
  $last = [pscustomobject]@{ app = $false; chain = (Get-ProcessChain); bridge = $false; helper = $false }
  while ((Get-Date) -lt $deadline) {
    $last = [pscustomobject]@{
      app = (@(Get-CodexAppProcesses -Root $Root).Count -gt 0)
      chain = Get-ProcessChain
      bridge = $false
      helper = [bool](Get-Process -Name ([IO.Path]::GetFileNameWithoutExtension($HelperProcessName)) -ErrorAction SilentlyContinue)
    }
    if ($last.app) {
      try {
        $probe = Test-BridgeClient
        $last.bridge = ($probe.exitCode -eq 0) -and ($probe.reply -ne $null) -and ([bool]$probe.reply.connected)
      } catch { $last.bridge = $false }
    }
    if ($last.app -and $last.chain.ok -and $last.bridge -and $last.helper) { return $last }
    if (-not $last.helper) { Start-Helper | Out-Null }
    Start-Sleep -Seconds 5
  }
  return $last
}

function Invoke-Rollback {
  # Restores durable state only. The app is relaunched only when this worker
  # actually stopped it, so a failure that happens before the stop - or in the
  # worker's pre-restart checks - can never disturb a live app.
  param([string]$RestoreCliPath, [string]$Aumid, [switch]$RelaunchApp)
  if ($NoRollback) {
    Write-Log "rollback suppressed by -NoRollback: installed state, auto-start and helper are left in place."
    return
  }
  Stop-Helper
  Restore-Autostart
  if (Test-Path -LiteralPath $InstallerPath) {
    & $InstallerPath -Mode Rollback
  }
  if ($RelaunchApp -and $Aumid) { Start-CodexApp -Aumid $Aumid -CliPath $RestoreCliPath -Downstream "" }
}

function Get-BridgeThreadCandidate {
  try {
    $probe = Test-BridgeClient
    if ($probe.exitCode -eq 0 -and $probe.reply) {
      # New bridges expose a foreground/root thread whose turn was accepted by
      # the backend. Prefer that proof over the app's merely observed selection.
      $accepted = [string]$probe.reply.backendAcceptedThreadId
      if (-not [string]::IsNullOrWhiteSpace($accepted)) {
        return [pscustomobject]@{ threadId = $accepted; source = "backend-accepted-root" }
      }
      # Compatibility with an already-installed older bridge. This is only an
      # untrusted one-shot candidate: the first force-submit below must prove it.
      $observed = [string]$probe.reply.threadId
      if (-not [string]::IsNullOrWhiteSpace($observed)) {
        return [pscustomobject]@{ threadId = $observed; source = "observed-selection-untrusted" }
      }
    }
  } catch { }
  return $null
}

function Wait-BridgeThreadCandidate {
  # Status stability is not validity. Wait only for a candidate to exist; the
  # first real force-submit is the sole gate that can authorize four more turns.
  param([int]$TimeoutSeconds = 120)
  $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
  while ((Get-Date) -lt $deadline) {
    $candidate = Get-BridgeThreadCandidate
    if ($candidate) { return $candidate }
    Start-Sleep -Seconds 3
  }
  return $null
}

function Get-RolloutFile {
  # The app-server appends each turn to a rollout JSONL whose file name embeds the
  # thread id, so the bridge's own reported threadId finds the right file.
  param([string]$ThreadId)
  $sessions = Join-Path $env:USERPROFILE ".codex\sessions"
  if (-not (Test-Path -LiteralPath $sessions)) { return $null }
  if (-not [string]::IsNullOrWhiteSpace($ThreadId)) {
    $match = @(Get-ChildItem -LiteralPath $sessions -Recurse -File -Filter "*$ThreadId*.jsonl" -ErrorAction SilentlyContinue |
      Sort-Object LastWriteTime -Descending)
    if ($match.Count -gt 0) { return $match[0].FullName }
  }
  $recent = @(Get-ChildItem -LiteralPath $sessions -Recurse -File -Filter "*.jsonl" -ErrorAction SilentlyContinue |
    Sort-Object LastWriteTime -Descending | Select-Object -First 4)
  if ($recent.Count -eq 0) { return $null }
  return $recent[0].FullName
}

function Read-FileTailLines {
  # Get-Content -Tail reads the whole file and the app-server's rollouts reach tens
  # of megabytes, so seek to the last slice and share-read it instead.
  param([string]$File, [int]$Bytes = 1572864)
  $stream = [IO.File]::Open($File, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::ReadWrite)
  try {
    $length = $stream.Length
    $start = if ($length -gt $Bytes) { $length - $Bytes } else { 0 }
    [void]$stream.Seek($start, [IO.SeekOrigin]::Begin)
    $buffer = New-Object byte[] ([int]($length - $start))
    $read = 0
    while ($read -lt $buffer.Length) {
      $chunk = $stream.Read($buffer, $read, $buffer.Length - $read)
      if ($chunk -le 0) { break }
      $read += $chunk
    }
    return ([Text.Encoding]::UTF8.GetString($buffer, 0, $read)).Split([char]10)
  } finally {
    $stream.Dispose()
  }
}

function Get-TurnEvidence {
  # Machine-checkable evidence, independent of the app's rendering:
  #   tokenSeen   - the token shows up in a user item of this thread
  #   userSeen    - that user item's text is exactly the prompt that was submitted
  #   completed   - this attempt's task_complete follows that exact user item
  param([string]$File, [string]$Token, [string]$ExpectedText, [string]$TurnId)
  $tokenSeen = $false
  $userSeen = $false
  $userText = ""
  $completed = $false
  $assistantText = ""
  $segmentOpen = $false
  $turnIdMatched = $false
  $boundaryCrossed = $false
  if ($File -and (Test-Path -LiteralPath $File)) {
    $scanned = 0
    foreach ($line in (Read-FileTailLines -File $File)) {
      if (-not $line) { continue }
      try { $parsed = $line | ConvertFrom-Json } catch { continue }
      $eventTurnIds = @(
        foreach ($container in @($parsed, $parsed.payload)) {
          if (-not $container) { continue }
          foreach ($name in @("turn_id", "turnId", "root_turn_id", "rootTurnId")) {
            $property = $container.PSObject.Properties[$name]
            if ($property -and -not [string]::IsNullOrWhiteSpace([string]$property.Value)) {
              [string]$property.Value
            }
          }
        }
      )
      $eventTurnIds = @($eventTurnIds | Sort-Object -Unique)
      $idsExactlyExpected = ($eventTurnIds.Count -eq 1) -and
        (-not [string]::IsNullOrWhiteSpace($TurnId)) -and
        ($eventTurnIds[0] -eq $TurnId)
      $eventType = [string]$parsed.payload.type
      if ([string]::IsNullOrWhiteSpace($eventType)) { $eventType = [string]$parsed.type }
      if (-not $tokenSeen) {
        if ($line.Contains($Token)) {
          if ($parsed.payload.role -eq "user") {
            $tokenSeen = $true
            try { $itemText = ($parsed.payload.content | Where-Object { $_.type -eq "input_text" } | Select-Object -First 1).text } catch { $itemText = $null }
            if (-not $itemText) {
              try { $itemText = ($parsed.payload.content | Select-Object -First 1).text } catch { $itemText = $null }
            }
            $userText = [string]$itemText
            $exactText = (-not [string]::IsNullOrWhiteSpace($userText)) -and ($userText.Trim() -eq $ExpectedText.Trim())
            $turnIdMatched = ($eventTurnIds.Count -gt 0) -and $idsExactlyExpected
            $identityMatches = ($eventTurnIds.Count -eq 0) -or $turnIdMatched
            $userSeen = $exactText -and $identityMatches
            $segmentOpen = $userSeen
          }
        }
        continue
      }
      if (-not $segmentOpen) { continue }
      # Once this attempt's segment is open, any explicitly identified event
      # from another turn closes it, regardless of that event's type.
      if ($eventTurnIds.Count -gt 0 -and -not $idsExactlyExpected) {
        $boundaryCrossed = $true
        break
      }
      if ($parsed.payload.role -eq "user") {
        $boundaryCrossed = $true
        break
      }
      $isTurnBoundary = $eventType -in @("turn_started", "turn_start", "task_started", "task_start", "turn_context")
      if ($isTurnBoundary -and (($eventTurnIds.Count -eq 0) -or
          [string]::IsNullOrWhiteSpace($TurnId) -or -not ($eventTurnIds -contains $TurnId))) {
        $boundaryCrossed = $true
        break
      }
      # Actual rollout fixture: { type: "event_msg", payload:
      # { type: "task_complete", turn_id: ... } }. eventType is resolved from
      # payload.type first (and top-level type only for the flat legacy shape).
      if ($eventType -eq "task_complete") {
        if ($eventTurnIds.Count -gt 0) {
          if ($idsExactlyExpected) {
            $turnIdMatched = $true
            $completed = $true
          } else {
            $boundaryCrossed = $true
          }
        } else {
          # Legacy events have no turn id. The exact unique user item and the
          # still-open segment bound this completion to the submitted attempt.
          $completed = $true
        }
        break
      }
      if ($scanned -gt 2000) { continue }
      $scanned++
      if (-not $assistantText) {
        if ($parsed -and $parsed.payload.role -eq "assistant") {
          $text = ($parsed.payload.content | Where-Object { $_.type -eq "output_text" } | Select-Object -First 1).text
          if ($text) { $assistantText = [string]$text }
        }
      }
    }
  }
  return [pscustomobject]@{
    tokenSeen = $tokenSeen
    userSeen = $userSeen
    userText = $userText
    completed = $completed
    assistantText = $assistantText
    turnIdMatched = $turnIdMatched
    boundaryCrossed = $boundaryCrossed
  }
}

function Invoke-SubmitTest {
  # Temporary test mode only: this drives the bridge CLI directly and changes no
  # persistent setting, so nothing here leaves a hard-block bypass behind. The
  # helper keeps its own gating untouched.
  #
  # The whole run is pinned to one candidate. Status and thread/read are not
  # proof that it is live: attempt 1 must be accepted, complete, and remain on
  # that same thread. Only then may attempts 2..Target run.
  $testTarget = [Math]::Max(1, [Math]::Min(5, $SubmitTestTarget))
  $deadline = (Get-Date).AddSeconds($SubmitTestDeadlineSeconds)
  $attempts = @()
  $completedCount = 0
  $targetThreadId = $null
  $candidateSource = $null
  $abortedReason = $null
  Start-Sleep -Seconds $SubmitTestSettleSeconds
  $candidate = Wait-BridgeThreadCandidate
  if ($candidate) {
    $targetThreadId = [string]$candidate.threadId
    $candidateSource = [string]$candidate.source
  }
  if ([string]::IsNullOrWhiteSpace($targetThreadId)) {
    $abortedReason = "no-thread-candidate: the bridge reported neither a backend-accepted root thread nor an observed selection"
  }
  for ($index = 1; $index -le $testTarget; $index++) {
    if ($abortedReason) { break }
    if ((Get-Date) -ge $deadline) { break }
    $token = "OCX-BRIDGE-TEST-$RunId-$index"
    $text = "$token. Reply with exactly OK-$index and nothing else."
    $attempt = [ordered]@{
      index = $index
      token = $token
      prompt = $text
      startedAt = [DateTimeOffset]::Now.ToString("o")
      exitCode = $null
      accepted = $false
      threadId = $null
      explicitThreadId = $targetThreadId
      turnId = $null
      requestId = $null
      sameThread = $false
      rolloutFile = $null
      statusThreadId = $null
      userTokenSeen = $false
      userMessageSeen = $false
      userText = ""
      turnCompleted = $false
      turnIdMatched = $false
      completionBoundaryCrossed = $false
      assistantText = ""
      outcome = "not-run"
      reply = $null
      backendError = ""
      backendMessage = ""
      evictionObserved = $false
    }
    # Sequential, never overlapping. We deliberately do not re-validate through
    # status stability: only the backend result and completed turn count.
    $statusCandidate = Get-BridgeThreadCandidate
    if ($statusCandidate) { $attempt.statusThreadId = [string]$statusCandidate.threadId }
    # --ocx-thread-id pins the injection to the captured thread; the flag is a
    # bridge argument and never becomes part of the prompt text.
    $output = & $BridgeExe --ocx-force-submit $text --ocx-thread-id $targetThreadId 2>$null
    $attempt.exitCode = $LASTEXITCODE
    try { $attempt.reply = (($output | Out-String).Trim() | ConvertFrom-Json) } catch { $attempt.reply = $null }
    if ($attempt.reply) {
      $attempt.accepted = [bool]$attempt.reply.ok
      $attempt.threadId = [string]$attempt.reply.threadId
      $attempt.turnId = [string]$attempt.reply.turnId
      $attempt.requestId = [string]$attempt.reply.requestId
      $attempt.backendError = [string]$attempt.reply.error
      $attempt.backendMessage = [string]$attempt.reply.message
    }
    if (-not $attempt.accepted) {
      $attempt.outcome = "rejected"
      if ($attempt.backendError -in @("backend-rejected", "no-active-thread") -or
          $attempt.backendMessage -match '(?i)thread\s+not\s+found|no[- ]active[- ]thread|no\s+rollout|not\s+materialized') {
        $postReject = Get-BridgeThreadCandidate
        $attempt.evictionObserved = (-not $postReject) -or ([string]$postReject.threadId -ne $targetThreadId)
      }
      $attempts += [pscustomobject]$attempt
      $abortedReason = "attempt $($index) rejected: $($attempt.backendError) $($attempt.backendMessage)"
      break
    }
    # The thread returned by the injected turn must be the pinned thread.
    $threadStable = ($attempt.threadId -eq $targetThreadId)
    $attempt.sameThread = [bool]$threadStable
    if (-not $threadStable) {
      $attempt.outcome = "thread-changed"
      $attempts += [pscustomobject]$attempt
      $abortedReason = "thread-changed: expected $targetThreadId, got $($attempt.threadId)"
      break
    }
    $rollout = Get-RolloutFile -ThreadId $attempt.threadId
    $attempt.rolloutFile = $rollout
    $waitUntil = (Get-Date).AddSeconds($SubmitTestPerAttemptSeconds)
    $evidence = [pscustomobject]@{ tokenSeen = $false; userSeen = $false; userText = ""; completed = $false; assistantText = ""; turnIdMatched = $false; boundaryCrossed = $false }
    while ((Get-Date) -lt $waitUntil) {
      $evidence = Get-TurnEvidence -File $rollout -Token $token -ExpectedText $text -TurnId $attempt.turnId
      if ($evidence.completed) { break }
      Start-Sleep -Seconds 3
    }
    $attempt.userTokenSeen = [bool]$evidence.tokenSeen
    $attempt.userMessageSeen = [bool]$evidence.userSeen
    $attempt.userText = [string]$evidence.userText
    $attempt.turnCompleted = [bool]$evidence.completed
    $attempt.assistantText = [string]$evidence.assistantText
    $attempt.turnIdMatched = [bool]$evidence.turnIdMatched
    $attempt.completionBoundaryCrossed = [bool]$evidence.boundaryCrossed
    # Only an exact user-item match plus a completed turn on the pinned thread
    # counts. A turn that completed without the submitted item observed is a
    # failure, never a success.
    if ($attempt.turnCompleted -and $attempt.userMessageSeen -and $attempt.sameThread) {
      $attempt.outcome = "completed"
      $completedCount++
    } elseif ($attempt.turnCompleted -and $attempt.userTokenSeen) {
      $attempt.outcome = "completed-user-item-mismatch"
    } elseif ($attempt.turnCompleted) {
      $attempt.outcome = "completed-without-user-item-observed"
    } else {
      $attempt.outcome = "accepted-completion-not-observed"
    }
    $attempts += [pscustomobject]$attempt
    $gatePassed = $attempt.accepted -and $attempt.turnCompleted -and $attempt.sameThread
    if (-not $gatePassed) {
      $abortedReason = "attempt $($index) failed gate: accepted=$($attempt.accepted), turnCompleted=$($attempt.turnCompleted), sameThread=$($attempt.sameThread)"
      break
    }
    if ($attempt.outcome -ne "completed") {
      $abortedReason = "attempt $($index) failed evidence check: $($attempt.outcome)"
      break
    }
    if ($completedCount -ge $testTarget) { break }
  }
  $chain = Get-ProcessChain
  $helperProcess = @(Get-Process -Name ([IO.Path]::GetFileNameWithoutExtension($HelperProcessName)) -ErrorAction SilentlyContinue)
  $helperLogTouched = $false
  $helperLog = Join-Path $HelperDir "helper.log"
  if (Test-Path -LiteralPath $helperLog) {
    # -Quiet returns a Boolean. Comparing it against $null (as before) is always
    # true, which made this flag permanently true. Take the Boolean itself.
    $helperLogTouched = [bool](Select-String -LiteralPath $helperLog -Pattern "OCX-BRIDGE-TEST" -SimpleMatch -Quiet)
  }
  $state = if ($abortedReason) { "failed" } elseif ($completedCount -ge $testTarget) { "succeeded" } elseif ($completedCount -gt 0) { "partial" } else { "inconclusive" }
  return [pscustomobject]@{
    target = $testTarget
    completedTurns = $completedCount
    attemptsRun = $attempts.Count
    state = $state
    abortedReason = $abortedReason
    targetThreadId = $targetThreadId
    candidateSource = $candidateSource
    threadId = $targetThreadId
    mode = "bridge-cli-direct"
    persistentBypassEnabled = $false
    uiVisibility = "unconfirmed"
    uiCheck = "pending-operator-visible-composer-check"
    chainAfterTestOk = [bool]$chain.ok
    chainAfterTestBridgePid = [int]$chain.bridgePid
    chainAfterTestDownstreamPid = [int]$chain.downstreamPid
    chainAfterTestRealCliPid = [int]$chain.realCliPid
    helperRunningAfterTest = ($helperProcess.Count -gt 0)
    helperLogMentionsTestToken = $helperLogTouched
    attempts = $attempts
  }
}

function Start-DetachedWorker {
  $shell = Get-Command pwsh.exe -ErrorAction SilentlyContinue
  if (-not $shell) { $shell = Get-Command powershell.exe -ErrorAction Stop }
  $commandLine = '"' + $shell.Source + '" -NoProfile -ExecutionPolicy Bypass -File "' + $PSCommandPath +
    '" -Worker -RunId ' + $RunId + ' -DetachDelaySeconds ' + $DetachDelaySeconds +
    ' -SubmitTestTarget ' + $SubmitTestTarget +
    ' -SubmitTestMaxAttempts ' + $SubmitTestMaxAttempts +
    ' -SubmitTestDeadlineSeconds ' + $SubmitTestDeadlineSeconds +
    ' -SubmitTestPerAttemptSeconds ' + $SubmitTestPerAttemptSeconds +
    ' -SubmitTestSettleSeconds ' + $SubmitTestSettleSeconds +
    $(if ($SkipSubmitTest) { ' -SkipSubmitTest' } else { '' })
  $created = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = $commandLine }
  if ($created.ReturnValue -ne 0) {
    throw "detached worker could not be created (Win32_Process.Create returned $($created.ReturnValue))"
  }
  return [int]$created.ProcessId
}

if ($ProbeEvidenceToken) {
  $probeFile = Get-RolloutFile -ThreadId $ProbeEvidenceThreadId
  $probeExpected = if ([string]::IsNullOrWhiteSpace($ProbeEvidenceExpectedText)) { $ProbeEvidenceToken } else { $ProbeEvidenceExpectedText }
  $probeEvidence = Get-TurnEvidence -File $probeFile -Token $ProbeEvidenceToken -ExpectedText $probeExpected
  Write-Host "rollout       : $probeFile"
  Write-Host "tokenSeen     : $($probeEvidence.tokenSeen)"
  Write-Host "userSeen      : $($probeEvidence.userSeen)   (requires the user item to equal the expected text exactly)"
  Write-Host "userText      : $($probeEvidence.userText)"
  Write-Host "completed     : $($probeEvidence.completed)"
  Write-Host "assistantText : $($probeEvidence.assistantText)"
  exit 0
}

if ($Remove) {
  Stop-Helper
  Restore-Autostart
  if (Test-Path -LiteralPath $InstallerPath) { & $InstallerPath -Mode Rollback }
  Write-Status -Stage "remove" -State "succeeded" -Message "helper stopped, auto-start restored, environment rolled back."
  exit 0
}

if (-not $Worker) {
  Write-Status -Stage "start" -State "running" -Message "foreground setup started"
  Build-Bridge
  Write-Log "bridge built and installed: $BridgeExe"
  Install-Helper
  Write-Log "helper built and installed: $HelperExe"
  # From here on the run changes durable state: the helper auto-start is
  # registered and the CLI environment is activated. Any failure in this region -
  # including the detached worker launch - must put the installer state and the
  # auto-start back exactly as they were, stop the helper, and leave the app
  # alone: the app is never restarted on a foreground failure.
  try {
    $autostartData = Install-Autostart
    Write-Log "helper auto-start registered: $autostartData"
    & $InstallerPath -Mode Install -KeyHelperPath $HelperExe -Force:$Force
    if ($LASTEXITCODE -ne 0) { throw "installer failed with exit code $LASTEXITCODE" }
    Write-Status -Stage "activate" -State "running" -Message "environment activated; bridge is installed" -Extra @{
      downstream = (Get-UserVariable $DownstreamVariable)
      helperAutostart = $autostartData
    }
    if ($NoRestart) {
      Write-Status -Stage "activate" -State "succeeded" -Message "setup complete; -NoRestart was given so the app was left alone."
      exit 0
    }
    $workerPid = Start-DetachedWorker
    Write-Status -Stage "detach" -State "running" -Message "detached worker started; the Codex app will be restarted exactly once." -Extra @{ detachedWorkerPid = $workerPid }
    Write-Host ("Detached worker started (PID $workerPid). Status: $StatusPath")
    exit 0
  } catch {
    $failure = $_.Exception.Message
    Write-Log "foreground activation failed: $failure"
    $rollbackNote = "installer state and auto-start restored, helper stopped"
    try { Invoke-Rollback -RelaunchApp:$false } catch { $rollbackNote = "rollback failed: $($_.Exception.Message)" }
    Write-Status -Stage "activate" -State "failed" -Message ($failure + "; " + $rollbackNote + "; the app was not restarted.") -Extra @{ appRestarted = $false }
    exit 1
  }
}

# ---------------------------------------------------------------- worker mode

Write-Log "worker $PID started for run $RunId"
$package = Get-CodexPackage
$aumid = if ($package -and $package.PackageFamilyName) { "$($package.PackageFamilyName)!App" } else { $null }
$restoreCliPath = Get-UserVariable $DownstreamVariable
$appStopped = $false

try {
  $problems = Test-PreRestart
  if ($problems.Count -gt 0) {
    # The foreground run already activated the installer state and the helper
    # auto-start, so a pre-restart failure still has to put them back. The app was
    # never stopped here, so the rollback must not relaunch it.
    Write-Log ("pre-restart checks failed: " + ($problems -join "; "))
    $rollbackNote = if ($NoRollback) { "rollback suppressed by -NoRollback: installed state and auto-start left in place" } else { "installer state and auto-start restored, helper stopped" }
    try { Invoke-Rollback -RelaunchApp:$false } catch { $rollbackNote = "rollback failed: $($_.Exception.Message)" }
    Write-Status -Stage "precheck" -State "failed" -Message ("pre-restart checks failed (" + ($problems -join "; ") + "); " + $rollbackNote + "; the app was not restarted.") -Extra @{ problems = $problems; appRestarted = $false }
    exit 1
  }
  Write-Log "pre-restart checks passed"

  Write-Status -Stage "restart" -State "running" -Message "waiting $DetachDelaySeconds s, then restarting the Codex app once."
  Start-Sleep -Seconds $DetachDelaySeconds
  # Set before the stop so a stop that fails part-way still counts as "the app was
  # touched" and gets a relaunch on the way out.
  $appStopped = $true
  Stop-CodexApp -Root $package.InstallLocation
  Start-Sleep -Seconds 2
  Start-CodexApp -Aumid $aumid -CliPath $BridgeExe -Downstream $restoreCliPath
  Write-Log "Codex app relaunched"

  $result = Test-PostRestart -Root $package.InstallLocation
  $extra = @{
    appRunning = [bool]$result.app
    chainOk = [bool]$result.chain.ok
    bridgePid = [int]$result.chain.bridgePid
    downstreamPid = [int]$result.chain.downstreamPid
    realCliPid = [int]$result.chain.realCliPid
    bridgeConnected = [bool]$result.bridge
    helperRunning = [bool]$result.helper
  }
  if ($result.app -and $result.chain.ok -and $result.bridge -and $result.helper) {
    Write-Status -Stage "verify" -State "succeeded" -Message "bridge -> downstream -> real codex chain is live and the bridge reports connected." -Extra $extra
    if ($SkipSubmitTest) { exit 0 }
    Write-Status -Stage "submit-test" -State "running" -Message "temporary test mode: injecting $SubmitTestTarget force-submit turns sequentially." -Extra $extra
    $test = Invoke-SubmitTest
    Write-Log ("submit test: " + $test.state + ", completed " + $test.completedTurns + "/" + $test.target + " in " + $test.attemptsRun + " attempts")
    # The UI rendering of an injected turn cannot be confirmed from here. That is
    # a recording gap, not an install failure, so the install is left in place
    # and the operator-visible check is carried as explicitly pending.
    Write-Status -Stage "submit-test" -State $test.state -Message "live force-submit test finished; UI rendering is not machine-confirmable from here." -Extra @{
      submitTestTarget = $test.target
      submitTestCompletedTurns = $test.completedTurns
      submitTestAttemptsRun = $test.attemptsRun
      submitTestMode = $test.mode
      submitTestPersistentBypassEnabled = $test.persistentBypassEnabled
      uiVisibility = $test.uiVisibility
      uiCheck = $test.uiCheck
      uiThreadId = $test.threadId
      chainAfterTestOk = $test.chainAfterTestOk
      chainAfterTestBridgePid = $test.chainAfterTestBridgePid
      chainAfterTestDownstreamPid = $test.chainAfterTestDownstreamPid
      chainAfterTestRealCliPid = $test.chainAfterTestRealCliPid
      helperRunningAfterTest = $test.helperRunningAfterTest
      helperLogMentionsTestToken = $test.helperLogMentionsTestToken
      submitTestAttempts = $test.attempts
    }
    exit 0
  }

  Write-Status -Stage "verify" -State "failed" -Message $(if ($NoRollback) { "post-restart health checks failed; -NoRollback keeps the installed feature in place." } else { "post-restart health checks failed; rolling back." }) -Extra $extra
  Invoke-Rollback -RelaunchApp:$appStopped -Aumid $aumid -RestoreCliPath $restoreCliPath
  $finishMessage = if ($NoRollback) { "post-restart checks failed; -NoRollback kept the installed feature and the relaunched app in place." } else { "post-restart checks failed, environment restored and the original app relaunched." }
  Write-Status -Stage "rollback" -State "failed" -Message $finishMessage -Extra $extra
  exit 1
} catch {
  $failure = $_.Exception.Message
  Write-Log "worker failed: $failure"
  try { Invoke-Rollback -RelaunchApp:$appStopped -Aumid $aumid -RestoreCliPath $restoreCliPath } catch { Write-Log "rollback failed: $($_.Exception.Message)" }
  Write-Status -Stage "worker" -State "failed" -Message $failure -Extra @{ appRestarted = [bool]$appStopped }
  exit 1
}
