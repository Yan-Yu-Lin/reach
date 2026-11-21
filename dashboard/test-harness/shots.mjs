// Screenshots of the redesigned dashboard against mock.mjs (SCENARIO=arthur, HBFMT=new, V2=1).
//   SHOTS=<dir> CHROME=<chrome-headless-shell> node shots.mjs
// Also prints layout facts (touch target sizes, horizontal overflow) for the report.
import fs from 'node:fs'

const { chromium } = await import(process.env.PLAYWRIGHT || '/Users/linyanyu/20-29-Development/21-Active-Projects/instagram-chat-export/node_modules/playwright-core/index.mjs')
const BASE = process.env.BASE || 'http://127.0.0.1:9388'
const SHOTS = process.env.SHOTS || '/tmp/reach-dash-v2/shots'
const ONLY = process.env.ONLY || ''
fs.mkdirSync(SHOTS, { recursive: true })
const ctl = async (q) => (await fetch(BASE + q)).json()
const sleep = (ms) => new Promise(r => setTimeout(r, ms))
const b64 = (o) => Buffer.from(JSON.stringify(o)).toString('base64url')
const FAKE = `${b64({ alg: 'none' })}.${b64({ sub: 'audit', exp: 4102444800 })}.x`

const browser = await chromium.launch({ executablePath: process.env.CHROME })
const facts = {}

async function open(viewport, scheme, { token = true, path = '/' } = {}) {
  const mobile = viewport.width < 600
  const ctx = await browser.newContext({ viewport, deviceScaleFactor: 2, isMobile: mobile, hasTouch: mobile, colorScheme: scheme })
  if (token) await ctx.addInitScript((t) => localStorage.setItem('reach-token', t), FAKE)
  const page = await ctx.newPage()
  await page.goto(BASE + path)
  if (token) await page.locator('.mrow-main:visible').first().waitFor()
  await sleep(500)
  return { ctx, page }
}
const shot = (page, name, full = false) => (!ONLY || name.includes(ONLY)) && page.screenshot({ path: `${SHOTS}/${name}.png`, fullPage: full })
async function measureTargets(page) {
  return page.evaluate(() => {
    const els = [...document.querySelectorAll('button, a, input, select, summary, [role="radio"]')]
      .filter(e => { const r = e.getBoundingClientRect(); const s = getComputedStyle(e); return r.width > 0 && r.height > 0 && s.visibility !== 'hidden' && e.type !== 'checkbox' })
    const small = els.map(e => { const r = e.getBoundingClientRect(); return { h: Math.round(r.height), w: Math.round(r.width), t: (e.getAttribute('aria-label') || e.textContent || e.tagName).trim().slice(0, 30) } })
      .filter(x => x.h < 44 || x.w < 44)
    return { interactive: els.length, under44: small, horizontalOverflow: document.documentElement.scrollWidth > innerWidth }
  })
}

const PHONE = { width: 393, height: 852 }
const DESK = { width: 1440, height: 900 }

// ---- phone, busy state (pending + 1 dropped + stuck removals)
await ctl('/__config?scenario=arthur&pending=2&v2=1&latency=20')
for (const scheme of ['dark', 'light']) {
  const { ctx, page } = await open(PHONE, scheme)
  await shot(page, `01-phone-home-busy-${scheme}`)
  if (scheme === 'dark') {
    facts.phoneHome = await measureTargets(page)
    await shot(page, '02-phone-home-busy-dark-full', true)
    // dormant group expanded with bulk disable
    await page.locator('.main .fgroup-dormant .fgroup-toggle').click(); await sleep(200)
    await page.locator('.main .fgroup-dormant').scrollIntoViewIfNeeded(); await sleep(200)
    await shot(page, '03-phone-dormant-expanded')
    await page.locator('.main .fgroup-dormant .fgroup-toggle').click()
  }
  // requests tab
  await page.locator('.tabbar .tab').nth(1).click(); await sleep(300)
  await shot(page, `04-phone-requests-${scheme}`)
  if (scheme === 'dark') {
    facts.phoneRequests = await measureTargets(page)
    await shot(page, '05-phone-requests-full', true)
    await page.locator('.req-card').nth(1).locator('.btn-primary').click(); await sleep(300)
    await shot(page, '06-phone-approve-confirm')
    await page.locator('.dialog .btn', { hasText: 'Cancel' }).click(); await sleep(200)
  }
  // machine detail (jack: dropped yesterday)
  await page.locator('.tabbar .tab').nth(0).click(); await sleep(300)
  await page.locator('.main .mrow-main', { hasText: 'jack' }).first().click(); await page.waitForSelector('.detail-name'); await sleep(300)
  await shot(page, `07-phone-detail-offline-${scheme}`)
  if (scheme === 'dark') {
    facts.phoneDetail = await measureTargets(page)
    await shot(page, '08-phone-detail-offline-full', true)
    await page.locator('.danger-zone .btn-danger').click(); await sleep(300)
    await page.locator('.dialog input').fill('jac'); await sleep(100)
    await shot(page, '09-phone-remove-type-to-confirm')
    facts.removeEnabledWithWrongName = await page.locator('.dialog .btn-danger-solid').isEnabled()
    await page.locator('.dialog input').fill('jack'); await sleep(100)
    facts.removeEnabledWithRightName = await page.locator('.dialog .btn-danger-solid').isEnabled()
    await page.locator('.dialog .btn', { hasText: 'Cancel' }).click(); await sleep(200)
    // online machine detail
    await page.goBack(); await sleep(300)
    await page.locator('.main .mrow-main', { hasText: 'omarchy' }).first().click(); await page.waitForSelector('.detail-name'); await sleep(300)
    await shot(page, '10-phone-detail-online')
    // stuck retiring
    await page.goBack(); await sleep(300)
    await page.locator('.main .mrow-main', { hasText: 'retire-a' }).first().click(); await page.waitForSelector('.detail-name'); await sleep(300)
    await shot(page, '11-phone-detail-stuck-full', true)
  }
  // settings
  await page.locator('.tabbar .tab').nth(2).click(); await sleep(300)
  await shot(page, `12-phone-settings-${scheme}`)
  if (scheme === 'dark') {
    facts.phoneSettings = await measureTargets(page)
    await shot(page, '13-phone-settings-full', true)
  }
  await ctx.close()
}

// ---- phone, calm state
await ctl('/__config?scenario=calm&pending=0')
for (const scheme of ['dark', 'light']) {
  const { ctx, page } = await open(PHONE, scheme)
  await shot(page, `14-phone-home-calm-${scheme}`)
  await ctx.close()
}

// ---- desktop
await ctl('/__config?scenario=arthur&pending=2')
for (const scheme of ['dark', 'light']) {
  const { ctx, page } = await open(DESK, scheme)
  await shot(page, `20-desktop-home-${scheme}`)
  await page.locator('.sidebar .mrow-main', { hasText: 'jack' }).first().click(); await page.waitForSelector('.detail-name'); await sleep(300)
  await shot(page, `21-desktop-detail-${scheme}`)
  if (scheme === 'dark') {
    await page.locator('.main').evaluate(e => e.scrollTo(0, 99999)); await sleep(200)
    await shot(page, '22-desktop-detail-bottom')
    await page.locator('details.advanced summary').click(); await sleep(200)
    await page.locator('details.advanced').scrollIntoViewIfNeeded(); await sleep(200)
    await shot(page, '23-desktop-detail-advanced-open')
    await page.locator('.topnav-item', { hasText: 'Settings' }).click(); await sleep(300)
    await shot(page, '24-desktop-settings')
    await page.locator('.topnav-item', { hasText: 'Requests' }).click(); await sleep(300)
    await shot(page, '25-desktop-requests')
    // keyboard a: opens the confirmation, never approves directly
    await page.locator('.topnav-item', { hasText: 'Machines' }).click(); await sleep(200)
    await ctl('/__reset')
    await page.locator('body').click({ position: { x: 900, y: 860 } })
    await page.keyboard.press('a'); await sleep(300)
    facts.keyboardAOpensDialog = await page.locator('.dialog').isVisible()
    facts.keyboardAApproveCalls = (await ctl('/__stats')).counts['POST /api/admin/requests/:id/approve'] || 0
    await shot(page, '26-desktop-key-a-confirm')
  }
  await ctx.close()
}
await ctl('/__config?scenario=calm&pending=0')
{ const { ctx, page } = await open(DESK, 'dark'); await shot(page, '27-desktop-home-calm-dark'); await ctx.close() }

// ---- login
{ const { ctx, page } = await open(PHONE, 'dark', { token: false, path: '/login' }); await shot(page, '30-phone-login'); facts.loginUsernamePrefill = await page.locator('input[name="username"]').inputValue(); await ctx.close() }

await ctl('/__config?scenario=arthur&pending=2')
fs.writeFileSync(`${SHOTS}/facts.json`, JSON.stringify(facts, null, 2))
console.log(JSON.stringify(facts, null, 2))
await browser.close()
