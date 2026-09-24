#Requires -Version 5.1
<#
.SYNOPSIS
  Builds enter-force-submit.exe with the csc.exe that ships with Windows.
.DESCRIPTION
  No SDK and no package install: the helper is one C# file compiled against the
  .NET Framework UI Automation assemblies already present on a Windows install.
  Output: bin\enter-force-submit.exe
#>
[CmdletBinding()]
param(
  [switch]$Force
)

$ErrorActionPreference = "Stop"

$root = $PSScriptRoot
$source = Join-Path $root "EnterForceSubmit.cs"
$outputDirectory = Join-Path $root "bin"
$output = Join-Path $outputDirectory "enter-force-submit.exe"

$frameworkRoot = Join-Path $env:WINDIR "Microsoft.NET\Framework64\v4.0.30319"
$compiler = Join-Path $frameworkRoot "csc.exe"
$uiAutomation = Join-Path $frameworkRoot "WPF"

$required = @(
  $compiler
  (Join-Path $uiAutomation "UIAutomationClient.dll")
  (Join-Path $uiAutomation "UIAutomationTypes.dll")
)
foreach ($path in $required) {
  if (-not (Test-Path -LiteralPath $path)) {
    throw "Required build input is missing: $path"
  }
}

if ((-not $Force) -and (Test-Path -LiteralPath $output)) {
  $built = (Get-Item -LiteralPath $output).LastWriteTimeUtc
  $edited = (Get-Item -LiteralPath $source).LastWriteTimeUtc
  if ($built -ge $edited) {
    Write-Host "up to date: $output"
    exit 0
  }
}

New-Item -ItemType Directory -Path $outputDirectory -Force | Out-Null

$compilerArguments = @(
  "/nologo"
  "/target:exe"
  "/optimize+"
  "/out:$output"
  "/reference:$(Join-Path $uiAutomation 'UIAutomationClient.dll')"
  "/reference:$(Join-Path $uiAutomation 'UIAutomationTypes.dll')"
  $source
)

& $compiler @compilerArguments

if ($LASTEXITCODE -ne 0) {
  throw "csc failed with exit code $LASTEXITCODE"
}

Write-Host "built: $output"
