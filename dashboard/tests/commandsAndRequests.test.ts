import { describe, expect, it } from 'vitest'
import type { RequestRow } from '../app/composables/useReachApi'
import { repairCommandFor, setupCommand, uninstallCommandFor, websocketSetupCommand } from '../app/utils/commands'
import { requestSlug, requestWarnings } from '../app/utils/requests'

const URL = 'https://tunnels.example.com'

describe('copied commands (A3)', () => {
  it('pipes setup.sh into bash, never sh (dash cannot run it)', () => {
    for (const cmd of [
      setupCommand(URL),
      websocketSetupCommand(URL),
      repairCommandFor(URL, { mode: 'system' }),
      uninstallCommandFor(URL, { mode: 'system' }),
      repairCommandFor(URL, { mode: 'user' }),
    ]) {
      expect(cmd).toMatch(/\| (sudo )?bash -s --/)
      expect(cmd).not.toMatch(/\|\s*(sudo\s+)?sh\b/)
    }
  })

  it('matches the README forms', () => {
    expect(repairCommandFor(URL, { mode: 'system' })).toBe(`curl -fsSL ${URL}/setup.sh | sudo bash -s -- --repair`)
    expect(uninstallCommandFor(URL, { mode: 'system' })).toBe(`curl -fsSL ${URL}/setup.sh | sudo bash -s -- --uninstall`)
    expect(websocketSetupCommand(URL)).toBe(`curl -fsSL ${URL}/setup.sh | bash -s -- --yes --transport websocket`)
  })

  it('does not run a user-mode repair through sudo', () => {
    expect(repairCommandFor(URL, { mode: 'user' })).toBe(`curl -fsSL ${URL}/setup.sh | bash -s -- --repair`)
  })

  it('uses PowerShell for Windows machines', () => {
    expect(repairCommandFor(URL + '/', { distro: 'windows 10' })).toBe(`iex "& { $(irm ${URL}/setup.ps1) } -Repair"`)
  })
})

function req(over: Partial<RequestRow>): RequestRow {
  return { id: 'r1', name: 'friend', status: 'pending', created_at: '', expires_at: '', ...over }
}

describe('request approval facts (P2-4 / P1-2)', () => {
  it('flags a forged source_ip on old servers as unverified', () => {
    const w = requestWarnings(req({ metadata: { source_ip: '125.229.144.147', target_user: 'kevin', slug: 'kevin' } }), new Set())
    expect(w).toContain('Source IP is self-reported; the server did not confirm it')
  })

  it('flags a mismatch between claimed and observed IP', () => {
    const w = requestWarnings(req({ observed_ip: '203.0.113.9', slug: 'kevin', target_user: 'kevin', metadata: { source_ip: '125.229.144.147' } }), new Set())
    expect(w).toContain('Claimed source IP differs from the one the server saw')
    expect(w).not.toContain('Source IP is self-reported; the server did not confirm it')
  })

  it('prefers the server-bound slug over the self-reported one', () => {
    expect(requestSlug(req({ slug: 'bound', metadata: { slug: 'claimed' } }))).toEqual({ value: 'bound', bound: true })
    expect(requestSlug(req({ metadata: { slug: 'claimed' } }))).toEqual({ value: 'claimed', bound: false })
  })

  it('warns about root, duplicates and unknown final names', () => {
    expect(requestWarnings(req({ observed_ip: '1.2.3.4', slug: 'jack', target_user: 'root' }), new Set(['jack']))).toEqual([
      'Logs in as root',
      'Name "jack" is already used by another machine',
    ])
    expect(requestWarnings(req({ observed_ip: '1.2.3.4', target_user: 'kevin' }), new Set())).toContain('Final ssh name is not known until setup runs')
  })
})
