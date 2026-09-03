<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import {
  NCard,
  NSelect,
  NSpace,
  NButton,
  NTabs,
  NTabPane,
  NTag,
  NEmpty,
  NSpin,
  NInput,
  useMessage
} from 'naive-ui'
import FileTree from '@/components/config/FileTree.vue'
import RevisionList from '@/components/config/RevisionList.vue'
import DriftPanel from '@/components/config/DriftPanel.vue'
import TemplateEditor from '@/components/config/TemplateEditor.vue'
import DiffView from '@/components/config/DiffView.vue'
import NginxEditor from '@/components/editor/NginxEditor.vue'
import {
  listConfigFiles,
  getConfigFile,
  listRevisions,
  diffRevisions,
  validateConfig,
  semanticCheck,
  listDrift,
  manualEdit,
  type ConfigFileItem,
  type RevisionView,
  type DiffResult,
  type NginxError,
  type SemanticIssue,
  type DriftItem
} from '@/api/configs'
import { listNodes, type NodeOut } from '@/api/nodes'

const message = useMessage()

const nodes = ref<NodeOut[]>([])
const nodeId = ref<number | null>(null)
const files = ref<ConfigFileItem[]>([])
const selectedId = ref<number | null>(null)
const currentFile = ref<ConfigFileItem | null>(null)
const draft = ref('')
const dirty = ref(false)
const loading = ref(false)
const saving = ref(false)
const errorLines = ref<number[]>([])

const revisions = ref<RevisionView[]>([])
const compareDiff = ref<DiffResult | null>(null)
const validating = ref(false)
const validateErrors = ref<NginxError[]>([])
const semantic = ref<SemanticIssue[]>([])
const semLoading = ref(false)
const drift = ref<DriftItem[]>([])
const driftLoading = ref(false)
const driftPaths = computed(() => drift.value.map((d) => d.path))

const nodeOptions = computed(() =>
  nodes.value.map((n) => ({ label: `${n.name} (#${n.id})`, value: n.id }))
)

async function loadNodes() {
  try {
    const { items } = await listNodes()
    nodes.value = items
    if (items.length && nodeId.value === null) nodeId.value = items[0].id
  } catch {
    // ignore
  }
}

async function loadFiles(id: number) {
  loading.value = true
  files.value = []
  selectedId.value = null
  currentFile.value = null
  draft.value = ''
  drift.value = []
  try {
    files.value = await listConfigFiles(id)
    await loadDrift(id)
  } catch {
    // ignore
  } finally {
    loading.value = false
  }
}

async function loadDrift(id: number) {
  driftLoading.value = true
  try {
    const r = await listDrift(id)
    drift.value = Array.isArray(r) ? (r as DriftItem[]) : (r as { items: DriftItem[] }).items ?? []
  } catch {
    drift.value = []
  } finally {
    driftLoading.value = false
  }
}

async function selectFile(id: number) {
  selectedId.value = id
  dirty.value = false
  errorLines.value = []
  validateErrors.value = []
  compareDiff.value = null
  try {
    const f = await getConfigFile(id)
    currentFile.value = f
    draft.value = f.current_content ?? ''
    revisions.value = await listRevisions(id)
  } catch {
    // ignore
  }
}

function onEdit(v: string) {
  draft.value = v
  dirty.value = true
  errorLines.value = []
}

async function onSave() {
  if (selectedId.value === null) return
  await validate() // 先校验
  if (validateErrors.value.length) {
    message.error('校验未通过，已拒绝保存并标红错误行')
    return
  }
  saving.value = true
  try {
    await manualEdit(selectedId.value, draft.value, 'Web 编辑器手动保存', 'web')
    message.success('已保存为新版本')
    dirty.value = false
    errorLines.value = []
    await selectFile(selectedId.value)
    await loadFiles(nodeId.value!)
  } catch {
    // ignore
  } finally {
    saving.value = false
  }
}

async function validate() {
  if (selectedId.value === null || !currentFile.value) return
  validating.value = true
  validateErrors.value = []
  errorLines.value = []
  try {
    const res = await validateConfig({
      node_id: nodeId.value!,
      conf_path: 'nginx.conf',
      files: [{ path: currentFile.value.path, content: draft.value }]
    })
    if (res.ok) {
      message.success('nginx -t 校验通过')
    } else {
      validateErrors.value = res.errors
      errorLines.value = res.errors.map((e) => e.line).filter((l) => l > 0)
      message.error(`校验失败：${res.errors.length} 处错误`)
    }
  } catch {
    // 拦截器已提示（如 Agent 离线 4103）
  } finally {
    validating.value = false
  }
}

async function onCompare(fileId: number, from: number, to: number) {
  try {
    compareDiff.value = await diffRevisions(fileId, from, to)
  } catch {
    // ignore
  }
}

async function onSemantic() {
  if (nodeId.value === null) return
  semLoading.value = true
  try {
    const r = await semanticCheck(nodeId.value)
    semantic.value = r.issues
    if (!r.issues.length) message.success('语义校验通过')
  } catch {
    // ignore
  } finally {
    semLoading.value = false
  }
}

const fmt = (t: string) => (t ? new Date(t).toLocaleString() : '—')

watch(nodeId, (v) => {
  if (v) loadFiles(v)
})

onMounted(async () => {
  await loadNodes()
  if (nodeId.value !== null) await loadFiles(nodeId.value)
})
</script>

<template>
  <div class="page">
    <n-card :bordered="false" class="head">
      <n-space justify="space-between" align="center">
        <div>
          <div class="title">配置中心</div>
          <div class="sub">文件树 · 编辑 · 版本对比 · 语义校验 · 漂移检测（M2 配置闭环）</div>
        </div>
        <n-space>
          <n-select
            v-model:value="nodeId"
            :options="nodeOptions"
            placeholder="选择节点"
            style="width: 220px"
          />
          <n-button @click="onSemantic" :loading="semLoading">语义校验</n-button>
        </n-space>
      </n-space>
    </n-card>

    <div class="cols">
      <!-- 左：文件树 -->
      <div class="col left">
        <div class="col-title">配置树</div>
        <n-spin :show="loading" class="fill">
          <FileTree :files="files" :selected-id="selectedId" :drift-paths="driftPaths" @select="selectFile" />
        </n-spin>
      </div>

      <!-- 中：编辑器 -->
      <div class="col center">
        <div class="col-title">
          <span v-if="currentFile">{{ currentFile.path }}</span>
          <span v-else>未选择文件</span>
          <n-tag v-if="dirty" size="small" type="warning" :bordered="false" style="margin-left: 8px">未保存</n-tag>
        </div>
        <div class="editor-wrap">
          <n-spin v-if="!currentFile" show>
            <n-empty description="从左侧选择配置文件" />
          </n-spin>
          <template v-else>
            <NginxEditor
              :model-value="draft"
              :error-lines="errorLines"
              @update:model-value="onEdit"
              @save="onSave"
            />
          </template>
        </div>
        <div class="actions">
          <n-button type="primary" :disabled="!dirty" :loading="saving" @click="onSave">保存 (Ctrl+S)</n-button>
          <n-button :loading="validating" @click="validate">校验 nginx -t</n-button>
        </div>
        <div v-if="validateErrors.length" class="errs">
          <div v-for="(e, i) in validateErrors" :key="i" class="err">
            <n-tag size="small" :bordered="false" type="error">{{ e.level }}</n-tag>
            <span class="el">行 {{ e.line }}</span>
            <span class="em">{{ e.message }}</span>
          </div>
        </div>
      </div>

      <!-- 右：信息面板 -->
      <div class="col right">
        <n-tabs type="line" animated class="fill">
          <n-tab-pane name="rev" tab="版本历史">
            <RevisionList
              v-if="selectedId !== null"
              :revisions="revisions"
              :file-id="selectedId"
              :current-rev-id="currentFile?.current_rev_id ?? 0"
              @compare="onCompare"
            />
            <n-empty v-else description="选择文件查看版本" size="small" />

            <div v-if="compareDiff" class="cmp">
              <div class="cmp-head">差异对比</div>
              <DiffView :diff="compareDiff" />
            </div>
          </n-tab-pane>

          <n-tab-pane name="sem" tab="校验结果">
            <div v-if="!semantic.length" class="empty">点击「语义校验」对节点运行规则引擎</div>
            <div v-for="(s, i) in semantic" :key="i" class="sitem" :class="s.severity">
              <div class="srow">
                <n-tag size="small" :bordered="false" :type="s.severity === 'error' ? 'error' : s.severity === 'warning' ? 'warning' : 'info'">
                  {{ s.severity }}
                </n-tag>
                <span class="rid">{{ s.rule_id }}</span>
                <span v-if="s.file" class="sf">{{ s.file }}<template v-if="s.line">:{{ s.line }}</template></span>
              </div>
              <div class="smsg">{{ s.message }}</div>
              <div v-if="s.fix" class="sfix">修复：{{ s.fix }}</div>
            </div>
          </n-tab-pane>

          <n-tab-pane name="drift" tab="漂移状态">
            <DriftPanel :items="drift" :loading="driftLoading" />
          </n-tab-pane>

          <n-tab-pane name="tmpl" tab="模板">
            <TemplateEditor :node-options="nodeOptions" />
          </n-tab-pane>
        </n-tabs>
      </div>
    </div>
  </div>
</template>

<style scoped>
.page {
  height: 100%;
  display: flex;
  flex-direction: column;
  padding: 16px;
  gap: 12px;
}
.head {
  flex: 0 0 auto;
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
.cols {
  flex: 1 1 auto;
  min-height: 0;
  display: grid;
  grid-template-columns: 260px 1fr 380px;
  gap: 12px;
}
.col {
  border: 1px solid var(--n-border-color);
  border-radius: 8px;
  display: flex;
  flex-direction: column;
  min-height: 0;
  overflow: hidden;
  background: var(--n-color);
}
.col-title {
  flex: 0 0 auto;
  padding: 10px 12px;
  font-weight: 600;
  border-bottom: 1px solid var(--n-border-color);
  display: flex;
  align-items: center;
}
.fill {
  flex: 1 1 auto;
  min-height: 0;
}
.left .fill,
.right .fill {
  overflow: auto;
}
.editor-wrap {
  flex: 1 1 auto;
  min-height: 0;
  margin: 10px 12px 0;
  overflow: hidden;
}
.actions {
  flex: 0 0 auto;
  display: flex;
  gap: 8px;
  padding: 10px 12px;
}
.errs {
  flex: 0 0 auto;
  padding: 0 12px 10px;
  max-height: 160px;
  overflow: auto;
}
.err {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 3px 0;
  font-size: 12px;
}
.el {
  color: #e06c75;
  font-family: monospace;
}
.em {
  color: #c2c8d1;
}
.cmp {
  margin-top: 12px;
}
.cmp-head {
  color: #8a8f99;
  font-size: 12px;
  margin-bottom: 4px;
}
.empty {
  color: #8a8f99;
  padding: 10px 2px;
}
.sitem {
  padding: 8px 10px;
  border-radius: 6px;
  margin-bottom: 6px;
  border-left: 3px solid var(--n-border-color);
}
.sitem.error {
  border-left-color: #e06c75;
  background: rgba(224, 108, 117, 0.08);
}
.sitem.warning {
  border-left-color: #e5c07b;
  background: rgba(229, 192, 123, 0.08);
}
.srow {
  display: flex;
  align-items: center;
  gap: 8px;
}
.rid {
  font-family: monospace;
  font-weight: 600;
}
.sf {
  color: #8a8f99;
  font-size: 12px;
}
.smsg {
  margin-top: 3px;
  font-size: 13px;
}
.sfix {
  margin-top: 3px;
  font-size: 12px;
  color: #4ec97f;
}
</style>
