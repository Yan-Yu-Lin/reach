// Browser regression tests for the dashboard's consistency and safety behaviour (astra
// review F02-F10, daybreak P3-5..P3-7). Each case drives a real generated bundle in Chromium
// against mock.mjs and asserts on what the user SEES and on the API calls the mock actually
// received - not on planner objects.
//
//   SITE=<generated .output/public> CHROME=<chrome-headless-shell> node regress.mjs [filter]
//
// Starts its own mock on PORT (default 9390). Exit code 1 if any case fails.
import { spawn } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const { chromium } = await import(process.env.PLAYWRIGHT || '/Users/linyanyu/20-29-Development/21-Active-Projects/instagram-chat-export/node_modules/playwright-core/index.mjs')
const HERE = path.dirname(fileURLToPath(import.meta.url))
const PORT = Number(process.env.PORT || 9390)
const BASE = `http://127.0.0.1:${PORT}`
const FILTER = process.argv[2] || ''
const sleep = (ms) => new Promise(r => setTimeout(r, ms))
const b64 = (o) => Buffer.from(JSON.stringify(o)).toString('base64url')
const FAKE = `${b64({ alg: 'none' })}.${b64({ sub: 'regress', sv: 0, exp: 4102444800 })}.x`

if (!process.env.SITE) { console.error('SITE is required'); process.exit(2) }
const mock = spawn(process.execPath, [path.join(HERE, 'mock.mjs')], { env: { ...process.env, PORT: String(PORT) }, stdio: ['ignore', 'pipe', 'inherit'] })
await new Promise((resolve, reject) => {
  mock.stdout.on('data', d => { if (String(d).includes('mock reachd')) resolve() })
  mock.on('exit', code => reject(new Error(`mock exited ${code}`)))
})

const ctl = async (q, body) => {
  const res = await fetch(BASE + q, body ? { method: 'POST', body: JSON.stringify(body) } : undefined)
  return res.json()
}
const reset = (q = '') => ctl(`/__resetall?scenario=arthur&hbfmt=new&v2=1&pending=0&latency=20${q ? `&${q}` : ''}`)
const calls = async (match) => (await ctl('/__stats')).log.filter(([, k]) => k.includes(match))

const browser = await chromium.launch({ executablePath: process.env.CHROME })
const results = []

async function open(viewport = { width: 1440, height: 900 }, { token = FAKE, path: p = '/', storage = 'local' } = {}) {
  const ctx = await browser.newContext({ viewport, colorScheme: 'dark' })
  if (token) await ctx.addInitScript(([t, where]) => { (where === 'local' ? localStorage : sessionStorage).setItem('reach-token', t) }, [token, storage])
  const page = await ctx.newPage()
  page.on('pageerror', e => console.error('  pageerror:', e.message))
  await page.goto(BASE + p)
  return { ctx, page }
}
const row = (page, slug) => page.locator('.sidebar .mrow', { has: page.locator('.mrow-name', { hasText: new RegExp(`^${slug}$`) }) })
async function rowText(page, slug) { return (await row(page, slug).innerText()).replace(/\s+/g, ' ') }
async function waitFor(fn, ms = 5000, what = 'condition') {
  const t0 = Date.now()
  let last
  while (Date.now() - t0 < ms) {
    try { last = await fn(); if (last) return last } catch (e) { last = e.message }
    await sleep(100)
  }
  throw new Error(`timed out waiting for ${what} (last: ${JSON.stringify(last)})`)
}
function expect(cond, msg) { if (!cond) throw new Error(msg) }
async function login(page) {
  await page.locator('input[name="username"]').waitFor()
  await page.fill('input[name="username"]', 'arthur')
  await page.fill('input[name="password"]', 'correct-horse')
  await page.click('form .btn-primary')
}
const machineId = async (slug) => (await ctl('/__mutate', { slug })).machine.id
const tokens = (page) => page.evaluate(() => [localStorage.getItem('reach-token'), sessionStorage.getItem('reach-token')])

async function test(name, fn) {
  if (FILTER && !name.includes(FILTER)) return
  const t0 = Date.now()
  try {
    await fn()
    results.push({ name, ok: true, ms: Date.now() - t0 })
    console.log(`PASS  ${name}`)
  } catch (e) {
    results.push({ name, ok: false, ms: Date.now() - t0, error: e.message })
    console.log(`FAIL  ${name}\n      ${e.message}`)
  }
}

// ---------------------------------------------------------------- F02 / daybreak P3-6
await test('F02 a machine disabled between the snapshot read and the subscribe shows Disabled', async () => {
  await reset()
  // The machines snapshot is slow; the event stream subscribes late. The change happens
  // after the snapshot was read but before the stream listens, so no event reaches the page.
  await ctl('/__config?sse_delay=600')
  await ctl('/__latency?key=GET /api/admin/machines&ms=300&times=1')
  const { ctx, page } = await open()
  await sleep(150) // snapshot request read (old state), stream accepted but not yet listening
  await ctl('/__mutate', { slug: 'omarchy', machine: { desired_state: 'disabled', status: 'disabled' } })
  await page.locator('.live-live').waitFor({ timeout: 5000 })
  await sleep(1500) // hello + snapshot have landed
  const online = await page.locator('.sidebar .fgroup-online .mrow-name').allInnerTexts()
  expect(!online.includes('omarchy'), `omarchy still listed under Online: ${online}`)
  // and the user sees it as Disabled when looking it up
  await page.fill('.sidebar .fleet-search input', 'omarchy')
  const text = await rowText(page, 'omarchy')
  expect(text.includes('Disabled'), `omarchy row: "${text}"`)
  await ctx.close()
})

// ---------------------------------------------------------------- F03
await test('F03 a slow machines response does not roll back a newer heartbeat state', async () => {
  await reset()
  const { ctx, page } = await open()
  await page.locator('.live-live').waitFor()
  await waitFor(async () => (await rowText(page, 'omarchy')).includes('Online'), 5000, 'omarchy online')
  // A full snapshot is read now (omarchy online) but answered in 1.2 s ...
  await ctl('/__latency?key=GET /api/admin/machines&ms=1200&times=1')
  await ctl('/__emit', { type: 'machine.resync_required', data: { reason: 'test' } })
  await sleep(200)
  // ... meanwhile omarchy degrades (live heartbeat + durable observed_changed).
  await ctl('/__mutate', { slug: 'omarchy', machine: { observed_state: 'degraded', status: 'degraded' } })
  await ctl('/__hb?slug=omarchy&observed=degraded')
  await waitFor(async () => (await rowText(page, 'omarchy')).includes('Degraded'), 3000, 'degraded from SSE')
  await sleep(1500) // the stale snapshot has landed by now
  const text = await rowText(page, 'omarchy')
  expect(text.includes('Degraded'), `stale snapshot rolled omarchy back: "${text}"`)
  expect(/just now|\d+s ago/.test(text), `heartbeat time rolled back: "${text}"`)
  await ctx.close()
})

await test('F03 a slow single-machine GET does not roll back a newer state either', async () => {
  await reset()
  const { ctx, page } = await open()
  await page.locator('.live-live').waitFor()
  await ctl('/__latency?key=GET /api/admin/machines/:id&ms=1200&times=1')
  // ssh_config.changed -> single GET for omarchy, read now (online), answered late
  await ctl('/__mutate', { slug: 'omarchy', emit: [{ type: 'ssh_config.changed', data: { reason: 'test' } }] })
  await sleep(200)
  await ctl('/__mutate', { slug: 'omarchy', machine: { observed_state: 'degraded', status: 'degraded' } })
  await ctl('/__hb?slug=omarchy&observed=degraded')
  await sleep(1600)
  const text = await rowText(page, 'omarchy')
  expect(text.includes('Degraded'), `stale GET rolled omarchy back: "${text}"`)
  await ctx.close()
})

// ---------------------------------------------------------------- F04
await test('F04 the open machine shows new diagnostics after heartbeats; closed ones cost no GET', async () => {
  await reset()
  const { ctx, page } = await open()
  await page.locator('.live-live').waitFor()
  await row(page, 'omarchy').waitFor()
  await ctl('/__mutate', { slug: 'omarchy', obs: { agent_version: 'v2-new', transport: 'websocket', persistence_backend: 'launchd' } })
  await ctl('/__reset')
  // heartbeats for a machine that is NOT open: no GET
  await ctl('/__hb?slug=ntut-vpn-help'); await ctl('/__hb?slug=ntut-vpn-help')
  await sleep(500)
  const gets = await calls('GET /api/admin/machines/:id')
  expect(gets.length === 0, `heartbeat of a closed machine triggered a GET: ${JSON.stringify(gets.map(g => g[3]))}`)
  // opening it fetches the details
  await row(page, 'omarchy').locator('.mrow-main').click()
  await waitFor(async () => (await page.locator('.detail').innerText()).includes('v2-new'), 4000, 'v2-new in diagnostics')
  const diag = await page.locator('.detail').innerText()
  expect(diag.includes('websocket') && diag.includes('launchd'), `diagnostics not refreshed: ${diag.slice(0, 300)}`)
  await ctx.close()
})

await test('F04 a heartbeat does not make an old tunnel report look current', async () => {
  await reset()
  // omarchy's stored observation is 12 h old and says connected/healthy; nothing else changes.
  const old = new Date(Date.now() - 12 * 3600_000).toISOString()
  await ctl('/__mutate', { slug: 'omarchy', obs: { heartbeat_at: old, tunnel_state: 'connected', local_ssh_state: 'healthy' } })
  const { ctx, page } = await open(undefined, { path: '/?m=' + (await ctl('/__mutate', { slug: 'omarchy' })).machine.id })
  await page.locator('.detail-name').waitFor()
  await page.locator('.live-live').waitFor()
  // A live heartbeat arrives, but the GET it would trigger fails: only the event is known.
  await ctl('/__fail?key=GET /api/admin/machines/:id&status=500&times=5')
  await ctl('/__emit', { live: true, type: 'agent.heartbeat', data: { machine_id: (await ctl('/__mutate', { slug: 'omarchy' })).machine.id, heartbeat_at: new Date().toISOString(), observed_state: 'online', status: 'online' } })
  await sleep(500)
  const checks = await page.locator('.checks').innerText()
  expect(/Tunnel\s+Unknown/.test(checks) && /Target sshd\s+Unknown/.test(checks), `12 h old tunnel report shown as current:\n${checks}`)
  await ctx.close()
})

await test('R2-02 the last heartbeat while a detail is open is followed by one fetch (trailing)', async () => {
  for (const fmt of ['new', 'old']) {
    await reset(`hbfmt=${fmt}&v2=${fmt === 'new' ? 1 : 0}`)
    const id = (await ctl('/__mutate', { slug: 'omarchy', obs: { agent_version: 'r2-before', transport: 'direct', persistence_backend: 'systemd' } })).machine.id
    const { ctx, page } = await open(undefined, { path: `/?m=${id}` })
    await waitFor(async () => (await page.locator('.detail').innerText()).includes('r2-before'), 5000, 'initial detail')
    await sleep(200)
    await ctl('/__reset')
    // the detail is OPEN; the server changes, ONE heartbeat inside the 15 s window, then silence
    await ctl('/__mutate', { slug: 'omarchy', obs: { agent_version: 'r2-final-heartbeat', transport: 'websocket', persistence_backend: 'launchd' } })
    await ctl('/__hb?slug=omarchy')
    await sleep(1000)
    expect((await calls('GET /api/admin/machines/:id')).length === 0, `${fmt}: fetched inside the window (no throttling)`)
    await waitFor(async () => (await page.locator('.detail').innerText()).includes('r2-final-heartbeat'), 17000, `${fmt}: trailing fetch`)
    const gets = (await calls('GET /api/admin/machines/:id')).length
    expect(gets === 1, `${fmt}: ${gets} detail GETs, expected exactly 1`)
    const d = await page.locator('.detail').innerText()
    expect(d.includes('websocket') && d.includes('launchd'), `${fmt}: diagnostics not updated`)
    await ctx.close()
  }
})

await test('R2-02 closing the detail cancels its pending trailing fetch', async () => {
  await reset()
  const id = await machineId('omarchy')
  const { ctx, page } = await open(undefined, { path: `/?m=${id}` })
  await page.locator('.detail-name').waitFor()
  await sleep(300)
  await ctl('/__hb?slug=omarchy') // schedules a trailing fetch
  await page.locator('.btn-back').click()
  await ctl('/__reset')
  await sleep(16000)
  expect((await calls('GET /api/admin/machines/:id')).length === 0, 'trailing fetch fired after the detail was closed')
  await ctx.close()
})

// ---------------------------------------------------------------- F05
await test('F05 reconnecting with a cursor still catches up live-only heartbeats', async () => {
  await reset()
  const { ctx, page } = await open()
  await page.locator('.live-live').waitFor()
  // give the client a cursor
  await ctl('/__emit', { type: 'machine.process_title_changed', data: { machine_id: 'm_none' } })
  await sleep(300)
  // stream drops; while it is down jack comes back (only a live heartbeat + no durable event)
  await ctl('/__ssepause?ms=1500')
  const jack = await ctl('/__mutate', { slug: 'jack', machine: { observed_state: 'online', status: 'online' }, obs: { heartbeat_at: new Date().toISOString(), tunnel_state: 'connected', local_ssh_state: 'healthy' } })
  expect(jack.machine.observed_state === 'online', 'fixture')
  await ctl('/__reset')
  await waitFor(async () => (await rowText(page, 'jack')).includes('Online'), 12000, 'jack online after reconnect')
  const resumed = (await ctl('/__stats')).log.some(([, k]) => k === 'GET /api/admin/machines')
  expect(resumed, 'no snapshot after the resumed hello')
  await ctx.close()
})

// ---------------------------------------------------------------- F06
await test('F06 a pending-requests failure is retried and the error clears', async () => {
  await reset()
  const { ctx, page } = await open()
  await page.locator('.live-live').waitFor()
  await ctl('/__fail?key=GET /api/admin/requests&status=500&times=1')
  await ctl('/__newrequest?name=late-friend')
  await waitFor(async () => (await page.locator('.notice-error').count()) > 0, 3000, 'error shown')
  await waitFor(async () => (await page.locator('.topnav-item', { hasText: 'Requests' }).innerText()).includes('1'), 6000, 'request appears after retry')
  await waitFor(async () => (await page.locator('.notice-error').count()) === 0, 3000, 'error cleared')
  await ctx.close()
})

// ---------------------------------------------------------------- F07
await test('F07 Disable all leaves out a machine that came back online while the dialog was open', async () => {
  await reset()
  const { ctx, page } = await open()
  await page.locator('.live-live').waitFor()
  await page.locator('.sidebar .fgroup-dormant .fgroup-toggle').click()
  await page.locator('.sidebar .fgroup-bulk button').click()
  const title0 = await page.locator('.dialog-title').innerText()
  const n0 = Number(/Disable (\d+)/.exec(title0)[1])
  // ntut-vdi-win (dormant) comes back
  await ctl('/__mutate', { slug: 'ntut-vdi-win', machine: { observed_state: 'online', status: 'online' } })
  await ctl('/__hb?slug=ntut-vdi-win&observed=online')
  await waitFor(async () => (await page.locator('.bulk-dropped').count()) > 0, 3000, 'dropped notice')
  const title1 = await page.locator('.dialog-title').innerText()
  expect(title1.includes(`Disable ${n0 - 1}`), `dialog still says "${title1}" (was ${n0})`)
  expect(!(await page.locator('.bulk-list').innerText()).includes('ntut-vdi-win'), 'ntut-vdi-win still in the list')
  await ctl('/__reset')
  await page.locator('.dialog .btn-danger-solid').click()
  await waitFor(async () => (await calls('/disable')).length === n0 - 1, 5000, `${n0 - 1} disable calls`)
  const disabled = (await ctl('/__stats')).log.filter(([, k]) => k.endsWith('/disable')).map(l => l[3])
  const vdi = (await ctl('/__mutate', { slug: 'ntut-vdi-win' })).machine
  expect(vdi.desired_state === 'active', `ntut-vdi-win was disabled (${disabled.length} calls)`)
  await ctx.close()
})

// ---------------------------------------------------------------- F08
await test('F08 a disabled machine does not claim Hub probe OK', async () => {
  await reset()
  const id = (await ctl('/__mutate', { slug: 'omarchy', machine: { desired_state: 'disabled', status: 'disabled' }, hub: { probe_state: 'reachable', last_probe_at: new Date(Date.now() - 86400_000).toISOString() } })).machine.id
  const { ctx, page } = await open(undefined, { path: `/?m=${id}` })
  await page.locator('.checks').waitFor()
  const checks = await page.locator('.checks').innerText()
  expect(!/\bOK\b/.test(checks), `disabled machine shows OK:\n${checks}`)
  expect(/Not checked/.test(checks), `no "Not checked":\n${checks}`)
  await ctx.close()
})

await test('F08 an active machine with a 1-day-old reachable probe shows Unknown, not OK', async () => {
  await reset()
  const id = (await ctl('/__mutate', { slug: 'omarchy', hub: { probe_state: 'reachable', last_probe_at: new Date(Date.now() - 86400_000).toISOString() } })).machine.id
  const { ctx, page } = await open(undefined, { path: `/?m=${id}` })
  await page.locator('.checks').waitFor()
  const probe = await page.locator('.check').first().innerText()
  expect(/Unknown/.test(probe) && !/\bOK\b/.test(probe), `stale probe: ${probe}`)
  await ctx.close()
})

await test('R2-03 a replayed health event (no server time) does not make a day-old probe OK, even if refresh fails', async () => {
  await reset('v2=0') // old server: health_changed carries no last_probe_at
  const old = new Date(Date.now() - 86400_000).toISOString()
  const id = (await ctl('/__mutate', { slug: 'omarchy', hub: { last_probe_at: old, probe_state: 'reachable' } })).machine.id
  const { ctx, page } = await open(undefined, { path: `/?m=${id}` })
  await waitFor(async () => (await page.locator('.check').first().innerText()).includes('Unknown'), 5000, 'Unknown before')
  await ctl('/__emit', { type: 'machine.process_title_changed', data: { machine_id: id } }) // gives the client a cursor
  await sleep(300)
  await ctl('/__fail?key=GET /api/admin/machines&status=500&times=20')
  await ctl('/__fail?key=GET /api/admin/machines/:id&status=500&times=20')
  await ctl('/__ssepause?ms=1500')
  // historical durable health event, replayed on reconnect; DB probe stays a day old
  await ctl('/__emit', { type: 'machine.health_changed', data: { machine_id: id, observed_state: 'online', status: 'online', probe_state: 'reachable' } })
  await page.locator('.notice-error').waitFor({ timeout: 10000 }) // refresh failed, as in astra's repro
  await sleep(1500)
  const probe = await page.locator('.check').first().innerText()
  expect(/Unknown/.test(probe) && !/\bOK\b/.test(probe), `replay made the probe look current: ${probe}`)
  const asked = (await ctl('/__stats')).log.filter(([, k]) => k === 'GET /api/admin/machines/:id').length
  expect(asked >= 1, 'no authoritative fetch after an undated probe result')
  await ctx.close()
})

await test('R2-03 a health event with the server probe time is judged by that time', async () => {
  await reset() // new server: last_probe_at in the payload
  const id = await machineId('omarchy')
  const { ctx, page } = await open(undefined, { path: `/?m=${id}` })
  await page.locator('.checks').waitFor()
  await ctl('/__fail?key=GET /api/admin/machines/:id&status=500&times=20')
  const old = new Date(Date.now() - 86400_000).toISOString()
  await ctl('/__emit', { type: 'machine.health_changed', data: { machine_id: id, observed_state: 'online', status: 'online', probe_state: 'reachable', last_probe_at: old, event_at: old } })
  await sleep(600)
  // the stored probe (20 s ago) is newer than the replayed one: still OK, not rolled back
  expect(/\bOK\b/.test(await page.locator('.check').first().innerText()), 'newer probe was overwritten by an older replay')
  const fresh = new Date().toISOString()
  await ctl('/__emit', { type: 'machine.health_changed', data: { machine_id: id, observed_state: 'degraded', status: 'degraded', probe_state: 'unreachable', last_probe_at: fresh, event_at: fresh } })
  await waitFor(async () => /Failed/.test(await page.locator('.check').first().innerText()), 3000, 'fresh unreachable shown')
  await ctx.close()
})

await test('R3-03 an undated reachable replayed over a FRESH failure does not show OK (refresh failing)', async () => {
  await reset('v2=0') // older reachd: no last_probe_at, no event_at
  const id = (await ctl('/__mutate', { slug: 'omarchy', hub: { probe_state: 'unreachable', last_probe_at: new Date().toISOString(), probe_error: 'connection refused' } })).machine.id
  const { ctx, page } = await open(undefined, { path: `/?m=${id}` })
  await waitFor(async () => /Failed/.test(await page.locator('.check').first().innerText()), 5000, 'Failed before')
  await ctl('/__emit', { type: 'machine.process_title_changed', data: { machine_id: id } })
  await sleep(300)
  await ctl('/__fail?key=GET /api/admin/machines&status=500&times=20')
  await ctl('/__fail?key=GET /api/admin/machines/:id&status=500&times=20')
  await ctl('/__ssepause?ms=1500')
  await ctl('/__emit', { type: 'machine.health_changed', data: { machine_id: id, observed_state: 'online', status: 'online', probe_state: 'reachable' } })
  await page.locator('.notice-error').waitFor({ timeout: 10000 })
  await sleep(1000)
  const probe = await page.locator('.check').first().innerText()
  expect(!/\bOK\b/.test(probe), `undated replay borrowed the fresh time: ${probe}`)
  expect(/Unknown/.test(probe), `expected Unknown: ${probe}`)
  await ctx.close()
})

await test('R3-03 an explicit empty probe time (never probed) does not borrow the old error', async () => {
  await reset()
  const id = (await ctl('/__mutate', { slug: 'omarchy', hub: { probe_state: 'unreachable', last_probe_at: new Date().toISOString(), probe_error: 'frozen timestamp fixture' } })).machine.id
  const { ctx, page } = await open(undefined, { path: `/?m=${id}` })
  await waitFor(async () => /frozen/.test(await page.locator('.check').first().innerText()), 5000, 'old error shown')
  // like astra's real-reachd run: every authoritative read fails, the event is all we get
  await ctl('/__fail?key=GET /api/admin/machines&status=503&times=20')
  await ctl('/__fail?key=GET /api/admin/machines/:id&status=503&times=20')
  await ctl('/__mutate', { slug: 'omarchy', hub: { probe_state: 'unknown', last_probe_at: '', probe_error: '' } })
  const at = new Date().toISOString()
  await ctl('/__emit', { type: 'machine.health_changed', data: { machine_id: id, observed_state: 'online', status: 'online', probe_state: 'unknown', last_probe_at: '', event_at: at } })
  await sleep(800)
  const probe = await page.locator('.check').first().innerText()
  expect(!/frozen|Failed/.test(probe), `borrowed the earlier observation: ${probe}`)
  await ctx.close()
})

await test('R3-04 an old dated health event with a DIFFERENT state changes neither badge nor probe', async () => {
  await reset()
  const id = (await ctl('/__mutate', { slug: 'omarchy', hub: { probe_state: 'reachable', last_probe_at: new Date().toISOString() } })).machine.id
  const { ctx, page } = await open(undefined, { path: `/?m=${id}` })
  await waitFor(async () => /Online/.test(await page.locator('.detail .badge').innerText()), 5000, 'Online before')
  await ctl('/__emit', { type: 'machine.process_title_changed', data: { machine_id: id } })
  await sleep(300)
  await ctl('/__fail?key=GET /api/admin/machines&status=500&times=20')
  await ctl('/__fail?key=GET /api/admin/machines/:id&status=500&times=20')
  await ctl('/__ssepause?ms=1500')
  const old = new Date(Date.now() - 86400_000).toISOString()
  await ctl('/__emit', { type: 'machine.health_changed', data: { machine_id: id, observed_state: 'offline', status: 'offline', probe_state: 'unreachable', last_probe_at: old, event_at: old } })
  await page.locator('.notice-error').waitFor({ timeout: 10000 })
  await sleep(1000)
  const badge = await page.locator('.detail .badge').innerText()
  const probe = await page.locator('.check').first().innerText()
  expect(/Online/.test(badge), `old event rolled the state back: ${badge}`)
  expect(/\bOK\b/.test(probe), `newer probe lost: ${probe}`)
  await ctx.close()
})

await test('R3-04 an undated REPLAYED state change (older reachd) does not roll the badge back', async () => {
  await reset('v2=0')
  const id = await machineId('omarchy')
  const { ctx, page } = await open(undefined, { path: `/?m=${id}` })
  await waitFor(async () => /Online/.test(await page.locator('.detail .badge').innerText()), 5000, 'Online before')
  await ctl('/__emit', { type: 'machine.process_title_changed', data: { machine_id: id } })
  await sleep(300)
  await ctl('/__fail?key=GET /api/admin/machines&status=500&times=20')
  await ctl('/__fail?key=GET /api/admin/machines/:id&status=500&times=20')
  await ctl('/__ssepause?ms=1500')
  await ctl('/__emit', { type: 'machine.observed_changed', data: { machine_id: id, observed_state: 'offline', status: 'offline' } })
  await page.locator('.notice-error').waitFor({ timeout: 10000 })
  await sleep(1000)
  expect(/Online/.test(await page.locator('.detail .badge').innerText()), 'replayed undated history changed the badge')
  await ctx.close()
})

// ---------------------------------------------------------------- F09
await test('F09 a failed disable is reported as a failed disable, not a stuck removal', async () => {
  await reset()
  await ctl('/__mutate', { slug: 'omarchy', machine: { desired_state: 'disabled', status: 'disable_failed', cleanup_state: 'disable_failed' } })
  const { ctx, page } = await open()
  await page.locator('.todo').waitFor()
  const todo = await page.locator('.todo').innerText()
  expect(/disable did not finish: omarchy/i.test(todo), `banner: ${todo}`)
  expect(!/removals? stuck:[^\n]*omarchy/i.test(todo), `omarchy reported as a stuck removal: ${todo}`)
  expect((await rowText(page, 'omarchy')).includes('Disable failed'), 'row label')
  await ctx.close()
})

// ---------------------------------------------------------------- F10
await test('F10 deep link survives the login detour', async () => {
  await reset('auth=1')
  const { ctx, page } = await open(undefined, { token: null, path: '/?tab=requests' })
  await page.locator('input[name="username"]').waitFor()
  await page.fill('input[name="username"]', 'arthur')
  await page.fill('input[name="password"]', 'correct-horse')
  await page.click('form .btn-primary')
  await waitFor(async () => page.url().includes('tab=requests'), 4000, 'back on ?tab=requests')
  await page.locator('h1.page-title', { hasText: 'Requests' }).waitFor()
  await ctx.close()
})

await test('F10 ?next cannot send you to another site', async () => {
  await reset('auth=1')
  for (const evil of ['//evil.example/x', 'https://evil.example', '/\\evil.example', '/%2F%2Fevil.example',
    // R2-04: become protocol-relative only after URL normalisation
    '/%2e%2e//evil.example/x', '/a/%2e%2e//evil.example/x', '/.%2e//evil.example/x']) {
    const { ctx, page } = await open(undefined, { token: null, path: `/login?next=${encodeURIComponent(evil)}` })
    await page.fill('input[name="username"]', 'arthur')
    await page.fill('input[name="password"]', 'correct-horse')
    await page.click('form .btn-primary')
    await sleep(800)
    const u = new URL(page.url())
    expect(u.origin === BASE, `${evil} led to ${page.url()}`)
    // signed in AND off the login page (astra R2-04: it used to stay stuck on /login),
    // on a single-slash in-app path
    expect(u.pathname !== '/login' && !u.pathname.startsWith('//'), `${evil}: ended on ${u.pathname}${u.search}`)
    expect(!!(await tokens(page))[1], `${evil}: not signed in`)
    await ctx.close()
  }
})

// ---------------------------------------------------------------- daybreak P3-5
await test('P3-5 revoke-all answered 401 is not shown as success; user re-confirms after login', async () => {
  // Starts directly on Settings with a valid stored token, so it does not depend on F10.
  await reset('auth=1')
  const { ctx, page } = await open(undefined, { token: FAKE, path: '/?tab=settings', storage: 'session' })
  await page.locator('.btn', { hasText: 'Sign out everywhere' }).waitFor()
  await ctl('/__revoke?close=0') // this login became invalid behind our back
  await ctl('/__reset')
  await page.locator('.btn', { hasText: 'Sign out everywhere' }).click()
  await page.locator('.dialog .btn-danger-solid').click()
  await page.locator('input[name="username"]').waitFor()
  const msg = await page.locator('[role="alert"]').first().innerText()
  expect(/did NOT happen/.test(msg), `login page message: ${msg}`)
  expect(!/Signed out everywhere\./.test(msg), 'shown as success')
  const sv0 = (await ctl('/__stats')).sessionVersion
  await page.fill('input[name="username"]', 'arthur')
  await page.fill('input[name="password"]', 'correct-horse')
  await page.click('form .btn-primary')
  // back on settings with the confirmation open again - nothing sent until confirmed
  await page.locator('.dialog-title', { hasText: 'Sign out everywhere?' }).waitFor()
  expect((await ctl('/__stats')).sessionVersion === sv0, 'revoke ran without confirmation')
  await page.locator('.dialog .btn-danger-solid').click()
  await page.locator('input[name="username"]').waitFor()
  expect((await ctl('/__stats')).sessionVersion === sv0 + 1, 'revoke-all was not sent after re-login')
  const ok = await page.locator('[role="alert"]').first().innerText()
  expect(/Signed out everywhere/.test(ok), `success message missing: ${ok}`)
  await ctx.close()
})

await test('revoked session: the open tab goes to login when its stream is closed', async () => {
  await reset('auth=1')
  const { ctx, page } = await open(undefined, { token: null, path: '/?tab=requests' })
  await page.fill('input[name="username"]', 'arthur')
  await page.fill('input[name="password"]', 'correct-horse')
  await page.click('form .btn-primary')
  await page.locator('.live-live').waitFor()
  await ctl('/__revoke') // another device signs out everywhere; server closes this stream
  await page.locator('input[name="username"]').waitFor({ timeout: 8000 })
  const next = new URL(page.url()).searchParams.get('next')
  expect(next === '/?tab=requests', `return path lost: ${page.url()}`)
  const stored = await page.evaluate(() => [localStorage.getItem('reach-token'), sessionStorage.getItem('reach-token')])
  expect(!stored[0] && !stored[1], 'token still stored')
  await ctx.close()
})

await test('sign-out in one tab signs out the other tabs of the browser', async () => {
  await reset()
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } })
  await ctx.addInitScript((t) => { if (!localStorage.getItem('reach-signout')) localStorage.setItem('reach-token', t) }, FAKE)
  const a = await ctx.newPage(); await a.goto(BASE + '/'); await a.locator('.live-live').waitFor()
  const b = await ctx.newPage(); await b.goto(BASE + '/?tab=settings'); await b.locator('.live-live').waitFor()
  await b.locator('.btn', { hasText: /^\s*Sign out\s*$/ }).click()
  await a.locator('input[name="username"]').waitFor({ timeout: 4000 })
  await ctx.close()
})

await test('R2-01 a late 401 for the old session does not sign out the new login (this tab or another)', async () => {
  await reset('auth=1')
  await ctl('/__revoke?close=0') // the stored token is already invalid
  await ctl('/__config?sse401_delay=2600')
  await ctl('/__latency?key=GET /api/admin/machines&ms=5000&times=1')
  await ctl('/__latency?key=GET /api/admin/requests&ms=5000&times=1')
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } })
  const a = await ctx.newPage()
  await a.addInitScript(t => { if (!sessionStorage.getItem('seeded')) { sessionStorage.setItem('seeded', '1'); sessionStorage.setItem('reach-token', t) } }, FAKE)
  await a.goto(`${BASE}/?tab=requests`)
  // first 401 (the delayed stream) sends A to login
  await a.locator('input[name="username"]').waitFor({ timeout: 7000 })
  expect(new URL(a.url()).searchParams.get('next') === '/?tab=requests', `first redirect lost next: ${a.url()}`)
  await login(a)
  await a.locator('.live-live').waitFor({ timeout: 6000 })
  const b = await ctx.newPage()
  await b.goto(`${BASE}/?tab=settings`)
  await login(b)
  await b.locator('.live-live').waitFor({ timeout: 6000 })
  // the bootstrap GETs sent with the OLD token answer 401 now
  await sleep(5500)
  expect(!new URL(a.url()).pathname.startsWith('/login'), `A was signed out by a stale 401: ${a.url()}`)
  expect(!new URL(b.url()).pathname.startsWith('/login'), `B was signed out by a stale 401: ${b.url()}`)
  expect(!!(await tokens(a))[1] && !!(await tokens(b))[1], 'a valid token was cleared')
  await ctx.close()
})

await test('R2-01 a second late 401 keeps the first saved return path', async () => {
  await reset('auth=1')
  await ctl('/__revoke?close=0')
  await ctl('/__config?sse401_delay=2600')
  await ctl('/__latency?key=GET /api/admin/machines&ms=5000&times=1')
  const { ctx, page } = await open(undefined, { token: FAKE, path: '/?tab=requests', storage: 'session' })
  await page.locator('input[name="username"]').waitFor({ timeout: 7000 })
  await sleep(5500) // the late machines 401 arrives while we wait on the login page
  expect(new URL(page.url()).searchParams.get('next') === '/?tab=requests', `next overwritten: ${page.url()}`)
  await ctx.close()
})

await test('R2-01 a late revoke-all success does not sign out a session started after it', async () => {
  await reset('auth=1')
  const { ctx, page } = await open(undefined, { token: FAKE, path: '/?tab=settings', storage: 'session' })
  await page.locator('.live-live').waitFor()
  await ctl('/__latency?key=POST /api/admin/sessions/revoke-all&ms=7000&times=1')
  await page.locator('.btn', { hasText: 'Sign out everywhere' }).click()
  await page.locator('.dialog .btn-danger-solid').click()
  // the mock revokes immediately but answers in 7 s; our stream is closed -> 401 -> login
  await page.locator('input[name="username"]').waitFor({ timeout: 7000 })
  await login(page)
  await page.locator('.live-live').waitFor({ timeout: 6000 })
  await sleep(6000) // the old POST's 200 lands now
  expect(!new URL(page.url()).pathname.startsWith('/login'), `new session signed out by the old revoke reply: ${page.url()}`)
  expect(!!(await tokens(page))[1], 'new token cleared')
  await ctx.close()
})

// ---------------------------------------------------------------- daybreak P3-7
await test('P3-7 login keeps the token in sessionStorage unless "keep me signed in" is ticked', async () => {
  await reset('auth=1')
  const { ctx, page } = await open(undefined, { token: null, path: '/login' })
  expect(!(await page.locator('.login-remember input').isChecked()), 'remember is ticked by default')
  await page.fill('input[name="username"]', 'arthur')
  await page.fill('input[name="password"]', 'correct-horse')
  await page.click('form .btn-primary')
  await page.locator('.live-live').waitFor()
  const [local, session] = await page.evaluate(() => [localStorage.getItem('reach-token'), sessionStorage.getItem('reach-token')])
  expect(!local && !!session, `local=${!!local} session=${!!session}`)
  await ctx.close()
})

// ---------------------------------------------------------------- action -> state, 409
await test('Remove: typed name sends one POST, the row moves to Retiring', async () => {
  await reset()
  const id = (await ctl('/__mutate', { slug: 'ntut-vpn-help' })).machine.id
  const { ctx, page } = await open(undefined, { path: `/?m=${id}` })
  await page.locator('.live-live').waitFor()
  await page.locator('.danger-zone .btn-danger').first().click()
  await page.locator('.dialog input').fill('ntut-vpn-hel')
  await page.keyboard.press('Enter')
  await sleep(300)
  expect((await calls('/remove')).length === 0, 'wrong name sent a POST')
  await page.locator('.dialog input').fill('ntut-vpn-help')
  await page.locator('.dialog .btn-danger-solid').click()
  await waitFor(async () => (await calls('/remove')).length === 1, 3000, 'one remove POST')
  await page.fill('.sidebar .fleet-search input', 'ntut-vpn-help')
  await waitFor(async () => (await rowText(page, 'ntut-vpn-help')).includes('Retiring'), 4000, 'row shows Retiring')
  expect((await page.locator('.detail .badge').innerText()).includes('Retiring'), 'detail badge')
  await ctx.close()
})

await test('a 409 from Disable is shown as an error and the machine stays active', async () => {
  await reset()
  const id = (await ctl('/__mutate', { slug: 'ntut-vpn-help' })).machine.id
  await ctl('/__fail?key=POST /api/admin/machines/:id/disable&status=409&error=could not revoke hub access')
  const { ctx, page } = await open(undefined, { path: `/?m=${id}` })
  await page.locator('.live-live').waitFor()
  await page.locator('.danger-zone .btn-warn').click()
  await page.locator('.dialog .btn-danger-solid').click()
  await waitFor(async () => (await page.locator('.toast.error').count()) > 0, 3000, 'error toast')
  expect((await page.locator('.toast.error').innerText()).includes('could not revoke hub access'), 'error text')
  expect((await page.locator('.toast.success').count()) === 0, 'success toast shown')
  expect((await rowText(page, 'ntut-vpn-help')).includes('Online'), 'row changed')
  await ctx.close()
})

console.log(`\n${results.filter(r => r.ok).length}/${results.length} passed`)
await browser.close()
mock.kill()
process.exit(results.every(r => r.ok) ? 0 : 1)
