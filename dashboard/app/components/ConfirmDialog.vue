<template>
  <div class="dialog-backdrop" @click.self="cancel">
    <div
      ref="panel"
      class="dialog"
      :class="{ 'dialog-danger': danger }"
      role="alertdialog"
      aria-modal="true"
      :aria-labelledby="titleId"
      :aria-describedby="bodyId"
      @keydown.escape.prevent="cancel"
    >
      <h2 :id="titleId" class="dialog-title">{{ title }}</h2>
      <div :id="bodyId" class="dialog-body"><slot /></div>

      <label v-if="typeToConfirm" class="dialog-type">
        <span>Type <strong class="mono">{{ typeToConfirm }}</strong> to confirm</span>
        <input
          ref="input"
          v-model="typed"
          class="input mono"
          autocomplete="off"
          autocapitalize="off"
          autocorrect="off"
          spellcheck="false"
          :placeholder="typeToConfirm"
          @keydown.enter.prevent="confirm"
        />
      </label>

      <div class="dialog-actions">
        <button ref="cancelBtn" type="button" class="btn" @click="cancel">Cancel</button>
        <button
          type="button"
          class="btn"
          :class="danger ? 'btn-danger-solid' : 'btn-primary'"
          :disabled="!canConfirm || busy || confirmDisabled"
          @click="confirm"
        >
          {{ busy ? 'Working...' : confirmLabel }}
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
const props = defineProps<{
  title: string
  confirmLabel: string
  danger?: boolean
  /** When set, the confirm button stays disabled until this exact text is typed. */
  typeToConfirm?: string
  busy?: boolean
  /** Extra condition from the caller (e.g. nothing left to act on). */
  confirmDisabled?: boolean
}>()
const emit = defineEmits<{ confirm: []; cancel: [] }>()

const uid = Math.random().toString(36).slice(2, 8)
const titleId = `dlg-title-${uid}`
const bodyId = `dlg-body-${uid}`
const typed = ref('')
const input = ref<HTMLInputElement | null>(null)
const cancelBtn = ref<HTMLButtonElement | null>(null)

const canConfirm = computed(() => !props.typeToConfirm || typed.value.trim() === props.typeToConfirm)

function confirm() {
  if (!canConfirm.value || props.busy || props.confirmDisabled) return
  emit('confirm')
}
function cancel() {
  if (props.busy) return
  emit('cancel')
}

onMounted(() => {
  // Focus the safe choice (or the name box), never the destructive button.
  nextTick(() => (input.value || cancelBtn.value)?.focus())
})
</script>
