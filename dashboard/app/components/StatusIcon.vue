<template>
  <!-- Shape differs per status so colour is never the only signal. -->
  <svg
    class="status-icon"
    :class="`st-${status}`"
    :width="size"
    :height="size"
    viewBox="0 0 16 16"
    aria-hidden="true"
    focusable="false"
  >
    <circle v-if="status === 'online'" cx="8" cy="8" r="6" fill="currentColor" />
    <template v-else-if="status === 'degraded'">
      <circle cx="8" cy="8" r="5.5" fill="none" stroke="currentColor" stroke-width="2" />
      <path d="M8 2.5a5.5 5.5 0 0 0 0 11z" fill="currentColor" />
    </template>
    <circle v-else-if="status === 'offline'" cx="8" cy="8" r="5" fill="none" stroke="currentColor" stroke-width="2.6" />
    <circle v-else-if="status === 'dormant'" cx="8" cy="8" r="5.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-dasharray="2.2 1.8" />
    <path v-else-if="status === 'pending'" class="st-pulse" d="M8 1.5L14.5 8 8 14.5 1.5 8z" fill="currentColor" />
    <template v-else-if="status === 'disabled'">
      <rect x="3.5" y="3" width="3" height="10" rx="1" fill="currentColor" />
      <rect x="9.5" y="3" width="3" height="10" rx="1" fill="currentColor" />
    </template>
    <template v-else-if="status === 'retiring' || status === 'stuck'">
      <path d="M13 8a5 5 0 1 1-1.6-3.7" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" />
      <path d="M13.6 1.8v3.6H10" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
    </template>
    <template v-else-if="status === 'failed' || status === 'disable_failed'">
      <path d="M8 1.8L15 14H1z" fill="currentColor" />
      <path d="M8 6v3.6M8 11.6v.1" stroke="var(--panel)" stroke-width="1.8" stroke-linecap="round" />
    </template>
    <path v-else-if="status === 'retired'" d="M4 4l8 8M12 4l-8 8" stroke="currentColor" stroke-width="2" stroke-linecap="round" />
    <template v-else>
      <circle cx="8" cy="8" r="5.5" fill="none" stroke="currentColor" stroke-width="1.6" />
      <circle cx="8" cy="8" r="1.4" fill="currentColor" />
    </template>
  </svg>
</template>

<script setup lang="ts">
import type { UiStatus } from '~/utils/status'

withDefaults(defineProps<{ status: UiStatus; size?: number }>(), { size: 14 })
</script>
