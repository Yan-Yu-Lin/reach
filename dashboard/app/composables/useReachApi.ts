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
  return { path: '/login', query: { reason: 'session-expired' } }
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
export type MachineWithTunnels = { machine: Machine; tunnels: Tunnel[]; agent_observation?: AgentObservation; hub_observation?: HubObservation; commands?: AgentCommand[] }
export type RequestRow = {
  id: string
  name: string
  status: string
  created_at: string
  expires_at: string
  approved_at?: string
  denied_at?: string
  setup_token_expires_at?: string
  metadata?: Record<string, any>
}

export function useReachApi() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase || '/api'
  const token = useState<string | null>('reach-token', () => null)

  if (import.meta.client && token.value === null) {
    token.value = localStorage.getItem('reach-token')
  }

  function setToken(next: string | null) {
    token.value = next
    if (import.meta.client) {
      if (next) localStorage.setItem('reach-token', next)
      else localStorage.removeItem('reach-token')
    }
  }

  async function request<T>(path: string, opts: RequestInit = {}): Promise<T> {
    const headers = new Headers(opts.headers || {})
    if (!headers.has('Content-Type') && opts.body) headers.set('Content-Type', 'application/json')
    if (token.value) headers.set('Authorization', `Bearer ${token.value}`)
    const res = await fetch(`${apiBase}${path}`, { ...opts, headers })
    const text = await res.text()
    let body: any = text
    try { body = text ? JSON.parse(text) : null } catch {}
    if (!res.ok) {
      const msg = body && typeof body === 'object' && body.error ? body.error : text || res.statusText
      const err = new ReachApiError(msg, res.status, body)
      if (res.status === 401 && path !== '/admin/login') {
        setToken(null)
        if (import.meta.client) await navigateTo(sessionExpiredLocation())
      }
      throw err
    }
    return body as T
  }

  async function login(username: string, password: string) {
    const res = await request<LoginResponse>('/admin/login', {
      method: 'POST',
      body: JSON.stringify({ username, password })
    })
    setToken(res.token)
    return res
  }

  return {
    token,
    setToken,
    login,
    logout: () => request<{ ok: boolean }>('/admin/logout', { method: 'POST' }),
    machines: () => request<MachineWithTunnels[]>('/admin/machines'),
    machine: (id: string) => request<MachineWithTunnels>(`/admin/machines/${encodeURIComponent(id)}`),
    requests: () => request<RequestRow[]>('/admin/requests'),
    approve: (id: string) => request(`/admin/requests/${encodeURIComponent(id)}/approve`, { method: 'POST' }),
    deny: (id: string) => request(`/admin/requests/${encodeURIComponent(id)}/deny`, { method: 'POST' }),
    disable: (id: string) => request(`/admin/machines/${encodeURIComponent(id)}/disable`, { method: 'POST' }),
    enable: (id: string) => request(`/admin/machines/${encodeURIComponent(id)}/enable`, { method: 'POST' }),
    remove: (id: string) => request(`/admin/machines/${encodeURIComponent(id)}/remove`, { method: 'POST' }),
    setProcessTitle: (id: string, config: ProcessTitleConfig | null) => request<{ machine: Machine }>(`/admin/machines/${encodeURIComponent(id)}/process-title`, {
      method: 'POST',
      body: JSON.stringify({ process_title_config: config })
    }),
    health: () => request<any[]>('/admin/health'),
    sshConfig: () => request<string>('/admin/ssh-config')
  }
}
