<script setup lang="ts">
import { h, reactive, ref } from 'vue'
import {
  NButton,
  NDataTable,
  NForm,
  NFormItem,
  NSelect,
  NSpace,
  NTag,
  useMessage,
  type DataTableColumns,
  type SelectOption
} from 'naive-ui'
import { aggregateLogs, type AggMetric, type AggRow } from '@/api/logs'

const message = useMessage()

const metricOptions: SelectOption[] = [
  { label: '热门 URI', value: 'top_uri' },
  { label: '热门 IP', value: 'top_ip' },
  { label: '热门 UA', value: 'top_ua' },
  { label: '状态码分布', value: 'status_dist' },
  { label: '响应耗时百分位', value: 'rt_percentile' }
]

const windowOptions: SelectOption[] = [
  { label: '近 1 小时', value: '1h' },
  { label: '近 24 小时', value: '24h' },
  { label: '近 7 天', value: '7d' }
]

const form = reactive({
  metric: 'top_uri' as AggMetric,
  window: '24h',
  top_n: 20
})

const loading = ref(false)
const rows = ref<AggRow[]>([])
const total = ref(0)
const tookMs = ref(0)
const metricLabel = ref('')

function metricText(m: string): string {
  const lbl = metricOptions.find((o) => o.value === m)?.label
  return typeof lbl === 'string' ? lbl : m
}

const columns: DataTableColumns<AggRow> = [
  { title: '分组键', key: 'Key', ellipsis: { tooltip: true }, minWidth: 140, render: (r) => r.Key || '(空)' },
  { title: '命中', key: 'Count', width: 110, sorter: (a, b) => a.Count - b.Count },
  { title: '错误数', key: 'Err', width: 90, render: (r) => h(NTag, { size: 'small', type: r.Err > 0 ? 'warning' : 'default' }, { default: () => r.Err }) },
  { title: 'P50(s)', key: 'P50', width: 90, render: (r) => (r.P50 ? r.P50.toFixed(3) : '-') },
  { title: 'P95(s)', key: 'P95', width: 90, render: (r) => (r.P95 ? r.P95.toFixed(3) : '-') },
  { title: 'P99(s)', key: 'P99', width: 90, render: (r) => (r.P99 ? r.P99.toFixed(3) : '-') }
]

async function doAggregate() {
  loading.value = true
  metricLabel.value = metricText(form.metric)
  try {
    const res = await aggregateLogs({
      metric: form.metric,
      window: form.window,
      top_n: form.top_n
    })
    rows.value = res.rows
    total.value = res.total
    tookMs.value = res.took_ms
  } catch {
    /* 拦截器已提示 */
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="agg">
    <n-form inline label-placement="left" :show-feedback="false">
      <n-space :size="10" align="center">
        <n-form-item label="指标">
          <n-select v-model:value="form.metric" :options="metricOptions" style="width: 160px" />
        </n-form-item>
        <n-form-item label="时间窗">
          <n-select v-model:value="form.window" :options="windowOptions" style="width: 130px" />
        </n-form-item>
        <n-form-item label="TopN">
          <n-select
            v-model:value="form.top_n"
            :options="[
              { label: '10', value: 10 },
              { label: '20', value: 20 },
              { label: '50', value: 50 },
              { label: '100', value: 100 }
            ]"
            style="width: 90px"
          />
        </n-form-item>
        <n-form-item>
          <n-button type="primary" :loading="loading" @click="doAggregate">聚合</n-button>
        </n-form-item>
      </n-space>
    </n-form>

    <div class="meta">
      <span v-if="total">指标 {{ metricLabel }} ｜ 参与聚合 {{ total }} 条 ｜ 耗时 {{ tookMs }}ms</span>
      <span v-else>选择指标后点击聚合</span>
    </div>

    <n-data-table
      :columns="columns"
      :data="rows"
      :loading="loading"
      :row-key="(r: AggRow) => r.Key"
      size="small"
      striped
      :max-height="520"
    />
  </div>
</template>

<style scoped>
.agg {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.meta {
  color: #8a8f99;
  font-size: 13px;
}
</style>
