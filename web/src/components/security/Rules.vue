<script setup lang="ts">
import { h, onMounted, ref } from 'vue'
import {
  NAlert,
  NButton,
  NDataTable,
  NTag,
  useMessage,
  type DataTableColumns
} from 'naive-ui'
import { listRules, type SecurityRule } from '@/api/security'

const message = useMessage()
const loading = ref(false)
const rules = ref<SecurityRule[]>([])

function levelType(l: string): 'error' | 'warning' | 'info' {
  if (l === 'CRITICAL') return 'error'
  if (l === 'WARN') return 'warning'
  return 'info'
}
function actionType(a: string): 'error' | 'warning' | 'default' {
  if (a === 'auto') return 'error'
  if (a === 'semi') return 'warning'
  return 'default'
}
function actionText(a: string): string {
  return a === 'auto' ? '自动处置' : a === 'semi' ? '半自动(待审批)' : '仅告警'
}

const columns: DataTableColumns<SecurityRule> = [
  { title: 'ID', key: 'id', width: 130, ellipsis: { tooltip: true } },
  { title: '名称', key: 'name', ellipsis: { tooltip: true }, minWidth: 130 },
  { title: '级别', key: 'level', width: 100, render: (r) => h(NTag, { size: 'small', type: levelType(r.level) }, { default: () => r.level }) },
  { title: '窗口', key: 'window', width: 80 },
  { title: '阈值', key: 'threshold', width: 80, render: (r) => r.threshold },
  { title: '动作', key: 'action', width: 130, render: (r) => h(NTag, { size: 'small', type: actionType(r.action) }, { default: () => actionText(r.action) }) },
  { title: '检测 SQL', key: 'sql', ellipsis: { tooltip: true }, minWidth: 160, render: (r) => h('code', { style: 'font-size:11px;opacity:.8' }, r.sql) }
]

async function load() {
  loading.value = true
  try {
    rules.value = await listRules()
  } catch {
    /* 拦截器已提示 */
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="rules">
    <n-alert type="info" title="规则监控（只读）" :show-icon="true" class="note">
      以下为当前生效的 {{ rules.length }} 条攻击检测规则（T066），由调度器（T067/T069）周期评估。
      阈值/动作的<strong>编辑持久化</strong>将在后续里程碑接入（需新增规则覆盖存储），当前页面为只读监控。
    </n-alert>
    <n-data-table
      :columns="columns"
      :data="rules"
      :loading="loading"
      :row-key="(r: SecurityRule) => r.id"
      size="small"
      striped
      :max-height="560"
    />
  </div>
</template>

<style scoped>
.rules {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.note {
  font-size: 12px;
}
</style>
