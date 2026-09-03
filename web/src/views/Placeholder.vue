<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { NEmpty, NCard } from 'naive-ui'

const route = useRoute()
const title = computed(() => (route.meta?.title as string) || '页面')
</script>

<template>
  <div class="ph">
    <n-card :title="title" class="ph-card">
      <n-empty :description="`「${title}」模块将在对应里程碑（M2–M9）落地`" />
    </n-card>
  </div>
</template>

<style scoped>
.ph {
  padding: 24px;
}
.ph-card {
  max-width: 720px;
}
</style>
