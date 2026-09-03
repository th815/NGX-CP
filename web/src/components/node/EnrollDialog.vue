<script setup lang="ts">
import { reactive, ref, computed } from 'vue'
import {
  NModal,
  NCard,
  NForm,
  NFormItem,
  NInput,
  NSelect,
  NButton,
  NSpace,
  NInputGroup,
  NInputGroupLabel,
  useMessage
} from 'naive-ui'
import { createNode, issueEnrollToken, type NodeRole, type NodeOut } from '@/api/nodes'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{
  (e: 'update:show', v: boolean): void
  (e: 'created', node: NodeOut): void
}>()

const message = useMessage()

const form = reactive({ name: '', address: '', role: 'real_server' as NodeRole })
const submitting = ref(false)
const token = ref('')
const nodeId = ref<number | null>(null)

const roleOptions = [
  { label: 'Nginx 真实服务器 (RS)', value: 'real_server' },
  { label: 'LVS Director', value: 'director' },
  { label: '未知（待识别）', value: 'unknown' }
]

const command = computed(() => {
  if (!token.value) return ''
  const server = window.location.origin
  return `ngxcp-agent enroll --server ${server} --token ${token.value}`
})

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
    const tk = await issueEnrollToken(node.id)
    token.value = tk.token
    nodeId.value = node.id
    message.success('节点已创建并生成接入令牌')
    emit('created', node)
  } catch {
    // 错误已由全局拦截器提示
  } finally {
    submitting.value = false
  }
}

function copy() {
  if (!command.value) return
  navigator.clipboard?.writeText(command.value)
  message.success('命令已复制')
}

function reset() {
  form.name = ''
  form.address = ''
  form.role = 'real_server'
  token.value = ''
  nodeId.value = null
}
</script>

<template>
  <n-modal
    :show="props.show"
    preset="card"
    title="添加节点"
    style="width: 560px"
    @update:show="(v: boolean) => emit('update:show', v)"
    @after-leave="reset"
  >
    <n-form v-if="!token" :model="form" label-placement="top">
      <n-form-item label="节点名称" required>
        <n-input v-model:value="form.name" placeholder="如 rs-nginx-01" />
      </n-form-item>
      <n-form-item label="管理地址（可选）">
        <n-input v-model:value="form.address" placeholder="如 10.0.0.11:22" />
      </n-form-item>
      <n-form-item label="角色">
        <n-select v-model:value="form.role" :options="roleOptions" />
      </n-form-item>
      <n-space justify="end">
        <n-button @click="close">取消</n-button>
        <n-button type="primary" :loading="submitting" @click="submit">创建并生成令牌</n-button>
      </n-space>
    </n-form>

    <div v-else>
      <n-space vertical :size="10">
        <n-input-group-label>① 在目标节点安装并运行 Agent 后，执行以下命令接入：</n-input-group-label>
        <n-input-group>
          <n-input :value="command" readonly />
          <n-button type="primary" @click="copy">复制</n-button>
        </n-input-group>
        <n-input-group-label>② 令牌仅展示一次，节点 ID：{{ nodeId }}</n-input-group-label>
        <n-space justify="end">
          <n-button @click="copy">复制命令</n-button>
          <n-button type="primary" @click="close">完成</n-button>
        </n-space>
      </n-space>
    </div>
  </n-modal>
</template>
