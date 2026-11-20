import { afterEach, describe, expect, it, vi } from 'vitest'

// The composable imports '~/utils/auth' (Nuxt alias, not resolved by plain vitest); the
// function under test does not use it.
vi.mock('~/utils/auth', () => ({ currentAppPath: () => '/', loginLocation: () => ({ path: '/login', query: {} }) }))
const { tokenFingerprint } = await import('../app/composables/useReachApi')

describe('tokenFingerprint (daybreak round 4 P3-03)', () => {
  afterEach(() => vi.restoreAllMocks())

  it('returns the SHA-256 hex digest', async () => {
    expect(await tokenFingerprint('abc')).toBe('ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad')
  })

  it('returns an empty string when crypto.subtle.digest rejects, instead of throwing', async () => {
    vi.spyOn(crypto.subtle, 'digest').mockRejectedValue(new Error('NotSupportedError'))
    await expect(tokenFingerprint('some-token')).resolves.toBe('')
  })

  it('returns an empty string for no token', async () => {
    expect(await tokenFingerprint(null)).toBe('')
    expect(await tokenFingerprint('')).toBe('')
  })
})
