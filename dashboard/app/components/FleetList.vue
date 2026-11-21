<template>
  <div class="fleet">
    <div class="fleet-search">
      <AppIcon name="search" :size="16" class="fleet-search-icon" />
      <input
        ref="filterInput"
        v-model="filter"
        class="input"
        type="search"
        placeholder="Find a machine"
        aria-label="Find a machine"
        @keydown.escape="filter = ''; ($event.target as HTMLInputElement).blur()"
      />
    </div>

    <section v-for="g in visibleGroups" :key="g.key" class="fgroup" :class="`fgroup-${g.key}`" :aria-labelledby="`fg-${g.key}`">
      <div class="fgroup-head">
        <button
          v-if="g.collapsible && !filter"
          type="button"
          class="fgroup-toggle"
          :aria-expanded="isOpen(g.key)"
          @click="toggle(g.key)"
        >
          <AppIcon :name="isOpen(g.key) ? 'chevron-down' : 'chevron'" :size="16" />
          <span :id="`fg-${g.key}`" class="fgroup-label">{{ g.label }}</span>
          <span class="fgroup-count">{{ g.items.length }}</span>
        </button>
        <h2 v-else class="fgroup-title">
          <span :id="`fg-${g.key}`" class="fgroup-label">{{ g.label }}</span>
          <span class="fgroup-count">{{ g.items.length }}</span>
        </h2>
        <span class="fgroup-hint">{{ g.hint }}</span>
      </div>

      <div v-if="g.key === 'dormant' && isOpen(g.key) && dormantActive.length > 0" class="fgroup-bulk">
        <span>None of these has been seen for over 7 days.</span>
        <button type="button" class="btn btn-sm-touch" @click="$emit('bulk-disable', dormantActive)">
          Disable all {{ dormantActive.length }}
        </button>
      </div>

      <div v-if="!g.collapsible || filter || isOpen(g.key)" class="fgroup-list">
        <MachineRow
          v-for="item in g.items"
          :key="item.machine.id"
          :item="item"
          :status="statusOf.get(item.machine.id) || 'unknown'"
          :active="item.machine.id === selectedId"
          :class="{ focused: item.machine.id === focusedId }"
          @select="$emit('select', $event)"
          @copy="$emit('copy', $event)"
        />
      </div>
    </section>

    <p v-if="filter && visibleGroups.length === 0" class="empty small">No machine matches "{{ filter }}".</p>
  </div>
</template>

<script setup lang="ts">
import type { FleetGroup, FleetGroupKey, UiStatus } from '~/utils/status'

const props = defineProps<{
  groups: FleetGroup[]
  statusOf: Map<string, UiStatus>
  selectedId: string | null
  focusedId?: string | null
}>()
defineEmits<{ select: [id: string]; copy: [text: string]; 'bulk-disable': [items: MachineWithTunnels[]] }>()

const filter = ref('')
const filterInput = ref<HTMLInputElement | null>(null)
const open = reactive(new Set<FleetGroupKey>())

const isOpen = (key: FleetGroupKey) => open.has(key)
function toggle(key: FleetGroupKey) {
  if (open.has(key)) open.delete(key)
  else open.add(key)
}

const visibleGroups = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return props.groups
  return props.groups
    .map(g => ({ ...g, items: g.items.filter(i => i.machine.slug.toLowerCase().includes(q)) }))
    .filter(g => g.items.length > 0)
})

const dormantActive = computed(() =>
  (props.groups.find(g => g.key === 'dormant')?.items || []).filter(i => i.machine.desired_state === 'active'),
)

/** Machines in display order, skipping collapsed groups (for j/k navigation). */
const flat = computed(() => {
  const out: MachineWithTunnels[] = []
  for (const g of visibleGroups.value) {
    if (g.collapsible && !filter.value && !open.has(g.key)) continue
    out.push(...g.items)
  }
  return out
})

defineExpose({
  focusFilter: () => filterInput.value?.focus(),
  flat,
})
</script>
