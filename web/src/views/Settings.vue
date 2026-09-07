<script setup lang="ts">
import { onMounted, ref } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NDescriptions,
  NDescriptionsItem,
  NForm,
  NFormItem,
  NInput,
  NSpace,
  NTag,
  useMessage
} from 'naive-ui'
import { useAppStore } from '@/stores/app'
import { getBootstrapInfo, listNodes } from '@/api/nodes'

const app = useAppStore()
const message = useMessage()

const token = ref(app.token)
const checking = ref(false)
const origin = window.location.origin

interface CheckResult {
  ok: boolean
  msg: string
  grpc?: string
  binaryReady?: boolean
  nodes?: number
}
const result = ref<CheckResult | null>(null)

function save() {
  app.setToken(token.value.trim())
  message.success('令牌已保存')
}

// 自检：拉引导信息 + 列节点，验证「控制面可达 + 令牌有效 + 二进制分发就绪」三件事。
async function check() {
  checking.value = true
  result.value = null
  try {
    const info = await getBootstrapInfo()
    const { items } = await listNodes()
    result.value = {
      ok: true,
      msg: '控制面可达，令牌有效',
      grpc: info.grpc_addr,
      binaryReady: info.binary_ready,
      nodes: items.length
    }
  } catch (e: unknown) {
    const err = e as { message?: string }
    result.value = { ok: false, msg: err?.message || '连接控制面失败' }
  } finally {
    checking.value = false
  }
}

onMounted(check)
</script>

<template>
  <div class="page">
    <n-card :bordered="false" title="系统设置">
      <n-form label-placement="top" style="max-width: 640px">
        <n-form-item label="管理员令牌（Bearer）">
          <n-input v-model:value="token" type="password" show-password-on="click" placeholder="auth_admin_token 的值" />
        </n-form-item>
        <n-form-item>
          <n-space>
            <n-button type="primary" @click="save">保存令牌</n-button>
            <n-button tertiary :loading="checking" @click="check">重新自检</n-button>
          </n-space>
        </n-form-item>
      </n-form>

      <n-alert type="info" title="令牌从哪来" style="max-width: 640px">
        在控制面主机执行下面这条命令，把输出的值填到上面（顶栏右上角的「管理员令牌」框也可以填）：
        <div class="cmd">grep auth_admin_token /opt/ngxcp/config.yaml</div>
      </n-alert>
    </n-card>

    <n-card :bordered="false" title="连接自检" style="margin-top: 16px">
      <n-descriptions bordered :column="1" size="small" style="max-width: 640px">
        <n-descriptions-item label="控制面">
          <n-tag :bordered="false" size="small">{{ origin }}</n-tag>
        </n-descriptions-item>
        <n-descriptions-item label="状态">
          <n-tag v-if="result" :type="result.ok ? 'success' : 'error'" :bordered="false" size="small">
            {{ result.msg }}
          </n-tag>
          <span v-else>—</span>
        </n-descriptions-item>
        <n-descriptions-item v-if="result?.ok" label="Agent gRPC">
          {{ result.grpc }}
        </n-descriptions-item>
        <n-descriptions-item v-if="result?.ok" label="已纳管节点">
          {{ result.nodes }} 个
        </n-descriptions-item>
        <n-descriptions-item v-if="result?.ok" label="二进制分发">
          <n-tag :type="result.binaryReady ? 'success' : 'warning'" :bordered="false" size="small">
            {{ result.binaryReady ? '已就绪' : '未启用' }}
          </n-tag>
        </n-descriptions-item>
      </n-descriptions>

      <n-alert
        v-if="result?.ok && result.binaryReady === false"
        type="warning"
        title="Agent 二进制分发未启用"
        style="max-width: 640px; margin-top: 12px"
      >
        节点用一键命令安装时会卡在「[2/5] 下载 Agent 二进制」404。在控制面宿主机执行：
        <div class="cmd">NGXCP_DEPLOY_HOST=root@&lt;控制面&gt; bash scripts/enable-agent-dist.sh</div>
      </n-alert>
    </n-card>
  </div>
</template>

<style scoped>
.page {
  padding: 20px;
}
.cmd {
  margin-top: 6px;
  padding: 8px 10px;
  border-radius: 6px;
  background: rgba(0, 0, 0, 0.04);
  font-family: monospace;
  font-size: 12px;
  word-break: break-all;
}
</style>
