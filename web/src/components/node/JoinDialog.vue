<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import {
  NAlert,
  NButton,
  NForm,
  NFormItem,
  NInput,
  NInputGroup,
  NModal,
  NSelect,
  NSpace,
  NTag,
  useMessage
} from 'naive-ui'
import {
  createNode,
  getBootstrapInfo,
  issueJoinToken,
  type NodeOut,
  type NodeRole
} from '@/api/nodes'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{
  (e: 'update:show', v: boolean): void
  (e: 'created', node: NodeOut): void
}>()

const message = useMessage()

const form = reactive({
  name: '',
  address: '',
  role: 'real_server' as NodeRole,
  ttl: '24h'
})
const submitting = ref(false)
const token = ref('')
const nodeId = ref<number | null>(null)
const expiresAt = ref('')
const origin = ref('')
const grpcAddr = ref('')
const binaryReady = ref(true)

const roleOptions = [
  { label: 'Nginx 真实服务器 (RS)', value: 'real_server' },
  { label: 'LVS Director', value: 'director' },
  { label: '未知（待识别）', value: 'unknown' }
]

const ttlOptions = [
  { label: '1 小时', value: '1h' },
  { label: '24 小时', value: '24h' },
  { label: '7 天', value: '168h' }
]

// 引导信息拉取失败时的兜底：沿用服务端 grpcPublicAddr 的默认端口 9443。
const fallbackOrigin = window.location.origin
const fallbackGrpc = window.location.hostname + ':9443'
const cpAddr = computed(() => origin.value || fallbackOrigin)
const grpc = computed(() => grpcAddr.value || fallbackGrpc)

// 一键安装命令：拉取引导 CA → 下载 Agent 二进制 → 写 systemd → 自注册上线（无审批）。
const command = computed(() => {
  if (!token.value) return ''
  return `curl -fsSL ${cpAddr.value}/agent/install.sh | sudo bash -s -- --cp ${cpAddr.value} --grpc ${grpc.value} --token ${token.value}`
})

// 弹窗打开时拉取引导信息，使命令自带正确的控制面地址与 gRPC 端口。
watch(
  () => props.show,
  async (v) => {
    if (!v) return
    try {
      const info = await getBootstrapInfo()
      origin.value = info.origin
      grpcAddr.value = info.grpc_addr
      binaryReady.value = info.binary_ready
    } catch {
      binaryReady.value = true // 仅用于拼命令，失败时静默回退到本地推导
    }
  }
)

function close() {
  emit('update:show', false)
}

async function submit() {
  if (!form.name) {
    message.warning('请填写节点名称')
    return
  }
  submitting.value = true
  try {
    const node = await createNode({ name: form.name, address: form.address, role: form.role })
    const tk = await issueJoinToken(node.id, form.ttl)
    token.value = tk.token
    expiresAt.value = tk.expires_at
    nodeId.value = node.id
    message.success('节点已登记并签发接入令牌')
    emit('created', node)
  } finally {
    submitting.value = false
  }
}

async function copy() {
  if (!command.value) return
  try {
    await navigator.clipboard.writeText(command.value)
    message.success('安装命令已复制')
  } catch {
    message.warning('复制失败，请手动选中命令复制')
  }
}

function reset() {
  form.name = ''
  form.address = ''
  form.role = 'real_server'
  form.ttl = '24h'
  token.value = ''
  expiresAt.value = ''
  nodeId.value = null
}
</script>

<template>
  <n-modal
    :show="props.show"
    preset="card"
    title="添加节点"
    style="width: 680px"
    @update:show="(v: boolean) => emit('update:show', v)"
    @after-leave="reset"
  >
    <n-form v-if="!token" :model="form" label-placement="top">
      <n-form-item label="节点名称（唯一标识）" required>
        <n-input v-model:value="form.name" placeholder="如 nginx-rs-01" />
      </n-form-item>
      <n-form-item label="管理地址（可选）">
        <n-input v-model:value="form.address" placeholder="如 192.168.5.7:22" />
      </n-form-item>
      <n-form-item label="节点角色">
        <n-select v-model:value="form.role" :options="roleOptions" />
      </n-form-item>
      <n-form-item label="令牌有效期（该窗口内须执行安装）">
        <n-select v-model:value="form.ttl" :options="ttlOptions" />
      </n-form-item>
      <n-space justify="end">
        <n-button @click="close">取消</n-button>
        <n-button type="primary" :loading="submitting" @click="submit">
          登记并生成安装命令
        </n-button>
      </n-space>
    </n-form>

    <div v-else>
      <n-space vertical :size="12">
        <n-alert v-if="!binaryReady" type="warning" title="控制面未启用 Agent 二进制分发">
          节点执行到「[2/5] 下载 Agent 二进制」会 404。请在控制面宿主机执行：
          <code>NGXCP_DEPLOY_HOST=root@&lt;控制面&gt; bash scripts/enable-agent-dist.sh</code>
        </n-alert>

        <div>
          在目标节点以 <strong>root</strong> 执行（复制后直接粘贴即可）：
        </div>
        <n-input-group>
          <n-input :value="command" readonly />
          <n-button type="primary" @click="copy">复制</n-button>
        </n-input-group>

        <n-space :size="8">
          <n-tag size="small" type="info">节点 ID：{{ nodeId }}</n-tag>
          <n-tag size="small">有效期至：{{ expiresAt || '—' }}</n-tag>
          <n-tag size="small">gRPC：{{ grpc }}</n-tag>
        </n-space>

        <div style="opacity: 0.75; font-size: 13px">
          脚本会自动拉取引导 CA、下载对应架构的 Agent 二进制、写入 systemd 并以 Join Token 自注册，
          无需审批即上线。纳管后凭同一令牌可重建客户端证书。
        </div>

        <n-space justify="end">
          <n-button @click="copy">复制命令</n-button>
          <n-button type="primary" @click="close">完成</n-button>
        </n-space>
      </n-space>
    </div>
  </n-modal>
</template>
