#Requires -Version 5.1
<#
.SYNOPSIS
  Keeps the Codex desktop composer send button and Enter key active when the
  account usage banner reports a hard rate-limit block.
.DESCRIPTION
  The patch changes only the renderer call-site value
  `rateLimitSendBlocked:Oi` to `rateLimitSendBlocked:!1`. The rate-limit banner,
  model metadata, request payload, and server-side enforcement remain intact.

  Microsoft Store updates replace app.asar, so rerun this script after an app
  update. Use -Restore to put back the archive saved for the installed version.
#>
[CmdletBinding()]
param(
  [switch]$Worker,
  [switch]$Restore,
  [string]$PatchedArchive,
  [string]$InstallLocation,
  [string]$RunId
)

$ErrorActionPreference = "Stop"
$PackageName = "OpenAI.Codex"
$PackageFamily = "OpenAI.Codex_2p2nqsd0c76g0"
$Aumid = "$PackageFamily!App"
$RelativeArchive = "app\resources\app.asar"
$BundlePath = @("webview", "assets", "app-primary-a7ff54c980af.js")
$OldGate = [Text.Encoding]::UTF8.GetBytes("rateLimitSendBlocked:Oi")
$NewGate = [Text.Encoding]::UTF8.GetBytes("rateLimitSendBlocked:!1")
$StateRoot = Join-Path $env:LOCALAPPDATA "OpenCodex\codex-desktop-patches\usage-submit"
$StatusPath = Join-Path $StateRoot "latest.json"

function Write-Status {
  param([string]$Stage, [string]$State, [string]$Message)
  New-Item -ItemType Directory -Path $StateRoot -Force | Out-Null
  $payload = [ordered]@{
    runId = $RunId
    stage = $Stage
    state = $State
    message = $Message
    updatedAt = [DateTimeOffset]::Now.ToString("o")
  }
  $temporary = "$StatusPath.tmp-$PID"
  $payload | ConvertTo-Json | Set-Content -LiteralPath $temporary -Encoding UTF8
  Move-Item -LiteralPath $temporary -Destination $StatusPath -Force
}

function Find-ByteSequence {
  param([byte[]]$Haystack, [byte[]]$Needle)
  $hits = [Collections.Generic.List[int]]::new()
  for ($i = 0; $i -le $Haystack.Length - $Needle.Length; $i++) {
    if ($Haystack[$i] -ne $Needle[0]) { continue }
    $matched = $true
    for ($j = 1; $j -lt $Needle.Length; $j++) {
      if ($Haystack[$i + $j] -ne $Needle[$j]) { $matched = $false; break }
    }
    if ($matched) { $hits.Add($i); $i += $Needle.Length - 1 }
  }
  return @($hits)
}

function Get-AsarEntry {
  param([string]$ArchivePath)
  $stream = [IO.File]::Open($ArchivePath, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
  try {
    $reader = [IO.BinaryReader]::new($stream)
    if ($reader.ReadUInt32() -ne 4) { throw "Unsupported ASAR header." }
    $headerSize = $reader.ReadUInt32()
    [void]$reader.ReadUInt32()
    $jsonSize = $reader.ReadUInt32()
    $jsonBytes = $reader.ReadBytes($jsonSize)
    $json = [Text.Encoding]::UTF8.GetString($jsonBytes)
    $node = ($json | ConvertFrom-Json).files
    foreach ($part in $BundlePath) {
      if (-not $node.PSObject.Properties[$part]) { throw "ASAR entry was not found: $($BundlePath -join '/')" }
      $node = $node.$part
      if ($part -ne $BundlePath[-1]) { $node = $node.files }
    }
    return [pscustomobject]@{
      HeaderSize = [int64]$headerSize
      JsonSize = [int]$jsonSize
      Json = $json
      Size = [int]$node.size
      Offset = [int64]$node.offset
      Hash = [string]$node.integrity.hash
      DataOffset = [int64]8 + [int64]$headerSize + [int64]$node.offset
    }
  } finally {
    $stream.Dispose()
  }
}

function New-PatchedArchive {
  param([string]$Source, [string]$Destination)
  Copy-Item -LiteralPath $Source -Destination $Destination -Force
  $entry = Get-AsarEntry -ArchivePath $Destination
  $stream = [IO.File]::Open($Destination, [IO.FileMode]::Open, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None)
  try {
    $stream.Position = $entry.DataOffset
    $bundle = [byte[]]::new($entry.Size)
    $read = $stream.Read($bundle, 0, $bundle.Length)
    if ($read -ne $bundle.Length) { throw "Could not read the complete renderer bundle." }

    $oldHits = @(Find-ByteSequence -Haystack $bundle -Needle $OldGate)
    $newHits = @(Find-ByteSequence -Haystack $bundle -Needle $NewGate)
    if ($oldHits.Count -eq 1 -and $newHits.Count -eq 0) {
      [Array]::Copy($NewGate, 0, $bundle, $oldHits[0], $NewGate.Length)
      $stream.Position = $entry.DataOffset
      $stream.Write($bundle, 0, $bundle.Length)
    } elseif ($oldHits.Count -eq 0 -and $newHits.Count -eq 1) {
      # Already patched; still normalize the integrity metadata below.
    } else {
      throw "Expected exactly one unpatched usage-send gate; found old=$($oldHits.Count), patched=$($newHits.Count)."
    }

    $sha = [Security.Cryptography.SHA256]::Create()
    try { $newHash = ([BitConverter]::ToString($sha.ComputeHash($bundle))).Replace("-", "").ToLowerInvariant() }
    finally { $sha.Dispose() }

    $oldHashCount = ([regex]::Matches($entry.Json, [regex]::Escape($entry.Hash))).Count
    if ($oldHashCount -ne 2) { throw "Expected the one-block ASAR integrity hash twice; found $oldHashCount copies." }
    $newJson = $entry.Json.Replace($entry.Hash, $newHash)
    $newJsonBytes = [Text.Encoding]::UTF8.GetBytes($newJson)
    if ($newJsonBytes.Length -ne $entry.JsonSize) { throw "ASAR header size changed unexpectedly." }
    $stream.Position = 16
    $stream.Write($newJsonBytes, 0, $newJsonBytes.Length)
    $stream.Flush($true)
  } finally {
    $stream.Dispose()
  }

  $verified = Get-AsarEntry -ArchivePath $Destination
  if ($verified.Hash -ne $newHash) { throw "Patched ASAR integrity metadata did not verify." }
}

function Stop-CodexPackageProcesses {
  param([string]$PackageRoot)
  $targets = @(Get-CimInstance Win32_Process | Where-Object {
    $_.ExecutablePath -and $_.ExecutablePath.StartsWith($PackageRoot, [StringComparison]::OrdinalIgnoreCase)
  })
  $targetIds = @{}
  foreach ($process in $targets) { $targetIds[[uint32]$process.ProcessId] = $true }
  $roots = @($targets | Where-Object { -not $targetIds.ContainsKey([uint32]$_.ParentProcessId) })
  foreach ($process in $roots) {
    $killer = Start-Process -FilePath "$env:SystemRoot\System32\taskkill.exe" `
      -ArgumentList @("/PID", [string]$process.ProcessId, "/T", "/F") `
      -WindowStyle Hidden -Wait -PassThru
    if ($killer.ExitCode -ne 0 -and (Get-Process -Id $process.ProcessId -ErrorAction SilentlyContinue)) {
      throw "Could not stop Codex process tree at PID $($process.ProcessId)."
    }
  }
  for ($attempt = 0; $attempt -lt 10; $attempt++) {
    $survivors = @(Get-CimInstance Win32_Process | Where-Object {
      $_.ExecutablePath -and $_.ExecutablePath.StartsWith($PackageRoot, [StringComparison]::OrdinalIgnoreCase)
    })
    if ($survivors.Count -eq 0) { return }
    Start-Sleep -Milliseconds 500
  }
  throw "Codex package processes survived shutdown."
}

function Copy-FileContents {
  param([string]$Source, [string]$Destination)
  $inputStream = [IO.File]::Open($Source, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
  try {
    # WindowsApps denies replacing a package member through its parent directory even
    # after elevation. Opening the already-owned file itself for writes is permitted.
    $outputStream = [IO.File]::Open($Destination, [IO.FileMode]::Open, [IO.FileAccess]::Write, [IO.FileShare]::None)
    try {
      $outputStream.SetLength($inputStream.Length)
      $inputStream.CopyTo($outputStream, 4MB)
      $outputStream.Flush($true)
    } finally {
      $outputStream.Dispose()
    }
  } finally {
    $inputStream.Dispose()
  }
}

if (-not $Worker) {
  Import-Module Appx -ErrorAction SilentlyContinue
  $pkg = Get-AppxPackage -Name $PackageName | Where-Object { $_.PackageFamilyName -eq $PackageFamily }
  if (-not $pkg -or -not $pkg.InstallLocation) { throw "Codex desktop MSIX package was not found." }
  $InstallLocation = $pkg.InstallLocation
  $source = Join-Path $InstallLocation $RelativeArchive
  $version = [string]$pkg.Version
  $RunId = [DateTimeOffset]::Now.ToString("yyyyMMdd-HHmmss")
  $versionRoot = Join-Path $StateRoot $version
  New-Item -ItemType Directory -Path $versionRoot -Force | Out-Null

  if ($Restore) {
    $backup = Join-Path $versionRoot "app.asar.original"
    if (-not (Test-Path -LiteralPath $backup)) { throw "No backup exists for Codex $version at $backup" }
    $PatchedArchive = $backup
    Write-Status -Stage "queued" -State "running" -Message "Original Codex archive restore is awaiting elevation."
  } else {
    $PatchedArchive = Join-Path $versionRoot "app.asar.usage-submit-patched"
    Write-Status -Stage "prepare" -State "running" -Message "Preparing the Codex usage-send patch."
    New-PatchedArchive -Source $source -Destination $PatchedArchive
    Write-Status -Stage "queued" -State "running" -Message "Patch prepared; installation is awaiting elevation."
  }

  $shell = (Get-Command powershell.exe -ErrorAction Stop).Source
  $arguments = @(
    "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", ('"{0}"' -f $PSCommandPath),
    "-Worker", "-RunId", $RunId,
    "-InstallLocation", ('"{0}"' -f $InstallLocation),
    "-PatchedArchive", ('"{0}"' -f $PatchedArchive)
  )
  if ($Restore) { $arguments += "-Restore" }
  Start-Process -FilePath $shell -ArgumentList $arguments -Verb RunAs -WindowStyle Hidden | Out-Null
  Write-Host "Codex patch worker started. The app will close and reopen after the UAC prompt is accepted."
  Write-Host "Status: $StatusPath"
  exit 0
}

try {
  if (-not $InstallLocation -or -not $PatchedArchive -or -not $RunId) { throw "Worker arguments are incomplete." }
  $target = Join-Path $InstallLocation $RelativeArchive
  $version = (Split-Path -Leaf (Split-Path -Parent $PatchedArchive))
  $backup = Join-Path (Split-Path -Parent $PatchedArchive) "app.asar.original"
  Write-Status -Stage "stop" -State "running" -Message "Stopping Codex desktop processes."
  Stop-CodexPackageProcesses -PackageRoot $InstallLocation

  if (-not $Restore -and -not (Test-Path -LiteralPath $backup)) {
    Copy-Item -LiteralPath $target -Destination $backup
  }

  Write-Status -Stage "install" -State "running" -Message $(if ($Restore) { "Restoring the original Codex archive." } else { "Installing the usage-send patch." })
  & "$env:SystemRoot\System32\takeown.exe" /F $target /A | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "Could not take ownership of the installed Codex archive." }
  & "$env:SystemRoot\System32\icacls.exe" $target /grant "*S-1-5-32-544:F" /Q | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "Could not grant archive replacement access." }
  Copy-FileContents -Source $PatchedArchive -Destination $target

  Write-Status -Stage "restart" -State "running" -Message "Restarting Codex desktop."
  Start-Process "shell:AppsFolder\$Aumid"
  Write-Status -Stage "complete" -State "succeeded" -Message $(if ($Restore) { "Original Codex archive restored." } else { "Usage-limit send blocking disabled; Codex restarted." })
} catch {
  Write-Status -Stage "failed" -State "failed" -Message $_.Exception.Message
  try { Start-Process "shell:AppsFolder\$Aumid" } catch {}
  throw
}
