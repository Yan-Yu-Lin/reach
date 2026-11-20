// End-to-end check against a REAL reachd (not the mock): an open dashboard tab whose session
// is revoked from another device must end up on the login page.
//
//   SITE=<generated .output/public> REACHD=http://127.0.0.1:19310 USER_NAME=... PASS=... \
//   CHROME=<headless shell> node real-backend-revoke.mjs
//
// Serves SITE on 127.0.0.1:19311 and proxies /api to REACHD (what nginx does on jason).
// Run reachd with a throwaway /tmp config (provisioning_enabled: false). Never point this at
// a production hub: it calls revoke-all.
import http from 'node:http'
import fs from 'node:fs'
import path from 'node:path'

const { chromium } = await import(process.env.PLAYWRIGHT || '/Users/linyanyu/20-29-Development/21-Active-Projects/instagram-chat-export/node_modules/playwright-core/index.mjs')
const SITE = process.env.SITE
const REACHD = new URL(process.env.REACHD || 'http://127.0.0.1:19310')
if (!['127.0.0.1', 'localhost'].includes(REACHD.hostname)) throw new Error('REACHD must be a local test instance')
const PORT = 19311
const BASE = `http://127.0.0.1:${PORT}`
const sleep = (ms) => new Promise(r => setTimeout(r, ms))

const server = http.createServer((req, res) => {
  if (req.url.startsWith('/api/')) {
    const up = http.request({ host: REACHD.hostname, port: REACHD.port, path: req.url, method: req.method, headers: { ...req.headers, host: REACHD.host, 'x-real-ip': '127.0.0.1' } }, r => {
      res.writeHead(r.statusCode, r.headers)
      r.pipe(res)
    })
    up.on('error', () => { res.writeHead(502); res.end() })
    req.pipe(up)
    res.on('close', () => up.destroy()) // client went away (also ends long SSE upstreams)
    return
  }
  const p = new URL(req.url, BASE).pathname
  let file = path.join(SITE, decodeURIComponent(p))
  if (!file.startsWith(SITE) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) file = path.join(SITE, 'index.html')
  const type = { '.js': 'application/javascript', '.css': 'text/css', '.html': 'text/html' }[path.extname(file)] || 'application/octet-stream'
  res.writeHead(200, { 'Content-Type': type }); res.end(fs.readFileSync(file))
})
await new Promise(r => server.listen(PORT, '127.0.0.1', r))

const browser = await chromium.launch({ executablePath: process.env.CHROME })
const out = {}
try {
  // Device A: signs in normally, opens the Requests tab, stream goes live.
  const a = await (await browser.newContext()).newPage()
  const apiLog = []
  a.on('response', r => { if (r.url().includes('/api/')) apiLog.push(`${r.request().method()} ${new URL(r.url()).pathname} ${r.status()}`) })
  await a.goto(`${BASE}/?tab=requests`)
  await a.fill('input[name="username"]', process.env.USER_NAME)
  await a.fill('input[name="password"]', process.env.PASS)
  await a.click('form .btn-primary')
  try {
    await a.locator('.live-live').waitFor({ timeout: 10000 })
  } catch (e) {
    console.error('device A did not go live:', a.url(), apiLog, (await a.locator('body').innerText()).slice(0, 400))
    throw e
  }
  out.deviceA_live = true
  out.deviceA_url_after_login = a.url().replace(BASE, '')

  // Device B: separate browser profile, signs in and presses Sign out everywhere.
  const b = await (await browser.newContext()).newPage()
  await b.goto(`${BASE}/?tab=settings`)
  await b.fill('input[name="username"]', process.env.USER_NAME)
  await b.fill('input[name="password"]', process.env.PASS)
  await b.click('form .btn-primary')
  await b.locator('.btn', { hasText: 'Sign out everywhere' }).waitFor({ timeout: 10000 })
  const t0 = Date.now()
  await b.locator('.btn', { hasText: 'Sign out everywhere' }).click()
  await b.locator('.dialog .btn-danger-solid').click()
  await b.locator('input[name="username"]').waitFor({ timeout: 10000 })
  out.deviceB_message = await b.locator('[role="alert"]').first().innerText()

  // Device A must leave on its own: reachd closes the stream, the reconnect gets 401.
  await a.locator('input[name="username"]').waitFor({ timeout: 20000 })
  out.deviceA_to_login_ms = Date.now() - t0
  out.deviceA_login_url = a.url().replace(BASE, '')
  out.deviceA_message = await a.locator('[role="alert"]').first().innerText().catch(() => '')
  out.deviceA_token_left = await a.evaluate(() => !!(localStorage.getItem('reach-token') || sessionStorage.getItem('reach-token')))
  out.deviceA_api_tail = apiLog.slice(-4)
  out.ok = out.deviceA_login_url.includes('next=%2F%3Ftab%3Drequests') || out.deviceA_login_url.includes('next=/?tab=requests')
  out.ok = out.ok && !out.deviceA_token_left && /Signed out everywhere/.test(out.deviceB_message)
} finally {
  await browser.close()
  server.close()
}
console.log(JSON.stringify(out, null, 2))
process.exit(out.ok ? 0 : 1)
