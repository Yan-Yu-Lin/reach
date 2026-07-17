<template>
  <div class="app-shell" :data-tick="timeTick">
    <!-- Topbar -->
    <header class="topbar">
      <div class="topbar-left">
        <span class="brand">Reach</span>
        <div class="sse-badge">
          <span class="sse-dot" :class="sseStatusClass" />
          <span>{{ sseLabel }}</span>
        </div>
      </div>
      <div class="topbar-right">
        <button class="btn btn-ghost btn-sm" @click="toggleTheme" :title="`Switch to ${isDark ? 'light' : 'dark'} mode`">
          {{ isDark ? 'Light' : 'Dark' }}
        </button>
        <button class="btn btn-ghost btn-sm" @click="showShortcuts = true" title="Keyboard shortcuts (?)">?</button>
        <button class="btn btn-ghost btn-sm" @click="logout">Logout</button>
      </div>
    </header>

    <!-- Fleet strip -->
    <aside class="fleet-strip" ref="stripRef">
      <div class="fleet-filter" v-if="!isMobile">
        <input
          ref="filterInput"
          v-model="filter"
          placeholder="Filter machines..."
          @keydown.escape="filter = ''; ($event.target as HTMLInputElement).blur()"
        />
      </div>

      <template v-for="(group, gi) in filteredGroups" :key="group.label">
        <div class="fleet-group-label">{{ group.label }}</div>

        <template v-if="group.collapsed && !expandedGroups.has(group.label)">
          <div class="fleet-collapsed-group" @click="expandedGroups.add(group.label)">
            {{ group.machines.length }} {{ group.label.toLowerCase() }}
          </div>
        </template>
        <template v-else>
          <div
            v-for="(item, i) in group.machines"
            :key="item.machine.id"
            class="fleet-item"
            :class="{
              active: selectedId === item.machine.id,
              focused: focusedIndex === flatIndex(gi, i),
            }"
            @click="selectMachine(item.machine.id)"
          >
            <span class="fleet-dot" :class="dotClass(item)" />
            <span class="fleet-name">{{ item.machine.slug }}</span>
            <span class="fleet-time">{{ rt.relative(item.agent_observation?.heartbeat_at) }}</span>
            <button
              class="fleet-copy-btn"
              @click.stop="copySSH(item.machine.slug)"
              title="Copy SSH command"
            >cp</button>
          </div>
        </template>
      </template>

      <div v-if="filteredGroups.length === 0 && machines.length > 0" class="empty-state" style="padding: 16px;">
        <div class="muted">No matches</div>
      </div>
    </aside>

    <!-- Stage -->
    <main class="stage" ref="stageRef">
      <div v-if="loading && machines.length === 0" class="load-state" role="status">
        Loading dashboard data...
      </div>
      <div v-if="fleetError" class="load-state error" role="alert">
        <span>{{ fleetError }}</span>
        <button class="btn btn-sm" :disabled="loading" @click="fleet.refresh">
          {{ loading ? 'Retrying...' : 'Retry' }}
        </button>
      </div>

      <!-- Summary (no selection, not settings) -->
      <template v-if="!selectedId && !showSettings">
        <div class="summary-stats">
          <div class="stat-card accent">
            <div class="stat-value">{{ onlineCount }}</div>
            <div class="stat-label">Reachable</div>
          </div>
          <div class="stat-card" :class="degradedCount > 0 ? 'warn' : ''">
            <div class="stat-value">{{ degradedCount }}</div>
            <div class="stat-label">Degraded</div>
          </div>
          <div class="stat-card" :class="offlineCount > 0 ? 'danger' : ''">
            <div class="stat-value">{{ offlineCount }}</div>
            <div class="stat-label">Offline</div>
          </div>
          <div class="stat-card" :class="pendingCount > 0 ? 'warn' : ''">
            <div class="stat-value">{{ pendingCount }}</div>
            <div class="stat-label">Pending</div>
          </div>
        </div>

        <!-- Pending approvals -->
        <section class="section" v-if="pendingReqs.length > 0">
          <div class="section-title">Pending approvals</div>
          <div
            v-for="req in pendingReqs"
            :key="req.id"
            class="approval-card"
          >
            <div class="approval-header">
              <span class="approval-name">{{ req.name }}</span>
              <span class="approval-time">{{ rt.relative(req.created_at) }}</span>
            </div>
            <div class="approval-meta">
              <div class="approval-meta-item" v-if="req.metadata?.target_user">
                <div class="approval-meta-label">Target user</div>
                <div>{{ req.metadata.target_user }}</div>
              </div>
              <div class="approval-meta-item" v-if="req.metadata?.distro">
                <div class="approval-meta-label">Distro / arch</div>
                <div>{{ req.metadata.distro }} {{ req.metadata.arch ? '/ ' + req.metadata.arch : '' }}</div>
              </div>
              <div class="approval-meta-item" v-if="req.metadata?.source_ip">
                <div class="approval-meta-label">Source IP</div>
                <div class="mono">{{ req.metadata.source_ip }}</div>
              </div>
              <div class="approval-meta-item" v-if="req.metadata?.mode">
                <div class="approval-meta-label">Mode</div>
                <div>{{ req.metadata.mode }}</div>
              </div>
            </div>
            <div class="approval-warnings" v-if="getWarnings(req).length > 0">
              <span class="warning-badge" v-for="w in getWarnings(req)" :key="w">{{ w }}</span>
            </div>
            <div class="approval-actions">
              <button class="btn btn-approve" @click="approveRequest(req.id)">Approve</button>
              <button class="btn" @click="approveAs(req)">Approve as...</button>
              <button class="btn btn-danger" @click="denyRequest(req.id)">Deny</button>
            </div>
          </div>
        </section>

        <!-- Recently changed -->
        <section class="section" v-if="recentlyChanged.length > 0">
          <div class="section-title">Recently changed</div>
          <div class="recent-list">
            <div
              v-for="item in recentlyChanged"
              :key="item.machine.id"
              class="recent-item"
              @click="selectMachine(item.machine.id)"
            >
              <span class="fleet-dot" :class="dotClass(item)" />
              <span class="fleet-name">{{ item.machine.slug }}</span>
              <span class="pill" :class="item.machine.observed_state">{{ item.machine.observed_state }}</span>
              <span class="fleet-time" style="margin-left:auto">{{ rt.relative(item.machine.updated_at || item.machine.created_at) }}</span>
            </div>
          </div>
        </section>

        <div v-if="machines.length === 0 && !loading" class="empty-state">
          <div class="empty-state-icon">~</div>
          <div>No machines yet. Run the setup script on a target to get started.</div>
        </div>
      </template>

      <!-- Machine detail -->
      <MachineDetail
        v-if="selectedId && !showSettings"
        :item="selectedMachine"
        :events="machineEvents"
        @back="selectedId = null"
        @action="handleMachineAction"
        @copy="copyText"
        @refresh="fleet.refreshMachine"
      />

      <!-- Settings (mobile tab or could be linked from topbar) -->
      <template v-if="showSettings">
        <div class="section">
          <div class="section-title">Setup</div>
          <div class="detail-card" style="margin-bottom:16px">
            <div class="detail-card-title">Setup one-liner</div>
            <div class="ssh-block" style="margin-bottom:0">
              <code class="ssh-cmd" style="font-size:13px;word-break:break-all">{{ setupCommand }}</code>
              <button class="btn btn-sm" @click="copyText(setupCommand)">Copy</button>
            </div>
          </div>
          <div class="detail-card" style="margin-bottom:16px">
            <div class="detail-card-title">Setup (no sudo)</div>
            <div class="ssh-block" style="margin-bottom:0">
              <code class="ssh-cmd" style="font-size:13px;word-break:break-all">{{ websocketSetupCommand }}</code>
              <button class="btn btn-sm" @click="copyText(websocketSetupCommand)">Copy</button>
            </div>
          </div>
        </div>
        <div class="section">
          <div class="section-title">Hub</div>
          <div class="detail-card" style="margin-bottom:16px">
            <dl>
              <div class="detail-row"><dt>Host</dt><dd>{{ reachHostLabel }}</dd></div>
              <div class="detail-row"><dt>Machines</dt><dd>{{ machines.length }}</dd></div>
              <div class="detail-row">
                <dt>SSE</dt>
                <dd>
                  <span class="sse-dot" :class="sseStatusClass" style="display:inline-block;vertical-align:middle" />
                  {{ sseLabel }}
                </dd>
              </div>
            </dl>
          </div>
        </div>
        <div class="section">
          <button class="btn btn-danger" @click="logout" style="width:100%">Logout</button>
        </div>
      </template>
    </main>

    <!-- Mobile tabs -->
    <nav class="mobile-tabs">
      <button class="mobile-tab" :class="{ active: mobileTab === 'inbox' }" @click="mobileTab = 'inbox'; selectedId = null">
        <span>{{ pendingCount > 0 ? `(${pendingCount})` : '' }} Inbox</span>
      </button>
      <button class="mobile-tab" :class="{ active: mobileTab === 'machines' }" @click="mobileTab = 'machines'">
        <span>Machines</span>
      </button>
      <button class="mobile-tab" :class="{ active: mobileTab === 'settings' }" @click="mobileTab = 'settings'">
        <span>Settings</span>
      </button>
    </nav>

    <!-- Toast container -->
    <div class="toast-container">
      <div v-for="t in toasts" :key="t.id" class="toast" :class="t.type">
        {{ t.message }}
      </div>
    </div>

    <!-- Keyboard shortcuts overlay -->
    <div v-if="showShortcuts" class="shortcuts-overlay" @click.self="showShortcuts = false">
      <div class="shortcuts-card">
        <h2>Keyboard shortcuts</h2>
        <div class="shortcut-row"><span>Navigate fleet</span><span class="shortcut-key">j</span> / <span class="shortcut-key">k</span></div>
        <div class="shortcut-row"><span>Open selected</span><span class="shortcut-key">Enter</span></div>
        <div class="shortcut-row"><span>Back to summary</span><span class="shortcut-key">Esc</span></div>
        <div class="shortcut-row"><span>Copy SSH command</span><span class="shortcut-key">c</span></div>
        <div class="shortcut-row"><span>Approve first pending</span><span class="shortcut-key">a</span></div>
        <div class="shortcut-row"><span>Filter fleet</span><span class="shortcut-key">/</span></div>
        <div class="shortcut-row"><span>Show this dialog</span><span class="shortcut-key">?</span></div>
        <div style="margin-top: 16px; text-align: right;">
          <button class="btn btn-sm" @click="showShortcuts = false">Close</button>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
const api = useReachApi()
const sse = useSSE()
const fleet = useFleet()
const rt = useRelativeTime()
const toast = useToast()
const themeCtrl = useTheme()
const runtimeConfig = useRuntimeConfig()

const reachBaseUrl = computed(() => {
  const configured = String(runtimeConfig.public.reachBaseUrl || '').replace(/\/$/, '')
  if (configured) return configured
  if (import.meta.client) return window.location.origin
  return 'https://tunnels.your-domain.example'
})
const setupCommand = computed(() => `curl -fsSL ${reachBaseUrl.value}/setup.sh | bash -s -- --yes`)
const websocketSetupCommand = computed(() => `${setupCommand.value} --transport websocket`)
const reachHostLabel = computed(() => {
  try { return new URL(reachBaseUrl.value).host } catch { return reachBaseUrl.value }
})

// Unwrap fleet refs for clean template access
const {
  machines, requests, loading, error: fleetError, selectedId,
  pendingRequests: pendingReqs, pendingCount, onlineCount,
  degradedCount, offlineCount, grouped, selectedMachine,
  recentlyChanged,
} = fleet

const toasts = toast.toasts

const filter = ref('')
const filterInput = ref<HTMLInputElement | null>(null)
const stripRef = ref<HTMLElement | null>(null)
const stageRef = ref<HTMLElement | null>(null)
const showShortcuts = ref(false)
const mobileTab = ref<'inbox' | 'machines' | 'settings'>('machines')
const expandedGroups = reactive(new Set<string>())
const focusedIndex = ref(-1)
const machineEvents = ref<any[]>([])
const isMobile = ref(false)
const isDark = computed(() => themeCtrl.resolved() === 'dark')
const timeTick = ref(0)
const showSettings = computed(() => isMobile.value && mobileTab.value === 'settings' && !selectedId.value)

const sseStatusClass = computed(() => sse.status.value)
const sseLabel = computed(() => {
  switch (sse.status.value) {
    case 'live': return 'Live'
    case 'reconnecting': return 'Reconnecting...'
    case 'stale': return 'Stale'
    default: return 'Connecting...'
  }
})

const filteredGroups = computed(() => {
  const q = filter.value.toLowerCase().trim()
  if (!q) return grouped.value
  return grouped.value
    .map(g => ({
      ...g,
      machines: g.machines.filter(m => m.machine.slug.toLowerCase().includes(q)),
    }))
    .filter(g => g.machines.length > 0)
})

const flatMachines = computed(() => {
  const result: MachineWithTunnels[] = []
  for (const g of filteredGroups.value) {
    if (g.collapsed && !expandedGroups.has(g.label)) continue
    result.push(...g.machines)
  }
  return result
})

function flatIndex(groupIdx: number, itemIdx: number): number {
  let idx = 0
  for (let g = 0; g < groupIdx; g++) {
    const group = filteredGroups.value[g]
    if (!group) continue
    if (group.collapsed && !expandedGroups.has(group.label)) continue
    idx += group.machines.length
  }
  return idx + itemIdx
}

function dotClass(item: MachineWithTunnels): string {
  const m = item.machine
  if (m.desired_state === 'disabled' || m.desired_state === 'retired') return m.desired_state
  if (m.status === 'pending') return 'pending'
  return m.observed_state || 'unknown'
}

function selectMachine(id: string) {
  selectedId.value = id
  focusedIndex.value = flatMachines.value.findIndex(m => m.machine.id === id)
}

function toggleTheme() {
  themeCtrl.toggle()
}

async function approveRequest(id: string) {
  try {
    await api.approve(id)
    toast.success('Request approved')
    await fleet.refresh()
  } catch (e: any) {
    toast.error(e.message || 'Approve failed')
  }
}

async function approveAs(req: RequestRow) {
  const name = prompt('Approve as (new name):', req.name)
  if (!name || name.trim() === '') return
  // The API approve endpoint doesn't support rename — approve first, then the client
  // will provision with the original name. For now, approve normally.
  // TODO: Add rename-on-approve to the Go API.
  await approveRequest(req.id)
}

async function denyRequest(id: string) {
  if (!confirm('Deny this request?')) return
  try {
    await api.deny(id)
    toast.success('Request denied')
    await fleet.refresh()
  } catch (e: any) {
    toast.error(e.message || 'Deny failed')
  }
}

async function handleMachineAction(action: string, id: string) {
  try {
    if (action === 'enable') await api.enable(id)
    else if (action === 'disable') await api.disable(id)
    else if (action === 'remove') await api.remove(id)
    toast.success(`Machine ${action}d`)
    await fleet.refresh()
  } catch (e: any) {
    toast.error(e.message || `${action} failed`)
  }
}

async function copyText(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    toast.success('Copied!')
  } catch {
    toast.error('Copy failed')
  }
}

function copySSH(slug: string) {
  copyText(`ssh ${slug}`)
}

function getWarnings(req: RequestRow): string[] {
  const warnings: string[] = []
  if (req.metadata?.target_user === 'root') warnings.push('root user')
  if (machines.value.some(m => m.machine.slug === req.metadata?.slug)) {
    warnings.push('duplicate name')
  }
  return warnings
}

async function logout() {
  fleet.dispose()
  try {
    await api.logout()
  } catch {
    // Still clear the local token if the network request fails.
  }
  api.setToken(null)
  navigateTo('/login')
}

// Keyboard shortcuts
function onKeydown(e: KeyboardEvent) {
  if (showShortcuts.value) {
    if (e.key === 'Escape' || e.key === '?') {
      showShortcuts.value = false
      e.preventDefault()
    }
    return
  }

  const tag = (e.target as HTMLElement).tagName
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return

  switch (e.key) {
    case 'j': {
      e.preventDefault()
      const max = flatMachines.value.length - 1
      if (max < 0) return
      focusedIndex.value = Math.min(focusedIndex.value + 1, max)
      break
    }
    case 'k': {
      e.preventDefault()
      if (flatMachines.value.length === 0) return
      focusedIndex.value = Math.max(focusedIndex.value - 1, 0)
      break
    }
    case 'Enter': {
      e.preventDefault()
      const m = flatMachines.value[focusedIndex.value]
      if (m) selectMachine(m.machine.id)
      break
    }
    case 'Escape': {
      e.preventDefault()
      selectedId.value = null
      break
    }
    case 'c': {
      e.preventDefault()
      const sel = selectedMachine.value
      if (sel) copySSH(sel.machine.slug)
      else {
        const f = flatMachines.value[focusedIndex.value]
        if (f) copySSH(f.machine.slug)
      }
      break
    }
    case 'a': {
      e.preventDefault()
      const first = pendingReqs.value[0]
      if (first) approveRequest(first.id)
      break
    }
    case '/': {
      e.preventDefault()
      filterInput.value?.focus()
      break
    }
    case '?': {
      e.preventDefault()
      showShortcuts.value = true
      break
    }
  }
}

let timeInterval: ReturnType<typeof setInterval> | undefined

onMounted(async () => {
  fleet.setupSSE()
  await fleet.refresh()
  document.addEventListener('keydown', onKeydown)
  checkMobile()
  window.addEventListener('resize', checkMobile)
  timeInterval = setInterval(() => timeTick.value++, 15_000)

  if ('Notification' in window && Notification.permission === 'default') {
    Notification.requestPermission()
  }
})

onBeforeUnmount(() => {
  fleet.dispose()
  document.removeEventListener('keydown', onKeydown)
  window.removeEventListener('resize', checkMobile)
  if (timeInterval) clearInterval(timeInterval)
})

function checkMobile() {
  isMobile.value = window.innerWidth < 769
}
</script>
