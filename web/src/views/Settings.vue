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
import { acknowledgeSetup, getBootstrapInfo, getSetupToken, listNodes, type SetupToken } from '@/api/nodes'

const app = useAppStore()
const message = useMessage()

const token = ref(app.token)
const checking = ref(false)
const acknowledging = ref(false)
const origin = window.location.origin

// 首跑一次性明文令牌（未确认时由控制面返回，已确认/未配置则为 null）。
const setup = ref<SetupToken | null>(null)

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

function copySetup(t: string) {
  navigator.clipboard.writeText(t)
  message.success('已复制')
}

// 首跑：尝试免鉴权获取一次性令牌，供页面直接展示（无需 SSH+grep）。
async function tryRevealSetupToken() {
  if (app.token) return // 本地已有令牌，无需揭示
  try {
    setup.value = await getSetupToken()
  } catch {
    setup.value = null
  }
}

// 完成首次设置：写入令牌 → 调用确认端点锁定（此后 setup-token 不再泄露）→ 刷新自检。
async function finishSetup() {
  if (!setup.value) return
  acknowledging.value = true
  try {
    app.setToken(setup.value.token.trim())
    token.value = setup.value.token.trim()
    await acknowledgeSetup()
    setup.value = null
    message.success('首次设置完成，令牌已锁定')
    await check()
  } catch (e: unknown) {
    const err = e as { message?: string }
    message.error('确认失败：' + (err?.message || '未知错误'))
  } finally {
    acknowledging.value = false
  }
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

onMounted(async () => {
  await tryRevealSetupToken()
  await check()
})
</script>

<template>
  <div class="page">
    <n-card :bordered="false" title="系统设置">
      <n-alert
        v-if="setup"
        type="warning"
        title="首次设置：这是你的管理员令牌（一次性明文，已确认后不再显示）"
        style="max-width: 640px; margin-bottom: 16px"
      >
        <p style="margin: 0 0 10px">装完控制面第一次打开网页就能拿到，不用登服务器 grep。复制保存好，点「完成首次设置」即锁定：</p>
        <div class="cmd">{{ setup.token }}</div>
        <n-space style="margin-top: 12px">
          <n-button size="small" @click="copySetup(setup.token)">复制</n-button>
          <n-button type="primary" size="small" :loading="acknowledging" @click="finishSetup">完成首次设置</n-button>
        </n-space>
      </n-alert>

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
        首跑直接在上方「首次设置」卡片一键获取并锁定（推荐，无需登服务器）。
        若页面未出现该卡片（例如已确认过、或手动改了 config 里的令牌），再到控制面主机取：
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
