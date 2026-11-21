// Commands the dashboard copies to the clipboard. setup.sh is a bash script
// (set -Eeuo pipefail, arrays), so it must be piped into bash, never sh: on Debian and
// Ubuntu sh is dash and dies on line 2.

type MachineLike = { mode?: string; distro?: string }

function isWindows(m: MachineLike) {
  return /windows/i.test(m.distro || '')
}

function base(url: string) {
  return url.replace(/\/+$/, '')
}

/** Fresh install. setup.sh finds sudo by itself and falls back to a user-mode install. */
export function setupCommand(url: string) {
  return `curl -fsSL ${base(url)}/setup.sh | bash -s -- --yes`
}

/** For networks that block SSH: tunnel over HTTPS (WebSocket) instead. Not about sudo. */
export function websocketSetupCommand(url: string) {
  return `${setupCommand(url)} --transport websocket`
}

export function windowsSetupCommand(url: string) {
  return `irm ${base(url)}/setup.ps1 | iex`
}

function shellAction(url: string, m: MachineLike, flag: '--repair' | '--uninstall', psFlag: '-Repair' | '-Uninstall') {
  if (isWindows(m)) return `iex "& { $(irm ${base(url)}/setup.ps1) } ${psFlag}"`
  // A user-mode agent lives in the user's home; running it through sudo would look in /root.
  const sudo = m.mode === 'user' ? '' : 'sudo '
  return `curl -fsSL ${base(url)}/setup.sh | ${sudo}bash -s -- ${flag}`
}

export function repairCommandFor(url: string, m: MachineLike) {
  return shellAction(url, m, '--repair', '-Repair')
}

export function uninstallCommandFor(url: string, m: MachineLike) {
  return shellAction(url, m, '--uninstall', '-Uninstall')
}
