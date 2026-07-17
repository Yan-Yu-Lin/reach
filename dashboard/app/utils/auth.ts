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
