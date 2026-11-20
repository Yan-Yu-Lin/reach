// Zero-dependency mock of the reachd admin API (shapes copied from api.go/models.go @ 8c74086,
// plus the 2026-11 contract in docs/EVENTS.md on fix/hardening-2026-11). Serves a dashboard
// bundle from SITE and fake data. No real tokens/secrets.
//
// Copied from ~/30-39-Research/34-tech-research/2026-11-20-reach-dashboard-shots/harness/ and extended.
// After astra's review (2026-11-21) it behaves like the server in the ways that matter for
// consistency bugs instead of hiding them:
//   - durable events live in a real log; a reconnect with Last-Event-ID replays exactly the
//     logged events after the cursor (HBFMT=old logs heartbeats too, capped at 500 like 8c74086;
//     HBFMT=new never logs or replays them), an out-of-range cursor gets resync_required;
//   - a response body is read when the request ARRIVES and sent after the latency, so a slow
//     response can be older than events published meanwhile (like a real DB read);
//   - POST actions change lifecycle state and publish the same events reachd does, and can be
//     told to fail (e.g. 409) through /__fail;
//   - login, session versions and revoke-all are simulated; with auth on, a revoked token gets
//     401 everywhere and (unless ?close=0) its open event streams are closed (hardening 2ef1460).
//
//   SCENARIO=audit|arthur|calm   fleet shape (see makeFleet)
//   HBFMT=old|new               heartbeat wire format
//   V2=1                        request rows carry observed_ip/slug/target_user; ?status= honoured
//   AUTH=1                      enforce tokens (default off, so the fake token always works)
import http from 'node:http'
import fs from 'node:fs'
import path from 'node:path'
import zlib from 'node:zlib'

const SITE = process.env.SITE || '/tmp/reach-fe-audit/site'
const PORT = Number(process.env.PORT || 9388)
const PASSWORD = 'correct-horse'
const DEFAULTS = {
  n: Number(process.env.N || 40),
  latencyMs: Number(process.env.LATENCY || 30),
  gzip: process.env.GZIP === '1',
  pending: Number(process.env.PENDING || 2),
  scenario: process.env.SCENARIO || 'audit',
  hbfmt: process.env.HBFMT || 'old',
  v2: process.env.V2 === '1',
  auth: process.env.AUTH === '1',
  sseDelayMs: 0, // wait this long between accepting /events and subscribing (snapshot gap)
  sse401DelayMs: 0, // a rejected /events answers its 401 this late (stale-401 races)
}
let state = { ...DEFAULTS }
let stats
let sseClients
let eventLog // durable events: { id, type, data }
let eventId
let failures // [{ key, status, error, times }]
let latencies // [{ key, ms, times }]
let sessionVersion
let sseRefusedUntil
let rejectedTokens // tokens the server no longer accepts (logout, targeted expiry)
let pendingRows

const iso = (msAgo) => new Date(Date.now() - msAgo).toISOString().replace(/\.\d+Z$/, 'Z')
const sleep = (ms) => new Promise(r => setTimeout(r, ms))
const clone = (v) => JSON.parse(JSON.stringify(v))

// ---------------------------------------------------------------- fleet fixtures
const names = ['omarchy', 'rtxws-colin', 'jack', 'ntut-vdi-win', 'gaming-pc', 'ntut-vpn-help', 'islabx6', 'mom-imac', 'dad-nas', 'pi-garage', 'lab-gpu-01', 'lab-gpu-02', 'vdi-ubuntu', 'friend-laptop', 'kevin-pc', 'school-ws', 'mac-mini', 'old-centos6', 'hotspot-box', 'arm-sbc']
let machines = []
function makeFleet(n) {
  if (state.scenario === 'arthur' || state.scenario === 'calm') return makeArthurFleet(state.scenario === 'calm')
  machines = []
  for (let i = 0; i < n; i++) {
    const slug = i < names.length ? names[i] : `box-${String(i).padStart(2, '0')}`
    const id = `m_${(i * 7919 + 1234).toString(36).padStart(6, 'x')}`
    // realistic mix: ~2/3 dead registrations like Arthur's DB (29 registered, 2 online)
    let desired = 'active', observed = 'online', status = 'active'
    if (i % 9 === 4) { desired = 'retiring'; observed = 'offline' }
    else if (i % 9 === 7) { desired = 'disabled'; observed = 'offline'; status = 'disabled' }
    else if (i % 5 === 2) { observed = 'offline' }
    else if (i % 7 === 3) { observed = 'gone' }
    else if (i % 11 === 5) { observed = 'degraded' }
    const hbAgo = observed === 'online' ? (i * 1300) % 45000 : 86400000 * (1 + (i % 20))
    machines.push({
      machine: {
        id, slug, display_name: slug, target_user: i % 6 === 0 ? 'root' : 'arthur', status, desired_state: desired, observed_state: observed,
        desired_generation: 3 + (i % 5), update_policy: i % 4 === 0 ? 'auto' : 'manual', mode: i % 3 === 0 ? 'user' : 'system',
        local_port: 22, distro: ['ubuntu 22.04', 'arch', 'darwin 15.4', 'windows 10', 'centos 6'][i % 5], arch: i % 4 === 1 ? 'arm64' : 'amd64',
        created_at: iso(86400000 * (60 - (i % 60))), updated_at: iso(3600000 * (i % 48)), desired_changed_at: iso(86400000 * (1 + (i % 30))),
        process_title_config: i === 0 ? { process_titles: ['Reach-Agent', 'tunnel vibes'], rotate_interval: '5s' } : undefined,
      },
      tunnels: [{ id: `t_${id}`, machine_id: id, hub_id: 'primary', unix_user: `rt-${id.slice(2)}`, remote_port: 9200 + i, status: observed === 'online' ? 'online' : 'offline', last_seen_at: iso(hbAgo), created_at: iso(86400000 * 30) }],
      agent_observation: {
        agent_version: i % 3 ? '0.1.0-dev.20261104T195003Z.g8c74086afe9e' : '0.1.0-dev.20260701T000000Z.g520d26c', applied_generation: 3 + (i % 5), heartbeat_at: iso(hbAgo),
        transport: i % 4 === 1 ? 'websocket' : 'direct', local_ssh_state: observed === 'online' ? 'healthy' : 'unknown', tunnel_state: observed === 'online' ? 'connected' : 'disconnected',
        tunnel_pid: 4000 + i, persistence_backend: 'systemd', persistence_quality: 'good', persistence_reboot_safe: true,
        last_error: observed === 'gone' ? 'dial tcp: i/o timeout' : undefined,
      },
      hub_observation: { probe_state: observed === 'online' ? 'reachable' : 'unreachable', last_probe_at: iso(20000), last_success_at: iso(hbAgo) },
      commands: [],
      update: { available: i % 3 === 0, current: '0.1.0', latest: '0.1.0-dev.20261104T195003Z.g8c74086afe9e' },
    })
  }
}

const arthurNames = ['omarchy', 'jack', 'ntut-vpn-help', 'ntut-vdi-win', 'ntut-vm', 'friend-year', 'another-windows-on-mac', 'docker-smoke-test-2', 'window-vm-on-mac', 'gaming-pc', 'mom-imac', 'dad-nas', 'pi-garage', 'lab-gpu-01', 'lab-gpu-02', 'vdi-ubuntu', 'kevin-pc', 'school-ws', 'mac-mini', 'old-centos6', 'hotspot-box', 'arm-sbc', 'retire-a', 'retire-b', 'retire-c', 'retire-d', 'retire-e', 'retire-f', 'retire-g']
function mk(i, slug, { desired = 'active', observed = 'online', status, hbAgo, changedAgo = 86400000 * 40, distro = 'arch' }) {
  const id = `m_${(i * 7919 + 1234).toString(36).padStart(6, 'x')}`
  const online = observed === 'online'
  return {
    machine: { id, slug, display_name: slug, target_user: 'arthur', status: status || (desired === 'active' ? observed : desired), desired_state: desired, observed_state: observed,
      desired_generation: 3, update_policy: 'manual', mode: 'system', local_port: 22, distro, arch: 'amd64',
      created_at: iso(86400000 * 120), updated_at: iso(changedAgo), desired_changed_at: iso(changedAgo), cleanup_state: desired === 'retiring' ? 'pending' : undefined },
    tunnels: [{ id: `t_${id}`, machine_id: id, hub_id: 'jason', unix_user: `rt-${id.slice(2)}`, remote_port: 9200 + i, status: online ? 'online' : 'offline', last_seen_at: iso(hbAgo), created_at: iso(86400000 * 120) }],
    agent_observation: { agent_version: '0.1.0-dev.20261104T195003Z.g8c74086afe9e', applied_generation: 3, heartbeat_at: iso(hbAgo), transport: i === 1 ? 'websocket' : 'direct',
      local_ssh_state: online ? 'healthy' : 'unknown', tunnel_state: online ? 'connected' : 'disconnected', persistence_backend: 'systemd', persistence_reboot_safe: true },
    hub_observation: { probe_state: online ? 'reachable' : 'unreachable', last_probe_at: iso(20000), last_success_at: iso(hbAgo) },
    commands: [], update: { available: false, current: '0.1.0', latest: '0.1.0-dev.20261104T195003Z.g8c74086afe9e' },
  }
}
function makeArthurFleet(calm) {
  machines = []
  arthurNames.forEach((slug, i) => {
    if (i === 0) machines.push(mk(i, slug, { hbAgo: 12000 }))
    else if (i === 1) machines.push(calm ? mk(i, slug, { hbAgo: 30000, distro: 'ubuntu 24.04' }) : mk(i, slug, { observed: 'offline', hbAgo: 86400000 * 1.2, distro: 'ubuntu 24.04' }))
    else if (i === 2) machines.push(mk(i, slug, { hbAgo: 20000, distro: 'ubuntu 22.04' }))
    else if (i === 9) machines.push(mk(i, slug, { desired: 'disabled', observed: 'offline', hbAgo: 86400000 * 90, distro: 'windows 10' }))
    else if (i >= 22) machines.push(mk(i, slug, calm
      ? { desired: 'disabled', observed: 'offline', hbAgo: 86400000 * 150 }
      : { desired: 'retiring', observed: 'offline', hbAgo: 86400000 * 150, changedAgo: 86400000 * 120 }))
    else machines.push(mk(i, slug, { observed: i % 3 ? 'gone' : 'offline', hbAgo: 86400000 * (20 + i * 4), distro: i % 4 ? 'ubuntu 22.04' : 'windows 10' }))
  })
  if (!calm) machines.push(mk(29, 'reach-old', { desired: 'retired', status: 'retired', observed: 'offline', hbAgo: 86400000 * 200 }))
}

function makePending() {
  pendingRows = []
  for (let i = 0; i < state.pending; i++) pendingRows.push(pendingRow(i))
}
function pendingRow(i, name) {
  // request 1 forges its source_ip to look like it came from home (P2-4)
  const meta = { target_user: i === 1 ? 'root' : 'kevin', distro: 'ubuntu 24.04', arch: 'amd64', source_ip: i === 1 ? '125.229.144.147' : `203.0.113.${10 + i}`, mode: 'system', slug: i === 1 ? 'jack' : 'kevin-laptop' }
  const row = { id: `r_pend${i}`, name: name || (['kevin-laptop', 'unknown-vps'][i % 2] + (i > 1 ? `-${i}` : '')), status: 'pending', metadata: meta, created_at: iso(60000 * (i + 1)), expires_at: iso(-3600000) }
  if (state.v2) Object.assign(row, { observed_ip: `203.0.113.${10 + i}`, target_user: meta.target_user, mode: 'system', local_port: 22, slug: i === 1 ? 'jack' : '' })
  return row
}
function requestsList(statusFilter) {
  const rows = [...pendingRows]
  if (statusFilter && state.v2) return rows.filter(r => r.status === statusFilter)
  for (let i = 0; i < 60; i++) rows.push({ id: `r_old${i}`, name: `old-${i}`, status: i % 3 ? 'provisioned' : 'denied', metadata: { distro: 'ubuntu 22.04', source_ip: '198.51.100.7', target_user: 'someone' }, created_at: iso(86400000 * i), expires_at: iso(86400000 * i - 3600000) })
  return rows
}

function resetAll(overrides = {}) {
  for (const c of sseClients || []) c.res.destroy()
  state = { ...DEFAULTS, ...overrides }
  stats = { counts: {}, inflight: 0, maxInflight: 0, log: [] }
  sseClients = new Set()
  eventLog = []
  eventId = 1000
  failures = []
  latencies = []
  sessionVersion = 0
  sseRefusedUntil = 0
  rejectedTokens = new Set()
  makeFleet(state.n)
  makePending()
}
resetAll()

// ---------------------------------------------------------------- events
function writeEvent(res, type, data, id) {
  res.write(`${id ? `id: ${id}\n` : ''}event: ${type}\ndata: ${JSON.stringify(data)}\n\n`)
}
/** Durable event: logged (replayable) and sent with an id. */
function publish(type, data) {
  const id = String(++eventId)
  eventLog.push({ id, type, data })
  if (eventLog.length > 20000) eventLog.splice(0, eventLog.length - 20000)
  for (const c of sseClients) if (c.live) writeEvent(c.res, type, data, id)
}
/** Live-only event (no id, never replayed). */
function publishLive(type, data) {
  for (const c of sseClients) if (c.live) writeEvent(c.res, type, data)
}
const broadcast = publish

// Heartbeats: 8c74086 stores them as durable events (with id); the 2026-11 server sends them
// live-only (no id line) and adds observed_state/status.
function heartbeat(m) {
  m.agent_observation.heartbeat_at = iso(0)
  if (state.hbfmt === 'new') {
    publishLive('agent.heartbeat', { machine_id: m.machine.id, heartbeat_at: m.agent_observation.heartbeat_at, observed_state: m.machine.observed_state, status: m.machine.status })
  } else publish('agent.heartbeat', { machine_id: m.machine.id, heartbeat_at: m.agent_observation.heartbeat_at })
}

function bySlug(slug) { return machines.find(m => m.machine.slug === slug) }
function byId(id) { return machines.find(m => m.machine.id === id) }

// ---------------------------------------------------------------- auth
const b64 = (o) => Buffer.from(JSON.stringify(o)).toString('base64url')
function issueToken(username) {
  return `${b64({ alg: 'none' })}.${b64({ sub: username, sv: sessionVersion, exp: Math.floor(Date.now() / 1000) + 3600 })}.mock`
}
function requestToken(req) {
  const m = /^Bearer (.+)$/.exec(req.headers.authorization || '')
  return m ? m[1] : ''
}
/** Reject one token (like reachd's per-token logout blacklist) and close its streams. */
function rejectToken(token, { close = true } = {}) {
  rejectedTokens.add(token)
  if (close) for (const c of [...sseClients]) if (c.token === token) { c.res.destroy(); sseClients.delete(c) }
}
function tokenOk(req) {
  if (!state.auth) return true
  const token = requestToken(req)
  if (!token || rejectedTokens.has(token)) return false
  try {
    const payload = JSON.parse(Buffer.from(token.split('.')[1], 'base64url').toString())
    return (payload.sv ?? 0) === sessionVersion
  } catch { return false }
}
function revokeAll({ close = true } = {}) {
  sessionVersion++
  if (close) for (const c of [...sseClients]) { c.res.destroy(); sseClients.delete(c) }
}

// ---------------------------------------------------------------- failure / latency injection
function takeRule(list, key) {
  const i = list.findIndex(r => r.key === key || (r.key.endsWith('*') && key.startsWith(r.key.slice(0, -1))))
  if (i < 0) return null
  const rule = list[i]
  if (rule.times !== undefined) {
    rule.times--
    if (rule.times <= 0) list.splice(i, 1)
  }
  return rule
}

// ---------------------------------------------------------------- storms
let stormTimer = null
function startStorm({ rate = 50, seconds = 10, kind = 'heartbeat' }) {
  stopStorm()
  const until = Date.now() + seconds * 1000
  const interval = Math.max(1, Math.round(1000 / rate))
  let k = 0
  stormTimer = setInterval(() => {
    if (Date.now() > until) return stopStorm()
    const perTick = Math.max(1, Math.round(rate * interval / 1000))
    for (let j = 0; j < perTick; j++) {
      const m = machines[k++ % machines.length]
      if (kind === 'heartbeat') heartbeat(m)
      else if (kind === 'flap') { // deploy/restart: state flaps + ssh_config.changed
        m.machine.observed_state = m.machine.observed_state === 'online' ? 'degraded' : 'online'
        const at = iso(0)
        m.hub_observation = { ...(m.hub_observation || {}), probe_state: 'reachable', last_probe_at: at }
        broadcast('machine.observed_changed', { machine_id: m.machine.id, observed_state: m.machine.observed_state, status: m.machine.status, ...(state.v2 ? { heartbeat_at: m.agent_observation.heartbeat_at, event_at: at } : {}) })
        broadcast('machine.' + m.machine.observed_state, { machine_id: m.machine.id, slug: m.machine.slug })
        broadcast('machine.health_changed', { machine_id: m.machine.id, observed_state: m.machine.observed_state, status: m.machine.status, probe_state: 'reachable', ...(state.v2 ? { last_probe_at: at, event_at: at } : {}) })
        if (j % 4 === 0) broadcast('ssh_config.changed', { machine_id: m.machine.id, reason: 'ssh_compat_changed' })
      }
    }
  }, interval)
}
function stopStorm() { if (stormTimer) clearInterval(stormTimer); stormTimer = null }
let realisticTimer = null

// ---------------------------------------------------------------- HTTP plumbing
function send(req, res, code, body, type = 'application/json') {
  let buf = Buffer.isBuffer(body) ? body : Buffer.from(typeof body === 'string' ? body : JSON.stringify(body))
  const headers = { 'Content-Type': type }
  if (state.gzip && /gzip/.test(req.headers['accept-encoding'] || '') && buf.length > 512) { buf = zlib.gzipSync(buf); headers['Content-Encoding'] = 'gzip' }
  headers['Content-Length'] = buf.length
  res.writeHead(code, headers); res.end(buf)
}
async function readBody(req) {
  const chunks = []
  for await (const c of req) chunks.push(c)
  const raw = Buffer.concat(chunks).toString()
  try { return raw ? JSON.parse(raw) : {} } catch { return {} }
}
function deepAssign(target, patch) {
  for (const [k, v] of Object.entries(patch || {})) {
    if (v && typeof v === 'object' && !Array.isArray(v) && target[k] && typeof target[k] === 'object') deepAssign(target[k], v)
    else target[k] = v
  }
}

// ---------------------------------------------------------------- control endpoints (/__*)
async function control(req, res, url) {
  const p = url.pathname
  const q = Object.fromEntries(url.searchParams)
  if (p === '/__stats') return send(req, res, 200, { ...stats, sse: sseClients.size, lastEventId: String(eventId), sessionVersion })
  if (p === '/__reset') { stats.counts = {}; stats.maxInflight = 0; stats.log = []; return send(req, res, 200, { ok: 1 }) }
  if (p === '/__resetall') { // everything back to defaults; query params override (scenario, hbfmt, v2, auth, pending, latency...)
    const o = {}
    for (const [k, v] of Object.entries(q)) {
      if (k === 'gzip' || k === 'v2' || k === 'auth') o[k] = v === '1'
      else if (k === 'scenario' || k === 'hbfmt') o[k] = v
      else if (k === 'latency') o.latencyMs = Number(v)
      else o[k] = Number(v)
    }
    stopStorm(); clearInterval(realisticTimer)
    resetAll(o)
    return send(req, res, 200, state)
  }
  if (p === '/__config') {
    for (const [k, v] of url.searchParams) {
      if (k === 'gzip' || k === 'v2' || k === 'auth') state[k] = v === '1'
      else if (k === 'latency') state.latencyMs = Number(v)
      else if (k === 'scenario' || k === 'hbfmt') state[k] = v
      else if (k === 'sse_delay') state.sseDelayMs = Number(v)
      else if (k === 'sse401_delay') state.sse401DelayMs = Number(v)
      else state[k] = Number(v)
    }
    if (url.searchParams.has('n') || url.searchParams.has('scenario')) makeFleet(state.n)
    if (url.searchParams.has('pending') || url.searchParams.has('v2')) makePending()
    return send(req, res, 200, state)
  }
  if (p === '/__fail') { // next `times` calls to `key` ("POST /api/admin/machines/:id/disable") fail
    failures.push({ key: q.key, status: Number(q.status || 500), error: q.error || 'injected failure', times: Number(q.times || 1) })
    return send(req, res, 200, { ok: 1 })
  }
  if (p === '/__latency') { // next `times` calls to `key` take `ms` (body still read on arrival)
    latencies.push({ key: q.key, ms: Number(q.ms || 1000), times: q.times ? Number(q.times) : 1 })
    return send(req, res, 200, { ok: 1 })
  }
  if (p === '/__mutate') { // change server state; optional events as reachd would publish them
    const b = await readBody(req)
    const m = b.slug ? bySlug(b.slug) : byId(b.id)
    if (!m) return send(req, res, 404, { error: 'no such machine' })
    if (b.machine) deepAssign(m.machine, b.machine)
    if (b.obs) deepAssign(m.agent_observation, b.obs)
    if (b.hub) m.hub_observation = { ...(m.hub_observation || {}), ...b.hub }
    for (const e of b.emit || []) {
      const data = { machine_id: m.machine.id, ...(e.data || {}) }
      if (e.live) publishLive(e.type, data)
      else publish(e.type, data)
    }
    return send(req, res, 200, m)
  }
  if (p === '/__hb') { // one heartbeat (stores heartbeat_at; publishes per HBFMT)
    const m = bySlug(q.slug)
    if (!m) return send(req, res, 404, { error: 'no such machine' })
    if (q.observed) { m.machine.observed_state = q.observed; m.machine.status = q.observed }
    heartbeat(m)
    return send(req, res, 200, { heartbeat_at: m.agent_observation.heartbeat_at })
  }
  if (p === '/__emit') { // raw event
    const b = await readBody(req)
    if (b.live) publishLive(b.type, b.data || {})
    else publish(b.type, b.data || {})
    return send(req, res, 200, { ok: 1, lastEventId: String(eventId) })
  }
  if (p === '/__newrequest') { // a new pending request + request.created
    const row = pendingRow(pendingRows.length, q.name || 'late-friend')
    row.id = `r_new${pendingRows.length}`
    pendingRows.push(row)
    publish('request.created', { request_id: row.id, name: row.name, status: 'pending' })
    return send(req, res, 200, row)
  }
  if (p === '/__reject-token') { const b = await readBody(req); rejectToken(b.token, { close: q.close !== '0' }); return send(req, res, 200, { ok: 1 }) }
  if (p === '/__revoke') { revokeAll({ close: q.close !== '0' }); return send(req, res, 200, { sessionVersion }) }
  if (p === '/__ssepause') { // refuse /events for ms (client reconnects later, with its cursor)
    sseRefusedUntil = Date.now() + Number(q.ms || 3000)
    for (const c of [...sseClients]) { c.res.destroy(); sseClients.delete(c) }
    return send(req, res, 200, { ok: 1 })
  }
  if (p === '/__storm') { startStorm({ rate: Number(q.rate || 50), seconds: Number(q.seconds || 10), kind: q.kind || 'heartbeat' }); return send(req, res, 200, { ok: 1 }) }
  if (p === '/__realistic') { // each online machine heartbeats every `period` ms (default 45s, compressible)
    clearInterval(realisticTimer); const period = Number(q.period || 45000)
    const online = machines.filter(m => m.machine.observed_state === 'online')
    const step = period / Math.max(1, online.length); let k = 0
    realisticTimer = setInterval(() => heartbeat(online[k++ % online.length]), step)
    return send(req, res, 200, { online: online.length, stepMs: step })
  }
  if (p === '/__stop') { stopStorm(); clearInterval(realisticTimer); return send(req, res, 200, { ok: 1 }) }
  if (p === '/__dropsse') { for (const c of [...sseClients]) { c.res.destroy(); sseClients.delete(c) } return send(req, res, 200, { ok: 1 }) }
  if (p === '/__pending') { state.pending = Number(q.n || 0); makePending(); publish('request.created', { request_id: 'r_new', name: 'kevin-laptop', status: 'pending' }); return send(req, res, 200, { ok: 1 }) }
  return send(req, res, 404, { error: 'unknown control' })
}

// ---------------------------------------------------------------- SSE
async function events(req, res) {
  if (!tokenOk(req)) { await sleep(state.sse401DelayMs || 0); return send(req, res, 401, { error: 'unauthorized' }) }
  if (Date.now() < sseRefusedUntil) return send(req, res, 503, { error: 'unavailable' })
  res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' })
  res.write('retry: 2000\n\n')
  const client = { res, live: false, token: requestToken(req) }
  sseClients.add(client)
  req.on('close', () => { clearInterval(client.ka); sseClients.delete(client) })
  if (state.sseDelayMs) await sleep(state.sseDelayMs) // events published now are not seen by this stream
  if (res.destroyed) return
  const cursorRaw = req.headers['last-event-id']
  if (cursorRaw) {
    const cursor = Number(cursorRaw)
    const oldest = eventLog.length ? Number(eventLog[0].id) : eventId + 1
    const replay = eventLog.filter(e => Number(e.id) > cursor)
    const limit = state.hbfmt === 'new' ? 5000 : 500
    if (!Number.isFinite(cursor) || cursor > eventId || cursor < oldest - 1 || replay.length > limit) {
      res.write(`id:\nevent: machine.resync_required\ndata: ${JSON.stringify({ reason: replay.length > limit ? 'replay_overflow' : 'cursor_out_of_range' })}\n\n`)
      res.end()
      sseClients.delete(client)
      return
    }
    for (const e of replay) writeEvent(res, e.type, e.data, e.id)
  }
  res.write(`event: hello\ndata: {"ok":true}\n\n`)
  client.live = true
  client.ka = setInterval(() => res.write(`: keepalive ${iso(0)}\n\n`), 25000)
}

// ---------------------------------------------------------------- admin API
function machineAction(m, action) {
  const mm = m.machine
  const now = iso(0)
  const set = (desired, status, extra = {}) => Object.assign(mm, { desired_state: desired, status, desired_changed_at: now, updated_at: now, desired_generation: mm.desired_generation + 1 }, extra)
  if (action === 'disable') { set('disabled', 'disabled', { cleanup_state: undefined }); publish('machine.desired_changed', { machine_id: mm.id, desired_state: 'disabled', generation: mm.desired_generation }); publish('machine.disabled', { machine_id: mm.id, slug: mm.slug }) }
  else if (action === 'enable') { set('active', mm.observed_state, { cleanup_state: undefined }); publish('machine.desired_changed', { machine_id: mm.id, desired_state: 'active', generation: mm.desired_generation }); publish('machine.enabled', { machine_id: mm.id, slug: mm.slug }) }
  else if (action === 'remove') { set('retiring', 'retiring', { cleanup_state: 'pending' }); publish('machine.desired_changed', { machine_id: mm.id, desired_state: 'retiring', generation: mm.desired_generation }); publish('machine.retiring', { machine_id: mm.id, slug: mm.slug }) }
  else if (action === 'retire-now') { set('retired', 'retired', { cleanup_state: 'complete' }); publish('machine.desired_changed', { machine_id: mm.id, desired_state: 'retired', generation: mm.desired_generation }); publish('machine.retired', { machine_id: mm.id, slug: mm.slug }) }
  else return false
  publish('ssh_config.changed', { machine_id: mm.id, reason: action })
  return true
}

async function api(req, res, url) {
  const p = url.pathname
  const key = `${req.method} ${p.replace(/\/m_[a-z0-9]+/, '/:id').replace(/\/r_[a-z0-9]+/, '/:id')}`
  const countKey = key + (url.search || '')
  stats.counts[countKey] = (stats.counts[countKey] || 0) + 1
  if (p === '/api/admin/events') return events(req, res)

  stats.inflight++; stats.maxInflight = Math.max(stats.maxInflight, stats.inflight)
  const body = req.method === 'POST' ? await readBody(req) : {}
  // Decide the response NOW (like a DB read at request time), send it after the latency.
  let reply
  const fail = takeRule(failures, key)
  if (fail) reply = [fail.status, { error: fail.error }]
  else if (p === '/api/admin/login') {
    reply = body.password === PASSWORD ? [200, { token: issueToken(body.username || 'arthur'), expires_in: 3600 }] : [401, { error: 'invalid credentials' }]
  } else if (!tokenOk(req)) reply = [401, { error: 'unauthorized' }]
  else reply = handle(req, p, url, key, body)
  // Snapshot the body now: the response must describe the state at read time, even if it is
  // sent after events that changed it (astra: the old mock read after the latency).
  reply = [reply[0], clone(reply[1])]
  const lat = takeRule(latencies, key)
  await sleep(lat ? lat.ms : state.latencyMs)
  stats.inflight--
  stats.log.push([Date.now(), countKey, reply[0], `${req.method} ${p}`])
  return send(req, res, reply[0], reply[1])
}

function handle(req, p, url, key, body) {
  if (p === '/api/admin/machines' && req.method === 'GET') {
    const all = url.searchParams.get('include_retired') === '1' || !state.v2
    return [200, machines.filter(m => all || m.machine.desired_state !== 'retired')]
  }
  if (p === '/api/admin/requests' && req.method === 'GET') return [200, requestsList(url.searchParams.get('status'))]
  const mm = p.match(/^\/api\/admin\/machines\/([^/]+)(?:\/(.*))?$/)
  if (mm) {
    const m = byId(mm[1])
    if (!m || (state.v2 && m.machine.desired_state === 'retired' && req.method === 'GET')) return [404, { error: 'machine not found' }]
    if (req.method === 'GET' && !mm[2]) return [200, m]
    if (req.method === 'POST' && machineAction(m, mm[2])) return [200, { status: m.machine.status, desired_state: m.machine.desired_state }]
    if (req.method === 'POST') return [200, { ok: true, machine: m.machine }]
    return [405, { error: 'method not allowed' }]
  }
  const rm = p.match(/^\/api\/admin\/requests\/([^/]+)\/(approve|deny)$/)
  if (rm && req.method === 'POST') {
    const row = pendingRows.find(r => r.id === rm[1] && r.status === 'pending')
    if (!row) return [404, { error: 'pending request not found' }]
    row.status = rm[2] === 'approve' ? 'approved' : 'denied'
    pendingRows = pendingRows.filter(r => r !== row)
    publish(`request.${row.status}`, { request_id: row.id })
    return [200, { request_id: row.id, status: row.status }]
  }
  if (p === '/api/admin/logout' && req.method === 'POST') {
    // reachd: blacklists the caller's token and closes ITS streams (other tokens unaffected)
    const token = requestToken(req)
    if (state.auth) queueMicrotask(() => rejectToken(token))
    return [200, { ok: true }]
  }
  if (p === '/api/admin/sessions/revoke-all' && req.method === 'POST') {
    if (!state.v2) return [404, { error: 'not found' }]
    queueMicrotask(() => revokeAll({ close: true }))
    return [200, { ok: true }]
  }
  return [404, { error: 'not found' }]
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, 'http://x')
  if (url.pathname.startsWith('/__')) return control(req, res, url)
  if (url.pathname.startsWith('/api/')) return api(req, res, url)
  // static SPA
  const p = url.pathname
  let file = path.join(SITE, decodeURIComponent(p))
  if (!file.startsWith(SITE) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) file = path.join(SITE, 'index.html')
  const ext = path.extname(file)
  const type = { '.js': 'application/javascript', '.css': 'text/css', '.html': 'text/html' }[ext] || 'application/octet-stream'
  send(req, res, 200, fs.readFileSync(file), type)
})
server.listen(PORT, '127.0.0.1', () => console.log(`mock reachd on http://127.0.0.1:${PORT} n=${state.n} scenario=${state.scenario} hbfmt=${state.hbfmt}`))
