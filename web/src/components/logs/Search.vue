<script setup lang="ts">
import { h, reactive, ref } from 'vue'
import {
  NButton,
  NDataTable,
  NDrawer,
  NDrawerContent,
  NForm,
  NFormItem,
  NInput,
  NInputGroup,
  NInputNumber,
  NSelect,
  NSpace,
  NTag,
  NDatePicker,
  NCheckbox,
  NEmpty,
  useMessage,
  type DataTableColumns,
  type SelectOption
} from 'naive-ui'
import { searchLogs, type LogEntry } from '@/api/logs'

const emit = defineEmits<{ (e: 'trace', rid: string): void }>()
const message = useMessage()

const form = reactive({
  timeRange: null as [number, number] | null,
  nodes: [] as string[],
  status: null as number | null,
  uri: '',
  ip: '',
  rid: '',
  rt_min: null as number | null,
  regex: false
})

const statusOptions: SelectOption[] = [
  { label: '2xx 成功', value: 200 },
  { label: '3xx 跳转', value: 300 },
  { label: '4xx 客户端错误', value: 400 },
  { label: '5xx 服务端错误', value: 500 }
]

const loading = ref(false)
const items = ref<LogEntry[]>([])
const total = ref(0)
const tookMs = ref(0)
const page = ref(1)
const pageSize = ref(50)

const showRaw = ref(false)
const rawText = ref('')

function statusType(s: number): 'success' | 'info' | 'warning' | 'error' {
  if (s >= 500) return 'error'
  if (s >= 400) return 'warning'
  if (s >= 300) return 'info'
  return 'success'
}

function openRaw(row: LogEntry) {
  rawText.value = JSON.stringify(row, null, 2)
  showRaw.value = true
}

const columns: DataTableColumns<LogEntry> = [
  { title: '时间', key: 'TS', width: 175, render: (r) => new Date(r.TS).toLocaleString() },
  { title: '节点', key: 'Node', width: 110 },
  { title: '状态码', key: 'Status', width: 80, render: (r) => h(NTag, { size: 'small', type: statusType(r.Status) }, { default: () => r.Status }) },
  { title: '远程地址', key: 'RemoteAddr', width: 130 },
  { title: 'URI', key: 'URI', ellipsis: { tooltip: true }, minWidth: 160 },
  { title: '耗时(ms)', key: 'RequestRT', width: 90, render: (r) => (r.RequestRT * 1000).toFixed(0) },
  { title: 'UA', key: 'UA', ellipsis: { tooltip: true }, minWidth: 120 },
  {
    title: '操作',
    key: 'actions',
    width: 150,
    render: (r) =>
      h(NSpace, { size: 4 }, {
        default: () => [
          h(NButton, { size: 'tiny', secondary: true, onClick: () => openRaw(r) }, { default: () => '原始' }),
          r.RID
            ? h(NButton, { size: 'tiny', secondary: true, type: 'info', onClick: () => emit('trace', r.RID) }, { default: () => '追踪' })
            : null
        ]
      })
  }
]

async function doSearch() {
  loading.value = true
  try {
    const res = await searchLogs({
      time_from: form.timeRange ? new Date(form.timeRange[0]).toISOString() : undefined,
      time_to: form.timeRange ? new Date(form.timeRange[1]).toISOString() : undefined,
      nodes: form.nodes.length ? form.nodes : undefined,
      status: form.status != null ? [form.status] : undefined,
      uri: form.uri || undefined,
      ip: form.ip || undefined,
      rid: form.rid || undefined,
      rt_min: form.rt_min != null ? form.rt_min : undefined,
      regex: form.regex,
      page: page.value,
      size: pageSize.value
    })
    items.value = res.items
    total.value = res.total
    tookMs.value = res.took_ms
  } catch {
    /* 拦截器已提示 */
  } finally {
    loading.value = false
  }
}

function onPageChange(p: number) {
  page.value = p
  doSearch()
}
</script>

<template>
  <div class="search">
    <n-form inline label-placement="left" :show-feedback="false">
      <n-space :size="10" wrap>
        <n-form-item label="时间窗">
          <n-date-picker
            v-model:value="form.timeRange"
            type="datetimerange"
            clearable
            style="width: 360px"
          />
        </n-form-item>
        <n-form-item label="状态码">
          <n-select v-model:value="form.status" :options="statusOptions" clearable placeholder="全部" style="width: 150px" />
        </n-form-item>
        <n-form-item label="IP">
          <n-input v-model:value="form.ip" placeholder="客户端地址" clearable style="width: 140px" />
        </n-form-item>
        <n-form-item label="URI">
          <n-input v-model:value="form.uri" placeholder="路径（正则可开）" clearable style="width: 180px" />
        </n-form-item>
        <n-form-item label="TraceID">
          <n-input v-model:value="form.rid" placeholder="request_id" clearable style="width: 160px" />
        </n-form-item>
        <n-form-item label="耗时≥(s)">
          <n-input-number v-model:value="form.rt_min" :min="0" placeholder="如 1" clearable style="width: 110px" />
        </n-form-item>
        <n-form-item>
          <n-checkbox v-model:checked="form.regex">URI 正则</n-checkbox>
        </n-form-item>
        <n-form-item>
          <n-button type="primary" :loading="loading" @click="doSearch">检索</n-button>
        </n-form-item>
      </n-space>
    </n-form>

    <div class="meta">
      <span v-if="total">共 {{ total }} 条，耗时 {{ tookMs }}ms（第 {{ page }} 页）</span>
      <span v-else>输入条件后点击检索</span>
    </div>

    <n-data-table
      :columns="columns"
      :data="items"
      :loading="loading"
      :row-key="(r: LogEntry) => r.TS + r.Node + r.URI"
      size="small"
      striped
      :pagination="{ page, pageSize, itemCount: total, showSizePicker: false, onUpdatePage: onPageChange }"
    />

    <n-drawer v-model:show="showRaw" :width="560" placement="right">
      <n-drawer-content title="原始日志 JSON（文本展示，防 XSS）">
        <pre class="raw">{{ rawText }}</pre>
      </n-drawer-content>
    </n-drawer>
  </div>
</template>

<style scoped>
.search {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.meta {
  color: #8a8f99;
  font-size: 13px;
}
.raw {
  white-space: pre-wrap;
  word-break: break-all;
  font-family: monospace;
  font-size: 12px;
  background: rgba(0, 0, 0, 0.03);
  padding: 10px;
  border-radius: 6px;
  max-height: 70vh;
  overflow: auto;
}
</style>
