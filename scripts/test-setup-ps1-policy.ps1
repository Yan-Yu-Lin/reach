# Tests for setup.ps1's sshd policy preflight (astra R3-09). Runs on any
# PowerShell 7 (bash scripts/run-setup-ps1-tests.sh uses the official
# container); Windows cmdlets are mocked. Not a substitute for a Windows VM.
$ErrorActionPreference = 'Stop'
function Say([string]$m) { Write-Host $m }
$path = Join-Path (Join-Path $PSScriptRoot '..') 'setup.ps1'
$tokens = $null; $errors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($path, [ref]$tokens, [ref]$errors)
if ($errors.Count -gt 0) { throw "setup.ps1 parse errors: $($errors | ForEach-Object Message)" }

# 1. On -Reinstall the preflight runs before anything is removed.
$src = [IO.File]::ReadAllText($path)
$pre = $src.IndexOf('$policyError = Test-ReachOpenSSHPolicy')
$uninstall = $src.IndexOf('Log "Uninstalling existing Reach Windows agent"')
if ($pre -lt 0 -or $uninstall -lt 0 -or $pre -gt $uninstall) { throw "policy preflight does not precede the reinstall uninstall" }
Say "ok: preflight precedes the reinstall uninstall"

# 2. The preflight function itself, against mocked cmdlets.
$fn = $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'Test-ReachOpenSSHPolicy' }, $true) | Select-Object -First 1
if (-not $fn) { throw "Test-ReachOpenSSHPolicy not found" }
. ([scriptblock]::Create($fn.Extent.Text))

$script:svc = $null
$script:mode = 'Auto'
function Get-Service { $script:svc }
function Get-CimInstance { [pscustomobject]@{ StartMode = $script:mode } }
function Set-Service { throw "Set-Service must not be called by the preflight" }

$svcObj = [pscustomobject]@{ Name = 'sshd' }
$cases = @(
  @($null, 'Auto', $false, $true),
  @($null, 'Auto', $true, $false),
  @($svcObj, 'Auto', $false, $false),
  @($svcObj, 'Manual', $false, $true),
  @($svcObj, 'Disabled', $false, $true),
  @($svcObj, 'Disabled', $true, $false)
)
foreach ($c in $cases) {
  $script:svc = $c[0]
  $script:mode = $c[1]
  $InstallOpenSSH = $c[2]
  $msg = Test-ReachOpenSSHPolicy
  $refused = -not [string]::IsNullOrEmpty($msg)
  if ($refused -ne $c[3]) { throw "service=$($null -ne $c[0]) mode=$($c[1]) opt=$($c[2]): refused=$refused" }
}
Say "ok: preflight refuses Manual/Disabled/missing sshd without -InstallOpenSSH and changes nothing"
Say "PASS: setup.ps1 policy preflight"
