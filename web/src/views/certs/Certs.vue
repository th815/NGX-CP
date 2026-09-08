<script setup lang="ts">
import { h, onMounted, ref, type VNodeChild } from 'vue'
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
  issueACME,
  renewCert,
  type CertView,
  type ValidationResult,
  type NodeBrief,
  type DistributeResult,
  type IssueACMEIn
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

// T045 ACME 签发弹窗状态。
interface IssueFormModel {
  domains: string[]
  email: string
  provider_type: string
  provider_token: string
  key_alg: string
  ca_dir_url: string
}
const showIssue = ref(false)
const issuing = ref(false)
const issueErrors = ref<string[]>([])
const issueForm = ref<IssueFormModel>({
  domains: [],
  email: '',
  provider_type: 'cloudflare',
  provider_token: '',
  key_alg: 'rsa2048',
  ca_dir_url: ''
})
const issueDomainsText = ref('')

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
    render: (r) => {
      const children: VNodeChild[] = []
      if (r.source === 'acme') {
        children.push(
          h(
            NButton,
            { size: 'small', type: 'warning', tertiary: true, onClick: () => renew(r) },
            { default: () => '续期' }
          )
        )
      }
      children.push(
        h(
          NButton,
          { size: 'small', tertiary: true, onClick: () => openDistribute(r) },
          { default: () => '分发' }
        )
      )
      children.push(
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
      )
      return h(NSpace, { size: 4 }, () => children)
    }
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

// ===== T045 ACME 签发 / 续期 =====
function resetIssueForm() {
  issueForm.value = {
    domains: [],
    email: '',
    provider_type: 'cloudflare',
    provider_token: '',
    key_alg: 'rsa2048',
    ca_dir_url: ''
  }
  issueDomainsText.value = ''
  issueErrors.value = []
}

function openIssue() {
  resetIssueForm()
  showIssue.value = true
}

async function doIssue() {
  const domains = issueDomainsText.value
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean)
  if (domains.length === 0) {
    message.warning('请至少填写一个域名')
    return
  }
  if (!issueForm.value.email.trim()) {
    message.warning('ACME 账户邮箱必填')
    return
  }
  if (!issueForm.value.provider_token.trim()) {
    message.warning('DNS-01 provider Token 必填')
    return
  }
  issuing.value = true
  issueErrors.value = []
  try {
    const payload: IssueACMEIn = {
      domains,
      email: issueForm.value.email.trim(),
      provider_type: issueForm.value.provider_type.trim() || 'cloudflare',
      provider_token: issueForm.value.provider_token,
      key_alg: issueForm.value.key_alg,
      ca_dir_url: issueForm.value.ca_dir_url.trim() || undefined
    }
    const view = await issueACME(payload)
    message.success(`证书已签发并入库（#${view.id}，${view.domain}）`)
    showIssue.value = false
    await load()
  } catch (e: any) {
    issueErrors.value = [e?.response?.data?.message || e?.message || '签发失败']
  } finally {
    issuing.value = false
  }
}

async function renew(r: CertView) {
  try {
    await renewCert(r.id)
    message.success(`已触发续期（#${r.id}），续期后将自动走发布流水线重分发到历史节点`)
    await load()
  } catch (e: any) {
    message.error(e?.response?.data?.message || e?.message || '续期失败')
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
        <n-space size="small">
          <n-button size="small" @click="openIssue">签发 ACME</n-button>
          <n-button type="primary" size="small" @click="showUpload = true">上传证书</n-button>
        </n-space>
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

    <n-modal
      v-model:show="showIssue"
      title="ACME 签发（DNS-01）"
      preset="card"
      style="width: 680px"
      :mask-closable="false"
    >
      <n-alert type="info" title="安全说明" style="margin-bottom: 12px">
        provider Token 仅在本次请求经 KMS 信封加密存储，控制面数据库与控制面 API 响应均不留存明文；续期将自动复用该 Token 与账户密钥。
      </n-alert>
      <n-alert v-if="issueErrors.length" type="error" title="签发失败" style="margin-bottom: 12px">
        <ul style="margin: 0; padding-left: 18px">
          <li v-for="(e, i) in issueErrors" :key="i">{{ e }}</li>
        </ul>
      </n-alert>
      <n-form label-placement="top">
        <n-form-item label="域名（逗号或空格分隔，支持 *.example.com 通配符）" required>
          <n-input
            v-model:value="issueDomainsText"
            type="textarea"
            :autosize="{ minRows: 2, maxRows: 4 }"
            placeholder="example.com, *.example.com"
          />
        </n-form-item>
        <n-form-item label="ACME 账户邮箱（必填）" required>
          <n-input v-model:value="issueForm.email" placeholder="admin@example.com" />
        </n-form-item>
        <n-form-item label="DNS-01 Provider 类型" required>
          <n-select
            v-model:value="issueForm.provider_type"
            :options="[
              { label: 'Cloudflare', value: 'cloudflare' },
              { label: '其他厂商（规划中，暂不可用）', value: 'coming-soon', disabled: true }
            ]"
          />
        </n-form-item>
        <n-form-item label="Provider API Token（必填）" required>
          <n-input
            v-model:value="issueForm.provider_token"
            type="password"
            show-password-on="click"
            placeholder="DNS provider 的 API Token"
          />
        </n-form-item>
        <n-space :size="12">
          <n-form-item label="密钥算法" style="flex: 1">
            <n-select
              v-model:value="issueForm.key_alg"
              :options="[
                { label: 'RSA-2048', value: 'rsa2048' },
                { label: 'ECDSA-P256', value: 'ecdsa256' }
              ]"
            />
          </n-form-item>
          <n-form-item label="CA 目录 URL（可选）" style="flex: 2">
            <n-input
              v-model:value="issueForm.ca_dir_url"
              placeholder="留空 = Let's Encrypt 生产；可填 staging 调试"
            />
          </n-form-item>
        </n-space>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="showIssue = false">取消</n-button>
          <n-button type="primary" :loading="issuing" @click="doIssue">签发并入库</n-button>
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
