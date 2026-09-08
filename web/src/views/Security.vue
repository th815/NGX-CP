<script setup lang="ts">
import { NCard, NTabs, NTabPane } from 'naive-ui'
import Alerts from '@/components/security/Alerts.vue'
import Rules from '@/components/security/Rules.vue'
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">安全预警</h2>
        <p class="page-sub">攻击预警事件流（T067 状态机 + T068 封禁变更单 + T069 分级处置）与防护规则监控（T066）。</p>
      </div>
    </div>

    <n-card :bordered="false">
      <n-tabs type="line">
        <n-tab-pane name="alerts" tab="告警事件">
          <Alerts />
        </n-tab-pane>
        <n-tab-pane name="rules" tab="防护规则">
          <Rules />
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
