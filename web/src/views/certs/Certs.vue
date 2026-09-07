<script setup lang="ts">
import { h, onMounted, ref } from 'vue'
import {
  NCard,
  NSpace,
  NButton,
  NDataTable,
  NTag,
  NModal,
  NForm,
  NFormItem,
  NInput,
  NAlert,
  NPopconfirm,
  NEmpty,
  NSpin,
  useMessage,
  type DataTableColumns
} from 'naive-ui'
import {
  listCerts,
  deleteCert,
  uploadCert,
  distributeCert,
  listNodesBrief,
  type CertView,
  type ValidationResult,
  type NodeBrief,
  type DistributeResult
} from '@/api/cert'

const message = useMessage()
const loading = ref(false)
const certs = ref<CertView[]>([])
const showUpload = ref(false)
const submitting = ref(false)
const form = ref({ domain: '', cert_pem: '', key_pem: '', chain_pem: '', issuer: '' })
const uploadErrors = ref<string[]>([])
const uploadWarnings = ref<string[]>([])

// T044 分发状态。
const showDistribute = ref(false)
const distributing = ref(false)
const distNodes = ref<NodeBrief[]>([])
const selectedNodeIds = ref<number[]>([])
const distResult = ref<DistributeResult | null>(null)
const currentCertId = ref<number | null>(null)

function expiryType(notAfter: string): 'success' | 'warning' | 'error' {
  const days = (new Date(notAfter).getTime() - Date.now()) / 86400000
  if (days < 7) return 'error'
  if (days < 30) return 'warning'
  return 'success'
}
function expiryText(notAfter: string): string {
  const days = Math.floor((new Date(notAfter).getTime() - Date.now()) / 86400000)
  if (days < 0) return `已过期 ${-days} 天`
  return `剩 ${days} 天`
}
const rowKey = (r: CertView) => r.id

const columns: DataTableColumns<CertView> = [
  { title: '主域名', key: 'domain', render: (r) => h('span', r.domain) },
  { title: 'SAN', key: 'sans', render: (r) => h('span', (r.sans || []).join(', ') || '-') },
  { title: '签发者', key: 'issuer' },
  { title: '算法', key: 'key_alg' },
  {
    title: '到期',
    key: 'not_after',
    render: (r) =>
      h(
        NTag,
        { type: expiryType(r.not_after), size: 'small', bordered: false },
        { default: () => expiryText(r.not_after) }
      )
  },
  {
    title: '来源',
    key: 'source',
    render: (r) =>
      h(
        NTag,
        { type: r.source === 'acme' ? 'info' : 'default', size: 'small', bordered: false },
        { default: () => (r.source === 'acme' ? 'ACME' : '上传') }
      )
  },
  {
    title: '操作',
    key: 'actions',
    render: (r) =>
      h(NSpace, { size: 4 }, () => [
        h(
          NButton,
          { size: 'small', tertiary: true, onClick: () => openDistribute(r) },
          { default: () => '分发' }
        ),
        h(
          NPopconfirm,
          { onPositiveClick: () => remove(r) },
          {
            trigger: () =>
              h(
                NButton,
                { size: 'small', type: 'error', quaternary: true },
                { default: () => '删除' }
              ),
            default: () => '确认删除该证书？私钥将一并销毁且不可恢复。'
          }
        )
      ])
  }
]

async function load() {
  loading.value = true
  try {
    certs.value = await listCerts()
  } finally {
    loading.value = false
  }
}

function resetForm() {
  form.value = { domain: '', cert_pem: '', key_pem: '', chain_pem: '', issuer: '' }
  uploadErrors.value = []
  uploadWarnings.value = []
}

async function submit() {
  if (!form.value.cert_pem.trim()) {
    message.warning('请粘贴证书 PEM')
    return
  }
  submitting.value = true
  uploadErrors.value = []
  uploadWarnings.value = []
  try {
    await uploadCert({
      domain: form.value.domain.trim() || undefined,
      cert_pem: form.value.cert_pem,
      key_pem: form.value.key_pem.trim() || undefined,
      chain_pem: form.value.chain_pem.trim() || undefined,
      issuer: form.value.issuer.trim() || undefined
    })
    message.success('证书已信封加密入库')
    showUpload.value = false
    resetForm()
    await load()
  } catch (e: any) {
    // 后端 6 项校验失败时 envelope.message 已含逐条原因（分号分隔），拦截层已弹一次；
    // 这里在表单区回显结构化结果，便于聚焦修改。
    const data = e?.response?.data?.data as ValidationResult | undefined
    if (data && (data.errors?.length || data.warnings?.length)) {
      uploadErrors.value = data.errors || []
      uploadWarnings.value = data.warnings || []
    } else {
      uploadErrors.value = [e?.response?.data?.message || e?.message || '上传失败']
    }
  } finally {
    submitting.value = false
  }
}

async function remove(r: CertView) {
  try {
    await deleteCert(r.id)
    message.success('已删除')
    await load()
  } catch (e: any) {
    message.error(e?.response?.data?.message || e?.message || '删除失败')
  }
}

// ===== T044 分发到节点 =====
async function openDistribute(r: CertView) {
  currentCertId.value = r.id
  selectedNodeIds.value = []
  distResult.value = null
  showDistribute.value = true
  try {
    distNodes.value = await listNodesBrief()
  } catch (e: any) {
    message.error(e?.response?.data?.message || e?.message || '获取节点列表失败')
  }
}

async function doDistribute() {
  if (currentCertId.value == null) return
  if (selectedNodeIds.value.length === 0) {
    message.warning('请至少选择一个目标节点')
    return
  }
  distributing.value = true
  distResult.value = null
  try {
    const res = await distributeCert(currentCertId.value, { node_ids: selectedNodeIds.value })
    distResult.value = res
    if (res.failed > 0) {
      message.warning(`分发完成：${res.deployed} 成功 / ${res.failed} 失败`)
    } else {
      message.success(`已分发到 ${res.deployed} 个节点`)
    }
  } catch (e: any) {
    message.error(e?.response?.data?.message || e?.message || '分发失败')
  } finally {
    distributing.value = false
  }
}

function resultTagType(status: string): 'success' | 'error' {
  return status === 'deployed' ? 'success' : 'error'
}

onMounted(load)
</script>

<template>
  <div class="page">
    <n-card :bordered="false" title="证书管理">
      <template #header-extra>
        <n-button type="primary" size="small" @click="showUpload = true">上传证书</n-button>
      </template>
      <n-spin :show="loading">
        <n-data-table
          v-if="certs.length"
          :columns="columns"
          :data="certs"
          :row-key="rowKey"
          :single-line="false"
        />
        <n-empty v-else description="暂无证书，点击右上角上传" />
      </n-spin>
    </n-card>

    <n-modal
      v-model:show="showUpload"
      title="上传证书"
      preset="card"
      style="width: 680px"
      :mask-closable="false"
    >
      <n-alert v-if="uploadErrors.length" type="error" title="校验未通过" style="margin-bottom: 12px">
        <ul style="margin: 0; padding-left: 18px">
          <li v-for="(e, i) in uploadErrors" :key="i">{{ e }}</li>
        </ul>
      </n-alert>
      <n-alert v-if="uploadWarnings.length" type="warning" title="警告" style="margin-bottom: 12px">
        <ul style="margin: 0; padding-left: 18px">
          <li v-for="(w, i) in uploadWarnings" :key="i">{{ w }}</li>
        </ul>
      </n-alert>
      <n-form label-placement="top">
        <n-form-item label="主域名（可选，留空自动取 SAN 首个）">
          <n-input v-model:value="form.domain" placeholder="example.com" />
        </n-form-item>
        <n-form-item label="证书 PEM（fullchain，含 leaf + intermediate）" required>
          <n-input
            v-model:value="form.cert_pem"
            type="textarea"
            :autosize="{ minRows: 5, maxRows: 12 }"
            placeholder="-----BEGIN CERTIFICATE-----"
          />
        </n-form-item>
        <n-form-item label="私钥 PEM（PKCS1 / PKCS8 / EC）">
          <n-input
            v-model:value="form.key_pem"
            type="textarea"
            :autosize="{ minRows: 4, maxRows: 12 }"
            placeholder="-----BEGIN PRIVATE KEY-----"
          />
        </n-form-item>
        <n-form-item label="中间证书链（可选，若未并入上方证书）">
          <n-input
            v-model:value="form.chain_pem"
            type="textarea"
            :autosize="{ minRows: 3, maxRows: 10 }"
            placeholder="-----BEGIN CERTIFICATE-----"
          />
        </n-form-item>
        <n-form-item label="签发者（可选，留空自动取）">
          <n-input v-model:value="form.issuer" placeholder="Let's Encrypt" />
        </n-form-item>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="showUpload = false">取消</n-button>
          <n-button type="primary" :loading="submitting" @click="submit">校验并入库</n-button>
        </n-space>
      </template>
    </n-modal>

    <n-modal
      v-model:show="showDistribute"
      title="分发证书到节点"
      preset="card"
      style="width: 640px"
      :mask-closable="false"
    >
      <n-alert type="info" title="安全说明" style="margin-bottom: 12px">
        私钥经 mTLS 下发到 Agent，仅在节点本地原子落盘（key 0600 / crt 0644），浏览器与控制面数据库均不留存明文。
      </n-alert>
      <n-form label-placement="top">
        <n-form-item label="目标节点（多选）">
          <n-select
            v-model:value="selectedNodeIds"
            multiple
            :options="distNodes.map((n) => ({ label: `${n.name} (${n.address})`, value: n.id }))"
            placeholder="选择要下发证书的节点"
          />
        </n-form-item>
      </n-form>

      <n-alert
        v-if="distResult"
        :type="distResult.failed > 0 ? 'warning' : 'success'"
        :title="`分发结果：成功 ${distResult.deployed} / 失败 ${distResult.failed} / 共 ${distResult.total}`"
        style="margin-bottom: 12px"
      >
        <n-space vertical :size="4">
          <div v-for="(it, i) in distResult.items" :key="i" style="display: flex; gap: 8px; align-items: center">
            <n-tag :type="resultTagType(it.status)" size="small" :bordered="false">{{ it.status }}</n-tag>
            <span>{{ it.node_name || ('节点#' + it.node_id) }}</span>
            <span v-if="it.error" style="color: #d03050">{{ it.error }}</span>
          </div>
        </n-space>
      </n-alert>

      <template #footer>
        <n-space justify="end">
          <n-button @click="showDistribute = false">关闭</n-button>
          <n-button type="primary" :loading="distributing" @click="doDistribute">下发</n-button>
        </n-space>
      </template>
    </n-modal>
  </div>
</template>

<style scoped>
.page {
  padding: 20px;
}
</style>
