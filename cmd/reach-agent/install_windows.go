//go:build windows

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func windowsIsElevated() bool {
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", `([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)`).Output()
	return err == nil && strings.EqualFold(strings.TrimSpace(string(out)), "True")
}

func applyWindowsModeDefaults(opt *installOptions) {
	if opt.InstallMode == "user" {
		// Per-user install: everything under %LOCALAPPDATA%\Reach, no admin needed.
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			if home := os.Getenv("USERPROFILE"); home != "" {
				base = filepath.Join(home, "AppData", "Local")
			}
		}
		root := filepath.Join(base, "Reach")
		if opt.ConfigDir == "" {
			opt.ConfigDir = root
		}
		if opt.DataDir == "" {
			opt.DataDir = filepath.Join(root, "data")
		}
		if opt.AgentPath == "" {
			opt.AgentPath = filepath.Join(root, "reach-agent.exe")
		}
		return
	}
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	programFiles := os.Getenv("ProgramW6432")
	if programFiles == "" {
		programFiles = os.Getenv("ProgramFiles")
	}
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	if opt.ConfigDir == "" {
		opt.ConfigDir = filepath.Join(programData, "Reach")
	}
	if opt.DataDir == "" {
		opt.DataDir = filepath.Join(programData, "Reach")
	}
	if opt.AgentPath == "" {
		opt.AgentPath = filepath.Join(programFiles, "Reach", "reach-agent.exe")
	}
}

func windowsCurrentUsername() string {
	if u := strings.TrimSpace(os.Getenv("USERNAME")); u != "" {
		return u
	}
	if out, err := exec.Command("whoami").Output(); err == nil {
		who := strings.TrimSpace(string(out))
		if i := strings.LastIndexAny(who, `\\/`); i >= 0 && i+1 < len(who) {
			return who[i+1:]
		}
		return who
	}
	return "windows"
}

func promptDefaultWindows(label, def string, secret bool) (string, error) {
	if def != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}
	if secret {
		fmt.Fprintln(os.Stderr, "(input is not hidden on Windows yet; prefer setup.ps1 -TokenFile)")
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		line = def
	}
	return line, nil
}

func windowsUserHome(user string) (string, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		return "", fmt.Errorf("empty Windows user")
	}
	current := windowsCurrentUsername()
	if sameWindowsUser(user, current) {
		if home := strings.TrimSpace(os.Getenv("USERPROFILE")); home != "" {
			return home, nil
		}
	}
	short := windowsShortUser(user)
	base := filepath.Join(os.Getenv("SystemDrive")+`\`, "Users")
	if os.Getenv("SystemDrive") == "" {
		base = `C:\Users`
	}
	for _, cand := range []string{filepath.Join(base, short), filepath.Join(base, user)} {
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			return cand, nil
		}
	}
	return "", fmt.Errorf("could not determine profile directory for Windows user %q; pass --target-user for the current logged-in user or create the profile first", user)
}

func sameWindowsUser(a, b string) bool {
	a = strings.ToLower(windowsShortUser(strings.TrimSpace(a)))
	b = strings.ToLower(windowsShortUser(strings.TrimSpace(b)))
	return a != "" && a == b
}

func windowsShortUser(user string) string {
	user = strings.TrimSpace(user)
	if i := strings.LastIndexAny(user, `\\/`); i >= 0 && i+1 < len(user) {
		return user[i+1:]
	}
	return user
}

func prepareWindowsLocalSSH(ctx context.Context, opt installOptions, targetHome string, compat SSHCompatConfig) (localSSHPlan, error) {
	if opt.InstallMode != "system" {
		return localSSHPlan{}, fmt.Errorf("Windows target support currently requires --mode system")
	}
	if err := ensureWindowsOpenSSHServer(ctx, opt.InstallOpenSSH); err != nil {
		return localSSHPlan{}, err
	}
	if err := probeSSH(ctx, "127.0.0.1", 22, 8*time.Second); err != nil {
		return localSSHPlan{}, fmt.Errorf("Windows OpenSSH Server did not answer on 127.0.0.1:22: %w", err)
	}
	banner := sshServerBanner(ctx, "127.0.0.1", 22, 2*time.Second)
	authFile, isAdmin := windowsAuthorizedKeysPath(opt.TargetUser, targetHome)
	mode := "windows-openssh"
	if isAdmin {
		mode = "windows-openssh-admin"
	}
	return localSSHPlan{Mode: mode, LocalPort: 22, AuthFile: authFile, ClientOptions: clientOptionsForServerBanner(banner, "")}, nil
}

// ensureWindowsOpenSSHServer starts an installed Windows OpenSSH Server.
// It installs the capability only when allowInstall is set: the Windows
// sshd listens on every interface, which Reach does not need (daybreak
// P1-5). Without it, a system install on a machine with no OpenSSH Server
// stops with instructions instead of silently opening port 22.
func ensureWindowsOpenSSHServer(ctx context.Context, allowInstall bool) error {
	ensureWindowsOpenSSHPath()
	if _, err := exec.LookPath("ssh-keygen.exe"); err != nil {
		if err := installWindowsCapability(ctx, "OpenSSH.Client~~~~0.0.1.0", "Windows OpenSSH Client"); err != nil {
			return err
		}
	}
	if _, err := exec.LookPath("sshd.exe"); err != nil {
		if !allowInstall {
			return fmt.Errorf("Windows OpenSSH Server is not installed. Installing it opens port 22 on all interfaces; rerun with --install-openssh (setup.ps1 -InstallOpenSSH) if you accept that and firewall port 22, or install without admin rights to use Reach's loopback-only built-in server")
		}
		fmt.Printf("[reach warning] installing Windows OpenSSH Server; it listens on all interfaces. Restrict port 22 in Windows Firewall if this PC should not accept SSH from the network.\n")
		if err := installWindowsCapability(ctx, "OpenSSH.Server~~~~0.0.1.0", "Windows OpenSSH Server"); err != nil {
			return err
		}
	}
	ensureWindowsOpenSSHPath()
	if out, err := powershellOutput(ctx, `
$svc = Get-Service -Name sshd -ErrorAction SilentlyContinue
if (-not $svc) { throw 'sshd service is not installed after OpenSSH setup' }
Set-Service -Name sshd -StartupType Automatic
if ($svc.Status -ne 'Running') { Start-Service -Name sshd }
(Get-Service -Name sshd).Status
`); err != nil {
		return fmt.Errorf("start/configure Windows sshd service failed: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

func ensureWindowsOpenSSHPath() {
	windir := os.Getenv("WINDIR")
	if windir == "" {
		windir = `C:\Windows`
	}
	candidates := []string{
		filepath.Join(windir, "System32", "OpenSSH"),
		filepath.Join(windir, "Sysnative", "OpenSSH"),
	}
	path := os.Getenv("PATH")
	for _, dir := range candidates {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		found := false
		for _, p := range filepath.SplitList(path) {
			if strings.EqualFold(p, dir) {
				found = true
				break
			}
		}
		if !found {
			path += string(os.PathListSeparator) + dir
		}
	}
	_ = os.Setenv("PATH", path)
}

func installWindowsCapability(ctx context.Context, capability, label string) error {
	state, _ := powershellOutput(ctx, `
param([string]$Name)
$cap = Get-WindowsCapability -Online -Name $Name -ErrorAction SilentlyContinue
if ($cap) { $cap.State }
`, capability)
	if strings.EqualFold(strings.TrimSpace(state), "Installed") {
		return nil
	}
	fmt.Printf("[reach] installing %s optional feature\n", label)
	if out, err := powershellOutput(ctx, `
param([string]$Name)
Add-WindowsCapability -Online -Name $Name | Out-String
`, capability); err != nil {
		return fmt.Errorf("install %s optional feature failed: %w: %s", label, err, strings.TrimSpace(out))
	}
	return nil
}

func windowsAuthorizedKeysPath(user, targetHome string) (string, bool) {
	if windowsTargetUserIsAdministrator(user) {
		programData := os.Getenv("ProgramData")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return filepath.Join(programData, "ssh", "administrators_authorized_keys"), true
	}
	return filepath.Join(targetHome, ".ssh", "authorized_keys"), false
}

func windowsTargetUserIsAdministrator(user string) bool {
	short := strings.ToLower(windowsShortUser(user))
	if short == "administrator" {
		return true
	}
	if sameWindowsUser(user, windowsCurrentUsername()) && windowsIsElevated() {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := powershellOutput(ctx, `
param([string]$User)
try {
  $acct = New-Object Security.Principal.NTAccount($User)
  $sid = $acct.Translate([Security.Principal.SecurityIdentifier]).Value
  $members = Get-LocalGroupMember -SID 'S-1-5-32-544' -ErrorAction Stop | ForEach-Object { $_.SID.Value }
  if ($members -contains $sid) { 'True' } else { 'False' }
} catch { 'False' }
`, user)
	return err == nil && strings.EqualFold(strings.TrimSpace(out), "True")
}

func installWindowsScheduledTask(ctx context.Context, opt installOptions, cfgPath string) error {
	if opt.InstallMode != "system" {
		return fmt.Errorf("Windows persistence currently supports system mode only")
	}
	script := `
param([string]$Exe, [string]$Cfg)
if ([string]::IsNullOrWhiteSpace($Exe)) { throw 'missing reach-agent executable path' }
if ([string]::IsNullOrWhiteSpace($Cfg)) { throw 'missing reach-agent config path' }
$taskPath = '\Reach\'
$taskName = 'reach-agent'
$taskArgs = 'run --config "' + ($Cfg -replace '"', '\"') + '"'
$action = New-ScheduledTaskAction -Execute $Exe -Argument $taskArgs
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit (New-TimeSpan -Seconds 0) -MultipleInstances IgnoreNew -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
Register-ScheduledTask -TaskPath $taskPath -TaskName $taskName -Action $action -Trigger $trigger -Settings $settings -Principal $principal -Description 'Reach target agent reverse SSH tunnel' -Force | Out-Null
Start-ScheduledTask -TaskPath $taskPath -TaskName $taskName
`
	if out, err := powershellOutput(ctx, script, opt.AgentPath, cfgPath); err != nil {
		return fmt.Errorf("install Windows scheduled task failed: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

func stopWindowsPersistence(ctx context.Context, opt installOptions) {
	if opt.InstallMode == "user" {
		_, _ = powershellOutput(ctx, `
$task = Get-ScheduledTask -TaskName 'reach-agent-user' -ErrorAction SilentlyContinue
if ($task) {
  Stop-ScheduledTask -TaskName 'reach-agent-user' -ErrorAction SilentlyContinue
  Unregister-ScheduledTask -TaskName 'reach-agent-user' -Confirm:$false
}
`)
		removePID(filepath.Join(opt.DataDir, "reach-agent.pid"))
		return
	}
	_, _ = powershellOutput(ctx, `
$task = Get-ScheduledTask -TaskPath '\Reach\' -TaskName 'reach-agent' -ErrorAction SilentlyContinue
if ($task) {
  Stop-ScheduledTask -TaskPath '\Reach\' -TaskName 'reach-agent' -ErrorAction SilentlyContinue
  Unregister-ScheduledTask -TaskPath '\Reach\' -TaskName 'reach-agent' -Confirm:$false
}
`)
}

// installWindowsUserPersistence registers a logon task for the current user
// (no SYSTEM, no elevation) and starts the agent now as a hidden detached
// process. A locked-down machine may refuse the task; the agent still runs
// until logoff.
func installWindowsUserPersistence(ctx context.Context, opt installOptions, cfgPath string) (string, error) {
	backend := "detached process for this login only"
	script := `
param([string]$Exe, [string]$Cfg)
$taskArgs = 'run --config "' + $Cfg + '"'
$action = New-ScheduledTaskAction -Execute $Exe -Argument $taskArgs
$trigger = New-ScheduledTaskTrigger -AtLogOn -User ([Security.Principal.WindowsIdentity]::GetCurrent().Name)
$settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -ExecutionTimeLimit (New-TimeSpan -Seconds 0) -MultipleInstances IgnoreNew -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
$principal = New-ScheduledTaskPrincipal -UserId ([Security.Principal.WindowsIdentity]::GetCurrent().Name) -LogonType Interactive -RunLevel Limited
Register-ScheduledTask -TaskName 'reach-agent-user' -Action $action -Trigger $trigger -Settings $settings -Principal $principal -Description 'Reach target agent (per-user)' -Force | Out-Null
`
	if out, err := powershellOutput(ctx, script, opt.AgentPath, cfgPath); err != nil {
		fmt.Printf("[reach warning] per-user logon task unavailable (%v): %s\n", err, strings.TrimSpace(out))
	} else {
		backend = "Windows Scheduled Task reach-agent-user (at logon) + detached process now"
	}
	if err := startDetached(opt.AgentPath, cfgPath, filepath.Join(opt.DataDir, "agent.log"), filepath.Join(opt.DataDir, "reach-agent.pid")); err != nil {
		return "", err
	}
	return backend, nil
}

func (d *Daemon) startWindowsLocalSSH(ctx context.Context) error {
	// Restart only; never install from the background daemon.
	return ensureWindowsOpenSSHServer(ctx, false)
}

// applyAuthorizedKeysPermissions is a no-op now: writeAuthorizedKeysFile
// sets the DACL on the replacement file before it becomes visible.
func applyAuthorizedKeysPermissions(authFile, user string, systemInstall bool) error {
	return nil
}

// setAuthorizedKeysACL sets the DACL that applyAuthorizedKeysPermissions
// used to apply after the fact, on path (the temp file), choosing the
// principals from target (the final authorized_keys path).
func setAuthorizedKeysACL(path, target, user string) error {
	args := authorizedKeysICACLSArgs(path, target, user, os.Getenv("ProgramData"), windowsCurrentUsername())
	if out, err := exec.Command("icacls.exe", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("icacls: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func windowsDistroString() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := powershellOutput(ctx, `(Get-CimInstance Win32_OperatingSystem | ForEach-Object { $_.Caption + ' ' + $_.Version })`)
	if err == nil && strings.TrimSpace(out) != "" {
		return strings.TrimSpace(out)
	}
	return "windows"
}

func powershellOutput(ctx context.Context, script string, args ...string) (string, error) {
	f, err := os.CreateTemp("", "reach-powershell-*.ps1")
	if err != nil {
		return "", err
	}
	scriptPath := f.Name()
	defer func() { _ = os.Remove(scriptPath) }()
	if _, err := f.WriteString(script); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	cmdArgs := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, "powershell.exe", cmdArgs...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
