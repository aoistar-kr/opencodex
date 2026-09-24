#Requires -Version 5.1
<#
.SYNOPSIS
  Retired. This script refuses to run and changes nothing.

.DESCRIPTION
  It used to stop every Codex desktop process, overwrite resources\app.asar
  inside the installed WindowsApps package, and relaunch the app. The write never
  succeeded, and the stop step is what kept closing the app while the operator
  was working in it.

  The approach is abandoned in favour of the app's supported CODEX_CLI_PATH
  override, persisted by scripts/install-codex-desktop-cli-path.ps1.

  This stub stays fail-closed so a stale command line or a habit cannot stop the
  app again. It declares no parameters, touches no process, archive, or setting,
  and exits non-zero.
#>
[CmdletBinding()]
param()

Write-Error "scripts/patch-codex-desktop-usage-submit.ps1 is retired. It cannot stop Codex or write app.asar. Use scripts/install-codex-desktop-cli-path.ps1 instead."
exit 1
