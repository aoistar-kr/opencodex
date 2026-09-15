param(
  [Parameter(Mandatory = $true)]
  [string]$PackagePath
)

$ErrorActionPreference = 'Stop'

$package = (Resolve-Path -LiteralPath $PackagePath).Path
if ([IO.Path]::GetExtension($package) -ne '.tgz') {
  throw "Expected a .tgz package: $package"
}

$npmCommand = Get-Command npm.cmd -ErrorAction SilentlyContinue
if (-not $npmCommand) { $npmCommand = Get-Command npm -ErrorAction Stop }
$prefix = (& $npmCommand.Source prefix -g).Trim()
if (-not [IO.Path]::IsPathFullyQualified($prefix)) { throw "npm global prefix is not absolute: $prefix" }

$installRoot = Join-Path $prefix 'node_modules\@bitkyc08\opencodex'
$expectedRoot = [IO.Path]::GetFullPath((Join-Path $prefix 'node_modules'))
$resolvedInstallRoot = [IO.Path]::GetFullPath($installRoot)
if (-not $resolvedInstallRoot.StartsWith($expectedRoot, [StringComparison]::OrdinalIgnoreCase)) {
  throw "Refusing to manage an OpenCodex path outside npm's global node_modules: $resolvedInstallRoot"
}

$backupBase = Join-Path $env:LOCALAPPDATA 'OpenCodex\cicd-backups'
$backup = Join-Path $backupBase ([DateTime]::UtcNow.ToString('yyyyMMdd-HHmmssfff'))
New-Item -ItemType Directory -Force -Path $backup | Out-Null

$packageBackup = Join-Path $backup 'package'
$wrapperBackup = Join-Path $backup 'wrappers'
New-Item -ItemType Directory -Force -Path $wrapperBackup | Out-Null

$wrapperNames = @('ocx', 'ocx.cmd', 'ocx.ps1', 'opencodex', 'opencodex.cmd', 'opencodex.ps1')
if (Test-Path -LiteralPath $installRoot) {
  Copy-Item -LiteralPath $installRoot -Destination $packageBackup -Recurse -Force
}
foreach ($name in $wrapperNames) {
  $candidate = Join-Path $prefix $name
  if (Test-Path -LiteralPath $candidate) {
    Copy-Item -LiteralPath $candidate -Destination (Join-Path $wrapperBackup $name) -Force
  }
}

function Restore-PreviousInstall {
  if (Test-Path -LiteralPath $installRoot) {
    Remove-Item -LiteralPath $installRoot -Recurse -Force
  }
  if (Test-Path -LiteralPath $packageBackup) {
    Copy-Item -LiteralPath $packageBackup -Destination $installRoot -Recurse -Force
  }
  foreach ($name in $wrapperNames) {
    $target = Join-Path $prefix $name
    $saved = Join-Path $wrapperBackup $name
    if (Test-Path -LiteralPath $saved) {
      Copy-Item -LiteralPath $saved -Destination $target -Force
    } elseif (Test-Path -LiteralPath $target) {
      Remove-Item -LiteralPath $target -Force
    }
  }
}

try {
  & $npmCommand.Source install -g --no-audit --no-fund $package
  if ($LASTEXITCODE -ne 0) { throw "npm install -g failed with exit code $LASTEXITCODE" }

  $manifestPath = Join-Path $installRoot 'package.json'
  if (-not (Test-Path -LiteralPath $manifestPath)) { throw "Installed OpenCodex package.json is missing" }
  $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
  if ($manifest.name -ne '@bitkyc08/opencodex') { throw "Installed package identity is invalid: $($manifest.name)" }
  Write-Host "Installed OpenCodex $($manifest.version) from validated CI artifact."
} catch {
  Write-Warning "OpenCodex deployment failed; restoring the previous global installation."
  Restore-PreviousInstall
  throw
}
