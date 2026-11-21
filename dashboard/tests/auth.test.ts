import { describe, expect, it } from 'vitest'
import { decodeJWTExpiresAt, isJWTExpired, safeReturnPath } from '../app/utils/auth'

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

describe('safeReturnPath', () => {
  it('keeps same-origin app paths with query and hash', () => {
    expect(safeReturnPath('/?tab=requests#probe')).toBe('/?tab=requests#probe')
    expect(safeReturnPath('/?m=m_abc')).toBe('/?m=m_abc')
  })

  it('rejects other sites, schemes and the login page', () => {
    for (const evil of ['//evil.example/x', 'https://evil.example', '/\\evil.example', 'javascript:alert(1)', '/login?next=/', 'evil', '', undefined]) {
      expect(safeReturnPath(evil)).toBe('/')
    }
  })

  it('rejects paths that only become protocol-relative after normalisation (astra R2-04)', () => {
    for (const evil of ['/%2e%2e//evil.example/x', '/a/%2e%2e//evil.example/x', '/.%2e//evil.example/x']) {
      expect(safeReturnPath(evil)).toBe('/')
    }
  })
})
