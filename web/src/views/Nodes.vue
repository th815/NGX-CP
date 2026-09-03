<script setup lang="ts">
import { h, onMounted, reactive, ref, computed } from 'vue'
import {
  NCard,
  NSpace,
  NGrid,
  NGi,
  NButton,
  NSelect,
  NDrawer,
  NDrawerContent,
  NTabs,
  NTabPane,
  NDescriptions,
  NDescriptionsItem,
  NDataTable,
  NTag,
  NEmpty,
  NSpin,
  NText,
  useMessage,
  type DataTableColumns
} from 'naive-ui'
import NodeCard from '@/components/node/NodeCard.vue'
import CapabilityPanel from '@/components/node/CapabilityPanel.vue'
import CompliancePanel from '@/components/node/CompliancePanel.vue'
import EnrollDialog from '@/components/node/EnrollDialog.vue'
import {
  listNodes,
  getNode,
  getCapability,
  getConfigFiles,
  getLogTargets,
  refreshCapability,
  type NodeOut,
  type CapabilityView,
  type ConfigFileView,
  type LogTargetView,
  type NodeRole,
  type NodeStatus
} from '@/api/nodes'

const message = useMessage()

const nodes = ref<NodeOut[]>([])
const versions = ref<Record<number, string>>({})
const loading = ref(false)

const filters = reactive<{ role: string | null; status: string | null }>({ role: null, status: null })

const roleOptions = [
  { label: '全部角色', value: '' },
  { label: 'Nginx RS', value: 'real_server' },
  { label: 'LVS Director', value: 'director' },
  { label: '未知', value: 'unknown' }
]
const statusOptions = [
  { label: '全部状态', value: '' },
  { label: '在线', value: 'online' },
  { label: '离线', value: 'offline' },
  { label: '异常', value: 'degraded' },
  { label: '接入中', value: 'enrolling' }
]

const showEnroll = ref(false)
const drawerOpen = ref(false)
const detailId = ref<number | null>(null)
const detailLoading = ref(false)
const detailNode = ref<NodeOut | null>(null)
const capability = ref<CapabilityView | null>(null)
const configFiles = ref<ConfigFileView[]>([])
const logTargets = ref<LogTargetView[]>([])

const fmtTime = (t?: string) => (t ? new Date(t).toLocaleString() : '—')

async function loadNodes() {
  loading.value = true
  try {
    const { items } = await listNodes({
      role: filters.role || undefined,
      status: filters.status || undefined
    })
    nodes.value = items
    // 并行拉取能力基线以在卡片展示 nginx 版本（4 节点规模可接受）。
    const caps = await Promise.all(items.map((n) => getCapability(n.id).catch(() => null)))
    const vmap: Record<number, string> = {}
    items.forEach((n, i) => {
      const c = caps[i]
      if (c?.nginx?.version) vmap[n.id] = c.nginx.version
    })
    versions.value = vmap
  } catch {
    // 全局拦截器已提示
  } finally {
    loading.value = false
  }
}

async function openDetail(id: number) {
  detailId.value = id
  drawerOpen.value = true
  detailLoading.value = true
  detailNode.value = null
  capability.value = null
  configFiles.value = []
  logTargets.value = []
  try {
    const [node, cap, cf, lt] = await Promise.all([
      getNode(id),
      getCapability(id).catch(() => null),
      getConfigFiles(id).catch(() => []),
      getLogTargets(id).catch(() => [])
    ])
    detailNode.value = node
    capability.value = cap
    configFiles.value = cf
    logTargets.value = lt
  } catch {
    // 全局拦截器已提示
  } finally {
    detailLoading.value = false
  }
}

async function onRefresh() {
  if (!detailId.value) return
  try {
    await refreshCapability(detailId.value)
    message.success('已触发能力刷新')
    await openDetail(detailId.value)
  } catch {
    // ignore
  }
}

const configColumns = computed<DataTableColumns<ConfigFileView>>(() => [
  { title: '路径', key: 'path', minWidth: 240 },
  { title: '大小(B)', key: 'size', width: 110, render: (row) => row.size.toLocaleString() },
  {
    title: 'SHA256',
    key: 'sha256',
    render: (row) => h('span', { class: 'mono' }, row.sha256.slice(0, 16) + '…')
  },
  { title: '修改时间', key: 'mod_time', width: 170, render: (row) => fmtTime(row.mod_time) }
])

const logColumns = computed<DataTableColumns<LogTargetView>>(() => [
  { title: '路径', key: 'path', minWidth: 240 },
  {
    title: '类型',
    key: 'type',
    width: 120,
    render: (row) => h(NTag, { size: 'small', bordered: false, type: row.is_off ? 'default' : 'info' }, { default: () => row.type })
  },
  { title: '格式', key: 'format', width: 100, render: (row) => row.format || '—' },
  {
    title: '采集',
    key: 'collect',
    width: 90,
    render: (row) => (row.is_off ? '关闭' : row.is_syslog ? 'syslog' : 'tail')
  },
  { title: '跳过原因', key: 'skip', render: (row) => row.skip_reason || '—' }
])

// h 已在顶部导入，供 render 使用

const system = computed(() => capability.value?.system)
const diskFree = computed(() => {
  const d = system.value?.disk_free
  if (!d) return []
  return Object.entries(d).map(([k, v]) => ({ path: k, freeGB: (v / 1024 / 1024 / 1024).toFixed(1) }))
})

onMounted(loadNodes)
</script>

<template>
  <div class="page">
    <n-card :bordered="false" class="head">
      <n-space justify="space-between" align="center">
        <div>
          <div class="title">节点管理</div>
          <div class="sub">共 {{ nodes.length }} 个节点 · 探针心跳维持 online / 超时转 offline</div>
        </div>
        <n-space>
          <n-select v-model:value="filters.role" :options="roleOptions" style="width: 150px" />
          <n-select v-model:value="filters.status" :options="statusOptions" style="width: 140px" />
          <n-button tertiary @click="loadNodes">刷新</n-button>
          <n-button type="primary" @click="showEnroll = true">添加节点</n-button>
        </n-space>
      </n-space>
    </n-card>

    <div class="list">
      <n-spin :show="loading">
        <n-empty v-if="!loading && nodes.length === 0" description="暂无节点，点击「添加节点」开始接入" />
        <n-grid v-else :cols="3" :x-gap="16" :y-gap="16" responsive="screen" item-responsive>
          <n-gi v-for="n in nodes" :key="n.id" span="24 m:12 l:8">
            <node-card :node="n" :nginx-version="versions[n.id]" @open="openDetail" />
          </n-gi>
        </n-grid>
      </n-spin>
    </div>

    <n-drawer v-model:show="drawerOpen" :width="640" placement="right">
      <n-drawer-content :title="detailNode ? detailNode.name : '节点详情'" closable>
        <n-spin :show="detailLoading">
          <template v-if="detailNode">
            <n-tabs type="line" animated>
              <!-- 概览 -->
              <n-tab-pane name="overview" tab="概览">
                <n-descriptions bordered :column="2" size="small">
                  <n-descriptions-item label="ID">{{ detailNode.id }}</n-descriptions-item>
                  <n-descriptions-item label="角色">{{ detailNode.role }}</n-descriptions-item>
                  <n-descriptions-item label="状态">{{ detailNode.status }}</n-descriptions-item>
                  <n-descriptions-item label="地址">{{ detailNode.address || '—' }}</n-descriptions-item>
                  <n-descriptions-item label="LVS 权重">{{ detailNode.lvs_weight }}</n-descriptions-item>
                  <n-descriptions-item label="LVS 启用">{{ detailNode.lvs_enabled ? '是' : '否' }}</n-descriptions-item>
                  <n-descriptions-item label="最后心跳">{{ fmtTime(detailNode.last_heartbeat_at) }}</n-descriptions-item>
                  <n-descriptions-item label="时钟偏差">
                    {{ detailNode.clock_skew_seconds !== undefined ? detailNode.clock_skew_seconds.toFixed(1) + 's' : '—' }}
                  </n-descriptions-item>
                </n-descriptions>
                <n-button size="small" style="margin-top: 12px" @click="onRefresh">触发能力刷新</n-button>
              </n-tab-pane>

              <!-- 能力基线 -->
              <n-tab-pane name="capability" tab="能力基线">
                <capability-panel :capability="capability" />
              </n-tab-pane>

              <!-- 配置树 -->
              <n-tab-pane name="config" tab="配置树">
                <n-data-table
                  v-if="configFiles.length"
                  :columns="configColumns"
                  :data="configFiles"
                  :row-key="(r: ConfigFileView) => r.path"
                  size="small"
                />
                <n-empty v-else description="暂无配置树快照（Agent 未上报 nginx -T）" />
              </n-tab-pane>

              <!-- 日志路径 -->
              <n-tab-pane name="logs" tab="日志路径">
                <n-data-table
                  v-if="logTargets.length"
                  :columns="logColumns"
                  :data="logTargets"
                  :row-key="(r: LogTargetView) => r.path"
                  size="small"
                />
                <n-empty v-else description="暂无日志采集目标" />
              </n-tab-pane>

              <!-- DR 合规 -->
              <n-tab-pane name="compliance" tab="DR 合规">
                <compliance-panel :compliance="detailNode.compliance" :fs-probe="detailNode.fs_probe" />
              </n-tab-pane>

              <!-- 系统信息 -->
              <n-tab-pane name="system" tab="系统信息">
                <n-descriptions v-if="system" bordered :column="2" size="small">
                  <n-descriptions-item label="OS">{{ system.os || '—' }}</n-descriptions-item>
                  <n-descriptions-item label="内核">{{ system.kernel || '—' }}</n-descriptions-item>
                  <n-descriptions-item label="Nginx 托管">{{ system.nginx_managed_by || '—' }}</n-descriptions-item>
                  <n-descriptions-item label="SELinux">{{ system.selinux_status || '—' }}</n-descriptions-item>
                  <n-descriptions-item label="ulimit nofile">{{ system.ulimit_nofile ?? '—' }}</n-descriptions-item>
                  <n-descriptions-item label="NTP 同步">{{ system.ntp_synced ? '已同步' : '未同步' }}</n-descriptions-item>
                  <n-descriptions-item label="时区">{{ system.timezone || '—' }}</n-descriptions-item>
                  <n-descriptions-item label="logrotate">{{ system.logrotate_conf || '—' }}</n-descriptions-item>
                </n-descriptions>
                <n-data-table
                  v-if="diskFree.length"
                  :columns="[{ title: '挂载点', key: 'path' }, { title: '可用(GB)', key: 'freeGB' }]"
                  :data="diskFree"
                  size="small"
                  style="margin-top: 12px"
                />
                <n-space v-if="system?.warnings?.length" vertical style="margin-top: 12px">
                  <n-tag v-for="w in system.warnings" :key="w" type="warning" :bordered="false">{{ w }}</n-tag>
                </n-space>
                <n-empty v-if="!system" description="暂无系统信息（Agent 未上报）" />
              </n-tab-pane>
            </n-tabs>
          </template>
        </n-spin>
      </n-drawer-content>
    </n-drawer>

    <enroll-dialog v-model:show="showEnroll" @created="loadNodes" />
  </div>
</template>

<style scoped>
.page {
  padding: 20px;
}
.head {
  margin-bottom: 16px;
}
.title {
  font-size: 18px;
  font-weight: 700;
}
.sub {
  font-size: 13px;
  color: #8a8f99;
  margin-top: 4px;
}
.list {
  min-height: 200px;
}
.mono {
  font-family: monospace;
  font-size: 12px;
}
</style>
