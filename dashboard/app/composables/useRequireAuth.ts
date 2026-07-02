export function useRequireAuth() {
  const api = useReachApi()
  if (!import.meta.client) return
  if (!api.token.value) {
    return navigateTo('/login')
  }
  if (isJWTExpired(api.token.value)) {
    api.setToken(null)
    return navigateTo({ path: '/login', query: { reason: 'session-expired' } })
  }
}

function isJWTExpired(token: string): boolean {
  const expiresAt = decodeJWTExpiresAt(token)
  if (!expiresAt) return false
  return expiresAt <= Date.now()
}

function decodeJWTExpiresAt(token: string): number | null {
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
