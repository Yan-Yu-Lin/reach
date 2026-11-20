// Drives a dashboard bundle against the local mock (mock.mjs) and measures load, event storms,
// long tasks and memory. Works for both the 8c74086 bundle and the 2026-11 redesign: the
// selectors below match either.
//
//   CHROME=<chrome-headless-shell> PLAYWRIGHT=<playwright-core/index.mjs> node measure.mjs <mode>
//   modes: load | storm | phone | leak | all
//
// Adapted from ~/30-39-Research/34-tech-research/2026-11-20-reach-dashboard-shots/harness/measure.mjs
import fs from 'node:fs'

const { chromium } = await import(process.env.PLAYWRIGHT || '/Users/linyanyu/20-29-Development/21-Active-Projects/instagram-chat-export/node_modules/playwright-core/index.mjs')

const BASE = process.env.BASE || 'http://127.0.0.1:9388'
const OUT = process.env.OUT || ''
const mode = process.argv[2] || 'all'
const ROW = '.fleet-item:visible, .mrow-main:visible'
const ROOT = '.app-shell, .shell'
const DETAIL = '.detail-slug, .detail-name'

const ctl = async (q) => (await fetch(BASE + q)).json()
const sleep = (ms) => new Promise(r => setTimeout(r, ms))
// fake unsigned JWT with exp far in the future (not a real credential; the mock never checks it)
const b64 = (o) => Buffer.from(JSON.stringify(o)).toString('base64url')
const FAKE = `${b64({ alg: 'none' })}.${b64({ sub: 'audit', exp: 4102444800 })}.x`

const browser = await chromium.launch({ executablePath: process.env.CHROME })
const out = {}

const DESKTOP = { width: 1440, height: 900 }
const PHONE = { width: 393, height: 852 }

async function newPage(viewport, opts = {}) {
  const mobile = viewport.width < 600
  const ctx = await browser.newContext({ viewport, deviceScaleFactor: opts.dpr || 1, isMobile: mobile, hasTouch: mobile, colorScheme: opts.scheme || 'dark' })
  await ctx.addInitScript((t) => { localStorage.setItem('reach-token', t) }, FAKE)
  const page = await ctx.newPage()
  const cdp = await ctx.newCDPSession(page)
  await cdp.send('Performance.enable')
  return { ctx, page, cdp }
}
async function metrics(cdp) {
  const { metrics } = await cdp.send('Performance.getMetrics')
  const m = Object.fromEntries(metrics.map(x => [x.name, x.value]))
  return { taskMs: +(m.TaskDuration * 1000).toFixed(0), scriptMs: +(m.ScriptDuration * 1000).toFixed(0), layoutMs: +(m.LayoutDuration * 1000).toFixed(0), recalcMs: +(m.RecalcStyleDuration * 1000).toFixed(0), layouts: m.LayoutCount, heapMB: +(m.JSHeapUsedSize / 1048576).toFixed(1), nodes: m.Nodes, listeners: m.JSEventListeners }
}
const diff = (a, b) => Object.fromEntries(Object.keys(b).map(k => [k, typeof b[k] === 'number' ? +(b[k] - a[k]).toFixed(1) : b[k]]))
const total = (counts) => Object.values(counts).reduce((a, b) => a + b, 0)

async function observe(page) {
  await page.evaluate((root) => {
    window.__lt = []
    new PerformanceObserver(l => l.getEntries().forEach(e => window.__lt.push(e.duration))).observe({ type: 'longtask', buffered: false })
    window.__mut = 0
    new MutationObserver(ms => { window.__mut += ms.length }).observe(document.querySelector(root), { subtree: true, childList: true, characterData: true, attributes: true })
  }, ROOT)
}
async function observed(page) {
  return page.evaluate(() => ({ longTasks: window.__lt.length, longTaskMaxMs: Math.round(Math.max(0, ...window.__lt)), longTaskSumMs: Math.round(window.__lt.reduce((a, b) => a + b, 0)), domMutations: window.__mut }))
}

/** Cold load over an emulated 4G link. Phone also gets 4x CPU throttling (mid-range phone). */
async function coldLoad(viewport, cfg, { gzip = false } = {}) {
  await ctl('/__stop'); await ctl(`/__config?${cfg}&gzip=${gzip ? 1 : 0}`); await ctl('/__reset')
  const { ctx, page, cdp } = await newPage(viewport)
  await cdp.send('Network.enable')
  await cdp.send('Network.emulateNetworkConditions', { offline: false, latency: 150, downloadThroughput: (1.6 * 1024 * 1024) / 8, uploadThroughput: (750 * 1024) / 8 })
  if (viewport.width < 600) await cdp.send('Emulation.setCPUThrottlingRate', { rate: 4 })
  let bytes = 0; cdp.on('Network.loadingFinished', e => { bytes += e.encodedDataLength })
  await page.addInitScript(() => {
    window.__lt = []
    new PerformanceObserver(l => l.getEntries().forEach(e => window.__lt.push(e.duration))).observe({ type: 'longtask', buffered: true })
  })
  const t0 = Date.now()
  await page.goto(BASE + '/', { waitUntil: 'load' })
  await page.locator(ROW).first().waitFor({ timeout: 30000 })
  const fleetVisibleMs = Date.now() - t0
  const nav = await page.evaluate(() => { const n = performance.getEntriesByType('navigation')[0]; const fcp = performance.getEntriesByName('first-contentful-paint')[0]; return { dcl: Math.round(n.domContentLoadedEventEnd), load: Math.round(n.loadEventEnd), fcp: fcp ? Math.round(fcp.startTime) : null } })
  await sleep(1500)
  const lt = await page.evaluate(() => ({ longTasks: window.__lt.length, longTaskMaxMs: Math.round(Math.max(0, ...window.__lt)) }))
  const counts = (await ctl('/__stats')).counts
  const r = { ...nav, fleetVisibleMs, transferKB: Math.round(bytes / 1024), apiCalls: counts, apiTotal: total(counts), ...lt, ...(await metrics(cdp)) }
  await ctx.close()
  return r
}

async function storm(viewport, q, waitMs, { throttle = 1, openDetail = false } = {}) {
  await ctl('/__stop'); await ctl('/__reset')
  const { ctx, page, cdp } = await newPage(viewport)
  await page.goto(BASE + '/'); await page.locator(ROW).first().waitFor(); await sleep(1500)
  if (openDetail) { await page.locator(ROW).nth(0).click(); await page.locator(DETAIL).first().waitFor(); await sleep(300) }
  if (throttle > 1) await cdp.send('Emulation.setCPUThrottlingRate', { rate: throttle })
  await observe(page)
  await ctl('/__reset')
  const m0 = await metrics(cdp)
  await ctl(q)
  await sleep(waitMs)
  const m1 = await metrics(cdp)
  const o = await observed(page)
  const st = await ctl('/__stats')
  const r = { apiCalls: st.counts, apiTotal: total(st.counts), maxInflight: st.maxInflight, ...o, delta: diff(m0, m1) }
  await ctx.close()
  return r
}

async function tapLatency() {
  await ctl('/__stop'); await ctl('/__reset')
  const { ctx, page, cdp } = await newPage(PHONE)
  await cdp.send('Emulation.setCPUThrottlingRate', { rate: 4 })
  await page.goto(BASE + '/'); await page.locator(ROW).first().waitFor(); await sleep(1500)
  await ctl('/__storm?rate=20&seconds=10&kind=flap'); await sleep(2000)
  const t0 = Date.now()
  await page.locator(ROW).nth(1).click()
  await page.locator(DETAIL).first().waitFor()
  const ms = Date.now() - t0
  await ctl('/__stop'); await ctx.close()
  return ms
}

const cfg40 = 'n=40&latency=30&pending=2'
if (mode === 'all' || mode === 'load') {
  out.load_desktop_40 = await coldLoad(DESKTOP, cfg40)
  out.load_desktop_40_gzip = await coldLoad(DESKTOP, cfg40, { gzip: true })
  out.load_phone_40 = await coldLoad(PHONE, cfg40)
  out.load_phone_40_gzip = await coldLoad(PHONE, cfg40, { gzip: true })
  out.load_desktop_200 = await coldLoad(DESKTOP, 'n=200&latency=30&pending=2')
  await ctl(`/__config?${cfg40}&gzip=0`)
}
if (mode === 'all' || mode === 'storm') {
  await ctl(`/__config?${cfg40}&gzip=0`)
  out.idle_realistic_desktop = await storm(DESKTOP, '/__realistic?period=15000', 15000)
  out.heartbeat_50ps_10s_desktop = await storm(DESKTOP, '/__storm?rate=50&seconds=10&kind=heartbeat', 12000)
  out.heartbeat_50ps_10s_phone_cpu4x = await storm(PHONE, '/__storm?rate=50&seconds=10&kind=heartbeat', 12000, { throttle: 4 })
  out.flap_20ps_10s_desktop = await storm(DESKTOP, '/__storm?rate=20&seconds=10&kind=flap', 13000)
  out.flap_20ps_10s_desktop_detail_open = await storm(DESKTOP, '/__storm?rate=20&seconds=10&kind=flap', 13000, { openDetail: true })
  out.flap_20ps_10s_phone_cpu4x = await storm(PHONE, '/__storm?rate=20&seconds=10&kind=flap', 13000, { throttle: 4 })
  await ctl('/__config?latency=400')
  out.flap_20ps_10s_slowapi_desktop = await storm(DESKTOP, '/__storm?rate=20&seconds=10&kind=flap', 18000)
  await ctl('/__config?latency=30')
}
if (mode === 'all' || mode === 'phone') {
  await ctl('/__config?n=29&latency=60&gzip=0&pending=0')
  out.phone_tap_to_detail_during_flap_ms = await tapLatency()
  await ctl(`/__config?${cfg40}`)
}
if (mode === 'all' || mode === 'leak') {
  await ctl('/__stop'); await ctl('/__reset'); await ctl('/__config?n=40&latency=30')
  const { ctx, page, cdp } = await newPage(DESKTOP)
  await page.goto(BASE + '/'); await page.locator(ROW).first().waitFor(); await sleep(1000)
  await page.locator(ROW).nth(3).click(); await sleep(300)
  const samples = []
  await ctl('/__storm?rate=20&seconds=120&kind=heartbeat')
  for (let i = 0; i <= 12; i++) { await cdp.send('HeapProfiler.collectGarbage').catch(() => {}); const m = await metrics(cdp); samples.push({ t: i * 10, heapMB: m.heapMB, nodes: m.nodes, listeners: m.listeners }); await sleep(10000) }
  await ctl('/__stop')
  const before = await metrics(cdp)
  for (let i = 0; i < 10; i++) { await ctl('/__dropsse'); await sleep(2500) }
  const after = await metrics(cdp)
  const st = await ctl('/__stats')
  out.leak = { samples, reconnect10: { before, after, sseClientsNow: st.sse, apiCalls: st.counts } }
  await ctx.close()
}

const json = JSON.stringify(out, null, 2)
if (OUT) fs.writeFileSync(OUT, json)
console.log(json)
await browser.close()
