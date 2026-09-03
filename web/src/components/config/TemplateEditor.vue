<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { NSelect, NButton, NSpace, NTag, useMessage, NTabs, NTabPane } from 'naive-ui'
import NginxEditor from '@/components/editor/NginxEditor.vue'
import { listTemplates, getTemplate, renderTemplate, type ConfigTemplate } from '@/api/configs'

const props = defineProps<{
  nodeOptions: { label: string; value: number }[]
}>()

const message = useMessage()
const templates = ref<ConfigTemplate[]>([])
const templateId = ref<number | null>(null)
const selectedTemplate = ref<ConfigTemplate | null>(null)
const nodeIds = ref<number[]>([])
const rendered = ref<Record<number, string>>({})
const loading = ref(false)

const templateOptions = computed(() =>
  templates.value.map((t) => ({ label: t.name, value: t.id }))
)

async function loadTemplates() {
  try {
    templates.value = await listTemplates()
    if (templates.value.length && templateId.value === null) {
      templateId.value = templates.value[0].id
      await loadContent()
    }
  } catch {
    // 拦截器已提示
  }
}

async function loadContent() {
  if (templateId.value === null) return
  try {
    selectedTemplate.value = await getTemplate(templateId.value)
  } catch {
    // ignore
  }
}

async function onRender() {
  if (templateId.value === null) return
  if (!nodeIds.value.length) {
    message.warning('请选择至少一个目标节点')
    return
  }
  loading.value = true
  try {
    rendered.value = await renderTemplate(templateId.value, nodeIds.value)
    const n = Object.keys(rendered.value).length
    message.success(`已渲染 ${n} 个节点`)
  } catch {
    // ignore
  } finally {
    loading.value = false
  }
}

onMounted(loadTemplates)
</script>

<template>
  <div class="tmpl">
    <n-space align="center" :size="10" style="margin-bottom: 10px">
      <n-select
        v-model:value="templateId"
        :options="templateOptions"
        placeholder="选择模板"
        style="width: 220px"
        @update:value="loadContent"
      />
      <n-select
        v-model:value="nodeIds"
        :options="nodeOptions"
        multiple
        placeholder="目标节点"
        style="min-width: 240px"
      />
      <n-button type="primary" :loading="loading" @click="onRender">渲染</n-button>
    </n-space>

    <div v-if="selectedTemplate" class="block">
      <div class="meta">
        <n-tag size="small" :bordered="false">应用路径：{{ selectedTemplate.applies_to || '—' }}</n-tag>
        <span class="vars">变量：{{ selectedTemplate.variables.join(', ') || '无' }}</span>
      </div>
      <div class="ed-head">模板内容</div>
      <div class="ed" style="height: 220px">
        <nginx-editor :model-value="selectedTemplate.content" :read-only="true" />
      </div>
    </div>

    <div v-if="Object.keys(rendered).length" class="block">
      <div class="ed-head">渲染结果</div>
      <n-tabs type="line" animated>
        <n-tab-pane
          v-for="(text, nid) in rendered"
          :key="nid"
          :name="String(nid)"
          :tab="'节点 #' + nid"
        >
          <div class="ed" style="height: 260px">
            <nginx-editor :model-value="text" :read-only="true" />
          </div>
        </n-tab-pane>
      </n-tabs>
    </div>

    <div v-if="!templates.length" class="empty">暂无模板（可在 T027 中通过 API 创建）</div>
  </div>
</template>

<style scoped>
.tmpl {
  font-size: 13px;
}
.meta {
  display: flex;
  align-items: center;
  gap: 12px;
  margin: 8px 0;
}
.vars {
  color: #8a8f99;
  font-size: 12px;
}
.ed-head {
  color: #8a8f99;
  font-size: 12px;
  margin: 6px 0 4px;
}
.ed {
  border: 1px solid var(--n-border-color);
  border-radius: 6px;
  overflow: hidden;
}
.block {
  margin-bottom: 14px;
}
.empty {
  color: #8a8f99;
  padding: 8px 2px;
}
</style>
