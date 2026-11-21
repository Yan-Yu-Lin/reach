export function decodeJWTExpiresAt(token: string): number | null {
  const payload = token.split('.')[1]
  if (!payload) return null
  try {
    const normalized = payload.replace(/-/g, '+').replace(/_/g, '/')
    const padded = normalized.padEnd(normalized.length + (4 - normalized.length % 4) % 4, '=')
    const claims = JSON.parse(atob(padded))
    return typeof claims.exp === 'number' ? claims.exp * 1000 : null
  } catch {
    return null
  }
}

export function isJWTExpired(token: string, now = Date.now()): boolean {
  const expiresAt = decodeJWTExpiresAt(token)
  return expiresAt !== null && expiresAt <= now
}

/**
 * Where to go after login. Only same-origin app paths are allowed: a single leading slash,
 * no scheme, no protocol-relative `//host` or `/\host`, no control characters, not /login.
 * Anything else falls back to '/', so `?next=` cannot become an open redirect.
 */
export function safeReturnPath(raw: unknown): string {
  if (typeof raw !== 'string' || raw.length === 0 || raw.length > 512) return '/'
  if (!raw.startsWith('/')) return '/'
  if (raw.startsWith('//') || raw.startsWith('/\\')) return '/'
  if (/[\u0000-\u001f\u007f\\]/.test(raw)) return '/'
  // Resolve against a dummy origin: the result must stay on it.
  let url: URL
  try {
    url = new URL(raw, 'https://reach.invalid')
  } catch {
    return '/'
  }
  if (url.origin !== 'https://reach.invalid') return '/'
  // Normalisation can turn encoded dot-segments into a protocol-relative path
  // ('/%2e%2e//evil.example' -> '//evil.example'); check the result, not just the input
  // (astra R2-04).
  if (url.pathname.startsWith('//') || url.pathname.startsWith('/\\')) return '/'
  if (url.pathname === '/login' || url.pathname.startsWith('/login/')) return '/'
  return `${url.pathname}${url.search}${url.hash}`
}

/** Login route that brings the user back to `fullPath` afterwards. */
export function loginLocation(fullPath: string | undefined, reason?: 'session-expired') {
  const query: Record<string, string> = {}
  if (reason) query.reason = reason
  const next = safeReturnPath(fullPath)
  if (next !== '/') query.next = next
  return { path: '/login', query }
}

/** Current in-app location (client only). Safe to call after an await, unlike useRoute(). */
export function currentAppPath(): string {
  if (typeof window === 'undefined') return '/'
  return `${window.location.pathname}${window.location.search}${window.location.hash}`
}
