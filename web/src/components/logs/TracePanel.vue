<script setup lang="ts">
import { h, ref, watch } from 'vue'
import {
  NButton,
  NInput,
  NInputGroup,
  NSpace,
  NTimeline,
  NTimelineItem,
  NTag,
  NEmpty,
  NAlert,
  useMessage
} from 'naive-ui'
import { traceLog, type TraceResult, type LogEntry } from '@/api/logs'

const props = defineProps<{ rid?: string }>()
const message = useMessage()

const ridInput = ref(props.rid ?? '')
const loading = ref(false)
const result = ref<TraceResult | null>(null)

// 父组件切到追踪 tab 并带 rid 时自动查询。
watch(
  () => props.rid,
  (v) => {
    if (v) {
      ridInput.value = v
      doTrace()
    }
  },
  { immediate: true }
)

function spanType(e: LogEntry): 'success' | 'warning' | 'error' | 'info' {
  if (e.Status >= 500) return 'error'
  if (e.Status >= 400) return 'warning'
  if (e.UpstreamRT > 1) return 'warning'
  return 'success'
}

async function doTrace() {
  const rid = ridInput.value.trim()
  if (!rid) {
    message.warning('请输入 TraceID / request_id')
    return
  }
  loading.value = true
  try {
    result.value = await traceLog(rid)
  } catch {
    /* 拦截器已提示 */
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="trace">
    <n-input-group>
      <n-input
        v-model:value="ridInput"
        placeholder="输入 TraceID / request_id 还原全链路"
        clearable
        @keyup.enter="doTrace"
      />
      <n-button type="primary" :loading="loading" @click="doTrace">追踪</n-button>
    </n-input-group>

    <n-alert type="info" title="链路时序提示" :show-icon="true" class="hint">
      时间线按各节点日志时间戳升序排列，标出「首跳」与「瓶颈（upstream_rt 最大）」节点。
      若时间出现乱序，请检查各节点 NTP 时钟同步（M7 / T077）。
    </n-alert>

    <n-empty v-if="!result" description="暂无链路数据" />
    <template v-else>
      <n-space :size="8" class="summary">
        <n-tag size="small" type="info">链路跨度 {{ result.spans.length }} 跳</n-tag>
        <n-tag v-if="result.first_hop" size="small" type="success">首跳：{{ result.first_hop }}</n-tag>
        <n-tag v-if="result.bottleneck" size="small" type="warning">瓶颈：{{ result.bottleneck }}</n-tag>
      </n-space>

      <n-timeline>
        <n-timeline-item
          v-for="(s, i) in result.spans"
          :key="i"
          :type="spanType(s)"
          :title="`${s.Node} · ${s.Status}`"
          :content="`${s.URI} ｜ upstream_rt ${(s.UpstreamRT * 1000).toFixed(0)}ms ｜ 总耗时 ${(s.RequestRT * 1000).toFixed(0)}ms`"
          :time="new Date(s.TS).toLocaleTimeString()"
        />
      </n-timeline>
    </template>
  </div>
</template>

<style scoped>
.trace {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.hint {
  font-size: 12px;
}
.summary {
  flex-wrap: wrap;
}
</style>
