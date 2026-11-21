<template>
  <span class="badge" :class="[`st-${status}`, { 'badge-lg': large }]">
    <StatusIcon :status="status" :size="large ? 16 : 12" />
    <span>{{ STATUS_LABEL[status] }}</span>
  </span>
</template>

<script setup lang="ts">
import { STATUS_LABEL, type UiStatus } from '~/utils/status'

defineProps<{ status: UiStatus; large?: boolean }>()
</script>
