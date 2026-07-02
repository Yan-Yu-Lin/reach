param(
  [string]$Name = "",
  [Alias('GodCode','AuthCode')][string]$Token = "",
  [Alias('AuthCodeFile')][string]$TokenFile = $env:REACH_AUTH_CODE_FILE,
  [Alias('User')][string]$TargetUser = "",
  [string]$ApiUrl = $(if ($env:REACH_API_URL) { $env:REACH_API_URL } else { "https://tunnels.your-domain.example" }),
  [ValidateSet('auto','direct','websocket')][string]$Transport = $(if ($env:REACH_TRANSPORT) { $env:REACH_TRANSPORT } else { "auto" }),
  [string]$Version = $(if ($env:REACH_AGENT_VERSION) { $env:REACH_AGENT_VERSION } else { "0.1.0-alpha" }),
  [switch]$Yes,
  [switch]$Repair,
  [switch]$UpdateAgent,
  [switch]$Uninstall,
  [switch]$Reinstall
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

function Log([string]$Message) { Write-Host "[reach] $Message" -ForegroundColor Cyan }
function Warn([string]$Message) { Write-Warning "[reach] $Message" }
function Die([string]$Message) { Write-Error "[reach] $Message"; exit 1 }

function Test-ReachAdmin {
  $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
  $principal = New-Object Security.Principal.WindowsPrincipal($identity)
  return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Get-ReachAgentAsset {
  try {
    $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
  } catch {
    $arch = $env:PROCESSOR_ARCHITECTURE
  }
  switch -Regex ($arch) {
    '^(X64|AMD64)$' { return 'reach-agent_windows_amd64.exe' }
    '^(Arm64|ARM64)$' { return 'reach-agent_windows_arm64.exe' }
    default { Die "Unsupported Windows architecture: $arch" }
  }
}

function Ensure-ReachWindowsCapability([string]$Name, [string]$Label) {
  $cap = Get-WindowsCapability -Online -Name $Name -ErrorAction SilentlyContinue
  if (-not $cap) {
    Die "$Label Windows capability is not available on this OS. Reach Windows targets require Windows 10/Server build 1809+ or a manually installed OpenSSH."
  }
  if ($cap.State -ne 'Installed') {
    Log "Installing $Label optional feature (may require Windows Update / Features on Demand access)"
    Add-WindowsCapability -Online -Name $Name | Out-Null
  }
}

function Add-ReachOpenSSHPath {
  $openSshDir = Join-Path $env:WINDIR 'System32\OpenSSH'
  if ((Test-Path -LiteralPath $openSshDir) -and (($env:Path -split ';') -notcontains $openSshDir)) {
    $env:Path = $env:Path + ';' + $openSshDir
  }
}

function Ensure-ReachOpenSSHServer {
  Log "Checking Windows OpenSSH Client/Server"
  Add-ReachOpenSSHPath
  if (-not (Get-Command ssh-keygen.exe -ErrorAction SilentlyContinue)) {
    Ensure-ReachWindowsCapability 'OpenSSH.Client~~~~0.0.1.0' 'OpenSSH.Client'
  }
  $svc = Get-Service -Name sshd -ErrorAction SilentlyContinue
  if (-not $svc) {
    Ensure-ReachWindowsCapability 'OpenSSH.Server~~~~0.0.1.0' 'OpenSSH.Server'
    $svc = Get-Service -Name sshd -ErrorAction SilentlyContinue
  }
  if (-not $svc) { Die "sshd service was not found after OpenSSH Server installation" }
  Add-ReachOpenSSHPath

  Set-Service -Name sshd -StartupType Automatic
  if ((Get-Service -Name sshd).Status -ne 'Running') {
    Start-Service -Name sshd
  }

  $deadline = (Get-Date).AddSeconds(15)
  while ((Get-Date) -lt $deadline) {
    $tcp = Test-NetConnection -ComputerName 127.0.0.1 -Port 22 -WarningAction SilentlyContinue
    if ($tcp.TcpTestSucceeded) { return }
    Start-Sleep -Milliseconds 500
  }
  Die "OpenSSH Server did not begin listening on 127.0.0.1:22"
}

function Get-ReachChecksum([string]$ChecksumsPath, [string]$Asset) {
  foreach ($line in Get-Content -LiteralPath $ChecksumsPath) {
    $parts = $line -split '\s+'
    if ($parts.Count -ge 2 -and ($parts[-1].TrimStart('*') -eq $Asset)) {
      return $parts[0].ToLowerInvariant()
    }
  }
  Die "No checksum found for $Asset"
}

function Invoke-ReachDownload([string]$Url, [string]$OutFile) {
  Log "Downloading $Url"
  Invoke-WebRequest -Uri $Url -OutFile $OutFile -UseBasicParsing
}

if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
  Die "setup.ps1 supports Windows only"
}
if ($PSVersionTable.PSVersion.Major -lt 5 -or ($PSVersionTable.PSVersion.Major -eq 5 -and $PSVersionTable.PSVersion.Minor -lt 1)) {
  Die "PowerShell 5.1 or later is required"
}
if ([Environment]::OSVersion.Version.Build -lt 17763) {
  Die "Windows 10 / Windows Server build 1809 (17763) or later is required for built-in OpenSSH Server"
}
if (-not (Test-ReachAdmin)) {
  Die "Reach Windows target support is admin-first for now. Re-run PowerShell as Administrator."
}

if (-not $TargetUser) { $TargetUser = $env:USERNAME }
if (-not $TargetUser) { Die "Could not determine target user; pass -TargetUser" }
if (-not $Token -and $TokenFile -and (Test-Path -LiteralPath $TokenFile)) {
  $Token = (Get-Content -LiteralPath $TokenFile -Raw).Trim()
}
if ($Token) { Warn "auth codes passed to setup.ps1 may be visible in process history; prefer -TokenFile for shared machines." }

$InstallDir = Join-Path $env:ProgramFiles 'Reach'
$ProgramDataDir = Join-Path $env:ProgramData 'Reach'
$AgentPath = Join-Path $InstallDir 'reach-agent.exe'

if ($Uninstall -or $Reinstall) {
  Log "Uninstalling existing Reach Windows agent"
  if (Test-Path -LiteralPath $AgentPath) {
    try { & $AgentPath uninstall --mode system --yes } catch { Warn "reach-agent uninstall returned: $_" }
  }
  try {
    $task = Get-ScheduledTask -TaskPath '\Reach\' -TaskName 'reach-agent' -ErrorAction SilentlyContinue
    if ($task) {
      Stop-ScheduledTask -TaskPath '\Reach\' -TaskName 'reach-agent' -ErrorAction SilentlyContinue
      Unregister-ScheduledTask -TaskPath '\Reach\' -TaskName 'reach-agent' -Confirm:$false
    }
  } catch { Warn "Task Scheduler cleanup returned: $_" }
  Remove-Item -LiteralPath $ProgramDataDir -Recurse -Force -ErrorAction SilentlyContinue
  Remove-Item -LiteralPath $AgentPath -Force -ErrorAction SilentlyContinue
  if ($Uninstall) { Log "Reach uninstall complete"; exit 0 }
}

if ($Repair -and -not $UpdateAgent) {
  if (-not (Test-Path -LiteralPath $AgentPath)) { Die "$AgentPath is missing; rerun setup.ps1 install" }
  & $AgentPath check --config (Join-Path $ProgramDataDir 'agent.yaml')
  Start-ScheduledTask -TaskPath '\Reach\' -TaskName 'reach-agent'
  Log "Repair requested; reach-agent scheduled task started."
  exit 0
}
if ($Repair -and $UpdateAgent) {
  Die "Windows self-update is not supported yet; use -Reinstall to replace the agent binary."
}

$asset = Get-ReachAgentAsset
$baseUrl = ($ApiUrl.TrimEnd('/')) + "/downloads/reach-agent/v$Version"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("reach-setup-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
try {
  $candidate = Join-Path $tmp 'reach-agent.exe'
  $checksums = Join-Path $tmp 'checksums.txt'
  Invoke-ReachDownload "$baseUrl/$asset" $candidate
  Invoke-ReachDownload "$baseUrl/checksums.txt" $checksums
  $expected = Get-ReachChecksum $checksums $asset
  $actual = (Get-FileHash -LiteralPath $candidate -Algorithm SHA256).Hash.ToLowerInvariant()
  if ($actual -ne $expected) { Die "reach-agent checksum mismatch: got $actual expected $expected" }

  Ensure-ReachOpenSSHServer

  Log "Installing reach-agent to $AgentPath"
  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  New-Item -ItemType Directory -Force -Path $ProgramDataDir | Out-Null
  Copy-Item -LiteralPath $candidate -Destination $AgentPath -Force

  $installArgs = @('install', '--mode', 'system', '--agent-path', $AgentPath, '--api-url', $ApiUrl, '--transport', $Transport, '--target-user', $TargetUser)
  if ($Name) { $installArgs += @('--name', $Name) }
  if ($Token) { $installArgs += @('--token', $Token) }
  if ($Yes) { $installArgs += '--yes' }

  Log "Handing off to reach-agent install"
  & $AgentPath @installArgs
  exit $LASTEXITCODE
} finally {
  Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
