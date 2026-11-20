<template>
  <div v-if="item" class="detail" :class="`st-${status}`">
    <!-- 1. Headline: name, big status, one plain sentence -->
    <header class="detail-head">
      <button type="button" class="btn btn-ghost btn-back" @click="$emit('back')">
        <AppIcon name="back" :size="18" /> <span>Machines</span>
      </button>
      <h1 class="detail-name mono">{{ item.machine.slug }}</h1>
      <div class="detail-status">
        <StatusBadge :status="status" large />
        <span class="detail-seen">{{ seenLabel }}</span>
      </div>
      <p class="detail-sentence">{{ sentence }}</p>
    </header>

    <div class="detail-cols">
      <div class="detail-col">
        <!-- 2. Primary action -->
        <section v-if="canSSH" class="card ssh-card" aria-label="SSH command">
          <code class="ssh-cmd mono">ssh {{ item.machine.slug }}</code>
          <button type="button" class="btn btn-primary btn-copy" @click="$emit('copy', `ssh ${item.machine.slug}`)">
            <AppIcon name="copy" :size="18" /> Copy
          </button>
        </section>

        <!-- 3. Connection check -->
        <section class="card" aria-labelledby="chk-title">
          <h2 id="chk-title" class="card-title">Connection check</h2>
          <ol class="checks">
            <li v-for="c in checks" :key="c.key" class="check" :class="`check-${c.state}`">
              <span class="check-mark" aria-hidden="true">
                <AppIcon :name="c.state === 'ok' ? 'check' : c.state === 'fail' ? 'x' : 'help'" :size="16" />
              </span>
              <span class="check-body">
                <span class="check-label">{{ c.label }}
                  <span class="check-verdict">{{ c.state === 'ok' ? 'OK' : c.state === 'fail' ? 'Failed' : 'Unknown' }}</span>
                </span>
                <span class="check-detail">{{ c.detail }}</span>
              </span>
            </li>
          </ol>
        </section>
      </div>

      <div class="detail-col">
        <!-- 4. Diagnostics -->
        <section class="card" aria-labelledby="diag-title">
          <h2 id="diag-title" class="card-title">Diagnostics</h2>
          <dl class="kv">
            <div><dt>Last heartbeat</dt><dd>{{ rt.relative(obs?.heartbeat_at) }}</dd></div>
            <div><dt>Agent version</dt><dd class="mono wrap">{{ obs?.agent_version || '—' }}
              <span v-if="item.update?.available && item.update.latest" class="tag tag-info">update available</span></dd></div>
            <div><dt>Transport</dt><dd>{{ obs?.transport || '—' }}</dd></div>
            <div><dt>System</dt><dd>{{ [item.machine.distro, item.machine.arch].filter(Boolean).join(' · ') || '—' }}</dd></div>
            <div><dt>Persistence</dt><dd>{{ obs?.persistence_backend || '—' }}{{ obs?.persistence_reboot_safe ? ' (survives reboot)' : '' }}</dd></div>
            <div><dt>Hub tunnel</dt><dd class="mono">{{ tunnelUser }} → :{{ tunnelPort }}</dd></div>
            <div v-if="obs?.last_error" class="kv-error"><dt>Last error</dt><dd class="wrap">{{ obs.last_error }}</dd></div>
            <div v-if="item.machine.provision_error" class="kv-error"><dt>Provision error</dt><dd class="wrap">{{ item.machine.provision_error }}</dd></div>
          </dl>
        </section>
      </div>
    </div>

    <!-- 5. Advanced, collapsed -->
    <details class="card advanced">
      <summary class="card-title advanced-summary">
        <span>Advanced</span>
        <span class="muted small">updates, process title, IDs, repair</span>
        <AppIcon name="chevron-down" :size="18" class="advanced-chevron" />
      </summary>

      <dl class="kv">
        <div><dt>Machine ID</dt><dd class="mono">{{ item.machine.id }}</dd></div>
        <div><dt>You set it to</dt><dd>{{ item.machine.desired_state }}<span class="muted"> (desired state)</span></dd></div>
        <div><dt>Hub observes</dt><dd>{{ item.machine.observed_state }}<span class="muted"> (from heartbeat + probe)</span></dd></div>
        <div v-if="item.machine.cleanup_state"><dt>Cleanup</dt><dd>{{ item.machine.cleanup_state }}</dd></div>
        <div><dt>Generation</dt><dd>{{ item.machine.desired_generation }}<span class="muted"> (agent applied {{ obs?.applied_generation ?? '—' }})</span></dd></div>
        <div><dt>Login user</dt><dd class="mono">{{ item.machine.target_user || 'root' }}</dd></div>
        <div><dt>Install mode</dt><dd>{{ item.machine.mode || 'system' }}</dd></div>
        <div><dt>Target ssh port</dt><dd>{{ item.machine.local_port || 22 }}</dd></div>
        <div><dt>Created</dt><dd>{{ rt.fullTime(item.machine.created_at) }}</dd></div>
        <div v-if="item.machine.expires_at"><dt>Expires</dt><dd>{{ rt.fullTime(item.machine.expires_at) }}</dd></div>
      </dl>

      <h3 class="sub-title">Agent updates</h3>
      <div class="field-row">
        <label class="field">
          <span>Update policy</span>
          <select v-model="updatePolicy" class="input" :disabled="updatePolicySaving" @change="saveUpdatePolicy">
            <option value="manual">Manual</option>
            <option value="auto">Auto-update</option>
            <option value="disabled">Disabled</option>
          </select>
        </label>
        <button type="button" class="btn" :disabled="!canQueueUpdate || updateSaving" @click="queueUpdateAgent">
          {{ updateSaving ? 'Queueing...' : updateButtonLabel }}
        </button>
      </div>

      <template v-if="item.commands && item.commands.length">
        <h3 class="sub-title">Open agent commands</h3>
        <ul class="cmd-list">
          <li v-for="cmd in item.commands" :key="cmd.id">
            <span class="mono">{{ cmd.type }}</span>
            <span class="tag tag-muted">{{ cmd.status }}</span>
            <span class="muted small">{{ rt.relative(cmd.created_at) }}</span>
          </li>
        </ul>
      </template>

      <h3 class="sub-title">Process title</h3>
      <p class="muted small">What reach-agent shows in <span class="mono">ps aux</span>. Leave empty and clear to use the agent's own setting.</p>
      <div class="field-row">
        <label class="field">
          <span>Single message</span>
          <input v-model="processTitleSingle" class="input" placeholder="Reach-Agent" />
        </label>
        <label class="field field-narrow">
          <span>Rotate every</span>
          <input v-model="processTitleInterval" class="input" placeholder="5s" />
        </label>
      </div>
      <label class="field">
        <span>Rotating messages (one per line, overrides the single message)</span>
        <textarea v-model="processTitleList" class="input" rows="3" placeholder="Reach-Agent" />
      </label>
      <p v-if="processTitleError" class="form-error">{{ processTitleError }}</p>
      <div class="field-row">
        <button type="button" class="btn btn-primary" :disabled="processTitleSaving" @click="saveProcessTitle">{{ processTitleSaving ? 'Saving...' : 'Save message' }}</button>
        <button type="button" class="btn" :disabled="processTitleSaving" @click="clearProcessTitle">Clear override</button>
        <span class="muted small">{{ processTitleSource }}</span>
      </div>

      <h3 class="sub-title">Run on the machine</h3>
      <p class="muted small">Paste on {{ item.machine.slug }} itself.</p>
      <div class="cmd-copy">
        <span class="cmd-copy-label">Repair (restart the agent)</span>
        <div class="cmd-copy-row">
          <code class="mono">{{ repairCommand }}</code>
          <button type="button" class="btn btn-icon" aria-label="Copy repair command" @click="$emit('copy', repairCommand)"><AppIcon name="copy" :size="18" /></button>
        </div>
      </div>
      <div class="cmd-copy">
        <span class="cmd-copy-label">Uninstall the agent</span>
        <div class="cmd-copy-row">
          <code class="mono">{{ uninstallCommand }}</code>
          <button type="button" class="btn btn-icon" aria-label="Copy uninstall command" @click="$emit('copy', uninstallCommand)"><AppIcon name="copy" :size="18" /></button>
        </div>
      </div>
    </details>

    <!-- 6. Danger zone, at the very bottom -->
    <section class="danger-zone" aria-labelledby="dz-title">
      <h2 id="dz-title" class="card-title">Danger zone</h2>

      <div v-if="item.machine.desired_state === 'active'" class="dz-row">
        <div class="dz-text">
          <strong>Disable</strong>
          <p>Cuts the tunnel: the hub blocks this machine's key and its <span class="mono">ssh {{ item.machine.slug }}</span> alias stops working. Nothing is deleted. You can Enable it again any time.</p>
        </div>
        <button type="button" class="btn btn-warn" @click="dialog = 'disable'">Disable</button>
      </div>
      <div v-else-if="item.machine.desired_state === 'disabled'" class="dz-row">
        <div class="dz-text">
          <strong>Enable</strong>
          <p>Unblocks the tunnel key. The agent reconnects on its next heartbeat.</p>
        </div>
        <button type="button" class="btn btn-primary" @click="dialog = 'enable'">Enable</button>
      </div>

      <div v-if="item.machine.desired_state === 'active' || item.machine.desired_state === 'disabled'" class="dz-row dz-severe">
        <div class="dz-text">
          <strong>Remove</strong>
          <p>Permanent. Tells the agent to uninstall itself and deletes the tunnel account, key and port on the hub. To use this machine again you must run setup and approve it again.</p>
        </div>
        <button type="button" class="btn btn-danger" @click="dialog = 'remove'">Remove...</button>
      </div>

      <div v-if="status === 'stuck' || status === 'failed' || item.machine.desired_state === 'retiring'" class="dz-row dz-severe">
        <div class="dz-text">
          <strong>Force retire</strong>
          <p>Finishes the removal on the hub without waiting for the agent. Use it when the machine is gone for good{{ status === 'failed' ? ', or to retry the failed cleanup' : '' }}.</p>
        </div>
        <button type="button" class="btn btn-danger" @click="dialog = 'retire-now'">Force retire...</button>
      </div>
    </section>

    <ConfirmDialog
      v-if="dialog === 'disable'"
      title="Disable this machine?"
      confirm-label="Disable"
      danger
      :busy="actionBusy"
      @cancel="dialog = null"
      @confirm="runAction('disable')"
    >
      <p><span class="mono">{{ item.machine.slug }}</span> will stop being reachable. The hub blocks its tunnel key and drops it from your Mac's ssh config.</p>
      <p>Nothing is deleted. Enable brings it back.</p>
    </ConfirmDialog>
    <ConfirmDialog
      v-if="dialog === 'enable'"
      title="Enable this machine?"
      confirm-label="Enable"
      :busy="actionBusy"
      @cancel="dialog = null"
      @confirm="runAction('enable')"
    >
      <p>The hub accepts <span class="mono">{{ item.machine.slug }}</span>'s tunnel key again.</p>
    </ConfirmDialog>
    <ConfirmDialog
      v-if="dialog === 'remove'"
      title="Remove permanently?"
      confirm-label="Remove machine"
      danger
      :type-to-confirm="item.machine.slug"
      :busy="actionBusy"
      @cancel="dialog = null"
      @confirm="runAction('remove')"
    >
      <p>This cannot be undone. The agent on <span class="mono">{{ item.machine.slug }}</span> is told to uninstall, and the hub deletes its tunnel account, key and port.</p>
      <p>If you only want to turn it off for now, use <strong>Disable</strong> instead.</p>
    </ConfirmDialog>
    <ConfirmDialog
      v-if="dialog === 'retire-now'"
      title="Force retire?"
      confirm-label="Force retire"
      danger
      :type-to-confirm="item.machine.slug"
      :busy="actionBusy"
      @cancel="dialog = null"
      @confirm="runAction('retire-now')"
    >
      <p>The hub deletes <span class="mono">{{ item.machine.slug }}</span>'s tunnel account, key and port now, without waiting for the agent to confirm.</p>
      <p>If the machine is still running, its agent stays installed there. Run the uninstall command on it yourself.</p>
    </ConfirmDialog>
  </div>
  <div v-else class="empty">
    <p>Machine not found. It may have been retired.</p>
    <button type="button" class="btn" @click="$emit('back')">Back to machines</button>
  </div>
</template>

<script setup lang="ts">
import { connectionChecks, lastSeenAt, statusSentence, uiStatus } from '~/utils/status'
import { repairCommandFor, uninstallCommandFor } from '~/utils/commands'

type Action = 'disable' | 'enable' | 'remove' | 'retire-now'

const props = defineProps<{ item: MachineWithTunnels | null; baseUrl: string }>()

const emit = defineEmits<{
  back: []
  action: [action: Action, id: string, done: () => void]
  copy: [text: string]
  refresh: [id: string]
}>()

const rt = useRelativeTime()
const now = useNow()
const api = useReachApi()
const toast = useToast()

const dialog = ref<Action | null>(null)
const actionBusy = ref(false)

const obs = computed(() => props.item?.agent_observation)
const status = computed(() => props.item ? uiStatus(props.item, now.value) : 'unknown')
const sentence = computed(() => props.item ? statusSentence(props.item, status.value, now.value) : '')
const checks = computed(() => props.item ? connectionChecks(props.item, now.value) : [])
const canSSH = computed(() => status.value === 'online' || status.value === 'degraded' || status.value === 'offline')
const seenLabel = computed(() => {
  if (!props.item) return ''
  const seen = lastSeenAt(props.item)
  return seen === null ? 'never seen' : `seen ${rt.relative(new Date(seen).toISOString())}`
})

const tunnelUser = computed(() => {
  const t = props.item?.tunnels[0]
  return t?.original_unix_user || t?.unix_user || '—'
})
const tunnelPort = computed(() => {
  const t = props.item?.tunnels[0]
  return t?.original_remote_port || t?.remote_port || '?'
})

const repairCommand = computed(() => props.item ? repairCommandFor(props.baseUrl, props.item.machine) : '')
const uninstallCommand = computed(() => props.item ? uninstallCommandFor(props.baseUrl, props.item.machine) : '')

function runAction(action: Action) {
  if (!props.item) return
  actionBusy.value = true
  emit('action', action, props.item.machine.id, () => {
    actionBusy.value = false
    dialog.value = null
  })
}

// Advanced: process title and update policy forms
const processTitleSingle = ref('')
const processTitleList = ref('')
const processTitleInterval = ref('5s')
const processTitleSaving = ref(false)
const processTitleError = ref('')
const updateSaving = ref(false)
const updatePolicy = ref<'manual' | 'auto' | 'disabled'>('manual')
const updatePolicySaving = ref(false)

const processTitleSource = computed(() => props.item?.machine.process_title_config ? 'Dashboard override active' : 'Using the agent\'s own setting')

watch(() => props.item?.machine.id, () => { resetProcessTitleForm(); resetUpdatePolicyForm(); dialog.value = null }, { immediate: true })
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
  const titles = processTitleList.value.split('\n').map(s => s.trim()).filter(Boolean)
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
  if (props.item.update.available === false) return 'Agent is up to date'
  return `Update agent to ${props.item.update.latest}`
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
</script>
