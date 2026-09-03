<script setup lang="ts">
import { computed } from 'vue'
import { NResult, NTag, NSpace, NAlert, NDescriptions, NDescriptionsItem } from 'naive-ui'
import type { NodeComplianceView, NodeFsProbeView } from '@/api/nodes'

const props = defineProps<{
  compliance?: NodeComplianceView | null
  fsProbe?: NodeFsProbeView | null
}>()

const checkedAt = (ts?: number) => (ts ? new Date(ts * 1000).toLocaleString() : '—')

const compList = computed(() => [
  { name: 'DR 合规自检', view: props.compliance },
  { name: '日志 / 文件系统探测', view: props.fsProbe }
])
</script>

<template>
  <div v-if="!compliance && !fsProbe" class="empty">尚未上报健康检查数据。</div>

  <template v-else>
    <div v-for="item in compList" :key="item.name" class="block">
      <n-descriptions :title="item.name" bordered :column="1" size="small">
        <n-descriptions-item label="状态">
          <n-tag v-if="item.view" :type="item.view.passed ? 'success' : 'error'" :bordered="false">
            {{ item.view.passed ? '通过' : '未通过' }}
          </n-tag>
          <n-tag v-else type="default" :bordered="false">未上报</n-tag>
        </n-descriptions-item>
        <n-descriptions-item label="检查时间">{{ checkedAt(item.view?.checked_at) }}</n-descriptions-item>
      </n-descriptions>

      <n-alert
        v-if="item.view && !item.view.passed"
        type="error"
        :title="`未通过项（${item.view.critical_failed.length}）`"
        style="margin-top: 10px"
      >
        <n-space :size="6">
          <n-tag v-for="f in item.view.critical_failed" :key="f" size="small" type="error" :bordered="false">
            {{ f }}
          </n-tag>
        </n-space>
      </n-alert>
    </div>

    <n-result
      v-if="compliance?.passed && fsProbe?.passed"
      size="small"
      status="success"
      title="节点健康"
      description="DR 合规与日志/FS 探测均通过"
    />
  </template>
</template>

<style scoped>
.empty {
  color: #8a8f99;
  padding: 12px 0;
}
.block {
  margin-bottom: 14px;
}
</style>
