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
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	programFiles := os.Getenv("ProgramFiles")
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
	if err := ensureWindowsOpenSSHServer(ctx); err != nil {
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

func ensureWindowsOpenSSHServer(ctx context.Context) error {
	ensureWindowsOpenSSHPath()
	if _, err := exec.LookPath("ssh-keygen.exe"); err != nil {
		if err := installWindowsCapability(ctx, "OpenSSH.Client~~~~0.0.1.0", "Windows OpenSSH Client"); err != nil {
			return err
		}
	}
	if _, err := exec.LookPath("sshd.exe"); err != nil {
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
	dir := filepath.Join(windir, "System32", "OpenSSH")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return
	}
	path := os.Getenv("PATH")
	for _, p := range filepath.SplitList(path) {
		if strings.EqualFold(p, dir) {
			return
		}
	}
	_ = os.Setenv("PATH", path+string(os.PathListSeparator)+dir)
}

func installWindowsCapability(ctx context.Context, capability, label string) error {
	script := `$Name = $args[0]; $cap = Get-WindowsCapability -Online -Name $Name -ErrorAction SilentlyContinue; if ($cap) { $cap.State }`
	state, _ := powershellOutput(ctx, script, capability)
	if strings.EqualFold(strings.TrimSpace(state), "Installed") {
		return nil
	}
	fmt.Printf("[reach] installing %s optional feature\n", label)
	if out, err := powershellOutput(ctx, `$Name = $args[0]; Add-WindowsCapability -Online -Name $Name | Out-String`, capability); err != nil {
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

func stopWindowsPersistence(ctx context.Context) {
	_, _ = powershellOutput(ctx, `
$task = Get-ScheduledTask -TaskPath '\Reach\' -TaskName 'reach-agent' -ErrorAction SilentlyContinue
if ($task) {
  Stop-ScheduledTask -TaskPath '\Reach\' -TaskName 'reach-agent' -ErrorAction SilentlyContinue
  Unregister-ScheduledTask -TaskPath '\Reach\' -TaskName 'reach-agent' -Confirm:$false
}
`)
}

func (d *Daemon) startWindowsLocalSSH(ctx context.Context) error {
	return ensureWindowsOpenSSHServer(ctx)
}

func applyAuthorizedKeysPermissions(authFile, user string, _ bool) error {
	abs, _ := filepath.Abs(authFile)
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	adminFile, _ := filepath.Abs(filepath.Join(programData, "ssh", "administrators_authorized_keys"))
	if strings.EqualFold(abs, adminFile) {
		cmd := exec.Command("icacls.exe", authFile, "/inheritance:r", "/grant", "*S-1-5-32-544:F", "/grant", "*S-1-5-18:F")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("set administrators_authorized_keys ACL failed: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	grantUser := user
	if grantUser == "" {
		grantUser = windowsCurrentUsername()
	}
	cmd := exec.Command("icacls.exe", authFile, "/inheritance:r", "/grant", grantUser+":F", "/grant", "*S-1-5-18:F", "/grant", "*S-1-5-32-544:F")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("set authorized_keys ACL failed: %w: %s", err, strings.TrimSpace(string(out)))
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
