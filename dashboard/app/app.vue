<template>
  <NuxtPage />
</template>

<script setup lang="ts">
const theme = useTheme()
onMounted(() => theme.init())
</script>
