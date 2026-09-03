<script setup lang="ts">
import { ref, computed } from 'vue'
import { NTag, NButton, NSpace } from 'naive-ui'
import type { RevisionView } from '@/api/configs'

const props = defineProps<{
  revisions: RevisionView[]
  fileId: number
  currentRevId: number
}>()

const emit = defineEmits<{ compare: [fileId: number, from: number, to: number] }>()

const from = ref<number | null>(null)
const to = ref<number | null>(null)

// 默认对比：最新两版（按时间倒序 r[0]=最新）。
const fmt = (t: string) => new Date(t).toLocaleString()
const sourceType = (s: string) =>
  s === 'sync'
    ? 'default'
    : s === 'manual_edit'
      ? 'info'
      : s === 'rollback'
        ? 'warning'
        : s === 'security_block'
          ? 'error'
          : s === 'cert_renew'
            ? 'success'
            : 'default'

const canCompare = computed(() => from.value !== null && to.value !== null && from.value !== to.value)

function compare() {
  if (!canCompare.value || from.value === null || to.value === null) return
  emit('compare', props.fileId, from.value, to.value)
}
</script>

<template>
  <div class="rev">
    <n-space v-if="revisions.length" align="center" :size="8" style="margin-bottom: 8px">
      <span class="lbl">对比：</span>
      <select v-model.number="from" class="sel">
        <option :value="null" disabled>旧版本</option>
        <option v-for="r in revisions" :key="r.id" :value="r.id">#{{ r.id }} · {{ fmt(r.created_at) }}</option>
      </select>
      <span class="arrow">→</span>
      <select v-model.number="to" class="sel">
        <option :value="null" disabled>新版本</option>
        <option v-for="r in revisions" :key="r.id" :value="r.id">#{{ r.id }} · {{ fmt(r.created_at) }}</option>
      </select>
      <n-button size="small" type="primary" :disabled="!canCompare" @click="compare">查看差异</n-button>
    </n-space>

    <div v-if="!revisions.length" class="empty">暂无版本记录</div>
    <div
      v-for="r in revisions"
      :key="r.id"
      class="item"
      :class="{ cur: r.id === currentRevId }"
    >
      <div class="row1">
        <span class="id">#{{ r.id }}</span>
        <n-tag size="small" :bordered="false" :type="sourceType(r.source)">{{ r.source }}</n-tag>
        <span class="author">{{ r.author }}</span>
        <span v-if="r.id === currentRevId" class="now">当前</span>
      </div>
      <div class="row2">
        <span class="time">{{ fmt(r.created_at) }}</span>
        <span v-if="r.message" class="msg">{{ r.message }}</span>
        <span class="sha">{{ r.sha256.slice(0, 12) }}</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.rev {
  font-size: 13px;
}
.lbl {
  color: #8a8f99;
}
.sel {
  height: 28px;
  border-radius: 4px;
  border: 1px solid var(--n-border-color);
  background: var(--n-color);
  color: var(--n-text-color);
  padding: 0 8px;
  max-width: 200px;
}
.arrow {
  color: #8a8f99;
}
.item {
  padding: 8px 10px;
  border-radius: 6px;
  border: 1px solid transparent;
  margin-bottom: 6px;
}
.item.cur {
  border-color: #18a058;
  background: rgba(24, 160, 88, 0.08);
}
.row1 {
  display: flex;
  align-items: center;
  gap: 8px;
}
.id {
  font-family: monospace;
  font-weight: 600;
}
.author {
  color: #8a8f99;
  font-size: 12px;
}
.now {
  margin-left: auto;
  color: #18a058;
  font-size: 12px;
}
.row2 {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-top: 3px;
  color: #8a8f99;
  font-size: 12px;
}
.msg {
  color: #c2c8d1;
}
.sha {
  font-family: monospace;
  margin-left: auto;
}
.empty {
  color: #8a8f99;
  padding: 8px 2px;
}
</style>
