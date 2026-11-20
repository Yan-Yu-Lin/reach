<template>
  <article class="req-card" :aria-labelledby="`req-${req.id}`">
    <header class="req-head">
      <h3 :id="`req-${req.id}`" class="req-name">{{ req.name }}</h3>
      <span class="req-time">{{ rt.relative(req.created_at) }}</span>
    </header>

    <dl class="req-facts">
      <div class="req-fact">
        <dt>Will appear as</dt>
        <dd>
          <template v-if="finalSlug.value">
            <span class="mono">ssh {{ finalSlug.value }}</span>
            <span v-if="!finalSlug.bound" class="tag tag-warn">self-reported, can change at setup</span>
          </template>
          <span v-else class="tag tag-warn">name not chosen yet; picked at setup</span>
        </dd>
      </div>
      <div class="req-fact">
        <dt>Login user</dt>
        <dd>
          <span class="mono">{{ targetUser.value || 'root (default)' }}</span>
          <span v-if="!targetUser.bound" class="tag tag-muted">self-reported</span>
        </dd>
      </div>
      <div class="req-fact">
        <dt>Connected from</dt>
        <dd v-if="req.observed_ip">
          <span class="mono">{{ req.observed_ip }}</span>
          <span class="tag tag-ok">seen by server</span>
        </dd>
        <dd v-else>
          <span class="mono">{{ req.metadata?.source_ip || 'unknown' }}</span>
          <span class="tag tag-warn">requester's own claim, not verified</span>
        </dd>
      </div>
      <div v-if="req.observed_ip && claimedIp && claimedIp !== req.observed_ip" class="req-fact">
        <dt>Claims to be from</dt>
        <dd>
          <span class="mono">{{ claimedIp }}</span>
          <span class="tag tag-danger">does not match what the server saw</span>
        </dd>
      </div>
      <div class="req-fact">
        <dt>System</dt>
        <dd>
          {{ systemLabel }}
          <span class="tag tag-muted">self-reported</span>
        </dd>
      </div>
    </dl>

    <ul v-if="warnings.length" class="req-warnings" aria-label="Warnings">
      <li v-for="w in warnings" :key="w"><AppIcon name="alert" :size="16" /> {{ w }}</li>
    </ul>

    <div class="req-actions">
      <button type="button" class="btn btn-ghost-danger" @click="$emit('deny', req)">Deny</button>
      <button type="button" class="btn btn-primary" @click="$emit('approve', req)">Review &amp; approve</button>
    </div>
  </article>
</template>

<script setup lang="ts">
import { requestSlug, requestTargetUser, requestWarnings } from '~/utils/requests'

const props = defineProps<{ req: RequestRow; existingSlugs: Set<string> }>()
defineEmits<{ approve: [req: RequestRow]; deny: [req: RequestRow] }>()

const rt = useRelativeTime()

const finalSlug = computed(() => requestSlug(props.req))
const targetUser = computed(() => requestTargetUser(props.req))
const claimedIp = computed(() => {
  const ip = props.req.metadata?.source_ip
  return typeof ip === 'string' ? ip : ''
})
const systemLabel = computed(() => {
  const m = props.req.metadata || {}
  const bits = [m.distro, m.arch].filter(Boolean)
  const mode = props.req.mode || m.mode
  if (mode) bits.push(`${mode} mode`)
  return bits.join(' · ') || 'unknown'
})
const warnings = computed(() => requestWarnings(props.req, props.existingSlugs))
</script>
