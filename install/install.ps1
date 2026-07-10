#!/usr/bin/env pwsh
#
# install.ps1 — install hermes-node on Windows and register it as a Task
# Scheduler task that starts at user logon.
#
# Downloads a release binary from GitHub Releases (https://github.com/blaspat/
# hermes-node/releases), drops it in %LOCALAPPDATA%\Programs\hermes-node\
# (and adds that directory to the user's PATH if missing), and registers a
# per-user scheduled task that runs the binary at logon.
#
# Parameters
# ----------
#   -Version <tag>    install a specific release (default: latest)
#   -DryRun           show what would happen, change nothing, exit 0
#   -PrintLayout      print the planned file paths and exit (no network call)
#   -NoService        install the binary but do not register a task
#   -Uninstall        remove the binary and task registration
#   -Yes              skip the "already installed" confirmation prompt
#   -Repo <owner/name> GitHub repo (default: blaspat/hermes-node)
#   -Help             show this message
#
# Environment overrides
# --------------------
#   HERMES_NODE_VERSION   default value of -Version
#   HERMES_NODE_REPO      default: blaspat/hermes-node
#   HERMES_NODE_BIN_DIR   override the install dir
#   HERMES_NODE_CONFIG_DIR override the config dir
#   HERMES_NODE_DRY_RUN   "1" to dry-run
#   HERMES_NODE_NO_SERVICE "1" to skip service registration
#
# Exit codes
# ----------
#   0  success (or dry-run completed without errors)
#   1  generic error
#   2  invalid arguments
#   3  download/verification failed
#   4  unsupported arch
#   5  service registration failed (binary was still installed)

[CmdletBinding()]
param(
  [string]$Version = $env:HERMES_NODE_VERSION,
  [switch]$DryRun = [bool]::Parse((
    if ($env:HERMES_NODE_DRY_RUN -eq '1') { 'True' } else { 'False' }
  )),
  [switch]$PrintLayout,
  [switch]$NoService = [bool]::Parse((
    if ($env:HERMES_NODE_NO_SERVICE -eq '1') { 'True' } else { 'False' }
  )),
  [switch]$Uninstall,
  [switch]$Yes = $false,
  [string]$Repo = (
    if ($env:HERMES_NODE_REPO) { $env:HERMES_NODE_REPO } else { 'blaspat/hermes-node' }
  )
)

$ErrorActionPreference = 'Stop'
$script:LogPrefix = '==>'
$script:BinName = 'hermes-node'
$script:TaskName = 'HermesNode'

function Write-Log {
  param([string]$Message)
  Write-Host "$script:LogPrefix $Message"
}

function Write-Warn {
  param([string]$Message)
  Write-Warning $Message
}

function Write-Die {
  param([string]$Message, [int]$Code = 1)
  Write-Error "error: $Message"
  exit $Code
}

function Show-Help {
  Get-Content $PSCommandPath |
    Select-String -Pattern '^(# |\.\.\/)' -NotMatch |
    Select-String -Pattern '^# ' |
    ForEach-Object { $_ -replace '^# ?', '' } |
    Where-Object { $_ -and $_ -notmatch '^Parameters$' -and $_ -notmatch '^Exit codes$' } |
    ForEach-Object { Write-Host $_ }
  exit 0
}

# Help is intentionally not exposed as a declared [switch] parameter (the
# intent is to keep the help text in the script header and not duplicate
# it in a Parameter() block). The -h/--help checks via $args cover the
# only two forms we want to accept.
if ($args -contains '-h' -or $args -contains '--help') {
  Show-Help
}

# ---------------------------------------------------------------------------
# Arch detection
# ---------------------------------------------------------------------------

$Arch = ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture).ToString().ToLower()
switch ($Arch) {
  'x64'  { $AssetArch = 'amd64' }
  'arm64'{ $AssetArch = 'arm64' }
  default { Write-Die "unsupported arch: $Arch" 4 }
}

$AssetName = "$script:BinName-windows-$AssetArch.exe"

# ---------------------------------------------------------------------------
# Path resolution
# ---------------------------------------------------------------------------

$BinDir = if ($env:HERMES_NODE_BIN_DIR) { $env:HERMES_NODE_BIN_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\hermes-node' }
$ConfigDir = if ($env:HERMES_NODE_CONFIG_DIR) { $env:HERMES_NODE_CONFIG_DIR } else { Join-Path $env:APPDATA 'hermes-node' }
$BinPath = Join-Path $BinDir "$script:BinName.exe"
$TaskCmd = "`"$BinPath`""

# ---------------------------------------------------------------------------
# -PrintLayout: report the planned layout and exit before doing anything
# ---------------------------------------------------------------------------

if ($PrintLayout) {
  $layout = [ordered]@{
    os           = 'windows'
    arch         = $AssetArch
    asset        = $AssetName
    binary       = $BinPath
    config_dir   = $ConfigDir
    service      = [ordered]@{
      kind     = 'task-scheduler'
      task     = $script:TaskName
    }
    version_requested = if ($Version) { $Version } else { 'latest' }
    dry_run     = [bool]$DryRun
  }
  $layout | ConvertTo-Json -Depth 5
  exit 0
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

function Get-InstalledVersion {
  param([string]$Path)
  if (-not (Test-Path -LiteralPath $Path)) { return $null }
  try {
    $out = & $Path --version 2>$null
    if ($LASTEXITCODE -ne 0) { return $null }
    if ($out -like 'hermes-node *') { return ($out -split ' ', 2)[1] }
  } catch {
    return $null
  }
  return $null
}

function Get-LatestReleaseTag {
  # Prefer gh (authenticated) and fall back to the public releases API. The
  # API path works for unauthenticated callers but is rate-limited to 60/h per
  # IP, which is fine for occasional installs.
  if (Get-Command gh -ErrorAction SilentlyContinue) {
    try {
      return gh release view --repo $Repo --json tagName --jq '.tagName' 2>$null
    } catch {
      # fall through to curl path
    }
  }
  $api = "https://api.github.com/repos/$Repo/releases/latest"
  try {
    $resp = Invoke-RestMethod -Uri $api -Method Get -TimeoutSec 15
    return $resp.tagName
  } catch {
    return $null
  }
}

function Confirm-Proceed {
  param([string]$Message)
  if ($Yes) { return $true }
  $old = $Host.UI.RawUI.ReadKey
  Write-Host ''
  return ($old.Character -ieq 'y')
}

function Invoke-Actual {
  [CmdletBinding()]
  param(
    [Parameter(Mandatory = $true)][scriptblock]$Block,
    [string]$WhatIf
  )
  if ($DryRun) {
    if ($WhatIf) {
      Write-Host "  [dry-run] $WhatIf"
    } else {
      Write-Host "  [dry-run] $($Block.ToString().Trim())"
    }
    return
  }
  & $Block
}

# ---------------------------------------------------------------------------
# Uninstall path
# ---------------------------------------------------------------------------

function Invoke-Uninstall {
  Write-Log "uninstalling $script:BinName"

  # Stop + remove the scheduled task (if any).
  $existing = Get-ScheduledTask -TaskName $script:TaskName -ErrorAction SilentlyContinue
  if ($existing) {
    Invoke-Actual -WhatIf "would run: Unregister-ScheduledTask -TaskName '$script:TaskName' -Confirm:`$false" -Block {
      Unregister-ScheduledTask -TaskName $script:TaskName -Confirm:$false
    }
    Write-Log "removed scheduled task: $script:TaskName"
  } else {
    Write-Log "no scheduled task named '$script:TaskName'"
  }

  if (Test-Path -LiteralPath $BinPath) {
    Invoke-Actual -WhatIf "would run: Remove-Item -LiteralPath '$BinPath' -Force" -Block {
      Remove-Item -LiteralPath $BinPath -Force
    }
    Write-Log "removed binary: $BinPath"
  } else {
    Write-Log "no binary at $BinPath"
  }

  Write-Log "config dir left in place: $ConfigDir (remove manually for a full wipe)"
  Write-Log "uninstall complete"
}

# ---------------------------------------------------------------------------
# Install path
# ---------------------------------------------------------------------------

function Invoke-Install {
  Write-Log "installing $script:BinName for windows/$AssetArch"

  # --- resolve version (--version > env > latest) ---------------------
  if (-not $Version) {
    Write-Log "looking up latest release of $Repo"
    if ($DryRun) {
      $Version = 'v0.0.0-dryrun'
      Write-Host "  [dry-run] would call: gh release view --repo $Repo"
    } else {
      $Version = Get-LatestReleaseTag
      if (-not $Version) {
        Write-Die "could not determine the latest release; pass -Version <tag> explicitly" 1
      }
    }
  }
  Write-Log "version: $Version"

  # --- check for existing install ------------------------------------
  if (Test-Path -LiteralPath $BinPath) {
    $existing = Get-InstalledVersion -Path $BinPath
    if ($existing) {
      Write-Log "already installed: $existing at $BinPath"
      if ($existing -eq $Version) {
        Write-Log "installed version matches; nothing to do"
      } elseif (-not (Confirm-Proceed "upgrade $existing -> $Version?")) {
        Write-Die "aborted by user"
      }
    } else {
      if (-not (Confirm-Proceed "replace existing file at $BinPath?")) {
        Write-Die "aborted by user"
      }
    }
  }

  # --- download -------------------------------------------------------
  # Asset filename is OS/ARCH only (matches scripts/build.sh):
  #   https://github.com/$Repo/releases/download/$Version/hermes-node-windows-<arch>.exe
  $DownloadUrl = "https://github.com/$Repo/releases/download/$Version/$script:BinName-windows-$AssetArch.exe"
  $TmpFile = $null
  if ($DryRun) {
    Write-Host "  [dry-run] would download: $DownloadUrl"
  } else {
    $TmpFile = Join-Path ([System.IO.Path]::GetTempPath()) ("hermes-node-install-" + [System.Guid]::NewGuid().ToString('N') + '.exe')
    try {
      Write-Log "downloading with Invoke-WebRequest"
      Invoke-WebRequest -Uri $DownloadUrl -OutFile $TmpFile -UseBasicParsing -TimeoutSec 60
      if ((Get-Item -LiteralPath $TmpFile).Length -eq 0) {
        Write-Die "downloaded file is empty: $TmpFile" 3
      }
    } catch {
      if (Test-Path -LiteralPath $TmpFile) { Remove-Item -LiteralPath $TmpFile -Force }
      Write-Die "download failed: $DownloadUrl ($($_.Exception.Message))" 3
    }
  }

  # --- place binary ---------------------------------------------------
  if (-not (Test-Path -LiteralPath $BinDir)) {
    Invoke-Actual -WhatIf "would run: New-Item -ItemType Directory -Path '$BinDir' -Force" -Block {
      New-Item -ItemType Directory -Path $BinDir -Force | Out-Null
    }
  }
  if ($DryRun) {
    Write-Host "  [dry-run] would install binary to $BinPath"
  } else {
    Invoke-Actual -WhatIf "would run: Move-Item '$TmpFile' '$BinPath' -Force" -Block {
      Move-Item -LiteralPath $TmpFile -Destination $BinPath -Force
    }
    Write-Log "installed binary: $BinPath"
  }

  # --- add to user PATH (best-effort) --------------------------------
  Add-ToUserPath -Dir $BinDir

  # --- service registration -----------------------------------------
  if ($NoService) {
    Write-Log "-NoService set; skipping scheduled task registration"
  } else {
    Register-ServiceTask
  }

  # --- user-facing summary ------------------------------------------
  Write-Host ""
  Write-Host "$script:BinName $Version is installed."
  Write-Host ""
  Write-Host "  binary:  $BinPath"
  Write-Host "  config:  $ConfigDir  (created on first run via 'hermes-node pair')"
  Write-Host "  service: Task Scheduler task '$script:TaskName' (triggers at logon)"
  Write-Host ""
  Write-Host "Next step — pair this node with your Hermes Agent brain:"
  Write-Host ""
  Write-Host "  $script:BinName pair --server <wss-url> --token <token>"
  Write-Host ""
}

# ---------------------------------------------------------------------------
# PATH manipulation
# ---------------------------------------------------------------------------

function Add-ToUserPath {
  param([string]$Dir)
  if ($DryRun) {
    Write-Host "  [dry-run] would add '$Dir' to user PATH if not already present"
    return
  }
  $current = [Environment]::GetEnvironmentVariable('Path', 'User')
  $parts = if ($current) { $current -split ';' } else { @() }
  $present = $parts | Where-Object { $_ -ieq $Dir }
  if ($present) {
    Write-Log "$Dir is already on the user PATH"
    return
  }
  $new = (@($Dir) + $parts) -join ';'
  [Environment]::SetEnvironmentVariable('Path', $new, 'User')
  Write-Log "added $Dir to user PATH (open a new shell to pick it up)"
}

# ---------------------------------------------------------------------------
# Service registration
# ---------------------------------------------------------------------------

function Register-ServiceTask {
  if ($DryRun) {
    Write-Host "  [dry-run] would register scheduled task '$script:TaskName'"
    Write-Host "  [dry-run] would run: Register-ScheduledTask ... -Trigger (New-ScheduledTaskTrigger -AtLogOn) -Settings (New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -RestartCount 5 -RestartInterval (New-TimeSpan -Minutes 1))"
    return
  }

  $existing = Get-ScheduledTask -TaskName $script:TaskName -ErrorAction SilentlyContinue
  if ($existing) {
    Unregister-ScheduledTask -TaskName $script:TaskName -Confirm:$false
  }

  $action = New-ScheduledTaskAction -Execute $BinPath
  $trigger = New-ScheduledTaskTrigger -AtLogOn
  $principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel Limited
  $settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -DontStopOnIdleEnd `
    -RestartCount 5 `
    -RestartInterval (New-TimeSpan -Minutes 1) `
    -ExecutionTimeLimit (New-TimeSpan -Seconds 0)

  try {
    Register-ScheduledTask `
      -TaskName $script:TaskName `
      -Action $action `
      -Trigger $trigger `
      -Principal $principal `
      -Settings $settings `
      -Description 'hermes-node: pairs with a Hermes Agent brain over WSS' | Out-Null
  } catch {
    Write-Warn "scheduled task registration failed: $($_.Exception.Message)"
    Write-Warn "the binary is installed; you can register the task manually with schtasks.exe /Create"
    return $false
  }
  Write-Log "scheduled task registered: $script:TaskName (AtLogOn)"
  return $true
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

if ($Uninstall) {
  Invoke-Uninstall
  exit 0
}

Invoke-Install
exit 0
