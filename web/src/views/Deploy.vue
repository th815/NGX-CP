<script setup lang="ts">
import { h, onBeforeUnmount, reactive, ref } from 'vue'
import {
  NButton,
  NDataTable,
  NDrawer,
  NDrawerContent,
  NForm,
  NFormItem,
  NInput,
  NModal,
  NSelect,
  NSpace,
  NTag,
  NTimeline,
  NTimelineItem,
  NEmpty,
  useMessage,
  useDialog,
  type DataTableColumns
} from 'naive-ui'
import {
  listChangeOrders,
  getChangeOrder,
  createChangeOrder,
  submitChangeOrder,
  approveChangeOrder,
  rejectChangeOrder,
  cancelChangeOrder,
  rollbackChangeOrder,
  getApprovalForOrder,
  type ChangeOrderView,
  type OrderStatus
} from '@/api/deploy'

const message = useMessage()
const dialog = useDialog()

const orders = ref<ChangeOrderView[]>([])
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    orders.value = await listChangeOrders()
  } catch {
    /* 错误由拦截器统一提示 */
  } finally {
    loading.value = false
  }
}
load()

// 状态 → 中文 + 标签色
const statusMeta: Record<OrderStatus, { type: 'default' | 'info' | 'success' | 'warning' | 'error'; text: string }> = {
  draft: { type: 'default', text: '草稿' },
  pending_approval: { type: 'warning', text: '待审批' },
  pending: { type: 'info', text: '待执行' },
  running: { type: 'info', text: '执行中' },
  success: { type: 'success', text: '成功' },
  failed: { type: 'error', text: '失败' },
  rolling_back: { type: 'warning', text: '回滚中' },
  rolled_back: { type: 'default', text: '已回滚' },
  partial_success: { type: 'warning', text: '部分成功' },
  rejected: { type: 'error', text: '已拒绝' },
  canceled: { type: 'default', text: '已取消' }
}

const columns: DataTableColumns<ChangeOrderView> = [
  { title: 'ID', key: 'id', width: 70 },
  { title: '标题', key: 'title' },
  {
    title: '类型',
    key: 'type',
    width: 100,
    render: (row) => h(NTag, { size: 'small' }, { default: () => row.type })
  },
  {
    title: '状态',
    key: 'status',
    width: 100,
    render: (row) => {
      const m = statusMeta[row.status]
      return h(NTag, { type: m.type, size: 'small' }, { default: () => m.text })
    }
  },
  { title: '目标节点', key: 'target_nodes', width: 120, render: (row) => row.target_nodes.join(', ') },
  { title: '提交人', key: 'created_by', width: 100 },
  { title: '创建时间', key: 'created_at', width: 180 },
  {
    title: '操作',
    key: 'actions',
    width: 90,
    render: (row) =>
      h(NButton, { size: 'small', onClick: () => openDetail(row) }, { default: () => '详情' })
  }
]

// ---- 创建弹窗 ----
const showCreate = ref(false)
const form = reactive({ title: '', type: 'config', target_nodes: '', created_by: 'admin', comment: '' })
const typeOptions = [
  { label: '配置变更', value: 'config' },
  { label: 'LVS 变更', value: 'lvs' },
  { label: '二进制升级', value: 'upgrade' },
  { label: '证书续期', value: 'cert_renew' }
]

async function doCreate() {
  if (!form.title) {
    message.warning('请填写标题')
    return
  }
  const nodes = form.target_nodes
    .split(/[,\s]+/)
    .map((s) => parseInt(s, 10))
    .filter((n) => !Number.isNaN(n))
  try {
    await createChangeOrder({
      title: form.title,
      type: form.type,
      target_nodes: nodes,
      created_by: form.created_by,
      comment: form.comment
    })
    message.success('已创建变更单')
    showCreate.value = false
    form.title = ''
    form.target_nodes = ''
    form.comment = ''
    load()
  } catch {
    /* 拦截器已提示 */
  }
}

// ---- 详情抽屉 + SSE 实时进度 ----
const showDetail = ref(false)
const current = ref<ChangeOrderView | null>(null)
const approval = ref<{ status: string; approver?: string; comment?: string } | null>(null)
const events = ref<{ step: string; status: string; message: string; timestamp: number }[]>([])
let es: EventSource | null = null

async function openDetail(row: ChangeOrderView) {
  current.value = row
  events.value = []
  showDetail.value = true
  try {
    approval.value = await getApprovalForOrder(row.id)
  } catch {
    approval.value = null
  }
  connectStream(row.id)
}

function connectStream(id: number) {
  if (es) es.close()
  es = new EventSource(`/api/v1/change-orders/${id}/stream`)
  es.onmessage = (e) => {
    try {
      const d = JSON.parse(e.data)
      events.value.push({ step: d.step, status: d.status, message: d.message, timestamp: d.timestamp })
    } catch {
      /* 忽略非 JSON 帧 */
    }
  }
  // 连接断开由浏览器自动重连（Last-Event-ID 由服务端支持时补帧）
}

onBeforeUnmount(() => {
  if (es) es.close()
})

async function refresh() {
  if (!current.value) return
  try {
    const r = await getChangeOrder(current.value.id)
    current.value = r
    await load()
  } catch {
    /* 忽略 */
  }
}

async function doSubmit() {
  if (!current.value) return
  try {
    const r = await submitChangeOrder(current.value.id)
    message.success(r.approval_required ? '已提交，等待审批' : '已提交')
    refresh()
  } catch {
    /* 忽略 */
  }
}
async function doApprove() {
  if (!current.value) return
  try {
    await approveChangeOrder(current.value.id, 'admin')
    message.success('已批准')
    refresh()
  } catch {
    /* 忽略 */
  }
}
async function doReject() {
  if (!current.value) return
  try {
    await rejectChangeOrder(current.value.id)
    message.success('已拒绝')
    refresh()
  } catch {
    /* 忽略 */
  }
}
async function doCancel() {
  if (!current.value) return
  try {
    await cancelChangeOrder(current.value.id)
    message.success('已取消')
    refresh()
  } catch {
    /* 忽略 */
  }
}
function doRollback() {
  if (!current.value) return
  dialog.warning({
    title: '发起回滚',
    content: '确认对该变更单发起回滚？',
    positiveText: '回滚',
    onPositiveClick: async () => {
      try {
        await rollbackChangeOrder(current.value!.id)
        message.success('已发起回滚')
        refresh()
      } catch {
        /* 忽略 */
      }
    }
  })
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">发布任务</h2>
        <p class="page-sub">把配置变更做成「可校验、可灰度、可观测、可回滚」的流水线。</p>
      </div>
      <n-button type="primary" @click="showCreate = true">新建变更单</n-button>
    </div>

    <n-data-table
      :columns="columns"
      :data="orders"
      :loading="loading"
      :row-key="(row: ChangeOrderView) => row.id"
      size="small"
      striped
    />

    <!-- 创建弹窗 -->
    <n-modal
      v-model:show="showCreate"
      title="新建变更单"
      preset="card"
      style="width: 520px"
    >
      <n-form label-placement="top">
        <n-form-item label="标题">
          <n-input v-model:value="form.title" placeholder="例如：更新 upstream 后端列表" />
        </n-form-item>
        <n-form-item label="类型">
          <n-select v-model:value="form.type" :options="typeOptions" />
        </n-form-item>
        <n-form-item label="目标节点（逗号或空格分隔的 ID）">
          <n-input v-model:value="form.target_nodes" placeholder="8, 9" />
        </n-form-item>
        <n-form-item label="提交人">
          <n-input v-model:value="form.created_by" />
        </n-form-item>
        <n-form-item label="备注">
          <n-input v-model:value="form.comment" type="textarea" :autosize="{ minRows: 2 }" />
        </n-form-item>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="showCreate = false">取消</n-button>
          <n-button type="primary" @click="doCreate">创建</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 详情抽屉 -->
    <n-drawer v-model:show="showDetail" :width="520" placement="right">
      <n-drawer-content :title="current ? `#${current.id} ${current.title}` : '变更单详情'">
        <template v-if="current">
          <n-space vertical :size="10">
            <n-space :size="8" align="center">
              <n-tag :type="statusMeta[current.status].type" size="small">
                {{ statusMeta[current.status].text }}
              </n-tag>
              <span class="muted">类型 {{ current.type }}</span>
              <span class="muted">目标节点 {{ current.target_nodes.join(', ') }}</span>
            </n-space>
            <div class="muted">提交人：{{ current.created_by }} ｜ 创建：{{ current.created_at }}</div>
            <div v-if="approval" class="muted">
              审批：{{ approval.status }}<template v-if="approval.approver">（{{ approval.approver }}）</template>
            </div>

            <n-space :size="8">
              <n-button
                v-if="current.status === 'draft'"
                type="primary"
                size="small"
                @click="doSubmit"
              >提交</n-button>
              <n-button
                v-if="current.status === 'pending_approval'"
                type="primary"
                size="small"
                @click="doApprove"
              >批准</n-button>
              <n-button
                v-if="current.status === 'pending_approval'"
                size="small"
                @click="doReject"
              >拒绝</n-button>
              <n-button
                v-if="current.status === 'pending' || current.status === 'failed'"
                size="small"
                @click="doCancel"
              >取消</n-button>
              <n-button
                v-if="current.status === 'running' || current.status === 'failed' || current.status === 'partial_success'"
                size="small"
                @click="doRollback"
              >回滚</n-button>
            </n-space>

            <div class="section-title">实时进度（SSE）</div>
            <n-empty v-if="events.length === 0" description="暂无事件" size="small" />
            <n-timeline v-else>
              <n-timeline-item
                v-for="(ev, i) in events"
                :key="i"
                :type="ev.status === 'failed' ? 'error' : ev.status === 'success' ? 'success' : 'info'"
                :title="`${ev.step} · ${ev.status}`"
                :content="ev.message"
                :time="new Date(ev.timestamp).toLocaleTimeString()"
              />
            </n-timeline>
          </n-space>
        </template>
      </n-drawer-content>
    </n-drawer>
  </div>
</template>

<style scoped>
.page {
  padding: 18px 22px;
}
.page-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 16px;
}
.page-title {
  margin: 0;
  font-size: 20px;
  font-weight: 700;
}
.page-sub {
  margin: 4px 0 0;
  color: #8a8f99;
  font-size: 13px;
}
.muted {
  color: #8a8f99;
  font-size: 13px;
}
.section-title {
  margin-top: 12px;
  font-weight: 600;
  font-size: 14px;
}
</style>
