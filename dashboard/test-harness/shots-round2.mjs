// Screenshots of the screens changed by the astra/daybreak round (2026-11-21, round 2).
//   SITE=<generated .output/public> SHOTS=<dir> CHROME=<headless shell> node shots-round2.mjs
// Starts its own mock (mock.mjs) on PORT (default 9392).
import { spawn } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const { chromium } = await import(process.env.PLAYWRIGHT || '/Users/linyanyu/20-29-Development/21-Active-Projects/instagram-chat-export/node_modules/playwright-core/index.mjs')
const HERE = path.dirname(fileURLToPath(import.meta.url))
const PORT = Number(process.env.PORT || 9392)
const BASE = `http://127.0.0.1:${PORT}`
const SHOTS = process.env.SHOTS
const sleep = (ms) => new Promise(r => setTimeout(r, ms))
const b64 = (o) => Buffer.from(JSON.stringify(o)).toString('base64url')
const FAKE = `${b64({ alg: 'none' })}.${b64({ sub: 'shots', sv: 0, exp: 4102444800 })}.x`

const mock = spawn(process.execPath, [path.join(HERE, 'mock.mjs')], { env: { ...process.env, PORT: String(PORT) }, stdio: ['ignore', 'pipe', 'inherit'] })
await new Promise(r => mock.stdout.on('data', d => { if (String(d).includes('mock reachd')) r() }))
const ctl = async (q, body) => (await fetch(BASE + q, body ? { method: 'POST', body: JSON.stringify(body) } : undefined)).json()
const reset = (q = '') => ctl(`/__resetall?scenario=arthur&hbfmt=new&v2=1&pending=0&latency=20${q ? `&${q}` : ''}`)

const browser = await chromium.launch({ executablePath: process.env.CHROME })
const PHONE = { width: 393, height: 852 }
async function open(viewport, { token = FAKE, p = '/' } = {}) {
  const mobile = viewport.width < 600
  const ctx = await browser.newContext({ viewport, deviceScaleFactor: 2, isMobile: mobile, hasTouch: mobile, colorScheme: 'dark' })
  if (token) await ctx.addInitScript((t) => localStorage.setItem('reach-token', t), token)
  const page = await ctx.newPage()
  await page.goto(BASE + p)
  return { ctx, page }
}
const shot = (page, name, fullPage = false) => page.screenshot({ path: `${SHOTS}/${name}.png`, fullPage })

try {
  // 40: login with "keep me signed in" off by default (P3-7)
  await reset('auth=1')
  { const { ctx, page } = await open(PHONE, { token: null, p: '/login' }); await page.locator('form').waitFor(); await shot(page, '40-phone-login-session-default'); await ctx.close() }

  // 41: revoke-all that hit 401 (P3-5): login page explains nothing was revoked
  { const { ctx, page } = await open(PHONE, { token: null, p: '/login?then=revoke-all' }); await page.locator('form').waitFor(); await shot(page, '41-phone-login-revoke-401'); await ctx.close() }

  // 42-43: Disable failed (F09): banner and detail
  await reset()
  const omarchy = (await ctl('/__mutate', { slug: 'omarchy', machine: { desired_state: 'disabled', status: 'disable_failed', cleanup_state: 'disable_failed' } })).machine.id
  { const { ctx, page } = await open(PHONE); await page.locator('.todo').waitFor(); await sleep(400); await shot(page, '42-phone-home-disable-failed')
    await page.goto(`${BASE}/?m=${omarchy}`); await page.locator('.danger-zone').waitFor(); await sleep(400); await shot(page, '43-phone-detail-disable-failed-full', true); await ctx.close() }

  // 44: disabled machine: Connection check says Not checked (F08)
  await reset()
  const vpn = (await ctl('/__mutate', { slug: 'ntut-vpn-help', machine: { desired_state: 'disabled', status: 'disabled' }, hub: { probe_state: 'reachable', last_probe_at: new Date(Date.now() - 86400_000).toISOString() } })).machine.id
  { const { ctx, page } = await open(PHONE, { p: `/?m=${vpn}` }); await page.locator('.checks').waitFor(); await sleep(400); await page.locator('.checks').scrollIntoViewIfNeeded(); await shot(page, '44-phone-detail-disabled-not-checked'); await ctx.close() }

  // 45: active machine, probe a day old: Unknown with the age (F08)
  await reset()
  const om = (await ctl('/__mutate', { slug: 'omarchy', hub: { probe_state: 'reachable', last_probe_at: new Date(Date.now() - 86400_000).toISOString() } })).machine.id
  { const { ctx, page } = await open(PHONE, { p: `/?m=${om}` }); await page.locator('.checks').waitFor(); await sleep(400); await page.locator('.checks').scrollIntoViewIfNeeded(); await shot(page, '45-phone-detail-stale-probe'); await ctx.close() }

  // 46: Disable all dialog after one machine came back (F07)
  await reset()
  { const { ctx, page } = await open(PHONE); await page.locator('.live-live').waitFor()
    await page.locator('.main .fgroup-dormant .fgroup-toggle').click(); await page.locator('.main .fgroup-bulk button').click()
    await ctl('/__mutate', { slug: 'ntut-vdi-win', machine: { observed_state: 'online', status: 'online' } }); await ctl('/__hb?slug=ntut-vdi-win&observed=online')
    await page.locator('.bulk-dropped').waitFor(); await sleep(300); await shot(page, '46-phone-bulk-disable-recheck'); await ctx.close() }
} finally {
  await browser.close()
  mock.kill()
}
console.log('done')
