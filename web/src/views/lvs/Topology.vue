<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { NAlert, NButton, NCard, NEmpty, NInputNumber, NSpin, NSpace, NTag, useMessage } from 'naive-ui'
import {
  getTopology,
  drainRealServer,
  restoreRealServer,
  setRealServerWeight,
  type Topology,
} from '@/api/lvs'
import TopoSvg from '@/components/lvs/TopoSvg.vue'

const message = useMessage()
const loading = ref(false)
const topo = ref<Topology | null>(null)
const pending = ref<string | null>(null)
const weights = reactive<Record<number, number>>({})

async function load() {
  loading.value = true
  try {
    topo.value = await getTopology()
    if (topo.value) {
      for (const r of topo.value.rs) {
        if (weights[r.id] === undefined) weights[r.id] = r.weight
      }
    }
  } catch (e: any) {
    message.error(e?.response?.data?.message || e?.message || '加载拓扑失败')
  } finally {
    loading.value = false
  }
}

async function withPending(key: string, fn: () => Promise<void>) {
  pending.value = key
  try {
    await fn()
    message.success('操作成功')
    await load()
  } catch (e: any) {
    message.error(e?.response?.data?.message || e?.message || '操作失败')
  } finally {
    pending.value = null
  }
}

function drain(id: number) {
  withPending('drain-' + id, () => drainRealServer(id))
}
function restore(id: number) {
  withPending('restore-' + id, () => restoreRealServer(id))
}
function setWeight(id: number) {
  const w = weights[id] ?? 1
  withPending('weight-' + id, () => setRealServerWeight(id, w))
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

          <n-card title="RS 权重编排（灰度摘除 / 恢复）" size="small" style="margin-top: 16px">
            <n-space vertical :size="10">
              <div v-for="r in topo.rs" :key="r.id" class="rs-row">
                <span class="rs-name">
                  {{ r.address || r.rip }} · {{ r.vs_key }} · w={{ r.weight }}
                  <template v-if="!r.enabled">· 已摘除</template>
                </span>
                <n-space :size="6">
                  <n-button
                    size="small"
                    type="warning"
                    :disabled="!r.enabled"
                    :loading="pending === 'drain-' + r.id"
                    @click="drain(r.id)"
                  >
                    摘除
                  </n-button>
                  <n-button
                    size="small"
                    type="primary"
                    :disabled="r.enabled"
                    :loading="pending === 'restore-' + r.id"
                    @click="restore(r.id)"
                  >
                    恢复
                  </n-button>
                  <n-input-number
                    size="small"
                    v-model:value="weights[r.id]"
                    :min="0"
                    :max="100"
                    style="width: 110px"
                  />
                  <n-button
                    size="small"
                    tertiary
                    :loading="pending === 'weight-' + r.id"
                    @click="setWeight(r.id)"
                  >
                    设权
                  </n-button>
                </n-space>
              </div>
            </n-space>
            <n-alert type="warning" style="margin-top: 12px">
              摘除仅将 LVS 权重置 0（流量不再命中该 RS），不停止 nginx；恢复下发基线权重。
              后端门禁：节点 degraded / offline 时操作被拒绝（纵深防御）。
            </n-alert>
          </n-card>

          <n-alert type="info" title="说明" style="margin-top: 12px">
            拓扑为只读可视化：VIP 经 LVS-DR 由双 Director 主备承载，流量分发到后端 RS。
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
.rs-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 6px 8px;
  border: 1px solid #eef2f7;
  border-radius: 6px;
}
.rs-name {
  font-size: 13px;
  color: #334155;
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
