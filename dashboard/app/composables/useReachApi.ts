import { currentAppPath, loginLocation } from '~/utils/auth'
type LoginResponse = { token: string; expires_in: number }

export class ReachApiError extends Error {
  status: number
  body: unknown

  constructor(message: string, status: number, body: unknown) {
    super(message)
    this.name = 'ReachApiError'
    this.status = status
    this.body = body
  }
}

function sessionExpiredLocation() {
  return loginLocation(currentAppPath(), 'session-expired')
}

/** Go to the login page unless we are already there (keeps the first saved `next`). */
export async function goToLoginOnce() {
  if (typeof window !== 'undefined' && window.location.pathname === '/login') return
  await navigateTo(sessionExpiredLocation())
}

export type ProcessTitleConfig = {
  process_title?: string
  process_titles?: string[]
  rotate_interval?: string
}

export type Machine = {
  id: string
  slug: string
  original_slug?: string
  display_name?: string
  target_user?: string
  status: string
  desired_state: string
  observed_state: string
  desired_generation: number
  update_policy: 'manual' | 'auto' | 'disabled'
  desired_changed_at?: string
  cleanup_state?: string
  mode?: string
  local_port: number
  persistence?: string
  distro?: string
  arch?: string
  provision_error?: string
  created_at: string
  updated_at?: string
  expires_at?: string
  disabled_at?: string
  retired_at?: string
  process_title_config?: ProcessTitleConfig
}

export type Tunnel = {
  id: string
  machine_id: string
  hub_id: string
  unix_user: string
  original_unix_user?: string
  remote_port: number
  original_remote_port?: number
  tunnel_pubkey?: string
  status: string
  last_seen_at?: string
  created_at: string
  expires_at?: string
}

export type AgentObservation = {
  agent_version?: string
  applied_generation: number
  heartbeat_at: string
  transport?: string
  transport_state?: string
  local_ssh_state?: string
  local_ssh_error?: string
  tunnel_state?: string
  tunnel_pid?: number
  tunnel_error?: string
  persistence_backend?: string
  persistence_quality?: string
  persistence_reboot_safe?: boolean
  last_error?: string
}
export type HubObservation = { probe_state: string; last_probe_at: string; last_success_at?: string; probe_error?: string }
export type AgentCommand = { id: string; type: string; status: string; generation: number; created_at: string; last_error?: string }
export type AgentUpdateHint = { available: boolean; current?: string; latest?: string; url?: string; sha256?: string; required?: boolean }
export type MachineWithTunnels = {
  machine: Machine
  tunnels: Tunnel[]
  agent_observation?: AgentObservation
  hub_observation?: HubObservation
  commands?: AgentCommand[]
  update?: AgentUpdateHint
  /**
   * Client-only: time of the newest live heartbeat event. Heartbeats only prove the agent is
   * alive; they do not carry the rest of agent_observation, whose fields stay as fresh as
   * agent_observation.heartbeat_at.
   */
  live_heartbeat_at?: string
}
export type RequestRow = {
  id: string
  name: string
  status: string
  created_at: string
  expires_at: string
  approved_at?: string
  denied_at?: string
  setup_token_expires_at?: string
  /** IP the server itself saw (newer servers). Trustworthy, unlike metadata.source_ip. */
  observed_ip?: string | null
  /** Fields the server binds at approval time (newer servers). Provision must match them. */
  slug?: string
  target_user?: string
  mode?: string
  local_port?: number
  /** Everything in here was sent by the requester and is NOT verified. */
  metadata?: Record<string, any>
}

const TOKEN_KEY = 'reach-token'
/** localStorage key whose change tells other tabs "signed out here, sign out too". */
export const SIGNOUT_KEY = 'reach-signout'

/**
 * Short non-secret fingerprint of a token (FNV-1a), so a sign-out broadcast can say WHICH
 * session ended without putting the token itself into the message.
 */
export function tokenFingerprint(t: string | null | undefined): string {
  if (!t) return ''
  let h = 0x811c9dc5
  for (let i = 0; i < t.length; i++) {
    h ^= t.charCodeAt(i)
    h = Math.imul(h, 0x01000193)
  }
  return (h >>> 0).toString(36)
}

/**
 * all:     the user signed out on purpose (Sign out, Sign out everywhere): every tab leaves.
 * expired: the server rejected token `fp`: only tabs still holding that same token leave.
 */
export type SignOutBroadcast = { at: number; reason: 'all' | 'expired'; fp: string }

export function parseSignOutBroadcast(raw: string | null): SignOutBroadcast | null {
  if (!raw) return null
  try {
    const v = JSON.parse(raw)
    if (v && (v.reason === 'all' || v.reason === 'expired') && typeof v.fp === 'string') return v
  } catch { /* older format: a bare timestamp */ }
  return { at: Number(raw) || 0, reason: 'all', fp: '' }
}

function readStoredToken(): string | null {
  // Session storage first: a "don't remember" login must win over an old remembered one.
  return sessionStorage.getItem(TOKEN_KEY) || localStorage.getItem(TOKEN_KEY)
}

export function useReachApi() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase || '/api'
  const token = useState<string | null>('reach-token', () => null)

  if (import.meta.client && token.value === null) {
    token.value = readStoredToken()
  }

  /**
   * remember=true keeps the token in localStorage (survives closing the browser, what a
   * phone needs). remember=false keeps it in sessionStorage only, so it is gone when the
   * tab closes. Either way an XSS could read it; see docs/review/2026-11-21-dashboard.md.
   */
  function setToken(next: string | null, remember = false) {
    token.value = next
    if (!import.meta.client) return
    localStorage.removeItem(TOKEN_KEY)
    sessionStorage.removeItem(TOKEN_KEY)
    if (next) (remember ? localStorage : sessionStorage).setItem(TOKEN_KEY, next)
  }

  /**
   * Forget the session here and tell the other tabs of this browser.
   *
   * `sentWith` is the token the failed/finished operation used. If the user has signed in
   * again since then (token changed), the result is about an old session: nothing is
   * cleared and false is returned (astra R2-01). Pass `undefined` only for an explicit
   * sign-out of whatever session is current.
   */
  function signOutLocally(sentWith?: string | null, reason: SignOutBroadcast['reason'] = 'all'): boolean {
    if (sentWith !== undefined && token.value !== sentWith) return false
    const fp = tokenFingerprint(token.value)
    setToken(null)
    if (import.meta.client) {
      const msg: SignOutBroadcast = { at: Date.now(), reason, fp }
      try { localStorage.setItem(SIGNOUT_KEY, JSON.stringify(msg)) } catch { /* private mode */ }
    }
    return true
  }

  /** The token currently in use (to remember what an operation was sent with). */
  function currentToken() {
    return token.value
  }

  async function request<T>(path: string, opts: RequestInit & { skipAuthRedirect?: boolean } = {}): Promise<T> {
    const headers = new Headers(opts.headers || {})
    if (!headers.has('Content-Type') && opts.body) headers.set('Content-Type', 'application/json')
    // Remember which session this request belongs to; a 401 only ends THAT session.
    const sentWith = token.value
    if (sentWith) headers.set('Authorization', `Bearer ${sentWith}`)

    const timeoutController = new AbortController()
    const timeout = setTimeout(() => timeoutController.abort(), 15_000)
    const requestController = new AbortController()
    const abortRequest = () => requestController.abort()
    timeoutController.signal.addEventListener('abort', abortRequest, { once: true })
    opts.signal?.addEventListener('abort', abortRequest, { once: true })
    if (opts.signal?.aborted) requestController.abort()

    let res: Response
    let text: string
    try {
      const { skipAuthRedirect: _skip, ...fetchOpts } = opts
      res = await fetch(`${apiBase}${path}`, { ...fetchOpts, headers, signal: requestController.signal })
      text = await res.text()
    } catch (error) {
      if (timeoutController.signal.aborted && !opts.signal?.aborted) {
        throw new ReachApiError('Request timed out. Please try again.', 0, null)
      }
      throw error
    } finally {
      clearTimeout(timeout)
      timeoutController.signal.removeEventListener('abort', abortRequest)
      opts.signal?.removeEventListener('abort', abortRequest)
    }

    let body: any = text
    try { body = text ? JSON.parse(text) : null } catch {}
    if (!res.ok) {
      const msg = body && typeof body === 'object' && body.error ? body.error : text || res.statusText
      const err = new ReachApiError(msg, res.status, body)
      if (res.status === 401 && path !== '/admin/login' && !opts.skipAuthRedirect) {
        // A late 401 for a session that has already been replaced (signed in again) or
        // already ended must not sign out the new one or rewrite the saved return path.
        if (signOutLocally(sentWith, 'expired') && import.meta.client) await goToLoginOnce()
      }
      throw err
    }
    return body as T
  }

  async function login(username: string, password: string, remember = false) {
    const res = await request<LoginResponse>('/admin/login', {
      method: 'POST',
      body: JSON.stringify({ username, password })
    })
    setToken(res.token, remember)
    return res
  }

  return {
    token,
    setToken,
    signOutLocally,
    currentToken,
    login,
    logout: () => request<{ ok: boolean }>('/admin/logout', { method: 'POST' }),
    /** Invalidates every dashboard login (and optionally Mac service tokens). Newer servers only. */
    // A 401 here means the handler never ran; the caller handles it (no silent redirect).
    revokeAllSessions: (includeServiceTokens = false) => request<{ ok: boolean }>('/admin/sessions/revoke-all', {
      skipAuthRedirect: true,
      method: 'POST',
      body: JSON.stringify({ include_service_tokens: includeServiceTokens })
    }),
    machines: () => request<MachineWithTunnels[]>('/admin/machines'),
    machine: (id: string) => request<MachineWithTunnels>(`/admin/machines/${encodeURIComponent(id)}`),
    /** Pending only. Older servers ignore the filter, so callers still filter by status. */
    pendingRequests: () => request<RequestRow[] | null>('/admin/requests?status=pending'),
    requests: () => request<RequestRow[]>('/admin/requests'),
    approve: (id: string) => request(`/admin/requests/${encodeURIComponent(id)}/approve`, { method: 'POST' }),
    deny: (id: string) => request(`/admin/requests/${encodeURIComponent(id)}/deny`, { method: 'POST' }),
    disable: (id: string) => request(`/admin/machines/${encodeURIComponent(id)}/disable`, { method: 'POST' }),
    enable: (id: string) => request(`/admin/machines/${encodeURIComponent(id)}/enable`, { method: 'POST' }),
    remove: (id: string) => request(`/admin/machines/${encodeURIComponent(id)}/remove`, { method: 'POST' }),
    /** Finish a stuck or failed removal on the hub without waiting for the agent. */
    retireNow: (id: string) => request(`/admin/machines/${encodeURIComponent(id)}/retire-now`, { method: 'POST' }),
    setProcessTitle: (id: string, config: ProcessTitleConfig | null) => request<{ machine: Machine }>(`/admin/machines/${encodeURIComponent(id)}/process-title`, {
      method: 'POST',
      body: JSON.stringify({ process_title_config: config })
    }),
    setUpdatePolicy: (id: string, policy: 'manual' | 'auto' | 'disabled') => request<{ machine: Machine }>(`/admin/machines/${encodeURIComponent(id)}/update-policy`, {
      method: 'POST',
      body: JSON.stringify({ update_policy: policy })
    }),
    updateAgent: (id: string) => request<{ ok: boolean }>(`/admin/machines/${encodeURIComponent(id)}/update-agent`, { method: 'POST' }),
    health: () => request<any[]>('/admin/health'),
    sshConfig: () => request<string>('/admin/ssh-config')
  }
}
