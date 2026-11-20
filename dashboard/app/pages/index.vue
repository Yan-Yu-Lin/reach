<template>
  <div class="shell" :class="{ 'has-detail': !!selectedId && tab === 'machines' }">
    <!-- Top bar: brand, live state, desktop nav, settings -->
    <header class="topbar">
      <button type="button" class="brand" @click="goHome">Reach</button>
      <span class="live" :class="`live-${sse.status.value}`" role="status">
        <span class="live-dot" aria-hidden="true" />
        <span>{{ sseLabel }}</span>
      </span>
      <nav class="topnav" aria-label="Sections">
        <button type="button" class="topnav-item" :aria-current="tab === 'machines' ? 'page' : undefined" @click="setTab('machines')">Machines</button>
        <button type="button" class="topnav-item" :aria-current="tab === 'requests' ? 'page' : undefined" @click="setTab('requests')">
          Requests<span v-if="pendingCount" class="count-badge">{{ pendingCount }}</span>
        </button>
        <button type="button" class="topnav-item" :aria-current="tab === 'settings' ? 'page' : undefined" @click="setTab('settings')">Settings &amp; setup</button>
      </nav>
      <button type="button" class="btn btn-icon topbar-theme" :aria-label="isDark ? 'Switch to light theme' : 'Switch to dark theme'" @click="themeCtrl.toggle()">
        <AppIcon :name="isDark ? 'sun' : 'moon'" :size="18" />
      </button>
    </header>

    <!-- Desktop sidebar: the fleet is always visible -->
    <aside v-if="isDesktop" class="sidebar" aria-label="Machines">
      <FleetList
        ref="sidebarList"
        :groups="groups"
        :status-of="statusOf"
        :selected-id="selectedId"
        :focused-id="focusedId"
        @select="openMachine"
        @copy="copyText"
        @bulk-disable="askBulkDisable"
      />
    </aside>

    <main ref="mainEl" class="main" tabindex="-1">
      <div v-if="!loaded && loading" class="notice" role="status">Loading machines...</div>
      <div v-if="fleetError" class="notice notice-error" role="alert">
        <span>{{ fleetError }}</span>
        <button type="button" class="btn" :disabled="loading" @click="fleet.refresh">{{ loading ? 'Retrying...' : 'Retry' }}</button>
      </div>

      <!-- MACHINES -->
      <template v-if="tab === 'machines'">
        <MachineDetail
          v-if="selectedId"
          :item="selectedMachine"
          :base-url="reachBaseUrl"
          @back="closeMachine"
          @action="handleMachineAction"
          @copy="copyText"
          @refresh="fleet.refreshMachine"
        />
        <template v-else>
          <!-- What needs me right now -->
          <section v-if="loaded" class="todo" :class="todoItems.length ? 'todo-busy' : 'todo-calm'" aria-labelledby="todo-title">
            <h1 id="todo-title" class="todo-title">
              <AppIcon :name="todoItems.length ? 'alert' : 'check'" :size="20" />
              {{ todoItems.length ? 'Needs you' : 'All good' }}
            </h1>
            <p v-if="!todoItems.length" class="todo-calm-text">
              {{ counts.online }} online{{ counts.dormant ? `, ${counts.dormant} dormant` : '' }}. Nothing to approve, nothing recently dropped.
            </p>
            <ul v-else class="todo-list">
              <li v-for="t in todoItems" :key="t.key">
                <button type="button" class="todo-item" :class="`st-${t.status}`" @click="t.go()">
                  <StatusIcon :status="t.status" :size="16" />
                  <span>{{ t.text }}</span>
                  <AppIcon name="chevron" :size="18" class="todo-chevron" />
                </button>
              </li>
            </ul>
          </section>

          <!-- Desktop-only overview cards; the sidebar already lists the fleet -->
          <section v-if="loaded" class="stats" aria-label="Fleet summary">
            <div v-for="s in statCards" :key="s.status" class="stat" :class="`st-${s.status}`">
              <StatusIcon :status="s.status" :size="16" />
              <span class="stat-num">{{ s.count }}</span>
              <span class="stat-label">{{ s.label }}</span>
            </div>
          </section>

          <section v-if="loaded && pending.length" class="desk-only" aria-labelledby="desk-req">
            <h2 id="desk-req" class="section-title">Waiting for approval</h2>
            <RequestCard
              v-for="req in pending"
              :key="req.id"
              :req="req"
              :existing-slugs="existingSlugs"
              @approve="askApprove"
              @deny="askDeny"
            />
          </section>

          <!-- Phone: the fleet list is the home screen -->
          <div v-if="!isDesktop" class="mobile-only">
            <FleetList
              :groups="groups"
              :status-of="statusOf"
              :selected-id="selectedId"
              @select="openMachine"
              @copy="copyText"
              @bulk-disable="askBulkDisable"
            />
          </div>

          <p v-if="loaded && machines.length === 0" class="empty">No machines yet. Open Settings &amp; setup to copy the install command.</p>
        </template>
      </template>

      <!-- REQUESTS -->
      <template v-else-if="tab === 'requests'">
        <h1 class="page-title">Requests</h1>
        <p class="page-lede">Someone ran the setup command and is asking to join. Check that it is really them before approving.</p>
        <RequestCard
          v-for="req in pending"
          :key="req.id"
          :req="req"
          :existing-slugs="existingSlugs"
          @approve="askApprove"
          @deny="askDeny"
        />
        <p v-if="loaded && pending.length === 0" class="empty">No requests waiting.</p>
      </template>

      <!-- SETTINGS -->
      <template v-else>
        <h1 class="page-title">Settings &amp; setup</h1>

        <section class="card" aria-labelledby="setup-title">
          <h2 id="setup-title" class="card-title">Add a machine</h2>
          <p class="muted small">Run one of these on the new machine. It finds sudo on its own and falls back to a user-only install when there is none. Then approve the request here.</p>
          <div v-for="c in setupCommands" :key="c.label" class="cmd-copy">
            <span class="cmd-copy-label">{{ c.label }}</span>
            <span v-if="c.hint" class="muted small">{{ c.hint }}</span>
            <div class="cmd-copy-row">
              <code class="mono">{{ c.cmd }}</code>
              <button type="button" class="btn btn-icon" :aria-label="`Copy: ${c.label}`" @click="copyText(c.cmd)"><AppIcon name="copy" :size="18" /></button>
            </div>
          </div>
        </section>

        <section class="card" aria-labelledby="notif-title">
          <h2 id="notif-title" class="card-title">Notifications</h2>
          <p class="muted small">Get a browser notification when a new request arrives while this page is open.</p>
          <div class="field-row">
            <button v-if="notifState === 'default'" type="button" class="btn" @click="enableNotifications"><AppIcon name="bell" :size="18" /> Turn on notifications</button>
            <span v-else-if="notifState === 'granted'" class="tag tag-ok">On for this browser</span>
            <span v-else-if="notifState === 'denied'" class="tag tag-warn">Blocked. Allow it in the browser's site settings.</span>
            <span v-else class="tag tag-muted">Not supported here. On iPhone, add the page to the Home Screen first.</span>
          </div>
        </section>

        <section class="card" aria-labelledby="look-title">
          <h2 id="look-title" class="card-title">Appearance</h2>
          <div class="segmented" role="radiogroup" aria-label="Theme">
            <button v-for="t in themes" :key="t.value" type="button" role="radio" :aria-checked="themeCtrl.theme.value === t.value" class="segmented-item" @click="themeCtrl.setTheme(t.value)">{{ t.label }}</button>
          </div>
        </section>

        <section class="card" aria-labelledby="hub-title">
          <h2 id="hub-title" class="card-title">Hub</h2>
          <dl class="kv">
            <div><dt>Host</dt><dd class="mono">{{ reachHostLabel }}</dd></div>
            <div><dt>Machines</dt><dd>{{ machines.length }}</dd></div>
            <div><dt>Live updates</dt><dd>{{ sseLabel }}</dd></div>
          </dl>
        </section>

        <section class="card" aria-labelledby="sess-title">
          <h2 id="sess-title" class="card-title">Session</h2>
          <p class="muted small">Your login is stored in this browser. If you lost a device or used a shared computer, sign out everywhere: every dashboard login stops working at once.</p>
          <div class="field-row">
            <button type="button" class="btn" @click="logout"><AppIcon name="logout" :size="18" /> Sign out</button>
            <button type="button" class="btn btn-warn" @click="dialog = { kind: 'revoke' }"><AppIcon name="shield" :size="18" /> Sign out everywhere</button>
          </div>
        </section>

        <section class="card desk-only" aria-labelledby="keys-title">
          <h2 id="keys-title" class="card-title">Keyboard shortcuts</h2>
          <dl class="kv kv-keys">
            <div><dt><kbd>j</kbd> <kbd>k</kbd></dt><dd>Move through machines</dd></div>
            <div><dt><kbd>Enter</kbd></dt><dd>Open machine</dd></div>
            <div><dt><kbd>Esc</kbd></dt><dd>Back</dd></div>
            <div><dt><kbd>c</kbd></dt><dd>Copy ssh command</dd></div>
            <div><dt><kbd>a</kbd></dt><dd>Review the first request (asks before approving)</dd></div>
            <div><dt><kbd>/</kbd></dt><dd>Find a machine</dd></div>
          </dl>
        </section>
      </template>
    </main>

    <!-- Phone bottom tabs, inside thumb reach -->
    <nav class="tabbar" aria-label="Sections">
      <button type="button" class="tab" :aria-current="tab === 'machines' ? 'page' : undefined" @click="setTab('machines')">
        <AppIcon name="list" :size="22" />
        <span>Machines</span>
        <span v-if="attentionCount" class="tab-dot" :aria-label="`${attentionCount} need attention`">{{ attentionCount }}</span>
      </button>
      <button type="button" class="tab" :aria-current="tab === 'requests' ? 'page' : undefined" @click="setTab('requests')">
        <AppIcon name="inbox" :size="22" />
        <span>Requests</span>
        <span v-if="pendingCount" class="tab-dot tab-dot-pending" :aria-label="`${pendingCount} waiting`">{{ pendingCount }}</span>
      </button>
      <button type="button" class="tab" :aria-current="tab === 'settings' ? 'page' : undefined" @click="setTab('settings')">
        <AppIcon name="settings" :size="22" />
        <span>Settings</span>
      </button>
    </nav>

    <div class="toasts" role="status" aria-live="polite">
      <div v-for="t in toasts" :key="t.id" class="toast" :class="t.type">{{ t.message }}</div>
    </div>

    <!-- Request dialogs -->
    <ConfirmDialog
      v-if="dialog?.kind === 'approve'"
      :title="`Approve ${dialog.req.name}?`"
      confirm-label="Approve"
      :busy="dialogBusy"
      @cancel="dialog = null"
      @confirm="approve(dialog.req)"
    >
      <p>It gets a tunnel account on the hub and shows up on your Mac as
        <span class="mono">ssh {{ requestSlug(dialog.req).value || '(name chosen at setup)' }}</span>.</p>
      <ul v-if="requestWarnings(dialog.req, existingSlugs).length" class="dialog-warnings">
        <li v-for="w in requestWarnings(dialog.req, existingSlugs)" :key="w"><AppIcon name="alert" :size="16" /> {{ w }}</li>
      </ul>
      <p class="muted small">Only approve if you know who sent this. Source IP and system details are what the requester claims.</p>
    </ConfirmDialog>
    <ConfirmDialog
      v-if="dialog?.kind === 'deny'"
      :title="`Deny ${dialog.req.name}?`"
      confirm-label="Deny"
      danger
      :busy="dialogBusy"
      @cancel="dialog = null"
      @confirm="deny(dialog.req)"
    >
      <p>The setup on that machine stops. They can send a new request later.</p>
    </ConfirmDialog>
    <ConfirmDialog
      v-if="dialog?.kind === 'bulk-disable'"
      :title="`Disable ${dialog.items.length} dormant machines?`"
      :confirm-label="`Disable ${dialog.items.length}`"
      danger
      :busy="dialogBusy"
      @cancel="dialog = null"
      @confirm="bulkDisable(dialog.items)"
    >
      <p>Their tunnel keys stop working, so a stolen copy of an old agent config is useless. Nothing is deleted; you can Enable any of them later.</p>
      <p class="mono small wrap">{{ dialog.items.map(i => i.machine.slug).join(', ') }}</p>
    </ConfirmDialog>
    <ConfirmDialog
      v-if="dialog?.kind === 'revoke'"
      title="Sign out everywhere?"
      confirm-label="Sign out everywhere"
      danger
      :busy="dialogBusy"
      @cancel="dialog = null"
      @confirm="revokeAll"
    >
      <p>Every dashboard login, including this one, stops working. You will need your password to sign in again.</p>
      <p class="muted small">The Mac ssh-config agent keeps working.</p>
    </ConfirmDialog>
  </div>
</template>

<script setup lang="ts">
import { setupCommand, websocketSetupCommand, windowsSetupCommand } from '~/utils/commands'
import { requestSlug, requestWarnings } from '~/utils/requests'
import { STATUS_LABEL, type UiStatus } from '~/utils/status'
import { mapSettledLimited } from '~/utils/concurrency'

type Tab = 'machines' | 'requests' | 'settings'
type Dialog =
  | { kind: 'approve'; req: RequestRow }
  | { kind: 'deny'; req: RequestRow }
  | { kind: 'bulk-disable'; items: MachineWithTunnels[] }
  | { kind: 'revoke' }

const api = useReachApi()
const sse = useSSE()
const fleet = useFleet()
const toast = useToast()
const themeCtrl = useTheme()
const runtimeConfig = useRuntimeConfig()
const route = useRoute()
const router = useRouter()
useClock()

const {
  machines, pending, loading, loaded, error: fleetError, selectedId, selectedMachine,
  statusOf, groups, counts, attentionCount, pendingCount,
} = fleet
const toasts = toast.toasts

const mainEl = ref<HTMLElement | null>(null)
const sidebarList = ref<{ focusFilter: () => void; flat: MachineWithTunnels[] } | null>(null)
const focusedId = ref<string | null>(null)
const dialog = ref<Dialog | null>(null)
const dialogBusy = ref(false)
const isDark = ref(true)
// Render one fleet list, not two: the sidebar on desktop, the home list on phones.
const desktopQuery = import.meta.client ? window.matchMedia('(min-width: 900px)') : null
const isDesktop = ref(!!desktopQuery?.matches)
const syncDesktop = () => { isDesktop.value = !!desktopQuery?.matches }
const notifState = ref<'default' | 'granted' | 'denied' | 'unsupported'>('unsupported')

const themes = [
  { value: 'system', label: 'System' },
  { value: 'light', label: 'Light' },
  { value: 'dark', label: 'Dark' },
] as const

// ---- routing: tab and selected machine live in the URL so the phone back button works
const tab = computed<Tab>(() => {
  const t = route.query.tab
  return t === 'requests' || t === 'settings' ? t : 'machines'
})
watch(() => route.query.m, (m) => {
  selectedId.value = typeof m === 'string' && m ? m : null
}, { immediate: true })

function setTab(next: Tab) {
  if (next === tab.value && !selectedId.value) return
  router.push({ query: next === 'machines' ? {} : { tab: next } })
}
function goHome() { setTab('machines') }
function openMachine(id: string) {
  focusedId.value = id
  if (selectedId.value === id && tab.value === 'machines') return
  router.push({ query: { m: id } })
}
function closeMachine() {
  if (window.history.state?.back) router.back()
  else router.replace({ query: {} })
}
watch([tab, selectedId], () => nextTick(() => mainEl.value?.scrollTo?.({ top: 0 })))

// ---- derived view data
const reachBaseUrl = computed(() => {
  const configured = String(runtimeConfig.public.reachBaseUrl || '').replace(/\/$/, '')
  if (configured) return configured
  if (import.meta.client) return window.location.origin
  return 'https://tunnels.your-domain.example'
})
const reachHostLabel = computed(() => {
  try { return new URL(reachBaseUrl.value).host } catch { return reachBaseUrl.value }
})
const setupCommands = computed(() => [
  { label: 'Linux / macOS', hint: '', cmd: setupCommand(reachBaseUrl.value) },
  { label: 'Linux / macOS on a network that blocks SSH', hint: 'Tunnels over HTTPS (WebSocket) instead of SSH port 443.', cmd: websocketSetupCommand(reachBaseUrl.value) },
  { label: 'Windows (PowerShell)', hint: 'Non-admin PowerShell installs for the current user only.', cmd: windowsSetupCommand(reachBaseUrl.value) },
])

const existingSlugs = computed(() => new Set(machines.value.map(m => m.machine.slug)))

const sseLabel = computed(() => {
  switch (sse.status.value) {
    case 'live': return 'Live'
    case 'reconnecting': return 'Reconnecting'
    case 'stale': return 'Stale'
    default: return 'Connecting'
  }
})

const statCards = computed(() => {
  const c = counts.value
  const list: Array<{ status: UiStatus; count: number; label: string }> = [
    { status: 'online', count: c.online, label: STATUS_LABEL.online },
    { status: 'degraded', count: c.degraded, label: STATUS_LABEL.degraded },
    { status: 'offline', count: c.offline, label: 'Offline < 7d' },
    { status: 'dormant', count: c.dormant, label: STATUS_LABEL.dormant },
    { status: 'disabled', count: c.disabled, label: STATUS_LABEL.disabled },
  ]
  if (c.stuck + c.failed > 0) list.push({ status: 'stuck', count: c.stuck + c.failed, label: 'Removal stuck' })
  return list
})

const todoItems = computed(() => {
  const out: Array<{ key: string; status: UiStatus; text: string; go: () => void }> = []
  const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`
  if (pendingCount.value) {
    out.push({ key: 'req', status: 'pending', text: `${plural(pendingCount.value, 'request', 'requests')} waiting for approval`, go: () => setTab('requests') })
  }
  const attention = groups.value.find(g => g.key === 'attention')?.items || []
  const byStatus = (s: UiStatus[]) => attention.filter(i => s.includes(statusOf.value.get(i.machine.id) || 'unknown'))
  const sections: Array<{ s: UiStatus[]; icon: UiStatus; one: string; many: string }> = [
    { s: ['offline'], icon: 'offline', one: 'machine dropped offline', many: 'machines dropped offline' },
    { s: ['degraded'], icon: 'degraded', one: 'machine degraded', many: 'machines degraded' },
    { s: ['stuck', 'failed'], icon: 'stuck', one: 'removal stuck', many: 'removals stuck' },
  ]
  for (const sec of sections) {
    const items = byStatus(sec.s)
    if (!items.length) continue
    const names = items.slice(0, 3).map(i => i.machine.slug).join(', ') + (items.length > 3 ? '...' : '')
    out.push({
      key: sec.icon,
      status: sec.icon,
      text: `${plural(items.length, sec.one, sec.many)}: ${names}`,
      go: () => items.length === 1 ? openMachine(items[0]!.machine.id) : focusGroup('attention'),
    })
  }
  return out
})

function focusGroup(key: string) {
  nextTick(() => document.querySelector<HTMLElement>(`.main .fgroup-${key}, .sidebar .fgroup-${key}`)?.scrollIntoView({ block: 'start', behavior: 'smooth' }))
}

// ---- actions
async function copyText(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    toast.success(`Copied: ${text.length > 40 ? text.slice(0, 40) + '...' : text}`)
  } catch {
    toast.error('Copy failed. Long-press the command to copy it.')
  }
}

function askApprove(req: RequestRow) { dialog.value = { kind: 'approve', req } }
function askDeny(req: RequestRow) { dialog.value = { kind: 'deny', req } }
function askBulkDisable(items: MachineWithTunnels[]) { if (items.length) dialog.value = { kind: 'bulk-disable', items } }

async function withDialog(fn: () => Promise<void>) {
  dialogBusy.value = true
  try {
    await fn()
    dialog.value = null
  } finally {
    dialogBusy.value = false
  }
}

function approve(req: RequestRow) {
  return withDialog(async () => {
    try {
      await api.approve(req.id)
      toast.success(`Approved ${req.name}. Setup on that machine continues by itself.`)
    } catch (e: any) {
      toast.error(e.message || 'Approve failed')
    }
    await fleet.refreshRequests()
  })
}

function deny(req: RequestRow) {
  return withDialog(async () => {
    try {
      await api.deny(req.id)
      toast.success(`Denied ${req.name}`)
    } catch (e: any) {
      toast.error(e.message || 'Deny failed')
    }
    await fleet.refreshRequests()
  })
}

function bulkDisable(items: MachineWithTunnels[]) {
  return withDialog(async () => {
    const results = await mapSettledLimited(items, 2, item => api.disable(item.machine.id))
    const failed = results.filter(r => r.status === 'rejected').length
    if (failed) toast.error(`${failed} of ${items.length} could not be disabled`)
    else toast.success(`Disabled ${items.length} dormant machines`)
    await fleet.refresh()
  })
}

async function revokeAll() {
  await withDialog(async () => {
    try {
      await api.revokeAllSessions(false)
    } catch (e: any) {
      if (e?.status === 404) {
        toast.error('This hub version cannot do that yet. Update reachd first.')
        return
      }
      if (e?.status !== 401) {
        toast.error(e.message || 'Could not sign out everywhere')
        return
      }
    }
    fleet.dispose()
    api.setToken(null)
    await navigateTo('/login')
  })
}

const ACTION_DONE: Record<string, string> = {
  disable: 'Disabled', enable: 'Enabled', remove: 'Removal started', 'retire-now': 'Retired',
}

async function handleMachineAction(action: 'disable' | 'enable' | 'remove' | 'retire-now', id: string, done: () => void) {
  try {
    if (action === 'enable') await api.enable(id)
    else if (action === 'disable') await api.disable(id)
    else if (action === 'remove') await api.remove(id)
    else await api.retireNow(id)
    toast.success(ACTION_DONE[action] || 'Done')
    if (action === 'retire-now') {
      await fleet.refresh()
      closeMachine()
    } else {
      await fleet.refreshMachine(id)
    }
  } catch (e: any) {
    toast.error(e.message || `${action} failed`)
    void fleet.refreshMachine(id)
  } finally {
    done()
  }
}

async function enableNotifications() {
  if (!('Notification' in window)) return
  try {
    notifState.value = await Notification.requestPermission()
  } catch {
    notifState.value = Notification.permission
  }
}

async function logout() {
  fleet.dispose()
  try { await api.logout() } catch { /* clear locally anyway */ }
  api.setToken(null)
  navigateTo('/login')
}

// ---- keyboard (desktop)
function onKeydown(e: KeyboardEvent) {
  if (dialog.value || e.metaKey || e.ctrlKey || e.altKey) return
  const el = e.target as HTMLElement
  if (el.closest('input, textarea, select, [contenteditable="true"], .dialog')) return
  const flat = sidebarList.value?.flat || []
  const idx = flat.findIndex(m => m.machine.id === focusedId.value)

  switch (e.key) {
    case 'j':
    case 'k': {
      e.preventDefault()
      if (!flat.length) return
      const next = e.key === 'j' ? Math.min(idx + 1, flat.length - 1) : Math.max(idx - 1, 0)
      focusedId.value = flat[next]!.machine.id
      nextTick(() => document.querySelector('.sidebar .mrow.focused')?.scrollIntoView({ block: 'nearest' }))
      break
    }
    case 'Enter':
      if (focusedId.value && el.tagName !== 'BUTTON') { e.preventDefault(); openMachine(focusedId.value) }
      break
    case 'Escape':
      if (selectedId.value) { e.preventDefault(); closeMachine() }
      break
    case 'c': {
      const target = selectedMachine.value || flat[idx]
      if (target) { e.preventDefault(); copyText(`ssh ${target.machine.slug}`) }
      break
    }
    case 'a': {
      // Never approves directly: opens the same confirmation as the button.
      const first = pending.value[0]
      if (first) { e.preventDefault(); askApprove(first) }
      break
    }
    case '/':
      e.preventDefault()
      sidebarList.value?.focusFilter()
      break
  }
}

let mq: MediaQueryList | null = null
const syncDark = () => { isDark.value = themeCtrl.resolved() === 'dark' }
watch(() => themeCtrl.theme.value, syncDark)

onMounted(() => {
  // One snapshot on load, started before the stream so the stream's first hello can skip it.
  void fleet.refresh().catch(() => {})
  fleet.setupSSE()
  document.addEventListener('keydown', onKeydown)
  mq = window.matchMedia('(prefers-color-scheme: dark)')
  mq.addEventListener('change', syncDark)
  desktopQuery?.addEventListener('change', syncDesktop)
  syncDark()
  notifState.value = 'Notification' in window ? Notification.permission : 'unsupported'
})

onBeforeUnmount(() => {
  fleet.dispose()
  document.removeEventListener('keydown', onKeydown)
  mq?.removeEventListener('change', syncDark)
  desktopQuery?.removeEventListener('change', syncDesktop)
})
</script>
