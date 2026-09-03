<script setup lang="ts">
import { ref } from 'vue'
import { NTag, NButton, NEmpty, NSpin } from 'naive-ui'
import DiffView from './DiffView.vue'
import type { DriftItem } from '@/api/configs'

const props = defineProps<{
  items: DriftItem[]
  loading?: boolean
}>()

const emit = defineEmits<{
  adopt: [item: DriftItem]
  overwrite: [item: DriftItem]
}>()

const expanded = ref<Set<string>>(new Set())

function toggle(p: string) {
  const s = new Set(expanded.value)
  s.has(p) ? s.delete(p) : s.add(p)
  expanded.value = s
}
const fmt = (t: string) => new Date(t).toLocaleString()
</script>

<template>
  <div class="drift">
    <n-spin :show="loading">
      <n-empty v-if="!items.length" description="未检测到漂移，配置与平台期望一致" size="small" />
      <div v-for="it in items" :key="it.path" class="item" :class="it.severity">
        <div class="head" @click="toggle(it.path)">
          <n-tag
            size="small"
            :bordered="false"
            :type="it.severity === 'critical' ? 'error' : 'warning'"
          >
            {{ it.severity === 'critical' ? '严重' : '警告' }}
          </n-tag>
          <span class="path">{{ it.path }}</span>
          <span class="time">{{ fmt(it.detected_at) }}</span>
          <span class="caret">{{ expanded.has(it.path) ? '▾' : '▸' }}</span>
        </div>
        <div v-if="expanded.has(it.path)" class="body">
          <DiffView :diff="it.diff" />
          <n-space :size="8" style="margin-top: 8px">
            <n-button size="small" @click="emit('overwrite', it)">用平台版本覆盖</n-button>
            <n-button size="small" @click="emit('adopt', it)">采纳节点版本</n-button>
          </n-space>
        </div>
      </div>
    </n-spin>
  </div>
</template>

<style scoped>
.drift {
  font-size: 13px;
}
.item {
  border-radius: 6px;
  margin-bottom: 8px;
  border: 1px solid var(--n-border-color);
  overflow: hidden;
}
.item.critical {
  border-color: rgba(224, 108, 117, 0.6);
}
.item.warning {
  border-color: rgba(229, 192, 123, 0.6);
}
.head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 10px;
  cursor: pointer;
  background: rgba(255, 255, 255, 0.02);
}
.path {
  font-family: monospace;
  font-weight: 600;
}
.time {
  color: #8a8f99;
  font-size: 12px;
  margin-left: auto;
}
.caret {
  color: #8a8f99;
}
.body {
  padding: 10px;
  border-top: 1px solid var(--n-border-color);
}
</style>
