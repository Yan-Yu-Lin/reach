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
  /** Client-only: server time (event_at) of the newest aggregate state applied from an event. */
  state_at?: string
  /**
   * Client-only: an event reported a probe result without its time (older reachd). The stored
   * observation is kept as a whole but cannot be trusted as current until the server confirms.
   */
  probe_unverified?: boolean
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
 * SHA-256 of a token (hex), so a sign-out broadcast can say WHICH session ended without
 * putting the token itself into the message (astra R3-06: the earlier 32-bit hash could
 * collide). Returns '' when WebCrypto is unavailable (non-secure context); receivers then
 * ignore targeted messages instead of guessing.
 */
export async function tokenFingerprint(t: string | null | undefined): Promise<string> {
  if (!t || typeof crypto === 'undefined' || !crypto.subtle) return ''
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(t))
  return [...new Uint8Array(digest)].map(b => b.toString(16).padStart(2, '0')).join('')
}

/**
 * all:     the user signed out on purpose (Sign out, Sign out everywhere): every tab leaves.
 * expired: the server rejected the token whose fingerprint is `fp`: only tabs still holding
 *          that same token leave, and only that token is removed from storage.
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

/**
 * Remove `t` from storage only where it is still the stored value (astra R3-01): the
 * localStorage slot is shared by every tab, and may hold ANOTHER tab's remembered login.
 */
export function forgetStoredToken(t: string | null | undefined) {
  if (!t || typeof window === 'undefined') return
  if (sessionStorage.getItem(TOKEN_KEY) === t) sessionStorage.removeItem(TOKEN_KEY)
  if (localStorage.getItem(TOKEN_KEY) === t) localStorage.removeItem(TOKEN_KEY)
}

function broadcast(msg: SignOutBroadcast) {
  try { localStorage.setItem(SIGNOUT_KEY, JSON.stringify(msg)) } catch { /* private mode */ }
}

export function useReachApi() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase || '/api'
  const token = useState<string | null>('reach-token', () => null)
  // "Not loaded yet" vs "signed out": once this tab has signed out it must not silently pick
  // up another tab's remembered login from storage (astra R3-01).
  const loadedFromStorage = useState<boolean>('reach-token-loaded', () => false)

  if (import.meta.client && !loadedFromStorage.value) {
    token.value = readStoredToken()
    loadedFromStorage.value = true
  }

  /**
   * Sign this tab in with `next` (or out, with null). remember=true keeps the token in
   * localStorage (survives closing the browser); remember=false keeps it in sessionStorage
   * only. The tab's previous token is removed from storage only where it is still the stored
   * value, so another tab's remembered login is not touched. Either way an XSS could read
   * it; see docs/review/2026-11-21-dashboard.md.
   */
  function setToken(next: string | null, remember = false) {
    const previous = token.value
    token.value = next
    loadedFromStorage.value = true
    if (!import.meta.client) return
    forgetStoredToken(previous)
    if (next && remember) {
      sessionStorage.removeItem(TOKEN_KEY) // must not shadow the remembered one in this tab
      localStorage.setItem(TOKEN_KEY, next)
    } else if (next) {
      sessionStorage.setItem(TOKEN_KEY, next)
    }
  }

  /**
   * End this tab's session and tell the other tabs of this browser.
   *
   * reason 'expired' (the server rejected `sentWith`): only if `sentWith` is still this tab's
   * token (astra R2-01) - a reply about a session the user already replaced changes nothing.
   * Only that token is removed from storage; tabs holding it follow, others stay.
   *
   * reason 'all' (the user pressed Sign out / Sign out everywhere): decided at the moment of
   * the tap, so it never depends on a callback that a 401 could have overtaken (astra
   * R3-02). Clears this browser's stored logins and every tab leaves. Pass sentWith to
   * guard against acting after a newer sign-in (late revoke-all reply).
   *
   * Returns whether anything was ended.
   */
  function signOutLocally(sentWith?: string | null, reason: SignOutBroadcast['reason'] = 'all'): boolean {
    if (sentWith !== undefined && token.value !== sentWith) return false
    const ended = token.value
    if (!ended && reason === 'expired') return false
    token.value = null
    loadedFromStorage.value = true
    if (!import.meta.client) return true
    if (reason === 'all') {
      sessionStorage.removeItem(TOKEN_KEY)
      localStorage.removeItem(TOKEN_KEY)
      broadcast({ at: Date.now(), reason: 'all', fp: '' })
    } else {
      forgetStoredToken(ended)
      // The fingerprint is async (WebCrypto). The decision above was already made
      // synchronously against this tab's token, so waiting here cannot reopen R2-01.
      void tokenFingerprint(ended).then(fp => { if (fp) broadcast({ at: Date.now(), reason: 'expired', fp }) })
    }
    return true
  }

  /**
   * Another tab broadcast a sign-out. 'all' ends this tab; 'expired' ends it only if it still
   * holds exactly that token (checked again after the async digest). Returns true if ended.
   */
  async function receiveSignOut(msg: SignOutBroadcast): Promise<boolean> {
    const mine = token.value
    if (!mine) return false
    if (msg.reason === 'expired') {
      if (!msg.fp || msg.fp !== await tokenFingerprint(mine)) return false
      if (token.value !== mine) return false // signed in again meanwhile
      forgetStoredToken(mine)
    } else {
      sessionStorage.removeItem(TOKEN_KEY)
    }
    token.value = null
    loadedFromStorage.value = true
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
    receiveSignOut,
    currentToken,
    login,
    // The caller signs out locally right away (R3-02); the server's answer changes nothing
    // here, so no automatic 401 handling either.
    logout: () => request<{ ok: boolean }>('/admin/logout', { method: 'POST', skipAuthRedirect: true }),
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
