<template>
  <div v-if="item">
    <!-- Header -->
    <div class="detail-header">
      <div class="detail-title">
        <button class="btn btn-ghost btn-sm" @click="$emit('back')">
          &larr; Back
        </button>
        <span class="fleet-dot" :class="dotClass" style="width:10px;height:10px" />
        <span class="detail-slug">{{ item.machine.slug }}</span>
        <span class="pill" :class="item.machine.observed_state">{{ item.machine.observed_state }}</span>
      </div>
      <div class="detail-actions">
        <button class="btn btn-sm" @click="$emit('action', 'enable', item.machine.id)" v-if="item.machine.desired_state === 'disabled'">Enable</button>
        <button class="btn btn-sm btn-danger" @click="confirmDisable" v-if="item.machine.desired_state === 'active'">Disable</button>
        <button class="btn btn-sm btn-danger" @click="confirmRemove">Remove</button>
      </div>
    </div>

    <!-- SSH command block -->
    <div class="ssh-block">
      <code class="ssh-cmd">ssh {{ item.machine.slug }}</code>
      <button class="btn btn-sm btn-primary" @click="$emit('copy', `ssh ${item.machine.slug}`)">Copy</button>
    </div>

    <!-- Connection chain -->
    <div class="conn-chain">
      <div class="chain-node">
        <div class="chain-node-label">Your Mac</div>
        <div class="chain-node-sub">ssh</div>
      </div>
      <div class="chain-segment">
        <div class="chain-line ok" />
      </div>
      <div class="chain-node">
        <div class="chain-node-label">{{ hubLabel }}:{{ hubPort }}</div>
        <div class="chain-node-sub">ProxyJump</div>
      </div>
      <div class="chain-segment">
        <div class="chain-line" :class="hubProbeOk ? 'ok' : 'fail'" />
        <div class="chain-status" :class="hubProbeOk ? 'ok' : 'fail'">{{ hubProbeOk ? 'OK' : 'Fail' }}</div>
      </div>
      <div class="chain-node">
        <div class="chain-node-label">{{ tunnelUser }}</div>
        <div class="chain-node-sub">R:{{ tunnelPort }}</div>
      </div>
      <div class="chain-segment">
        <div class="chain-line" :class="tunnelOk ? 'ok' : 'fail'" />
        <div class="chain-status" :class="tunnelOk ? 'ok' : 'fail'">{{ tunnelOk ? 'OK' : tunnelState }}</div>
      </div>
      <div class="chain-node">
        <div class="chain-node-label">target sshd</div>
        <div class="chain-node-sub">:{{ item.machine.local_port || 22 }}</div>
      </div>
    </div>

    <!-- Detail grid -->
    <div class="detail-grid">
      <!-- Agent diagnostics -->
      <div class="detail-card">
        <div class="detail-card-title">Agent diagnostics</div>
        <dl>
          <div class="detail-row">
            <dt>Agent version</dt>
            <dd>
              {{ obs?.agent_version || '—' }}
              <span v-if="item.update?.latest" class="muted" style="margin-left:4px">latest {{ item.update.latest }}</span>
            </dd>
          </div>
          <div class="detail-row">
            <dt>OS / distro</dt>
            <dd>{{ item.machine.distro || '—' }} / {{ item.machine.arch || '—' }}</dd>
          </div>
          <div class="detail-row">
            <dt>Transport</dt>
            <dd>{{ obs?.transport || '—' }}</dd>
          </div>
          <div class="detail-row">
            <dt>Persistence</dt>
            <dd>{{ obs?.persistence_backend || '—' }} {{ obs?.persistence_reboot_safe ? '(reboot-safe)' : '' }}</dd>
          </div>
          <div class="detail-row">
            <dt>Last heartbeat</dt>
            <dd>{{ rt.relative(obs?.heartbeat_at) }}</dd>
          </div>
          <div class="detail-row">
            <dt>Local SSH</dt>
            <dd>
              <span class="pill" :class="localSSHClass">{{ obs?.local_ssh_state || '—' }}</span>
              <span v-if="obs?.local_ssh_error" class="muted" style="margin-left:4px">{{ obs.local_ssh_error }}</span>
            </dd>
          </div>
          <div class="detail-row">
            <dt>Tunnel state</dt>
            <dd>
              <span class="pill" :class="tunnelStateClass">{{ obs?.tunnel_state || '—' }}</span>
              <span v-if="obs?.tunnel_error" class="muted" style="margin-left:4px">{{ obs.tunnel_error }}</span>
            </dd>
          </div>
          <div class="detail-row" v-if="obs?.last_error">
            <dt>Last error</dt>
            <dd style="color:var(--danger)">{{ obs.last_error }}</dd>
          </div>
        </dl>
      </div>

      <!-- Machine info -->
      <div class="detail-card">
        <div class="detail-card-title">Machine info</div>
        <dl>
          <div class="detail-row">
            <dt>ID</dt>
            <dd class="mono">{{ item.machine.id }}</dd>
          </div>
          <div class="detail-row">
            <dt>Desired state</dt>
            <dd><span class="pill" :class="item.machine.desired_state">{{ item.machine.desired_state }}</span></dd>
          </div>
          <div class="detail-row">
            <dt>Effective status</dt>
            <dd><span class="pill" :class="item.machine.status">{{ item.machine.status }}</span></dd>
          </div>
          <div class="detail-row">
            <dt>Generation</dt>
            <dd>{{ item.machine.desired_generation }}</dd>
          </div>
          <div class="detail-row">
            <dt>Update policy</dt>
            <dd>
              <select v-model="updatePolicy" class="process-title-input" style="max-width:150px" :disabled="updatePolicySaving" @change="saveUpdatePolicy">
                <option value="manual">Manual</option>
                <option value="auto">Auto-update</option>
                <option value="disabled">Disabled</option>
              </select>
            </dd>
          </div>
          <div class="detail-row">
            <dt>Target user</dt>
            <dd>{{ item.machine.target_user || 'root' }}</dd>
          </div>
          <div class="detail-row">
            <dt>Mode</dt>
            <dd>{{ item.machine.mode || 'system' }}</dd>
          </div>
          <div class="detail-row">
            <dt>Local port</dt>
            <dd>{{ item.machine.local_port }}</dd>
          </div>
          <div class="detail-row">
            <dt>Created</dt>
            <dd>{{ rt.fullTime(item.machine.created_at) }}</dd>
          </div>
          <div class="detail-row" v-if="item.machine.expires_at">
            <dt>Expires</dt>
            <dd>{{ rt.fullTime(item.machine.expires_at) }}</dd>
          </div>
        </dl>
      </div>

      <!-- Process title -->
      <div class="detail-card wide">
        <div class="detail-card-title">Process title message</div>
        <div class="muted" style="font-size:12px;margin-bottom:12px">
          Controls what reach-agent displays in <span class="mono">ps aux</span>. Leave blank and clear to use the agent's local config/default.
        </div>
        <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-bottom:12px">
          <label style="display:flex;flex-direction:column;gap:6px;font-size:13px">
            <span class="muted">Single message</span>
            <input v-model="processTitleSingle" class="process-title-input" placeholder="Reach-Agent" />
          </label>
          <label style="display:flex;flex-direction:column;gap:6px;font-size:13px">
            <span class="muted">Rotate interval</span>
            <input v-model="processTitleInterval" class="process-title-input" placeholder="5s" />
          </label>
        </div>
        <label style="display:flex;flex-direction:column;gap:6px;font-size:13px;margin-bottom:12px">
          <span class="muted">Rotating messages (one per line, overrides single message)</span>
          <textarea v-model="processTitleList" class="process-title-textarea" rows="4" placeholder="Reach-Agent\n今天也要開心 🚀\nTunnel vibes only" />
        </label>
        <div v-if="processTitleError" style="color:var(--danger);font-size:13px;margin-bottom:8px">{{ processTitleError }}</div>
        <div style="display:flex;gap:8px;align-items:center">
          <button class="btn btn-primary" :disabled="processTitleSaving" @click="saveProcessTitle">{{ processTitleSaving ? 'Saving...' : 'Save message' }}</button>
          <button class="btn" :disabled="processTitleSaving" @click="clearProcessTitle">Clear dashboard override</button>
          <span class="muted" style="font-size:12px">{{ processTitleSource }}</span>
        </div>
      </div>

      <!-- Tunnel info -->
      <div class="detail-card" v-if="item.tunnels.length > 0">
        <div class="detail-card-title">Tunnel</div>
        <dl v-for="t in item.tunnels" :key="t.id">
          <div class="detail-row">
            <dt>Hub</dt>
            <dd>{{ t.hub_id }}</dd>
          </div>
          <div class="detail-row">
            <dt>Unix user</dt>
            <dd class="mono">{{ t.original_unix_user || t.unix_user }}</dd>
          </div>
          <div class="detail-row">
            <dt>Remote port</dt>
            <dd>{{ t.original_remote_port || t.remote_port }}</dd>
          </div>
          <div class="detail-row">
            <dt>Status</dt>
            <dd><span class="pill" :class="t.status">{{ t.status }}</span></dd>
          </div>
          <div class="detail-row">
            <dt>Last seen</dt>
            <dd>{{ rt.relative(t.last_seen_at) }}</dd>
          </div>
        </dl>
      </div>

      <!-- Hub probe -->
      <div class="detail-card" v-if="item.hub_observation">
        <div class="detail-card-title">Hub probe</div>
        <dl>
          <div class="detail-row">
            <dt>Probe state</dt>
            <dd><span class="pill" :class="item.hub_observation.probe_state">{{ item.hub_observation.probe_state }}</span></dd>
          </div>
          <div class="detail-row">
            <dt>Last probe</dt>
            <dd>{{ rt.relative(item.hub_observation.last_probe_at) }}</dd>
          </div>
          <div class="detail-row" v-if="item.hub_observation.last_success_at">
            <dt>Last success</dt>
            <dd>{{ rt.relative(item.hub_observation.last_success_at) }}</dd>
          </div>
          <div class="detail-row" v-if="item.hub_observation.probe_error">
            <dt>Error</dt>
            <dd style="color:var(--danger)">{{ item.hub_observation.probe_error }}</dd>
          </div>
        </dl>
      </div>

      <!-- Actions -->
      <div class="detail-card">
        <div class="detail-card-title">Actions</div>
        <div style="display:flex;flex-direction:column;gap:8px">
          <button class="btn" @click="$emit('copy', `ssh ${item.machine.slug}`)">Copy SSH command</button>
          <button class="btn btn-primary" :disabled="!canQueueUpdate || updateSaving" @click="queueUpdateAgent">
            {{ updateSaving ? 'Queueing...' : updateButtonLabel }}
          </button>
          <button class="btn" @click="$emit('copy', repairCommand)">Copy repair command</button>
          <button class="btn" @click="$emit('copy', uninstallCommand)">Copy uninstall command</button>
          <div style="border-top:1px solid var(--border);padding-top:8px;margin-top:4px">
            <button class="btn btn-danger" style="width:100%" @click="confirmRemove">Remove machine</button>
          </div>
        </div>
      </div>

      <!-- Commands -->
      <div class="detail-card" v-if="item.commands && item.commands.length > 0">
        <div class="detail-card-title">Agent commands</div>
        <dl v-for="cmd in item.commands" :key="cmd.id">
          <div class="detail-row">
            <dt>{{ cmd.type }}</dt>
            <dd>
              <span class="pill" :class="cmd.status">{{ cmd.status }}</span>
              <span class="muted" style="margin-left:4px">{{ rt.relative(cmd.created_at) }}</span>
            </dd>
          </div>
        </dl>
      </div>
    </div>
  </div>
  <div v-else class="empty-state">
    <div>Machine not found or loading...</div>
  </div>
</template>

<script setup lang="ts">
const props = defineProps<{
  item: MachineWithTunnels | null
  events: any[]
}>()

const emit = defineEmits<{
  back: []
  action: [action: string, id: string]
  copy: [text: string]
  refresh: [id: string]
}>()

const rt = useRelativeTime()
const api = useReachApi()
const toast = useToast()

const obs = computed(() => props.item?.agent_observation)
const processTitleSingle = ref('')
const processTitleList = ref('')
const processTitleInterval = ref('5s')
const processTitleSaving = ref(false)
const processTitleError = ref('')
const updateSaving = ref(false)
const updatePolicy = ref<'manual' | 'auto' | 'disabled'>('manual')
const updatePolicySaving = ref(false)

const processTitleSource = computed(() => props.item?.machine.process_title_config ? 'Dashboard override active' : 'Using agent local config/default')

watch(() => props.item?.machine.id, () => { resetProcessTitleForm(); resetUpdatePolicyForm() }, { immediate: true })
watch(() => props.item?.machine.process_title_config, () => resetProcessTitleForm())
watch(() => props.item?.machine.update_policy, () => resetUpdatePolicyForm())

function resetProcessTitleForm() {
  const cfg = props.item?.machine.process_title_config
  processTitleSingle.value = cfg?.process_title || ''
  processTitleList.value = (cfg?.process_titles || []).join('\n')
  processTitleInterval.value = cfg?.rotate_interval || '5s'
  processTitleError.value = ''
}

function resetUpdatePolicyForm() {
  updatePolicy.value = props.item?.machine.update_policy || 'manual'
}

function buildProcessTitleConfig(): ProcessTitleConfig | null {
  const titles = processTitleList.value
    .split('\n')
    .map(s => s.trim())
    .filter(Boolean)
  const single = processTitleSingle.value.trim()
  const interval = processTitleInterval.value.trim()
  if (!titles.length && !single) return null
  const cfg: ProcessTitleConfig = {}
  if (titles.length) cfg.process_titles = titles
  else cfg.process_title = single
  if (interval) cfg.rotate_interval = interval
  return cfg
}

async function saveProcessTitle() {
  if (!props.item) return
  processTitleSaving.value = true
  processTitleError.value = ''
  try {
    await api.setProcessTitle(props.item.machine.id, buildProcessTitleConfig())
    toast.success('Process title saved')
    emit('refresh', props.item.machine.id)
  } catch (e: any) {
    processTitleError.value = e.message || 'Failed to save process title'
    toast.error(processTitleError.value)
  } finally {
    processTitleSaving.value = false
  }
}

async function clearProcessTitle() {
  if (!props.item) return
  processTitleSaving.value = true
  processTitleError.value = ''
  try {
    await api.setProcessTitle(props.item.machine.id, null)
    toast.success('Process title override cleared')
    emit('refresh', props.item.machine.id)
  } catch (e: any) {
    processTitleError.value = e.message || 'Failed to clear process title'
    toast.error(processTitleError.value)
  } finally {
    processTitleSaving.value = false
  }
}

async function saveUpdatePolicy() {
  if (!props.item) return
  updatePolicySaving.value = true
  try {
    await api.setUpdatePolicy(props.item.machine.id, updatePolicy.value)
    toast.success('Update policy saved')
    emit('refresh', props.item.machine.id)
  } catch (e: any) {
    toast.error(e.message || 'Failed to save update policy')
    resetUpdatePolicyForm()
  } finally {
    updatePolicySaving.value = false
  }
}

const updateCommandOpen = computed(() => (props.item?.commands || []).some(cmd => cmd.type === 'update_agent' && (cmd.status === 'pending' || cmd.status === 'sent')))
const canQueueUpdate = computed(() => !!props.item && !!props.item.update?.latest && props.item.update.available && props.item.machine.update_policy !== 'disabled' && !updateCommandOpen.value && props.item.machine.desired_state !== 'retired' && props.item.machine.desired_state !== 'retiring')
const updateButtonLabel = computed(() => {
  if (updateCommandOpen.value) return 'Agent update queued'
  if (!props.item?.update?.latest) return 'No update version configured'
  if (props.item.machine.update_policy === 'disabled') return 'Updates disabled'
  if (props.item.update.available === false) return 'Agent is latest'
  return `Update Agent to ${props.item.update.latest}`
})

async function queueUpdateAgent() {
  if (!props.item || !canQueueUpdate.value) return
  updateSaving.value = true
  try {
    await api.updateAgent(props.item.machine.id)
    toast.success('Agent update queued')
    emit('refresh', props.item.machine.id)
  } catch (e: any) {
    toast.error(e.message || 'Failed to queue agent update')
  } finally {
    updateSaving.value = false
  }
}

const dotClass = computed(() => {
  if (!props.item) return 'unknown'
  const m = props.item.machine
  if (m.desired_state === 'disabled' || m.desired_state === 'retired') return m.desired_state
  return m.observed_state || 'unknown'
})

const hubLabel = computed(() => {
  const tunnel = props.item?.tunnels[0]
  return tunnel?.hub_id || 'hub'
})

const hubPort = computed(() => '443')

const tunnelUser = computed(() => {
  const tunnel = props.item?.tunnels[0]
  return tunnel?.original_unix_user || tunnel?.unix_user || 'rt-???'
})

const tunnelPort = computed(() => {
  const tunnel = props.item?.tunnels[0]
  return tunnel?.original_remote_port || tunnel?.remote_port || '?'
})

const hubProbeOk = computed(() => {
  if (!props.item?.hub_observation) return true
  return props.item.hub_observation.probe_state === 'reachable'
})

const tunnelOk = computed(() => {
  const state = obs.value?.tunnel_state
  return state === 'connected' || state === 'running'
})

const tunnelState = computed(() => obs.value?.tunnel_state || 'unknown')

const localSSHClass = computed(() => {
  const state = obs.value?.local_ssh_state
  if (state === 'running' || state === 'ok' || state === 'healthy') return 'online'
  if (state === 'stopped' || state === 'error' || state === 'down') return 'offline'
  return 'unknown'
})

const tunnelStateClass = computed(() => {
  const state = obs.value?.tunnel_state
  if (state === 'connected' || state === 'running') return 'online'
  if (state === 'disconnected' || state === 'error' || state === 'failed') return 'offline'
  return 'unknown'
})

const runtimeConfig = useRuntimeConfig()
const reachBaseUrl = computed(() => {
  const configured = String(runtimeConfig.public.reachBaseUrl || '').replace(/\/$/, '')
  if (configured) return configured
  if (import.meta.client) return window.location.origin
  return 'https://tunnels.your-domain.example'
})

const repairCommand = computed(() => {
  if (!props.item) return ''
  return `curl -fsSL ${reachBaseUrl.value}/setup.sh | sudo sh -s -- --repair`
})

const uninstallCommand = computed(() => {
  if (!props.item) return ''
  return `curl -fsSL ${reachBaseUrl.value}/setup.sh | sudo sh -s -- --uninstall`
})

function confirmDisable() {
  if (!props.item) return
  if (confirm(`Disable ${props.item.machine.slug}?`)) {
    emit('action', 'disable', props.item.machine.id)
  }
}

function confirmRemove() {
  if (!props.item) return
  if (confirm(`Remove ${props.item.machine.slug}? This retires the DB row and removes hub-side files.`)) {
    emit('action', 'remove', props.item.machine.id)
  }
}
</script>
