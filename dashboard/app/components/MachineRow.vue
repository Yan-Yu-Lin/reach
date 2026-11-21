<template>
  <div class="mrow" :class="[`st-${status}`, { active }]">
    <button type="button" class="mrow-main" :aria-current="active ? 'true' : undefined" @click="$emit('select', item.machine.id)">
      <StatusIcon :status="status" :size="14" class="mrow-icon" />
      <span class="mrow-body">
        <span class="mrow-line1">
          <span class="mrow-name mono">{{ item.machine.slug }}</span>
          <span class="mrow-time">{{ timeLabel }}</span>
        </span>
        <span class="mrow-line2">
          <span class="mrow-state">{{ STATUS_LABEL[status] }}</span>
          <span v-if="meta" class="mrow-meta">· {{ meta }}</span>
        </span>
      </span>
    </button>
    <button
      v-if="copyable"
      type="button"
      class="mrow-copy"
      :aria-label="`Copy ssh ${item.machine.slug}`"
      :title="`Copy: ssh ${item.machine.slug}`"
      @click="$emit('copy', `ssh ${item.machine.slug}`)"
    >
      <AppIcon name="copy" :size="18" />
    </button>
    <span v-else class="mrow-copy mrow-copy-empty" aria-hidden="true" />
  </div>
</template>

<script setup lang="ts">
import { lastSeenAt, STATUS_LABEL, type UiStatus } from '~/utils/status'

const props = defineProps<{ item: MachineWithTunnels; status: UiStatus; active?: boolean }>()
defineEmits<{ select: [id: string]; copy: [text: string] }>()

const rt = useRelativeTime()

const copyable = computed(() => props.status !== 'retired' && props.status !== 'retiring' && props.status !== 'stuck')

const timeLabel = computed(() => {
  const seen = lastSeenAt(props.item)
  if (props.status === 'retiring' || props.status === 'stuck' || props.status === 'disabled') {
    // How long it has been in that state, e.g. "since 120d ago".
    return `since ${rt.relative(props.item.machine.desired_changed_at || props.item.machine.updated_at)}`
  }
  return seen === null ? 'never' : rt.relative(new Date(seen).toISOString())
})

const meta = computed(() => {
  const parts: string[] = []
  const transport = props.item.agent_observation?.transport
  if (transport) parts.push(transport)
  const distro = props.item.machine.distro
  if (distro) parts.push(distro.split(' ')[0]!)
  return parts.join(' · ')
})
</script>
