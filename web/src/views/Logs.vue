<script setup lang="ts">
import { ref } from 'vue'
import { NCard, NTabs, NTabPane } from 'naive-ui'
import Search from '@/components/logs/Search.vue'
import Aggregate from '@/components/logs/Aggregate.vue'
import TracePanel from '@/components/logs/TracePanel.vue'

const traceRid = ref('')
const tab = ref('search')

function onTrace(rid: string) {
  traceRid.value = rid
  tab.value = 'trace'
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">日志中心</h2>
        <p class="page-sub">多维检索 · TraceID 全链路追踪 · 聚合分析（数据来自 T060 标准日志 + T061 采集 + T063 落库）。</p>
      </div>
    </div>

    <n-card :bordered="false">
      <n-tabs v-model:value="tab" type="line">
        <n-tab-pane name="search" tab="检索">
          <Search @trace="onTrace" />
        </n-tab-pane>
        <n-tab-pane name="aggregate" tab="聚合">
          <Aggregate />
        </n-tab-pane>
        <n-tab-pane name="trace" tab="链路追踪">
          <TracePanel :rid="traceRid" />
        </n-tab-pane>
      </n-tabs>
    </n-card>
  </div>
</template>

<style scoped>
.page {
  padding: 18px 22px;
}
.page-head {
  margin-bottom: 16px;
}
.page-title {
  margin: 0;
  font-size: 20px;
  font-weight: 700;
}
.page-sub {
  margin: 4px 0 0;
  color: #8a8f99;
  font-size: 13px;
}
</style>
