<script setup lang="ts">
import { computed } from 'vue'
import { NCard, NTag, NThing, NSpace, NText } from 'naive-ui'
import type { NodeOut, NodeStatus, NodeRole } from '@/api/nodes'

const props = defineProps<{
  node: NodeOut
  nginxVersion?: string
}>()

const emit = defineEmits<{ (e: 'open', id: number): void }>()

const statusMeta: Record<NodeStatus, { label: string; color: string; bg: string }> = {
  online: { label: '在线', color: '#18a058', bg: 'rgba(24,160,88,0.12)' },
  offline: { label: '离线', color: '#8a8f99', bg: 'rgba(138,143,153,0.12)' },
  degraded: { label: '异常', color: '#f0a020', bg: 'rgba(240,160,32,0.14)' },
  enrolling: { label: '接入中', color: '#2080f0', bg: 'rgba(32,128,240,0.12)' }
}

const roleMeta: Record<NodeRole, { label: string; type: 'success' | 'warning' | 'default' | 'info' }> = {
  real_server: { label: 'Nginx RS', type: 'success' },
  director: { label: 'LVS Director', type: 'warning' },
  unknown: { label: '未知', type: 'default' }
}

const sm = computed(() => statusMeta[props.node.status] || statusMeta.offline)
const rm = computed(() => roleMeta[props.node.role] || roleMeta.unknown)

const lastHb = computed(() => {
  const t = props.node.last_heartbeat_at
  if (!t) return '—'
  const d = new Date(t)
  return d.toLocaleString()
})
</script>

<template>
  <n-card hoverable class="card" @click="emit('open', node.id)">
    <n-thing>
      <template #header>
        <n-space align="center" :size="8">
          <span class="status-dot" :style="{ background: sm.color }" />
          <span class="node-name">{{ node.name }}</span>
        </n-space>
      </template>
      <template #header-extra>
        <n-tag :type="rm.type" size="small" :bordered="false">{{ rm.label }}</n-tag>
      </template>

      <n-space vertical :size="6">
        <div class="row">
          <span class="k">地址</span><n-text depth="3">{{ node.address || '—' }}</n-text>
        </div>
        <div class="row">
          <span class="k">Nginx</span>
          <n-text depth="3">{{ nginxVersion || (node.role === 'director' ? '非 Nginx 节点' : '—') }}</n-text>
        </div>
        <div class="row">
          <span class="k">心跳</span><n-text depth="3">{{ lastHb }}</n-text>
          <span v-if="node.clock_skew_seconds !== undefined" class="skew">
            偏差 {{ node.clock_skew_seconds.toFixed(1) }}s
          </span>
        </div>
        <div class="row">
          <span class="k">状态</span>
          <span class="status-pill" :style="{ color: sm.color, background: sm.bg }">{{ sm.label }}</span>
        </div>
      </n-space>
    </n-thing>
  </n-card>
</template>

<style scoped>
.card {
  cursor: pointer;
}
.status-dot {
  width: 10px;
  height: 10px;
  border-radius: 50%;
  display: inline-block;
}
.node-name {
  font-weight: 600;
  font-size: 15px;
}
.row {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 13px;
}
.k {
  width: 42px;
  color: #8a8f99;
  flex: 0 0 auto;
}
.skew {
  color: #f0a020;
  font-size: 12px;
}
.status-pill {
  padding: 1px 10px;
  border-radius: 10px;
  font-size: 12px;
  font-weight: 600;
}
</style>
