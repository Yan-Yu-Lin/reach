import { describe, expect, it } from 'vitest'
import { decodeJWTExpiresAt, isJWTExpired } from '../app/utils/auth'

function token(payload: object) {
  const encoded = btoa(JSON.stringify(payload)).replace(/=/g, '').replace(/\+/g, '-').replace(/\//g, '_')
  return `header.${encoded}.signature`
}

describe('JWT expiry helpers', () => {
  it('decodes expiry and detects expiration', () => {
    const value = token({ exp: 123 })
    expect(decodeJWTExpiresAt(value)).toBe(123_000)
    expect(isJWTExpired(value, 123_000)).toBe(true)
    expect(isJWTExpired(value, 122_999)).toBe(false)
  })

  it('treats opaque or malformed tokens as unexpired', () => {
    expect(decodeJWTExpiresAt('service-token')).toBeNull()
    expect(isJWTExpired('service-token')).toBe(false)
  })
})
