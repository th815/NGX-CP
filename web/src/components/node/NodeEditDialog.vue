<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import {
  NButton,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NModal,
  NSelect,
  NSpace,
  NSwitch,
  useMessage
} from 'naive-ui'
import { updateNode, type NodeOut, type NodeRole } from '@/api/nodes'

const props = defineProps<{ show: boolean; node: NodeOut | null }>()
const emit = defineEmits<{
  (e: 'update:show', v: boolean): void
  (e: 'updated', n: NodeOut): void
}>()

const message = useMessage()

const form = reactive({
  name: '',
  address: '',
  role: 'real_server' as NodeRole,
  lvs_weight: 1,
  lvs_enabled: true
})
const submitting = ref(false)

const roleOptions = [
  { label: 'Nginx 真实服务器 (RS)', value: 'real_server' },
  { label: 'LVS Director', value: 'director' },
  { label: '未知（待识别）', value: 'unknown' }
]

watch(
  () => props.node,
  (n) => {
    if (!n) return
    form.name = n.name
    form.address = n.address || ''
    form.role = n.role
    form.lvs_weight = n.lvs_weight ?? 1
    form.lvs_enabled = n.lvs_enabled ?? true
  },
  { immediate: true }
)

async function submit() {
  if (!props.node) return
  submitting.value = true
  try {
    const out = await updateNode(props.node.id, {
      name: form.name || undefined,
      address: form.address || undefined,
      role: form.role,
      lvs_weight: form.lvs_weight,
      lvs_enabled: form.lvs_enabled
    })
    message.success('节点已更新')
    emit('updated', out)
    emit('update:show', false)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <n-modal
    :show="props.show"
    preset="card"
    :title="props.node ? `编辑节点 · ${props.node.name}` : '编辑节点'"
    style="width: 560px"
    @update:show="(v: boolean) => emit('update:show', v)"
  >
    <n-form :model="form" label-placement="top">
      <n-form-item label="节点名称">
        <n-input v-model:value="form.name" placeholder="如 192.168.5.8" />
      </n-form-item>
      <n-form-item label="管理地址">
        <n-input v-model:value="form.address" placeholder="如 192.168.5.8:22" />
      </n-form-item>
      <n-form-item label="角色">
        <n-select v-model:value="form.role" :options="roleOptions" />
      </n-form-item>
      <n-form-item label="LVS 权重">
        <n-input-number v-model:value="form.lvs_weight" :min="0" :max="100" />
      </n-form-item>
      <n-form-item label="LVS 启用">
        <n-switch v-model:value="form.lvs_enabled" />
      </n-form-item>
      <n-space justify="end">
        <n-button @click="emit('update:show', false)">取消</n-button>
        <n-button type="primary" :loading="submitting" @click="submit">保存</n-button>
      </n-space>
    </n-form>
  </n-modal>
</template>
