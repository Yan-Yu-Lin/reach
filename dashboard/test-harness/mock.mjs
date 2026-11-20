// Zero-dependency mock of the reachd admin API (shapes copied from api.go/models.go @ 8c74086,
// plus the 2026-11 event/request changes from docs/EVENTS.md). Serves a dashboard bundle from
// SITE and fake data. No real tokens/secrets.
//
// Copied from ~/30-39-Research/34-tech-research/2026-11-20-reach-dashboard-shots/harness/ and extended:
//   SCENARIO=audit   the original 2026-11-20 mix (default; N machines)
//   SCENARIO=arthur  shaped like the real DB: 29 machines, 2 online, 1 dropped yesterday,
//                    9 stuck retiring, the rest dormant for weeks
//   SCENARIO=calm    nothing to do: 2 online, the rest dormant/disabled, 0 pending
//   HBFMT=old        agent.heartbeat with an id and only {machine_id, heartbeat_at} (8c74086)
//   HBFMT=new        live-only heartbeat: no id, + observed_state/status (docs/EVENTS.md)
//   V2=1             request rows carry observed_ip/slug/target_user; ?status=pending honoured
import http from 'node:http'
import fs from 'node:fs'
import path from 'node:path'
import zlib from 'node:zlib'

const SITE = process.env.SITE || '/tmp/reach-fe-audit/site'
const PORT = Number(process.env.PORT || 9388)
const state = {
  n: Number(process.env.N || 40),
  latencyMs: Number(process.env.LATENCY || 30),
  gzip: process.env.GZIP === '1',
  pending: Number(process.env.PENDING || 2),
  scenario: process.env.SCENARIO || 'audit',
  hbfmt: process.env.HBFMT || 'old',
  v2: process.env.V2 === '1',
}
const stats = { counts: {}, inflight: 0, maxInflight: 0, log: [] }
const sseClients = new Set()
let eventId = 1000

const names = ['omarchy', 'rtxws-colin', 'jack', 'ntut-vdi-win', 'gaming-pc', 'ntut-vpn-help', 'islabx6', 'mom-imac', 'dad-nas', 'pi-garage', 'lab-gpu-01', 'lab-gpu-02', 'vdi-ubuntu', 'friend-laptop', 'kevin-pc', 'school-ws', 'mac-mini', 'old-centos6', 'hotspot-box', 'arm-sbc']
const iso = (msAgo) => new Date(Date.now() - msAgo).toISOString().replace(/\.\d+Z$/, 'Z')
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
    else if (i === 2) machines.push(mk(i, slug, calm ? { hbAgo: 20000, distro: 'ubuntu 22.04' } : { observed: 'online', hbAgo: 20000, distro: 'ubuntu 22.04' }))
    else if (i === 9) machines.push(mk(i, slug, { desired: 'disabled', observed: 'offline', hbAgo: 86400000 * 90, distro: 'windows 10' }))
    else if (i >= 22) machines.push(mk(i, slug, calm
      ? { desired: 'disabled', observed: 'offline', hbAgo: 86400000 * 150 }
      : { desired: 'retiring', observed: 'offline', hbAgo: 86400000 * 150, changedAgo: 86400000 * 120 }))
    else machines.push(mk(i, slug, { observed: i % 3 ? 'gone' : 'offline', hbAgo: 86400000 * (20 + i * 4), distro: i % 4 ? 'ubuntu 22.04' : 'windows 10' }))
  })
  if (!calm) machines.push(mk(29, 'reach-old', { desired: 'retired', status: 'retired', observed: 'offline', hbAgo: 86400000 * 200 }))
}
makeFleet(state.n)

function requestsList(onlyPending) {
  const out = []
  for (let i = 0; i < state.pending; i++) {
    // request 1 forges its source_ip to look like it came from home (P2-4)
    const meta = { target_user: i === 1 ? 'root' : 'kevin', distro: 'ubuntu 24.04', arch: 'amd64', source_ip: i === 1 ? '125.229.144.147' : `203.0.113.${10 + i}`, mode: 'system', slug: i === 1 ? 'jack' : 'kevin-laptop' }
    const row = { id: `r_pend${i}`, name: ['kevin-laptop', 'unknown-vps'][i % 2] + (i > 1 ? `-${i}` : ''), status: 'pending', metadata: meta, created_at: iso(60000 * (i + 1)), expires_at: iso(-3600000) }
    if (state.v2) Object.assign(row, { observed_ip: `203.0.113.${10 + i}`, target_user: meta.target_user, mode: 'system', local_port: 22, slug: i === 1 ? 'jack' : '' })
    out.push(row)
  }
  if (onlyPending && state.v2) return out
  for (let i = 0; i < 60; i++) out.push({ id: `r_old${i}`, name: `old-${i}`, status: i % 3 ? 'provisioned' : 'denied', metadata: { distro: 'ubuntu 22.04', source_ip: '198.51.100.7', target_user: 'someone' }, created_at: iso(86400000 * i), expires_at: iso(86400000 * i - 3600000) })
  return out
}

function send(req, res, code, body, type = 'application/json') {
  let buf = Buffer.isBuffer(body) ? body : Buffer.from(typeof body === 'string' ? body : JSON.stringify(body))
  const headers = { 'Content-Type': type }
  if (state.gzip && /gzip/.test(req.headers['accept-encoding'] || '') && buf.length > 512) { buf = zlib.gzipSync(buf); headers['Content-Encoding'] = 'gzip' }
  headers['Content-Length'] = buf.length
  res.writeHead(code, headers); res.end(buf)
}
const sleep = (ms) => new Promise(r => setTimeout(r, ms))

function writeEvent(res, type, data, id = String(++eventId)) {
  res.write(`id: ${id}\nevent: ${type}\ndata: ${JSON.stringify(data)}\n\n`)
}
function broadcast(type, data) { const id = String(++eventId); for (const c of sseClients) writeEvent(c, type, data, id) }
// Heartbeats: 8c74086 stores them as durable events (with id); the 2026-11 server sends them
// live-only (no id line) and adds observed_state/status.
function heartbeat(m) {
  m.agent_observation.heartbeat_at = iso(0)
  if (state.hbfmt === 'new') {
    const data = { machine_id: m.machine.id, heartbeat_at: m.agent_observation.heartbeat_at, observed_state: m.machine.observed_state, status: m.machine.status }
    for (const c of sseClients) c.write(`event: agent.heartbeat\ndata: ${JSON.stringify(data)}\n\n`)
  } else broadcast('agent.heartbeat', { machine_id: m.machine.id, heartbeat_at: m.agent_observation.heartbeat_at })
}

// control: storm generators
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
      else if (kind === 'flap') { // deploy/restart: state flaps + ssh_config.changed (full refresh trigger)
        m.machine.observed_state = m.machine.observed_state === 'online' ? 'degraded' : 'online'
        broadcast('machine.observed_changed', { machine_id: m.machine.id, observed_state: m.machine.observed_state, status: m.machine.status })
        broadcast('machine.' + m.machine.observed_state, { machine_id: m.machine.id, slug: m.machine.slug })
        broadcast('machine.health_changed', { machine_id: m.machine.id, observed_state: m.machine.observed_state, status: m.machine.status, probe_state: 'reachable' })
        if (j % 4 === 0) broadcast('ssh_config.changed', { machine_id: m.machine.id, reason: 'ssh_compat_changed' })
      }
    }
  }, interval)
}
function stopStorm() { if (stormTimer) clearInterval(stormTimer); stormTimer = null }
let realisticTimer = null

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, 'http://x')
  const p = url.pathname
  if (p.startsWith('/__')) {
    if (p === '/__stats') return send(req, res, 200, { ...stats, sse: sseClients.size })
    if (p === '/__reset') { stats.counts = {}; stats.maxInflight = 0; stats.log = []; return send(req, res, 200, { ok: 1 }) }
    if (p === '/__config') { for (const [k, v] of url.searchParams) { if (k === 'gzip') state.gzip = v === '1'; else if (k === 'v2') state.v2 = v === '1'; else if (k === 'latency') state.latencyMs = Number(v); else if (k === 'scenario' || k === 'hbfmt') state[k] = v; else state[k] = Number(v) } if (url.searchParams.has('n') || url.searchParams.has('scenario')) makeFleet(state.n); return send(req, res, 200, state) }
    if (p === '/__storm') { startStorm({ rate: Number(url.searchParams.get('rate') || 50), seconds: Number(url.searchParams.get('seconds') || 10), kind: url.searchParams.get('kind') || 'heartbeat' }); return send(req, res, 200, { ok: 1 }) }
    if (p === '/__realistic') { // each online machine heartbeats every `period` ms (default 45s, compressible)
      clearInterval(realisticTimer); const period = Number(url.searchParams.get('period') || 45000)
      const online = machines.filter(m => m.machine.observed_state === 'online')
      const step = period / Math.max(1, online.length); let k = 0
      realisticTimer = setInterval(() => heartbeat(online[k++ % online.length]), step)
      return send(req, res, 200, { online: online.length, stepMs: step })
    }
    if (p === '/__stop') { stopStorm(); clearInterval(realisticTimer); return send(req, res, 200, { ok: 1 }) }
    if (p === '/__dropsse') { for (const c of sseClients) c.destroy(); return send(req, res, 200, { ok: 1 }) }
    if (p === '/__pending') { state.pending = Number(url.searchParams.get('n') || 0); broadcast('request.created', { request_id: 'r_new', name: 'kevin-laptop', status: 'pending' }); return send(req, res, 200, { ok: 1 }) }
  }
  if (p.startsWith('/api/')) {
    const key = p.replace(/\/m_[a-z0-9]+/, '/:id').replace(/\/r_[a-z0-9]+/, '/:id') + (url.search ? url.search : '')
    stats.counts[`${req.method} ${key}`] = (stats.counts[`${req.method} ${key}`] || 0) + 1
    if (p === '/api/admin/events') {
      res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' })
      res.write('retry: 2000\n\n')
      const last = Number(req.headers['last-event-id'] || 0)
      if (last && state.hbfmt !== 'new') { // replay (server caps at 500); new servers never replay heartbeats
        const n = Math.min(500, Math.max(0, eventId - last))
        for (let i = 0; i < n; i++) { const m = machines[i % machines.length]; writeEvent(res, 'agent.heartbeat', { machine_id: m.machine.id, heartbeat_at: iso(0) }, String(last + i + 1)) }
      }
      res.write(`event: hello\ndata: {"ok":true}\n\n`)
      sseClients.add(res)
      const ka = setInterval(() => res.write(`: keepalive ${iso(0)}\n\n`), 25000)
      req.on('close', () => { clearInterval(ka); sseClients.delete(res) })
      return
    }
    stats.inflight++; stats.maxInflight = Math.max(stats.maxInflight, stats.inflight)
    await sleep(state.latencyMs)
    stats.inflight--
    stats.log.push([Date.now(), `${req.method} ${key}`])
    if (p === '/api/admin/machines') return send(req, res, 200, machines)
    if (p === '/api/admin/requests') return send(req, res, 200, requestsList(url.searchParams.get('status') === 'pending'))
    const mm = p.match(/^\/api\/admin\/machines\/([^/]+)(?:\/(.*))?$/)
    if (mm) {
      const m = machines.find(x => x.machine.id === mm[1]); if (!m) return send(req, res, 404, { error: 'machine not found' })
      if (req.method === 'GET') return send(req, res, 200, m)
      return send(req, res, 200, { ok: true, machine: m.machine })
    }
    if (p.startsWith('/api/admin/requests/')) return send(req, res, 200, { ok: true })
    if (p === '/api/admin/logout') return send(req, res, 200, { ok: true })
    if (p === '/api/admin/login') return send(req, res, 401, { error: 'invalid credentials' })
    return send(req, res, 404, { error: 'not found' })
  }
  // static SPA
  let file = path.join(SITE, decodeURIComponent(p))
  if (!file.startsWith(SITE) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) file = path.join(SITE, 'index.html')
  const ext = path.extname(file)
  const type = { '.js': 'application/javascript', '.css': 'text/css', '.html': 'text/html' }[ext] || 'application/octet-stream'
  send(req, res, 200, fs.readFileSync(file), type)
})
server.listen(PORT, '127.0.0.1', () => console.log(`mock reachd on http://127.0.0.1:${PORT} n=${state.n}`))
