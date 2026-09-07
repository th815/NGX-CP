<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { NAlert, NButton, NCard, NEmpty, NSpin, NSpace, NTag, useMessage } from 'naive-ui'
import { getTopology, type Topology } from '@/api/lvs'
import TopoSvg from '@/components/lvs/TopoSvg.vue'

const message = useMessage()
const loading = ref(false)
const topo = ref<Topology | null>(null)

async function load() {
  loading.value = true
  try {
    topo.value = await getTopology()
  } catch (e: any) {
    message.error(e?.response?.data?.message || e?.message || '加载拓扑失败')
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page">
    <n-card :bordered="false" title="LVS 拓扑" size="small">
      <template #header-extra>
        <n-button size="small" tertiary :loading="loading" @click="load">刷新</n-button>
      </template>

      <n-spin :show="loading">
        <n-empty v-if="!loading && topo && topo.directors.length === 0" description="暂无 Director 数据" />

        <template v-else-if="topo">
          <TopoSvg :topo="topo" />

          <div class="legend">
            <n-tag size="small" :bordered="false" type="success">持有 VIP / 活跃</n-tag>
            <n-tag size="small" :bordered="false" type="warning">排空</n-tag>
            <n-tag size="small" :bordered="false" type="error">DOWN / 禁用</n-tag>
            <n-tag size="small" :bordered="false">待机备机（未持有 VIP）</n-tag>
          </div>

          <n-alert type="info" title="说明" style="margin-top: 12px">
            拓扑为只读可视化：VIP 经 LVS-DR 由双 Director 主备承载，流量分发到后端 RS。
            主备差异仅 state / priority / unicast 三项；其余配置必须完全一致（T052 合规自检在后继轮次补齐）。
          </n-alert>

          <n-card :bordered="false" title="Director 明细" size="small" style="margin-top: 16px">
            <pre class="raw">{{ JSON.stringify(topo.directors, null, 2) }}</pre>
          </n-card>
          <n-card :bordered="false" title="虚拟服务 / RS 明细" size="small" style="margin-top: 12px">
            <pre class="raw">{{ JSON.stringify({ vs: topo.vs, rs: topo.rs }, null, 2) }}</pre>
          </n-card>
        </template>
      </n-spin>
    </n-card>
  </div>
</template>

<style scoped>
.page {
  padding: 20px;
}
.legend {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 12px;
}
.raw {
  font-size: 12px;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
  background: #f8fafc;
  padding: 10px;
  border-radius: 6px;
  margin: 0;
}
</style>
