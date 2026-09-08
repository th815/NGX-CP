<script setup lang="ts">
import { h, onMounted, reactive, ref } from 'vue'
import {
  NButton,
  NDataTable,
  NDrawer,
  NDrawerContent,
  NForm,
  NFormItem,
  NInput,
  NSelect,
  NSpace,
  NTag,
  NRadioGroup,
  NRadioButton,
  NEmpty,
  NAlert,
  useMessage,
  useDialog,
  type DataTableColumns,
  type SelectOption
} from 'naive-ui'
import {
  listEvents,
  handleEvent,
  blockEvent,
  blockIP,
  type SecurityEvent
} from '@/api/security'

const message = useMessage()
const dialog = useDialog()

const filter = reactive({
  level: null as string | null,
  handled: '' as string,
  page: 1,
  size: 50
})

const levelOptions: SelectOption[] = [
  { label: '全部级别', value: '' },
  { label: 'CRITICAL', value: 'CRITICAL' },
  { label: 'WARN', value: 'WARN' },
  { label: 'INFO', value: 'INFO' }
]
const handledOptions: SelectOption[] = [
  { label: '全部', value: '' },
  { label: '未处置', value: 'false' },
  { label: '已处置', value: 'true' }
]

// filter.handled 用字符串 'true'/'false'/'' 表达，转 boolean 传给 API。
function handledToBool(v: string | null): boolean | undefined {
  if (v === null || v === '') return undefined
  return v === 'true'
}

const loading = ref(false)
const items = ref<SecurityEvent[]>([])
const total = ref(0)

const showDetail = ref(false)
const current = ref<SecurityEvent | null>(null)

const blockIPForm = reactive({ ip: '', reason: '' })

function levelType(l: string): 'error' | 'warning' | 'info' {
  if (l === 'CRITICAL') return 'error'
  if (l === 'WARN') return 'warning'
  return 'info'
}
function actionType(a: string): 'default' | 'success' | 'warning' {
  if (a === 'blocked') return 'success'
  if (a === 'ignored') return 'warning'
  return 'default'
}

const columns: DataTableColumns<SecurityEvent> = [
  { title: 'ID', key: 'id', width: 60 },
  {
    title: '级别',
    key: 'level',
    width: 100,
    render: (r) => h(NTag, { size: 'small', type: levelType(r.level) }, { default: () => r.level })
  },
  { title: '规则', key: 'rule_name', ellipsis: { tooltip: true }, minWidth: 130 },
  { title: '节点', key: 'node', width: 110 },
  {
    title: '处置',
    key: 'handled',
    width: 110,
    render: (r) =>
      h(NSpace, { size: 4, align: 'center' }, {
        default: () => [
          h(NTag, { size: 'small', type: r.handled ? 'success' : 'default' }, { default: () => (r.handled ? '已处置' : '待处理') }),
          r.handled ? h(NTag, { size: 'small', type: actionType(r.action) }, { default: () => r.action }) : null
        ]
      })
  },
  { title: '时间', key: 'created_at', width: 170, render: (r) => new Date(r.created_at).toLocaleString() },
  {
    title: '操作',
    key: 'actions',
    width: 200,
    render: (r) =>
      h(NSpace, { size: 4 }, {
        default: () => [
          h(NButton, { size: 'tiny', secondary: true, onClick: () => openDetail(r) }, { default: () => '详情' }),
          h(NButton, { size: 'tiny', secondary: true, type: 'error', disabled: r.handled, onClick: () => doBlock(r) }, { default: () => '封禁' }),
          h(NButton, { size: 'tiny', secondary: true, type: 'default', disabled: r.handled, onClick: () => doHandle(r, 'ignored') }, { default: () => '忽略' })
        ]
      })
  }
]

async function load() {
  loading.value = true
  try {
    const res = await listEvents({
      level: filter.level || undefined,
      handled: handledToBool(filter.handled),
      page: filter.page,
      size: filter.size
    })
    items.value = res.items
    total.value = res.total
  } catch {
    /* 拦截器已提示 */
  } finally {
    loading.value = false
  }
}

function openDetail(r: SecurityEvent) {
  current.value = r
  showDetail.value = true
}

function doHandle(r: SecurityEvent, action: 'blocked' | 'ignored') {
  dialog.warning({
    title: action === 'ignored' ? '忽略该事件' : '标记已处置',
    content: `确认将事件 #${r.id}（${r.rule_name}）标记为「${action}」？`,
    positiveText: '确认',
    onPositiveClick: async () => {
      try {
        await handleEvent(r.id, action)
        message.success('已处置')
        load()
      } catch {
        /* 忽略 */
      }
    }
  })
}

function doBlock(r: SecurityEvent) {
  dialog.warning({
    title: '一键封禁来源 IP',
    content: `将从事件 #${r.id} 的样本中提取来源 IP 并生成 security_block 变更单（走发布流水线，可回滚）。`,
    positiveText: '封禁',
    onPositiveClick: async () => {
      try {
        const co = await blockEvent(r.id)
        message.success(`已生成封禁变更单 #${co.id}`)
        load()
      } catch {
        /* 拦截器已提示 */
      }
    }
  })
}

async function doBlockIP() {
  if (!blockIPForm.ip) {
    message.warning('请输入要封禁的 IP')
    return
  }
  try {
    const co = await blockIP(blockIPForm.ip, blockIPForm.reason || '手动封禁')
    message.success(`已生成封禁变更单 #${co.id}`)
    blockIPForm.ip = ''
    blockIPForm.reason = ''
    load()
  } catch {
    /* 拦截器已提示 */
  }
}

function onPageChange(p: number) {
  filter.page = p
  load()
}

onMounted(load)
</script>

<template>
  <div class="alerts">
    <n-alert type="warning" title="封禁走发布流水线" :show-icon="true" class="top-note">
      封禁/解封均生成 security_block 变更单（LVS 优雅灰度 + 自动回滚），可在「发布任务」页跟踪与回滚；不旁路直改线上。
    </n-alert>

    <n-card size="small" title="直接封禁 IP" :bordered="true" class="block-panel">
      <n-space :size="10" align="center">
        <n-input v-model:value="blockIPForm.ip" placeholder="要封禁的 IP，如 203.0.113.45" style="width: 220px" />
        <n-input v-model:value="blockIPForm.reason" placeholder="原因（可选）" style="width: 220px" />
        <n-button type="error" @click="doBlockIP">封禁</n-button>
      </n-space>
    </n-card>

    <n-form inline label-placement="left" :show-feedback="false" class="filters">
      <n-space :size="10" align="center">
        <n-form-item label="级别">
          <n-select v-model:value="filter.level" :options="levelOptions" style="width: 130px" @update:value="load" />
        </n-form-item>
        <n-form-item label="状态">
          <n-select v-model:value="filter.handled" :options="handledOptions" style="width: 120px" @update:value="load" />
        </n-form-item>
        <n-form-item>
          <n-button @click="load">刷新</n-button>
        </n-form-item>
      </n-space>
    </n-form>

    <n-data-table
      :columns="columns"
      :data="items"
      :loading="loading"
      :row-key="(r: SecurityEvent) => r.id"
      size="small"
      striped
      :pagination="{ page: filter.page, pageSize: filter.size, itemCount: total, showSizePicker: false, onUpdatePage: onPageChange }"
    />

    <n-drawer v-model:show="showDetail" :width="560" placement="right">
      <n-drawer-content :title="current ? `#${current.id} ${current.rule_name}` : '事件详情'">
        <template v-if="current">
          <n-space vertical :size="8">
            <n-space :size="8" align="center">
              <n-tag :type="levelType(current.level)" size="small">{{ current.level }}</n-tag>
              <n-tag :type="current.handled ? 'success' : 'default'" size="small">{{ current.handled ? '已处置' : '待处理' }}</n-tag>
              <span class="muted">节点 {{ current.node }}</span>
            </n-space>
            <div class="muted">规则 ID：{{ current.rule_id }} ｜ 创建：{{ new Date(current.created_at).toLocaleString() }}</div>
            <div class="section-title">证据样本（文本展示，防 XSS）</div>
            <pre class="raw">{{ current.sample }}</pre>
            <n-space :size="8">
              <n-button
                v-if="!current.handled"
                size="small"
                type="error"
                @click="doBlock(current); showDetail = false"
              >封禁来源 IP</n-button>
              <n-button
                v-if="!current.handled"
                size="small"
                @click="doHandle(current, 'ignored'); showDetail = false"
              >忽略</n-button>
            </n-space>
          </n-space>
        </template>
      </n-drawer-content>
    </n-drawer>
  </div>
</template>

<style scoped>
.alerts {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.top-note {
  font-size: 12px;
}
.block-panel {
  background: rgba(0, 0, 0, 0.02);
}
.filters {
  margin-top: 4px;
}
.muted {
  color: #8a8f99;
  font-size: 13px;
}
.section-title {
  font-weight: 600;
  font-size: 14px;
}
.raw {
  white-space: pre-wrap;
  word-break: break-all;
  font-family: monospace;
  font-size: 12px;
  background: rgba(0, 0, 0, 0.03);
  padding: 10px;
  border-radius: 6px;
  max-height: 40vh;
  overflow: auto;
}
</style>
