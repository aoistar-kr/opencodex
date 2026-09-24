#Requires -Version 5.1
[CmdletBinding()]
param(
  [switch]$Worker,
  [string]$RunId,
  [ValidateRange(0, 300)]
  [int]$DelaySeconds = 12
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$deploymentRoot = Join-Path $env:USERPROFILE ".opencodex\deployments"
$statusPath = Join-Path $deploymentRoot "local-2.64.0-latest.json"

function Write-DeploymentState {
  param(
    [Parameter(Mandatory = $true)][string]$Stage,
    [Parameter(Mandatory = $true)][string]$State,
    [Parameter(Mandatory = $true)][string]$Message,
    [string]$LogPath,
    [int]$WorkerPid = 0
  )
  New-Item -ItemType Directory -Path $deploymentRoot -Force | Out-Null
  $payload = [ordered]@{
    runId = $RunId
    stage = $Stage
    state = $State
    message = $Message
    workerPid = $WorkerPid
    logPath = $LogPath
    updatedAt = [DateTimeOffset]::Now.ToString("o")
  }
  $temporaryPath = "$statusPath.tmp-$PID"
  $payload | ConvertTo-Json | Set-Content -LiteralPath $temporaryPath -Encoding UTF8
  Move-Item -LiteralPath $temporaryPath -Destination $statusPath -Force
}

if (-not $Worker) {
  New-Item -ItemType Directory -Path $deploymentRoot -Force | Out-Null
  if ([string]::IsNullOrWhiteSpace($RunId)) {
    $RunId = [DateTimeOffset]::Now.ToString("yyyyMMdd-HHmmss")
  }
  $shell = Get-Command pwsh.exe -ErrorAction SilentlyContinue
  if (-not $shell) { $shell = Get-Command powershell.exe -ErrorAction Stop }
  $argumentList = @(
    "-NoProfile",
    "-ExecutionPolicy", "Bypass",
    "-File", $PSCommandPath,
    "-Worker",
    "-RunId", $RunId,
    "-DelaySeconds", [string]$DelaySeconds
  )
  $process = Start-Process -FilePath $shell.Source -ArgumentList $argumentList -WindowStyle Hidden -PassThru
  $logPath = Join-Path $deploymentRoot "local-2.64.0-$RunId.log"
  Write-DeploymentState -Stage "queued" -State "running" -Message "Detached deployment worker started." -LogPath $logPath -WorkerPid $process.Id
  Write-Host "Detached deployment worker started (PID $($process.Id))."
  Write-Host "Status: $statusPath"
  Write-Host "Log:    $logPath"
  exit 0
}

if ([string]::IsNullOrWhiteSpace($RunId)) { throw "Worker mode requires -RunId." }
$logPath = Join-Path $deploymentRoot "local-2.64.0-$RunId.log"
New-Item -ItemType Directory -Path $deploymentRoot -Force | Out-Null
Start-Transcript -LiteralPath $logPath -Append | Out-Null

function Invoke-NativeStep {
  param(
    [Parameter(Mandatory = $true)][string]$Stage,
    [Parameter(Mandatory = $true)][string]$Message,
    [Parameter(Mandatory = $true)][scriptblock]$Action
  )
  Write-DeploymentState -Stage $Stage -State "running" -Message $Message -LogPath $logPath -WorkerPid $PID
  & $Action
  if ($LASTEXITCODE -ne 0) { throw "$Stage failed with exit code $LASTEXITCODE." }
}

try {
  Start-Sleep -Seconds $DelaySeconds
  Set-Location -LiteralPath $repoRoot

  $branch = (& git branch --show-current).Trim()
  if ($LASTEXITCODE -ne 0 -or $branch -ne "custom") {
    throw "Expected branch 'custom', found '$branch'."
  }
  $version = (Get-Content -Raw -LiteralPath (Join-Path $repoRoot "package.json") | ConvertFrom-Json).version
  if ($version -ne "2.64.0") { throw "Expected package version 2.64.0, found $version." }

  Invoke-NativeStep -Stage "commit" -Message "Staging and committing the complete working tree." -Action {
    & git add -A
    if ($LASTEXITCODE -ne 0) { throw "git add failed with exit code $LASTEXITCODE." }
    & git diff --cached --quiet
    $diffExit = $LASTEXITCODE
    if ($diffExit -eq 1) {
      & git commit -m "fix(codex): preserve account access metadata"
    } elseif ($diffExit -gt 1) {
      throw "git diff --cached failed with exit code $diffExit."
    }
  }

  Invoke-NativeStep -Stage "push" -Message "Pushing the custom branch to origin." -Action {
    & git push origin HEAD:custom
  }

  $bun = Join-Path $repoRoot "node_modules\.bin\bun.exe"
  if (-not (Test-Path -LiteralPath $bun)) { throw "Repository Bun runtime is missing: $bun" }
  Invoke-NativeStep -Stage "build" -Message "Building the GUI and preparing the npm package." -Action {
    & $bun run build:gui
  }

  $npm = Get-Command npm.cmd -ErrorAction SilentlyContinue
  if (-not $npm) { $npm = Get-Command npm -ErrorAction Stop }
  $artifactRoot = Join-Path $repoRoot ".tmp\local-deploy-$RunId"
  New-Item -ItemType Directory -Path $artifactRoot -Force | Out-Null
  Write-DeploymentState -Stage "pack" -State "running" -Message "Packing version 2.64.0." -LogPath $logPath -WorkerPid $PID
  $packJson = & $npm.Source pack --json --ignore-scripts --pack-destination $artifactRoot
  if ($LASTEXITCODE -ne 0) { throw "npm pack failed with exit code $LASTEXITCODE." }
  $packResult = $packJson | ConvertFrom-Json
  $packageName = @($packResult)[0].filename
  if ([string]::IsNullOrWhiteSpace($packageName)) { throw "npm pack did not return a package filename." }
  $packagePath = Join-Path $artifactRoot $packageName

  Invoke-NativeStep -Stage "install" -Message "Installing the packed 2.64.0 build globally." -Action {
    & $npm.Source install -g --force --allow-scripts=bun $packagePath
  }

  $ocxPath = Join-Path $env:APPDATA "npm\ocx.cmd"
  if (-not (Test-Path -LiteralPath $ocxPath)) { throw "Installed ocx launcher is missing: $ocxPath" }
  Invoke-NativeStep -Stage "restart" -Message "Restarting the OpenCodex background service." -Action {
    & $ocxPath service restart
  }

  Write-DeploymentState -Stage "health" -State "running" -Message "Waiting for the proxy health check." -LogPath $logPath -WorkerPid $PID
  $healthy = $false
  for ($attempt = 1; $attempt -le 20; $attempt++) {
    try {
      $response = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:10100/healthz" -TimeoutSec 5
      if ($response.StatusCode -eq 200) { $healthy = $true; break }
    } catch {
      Start-Sleep -Seconds 2
    }
  }
  if (-not $healthy) { throw "OpenCodex did not become healthy on port 10100." }

  $installedVersion = (& $ocxPath --version | Out-String).Trim()
  Write-DeploymentState -Stage "complete" -State "succeeded" -Message "Committed, pushed, built, installed, and restarted successfully. $installedVersion" -LogPath $logPath -WorkerPid $PID
} catch {
  Write-DeploymentState -Stage "failed" -State "failed" -Message $_.Exception.Message -LogPath $logPath -WorkerPid $PID
  Write-Error $_
  exit 1
} finally {
  Stop-Transcript | Out-Null
}
