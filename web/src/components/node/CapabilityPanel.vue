<script setup lang="ts">
import { ref, computed } from 'vue'
import { NDescriptions, NDescriptionsItem, NTag, NSpace, NCollapse, NCollapseItem, NCode } from 'naive-ui'
import type { CapabilityView } from '@/api/nodes'

const props = defineProps<{ capability?: CapabilityView | null }>()

const showAllModules = ref(false)

const modules = computed(() => props.capability?.nginx?.modules ?? [])
const visibleModules = computed(() =>
  showAllModules.value ? modules.value : modules.value.slice(0, 16)
)
</script>

<template>
  <div v-if="!capability" class="empty">暂无能力基线数据（节点尚未上报）。</div>

  <template v-else>
    <n-descriptions
      v-if="capability.nginx"
      title="Nginx 编译画像"
      bordered
      :column="2"
      size="small"
    >
      <n-descriptions-item label="版本">{{ capability.nginx.version || '—' }}</n-descriptions-item>
      <n-descriptions-item label="Prefix">{{ capability.nginx.prefix || '—' }}</n-descriptions-item>
      <n-descriptions-item label="配置路径">{{ capability.nginx.conf_path || '—' }}</n-descriptions-item>
      <n-descriptions-item label="二进制">{{ capability.nginx.sbin_path || '—' }}</n-descriptions-item>
      <n-descriptions-item label="配置哈希">
        <span class="mono">{{ (capability.nginx.config_hash || '—').slice(0, 16) }}…</span>
      </n-descriptions-item>
      <n-descriptions-item label="一致性校验和">
        <span class="mono">{{ (capability.checksum || '—').slice(0, 16) }}…</span>
      </n-descriptions-item>
    </n-descriptions>

    <div v-if="capability.nginx" class="modules">
      <div class="mod-head">
        <span>编译模块（{{ modules.length }}）</span>
        <n-tag v-if="modules.length > 16" size="small" :bordered="false" type="primary" style="cursor:pointer" @click="showAllModules = !showAllModules">
          {{ showAllModules ? '收起' : '展开全部' }}
        </n-tag>
      </div>
      <n-space :size="6">
        <n-tag v-for="m in visibleModules" :key="m" size="small" :bordered="false">{{ m }}</n-tag>
      </n-space>
    </div>

    <n-collapse v-if="capability.nginx?.raw_args" class="raw">
      <n-collapse-item title="原始 configure 参数" name="raw">
        <n-code :code="capability.nginx.raw_args" word-wrap />
      </n-collapse-item>
    </n-collapse>

    <n-descriptions
      v-if="!capability.nginx"
      title="主机画像"
      bordered
      :column="2"
      size="small"
    >
      <n-descriptions-item label="角色提示">非 Nginx 节点（如 LVS Director）</n-descriptions-item>
      <n-descriptions-item label="Keepalived">{{ capability.has_keepalived ? '已安装' : '未安装' }}</n-descriptions-item>
      <n-descriptions-item label="IPVS">{{ capability.has_ipvsadm ? '已加载' : '未加载' }}</n-descriptions-item>
      <n-descriptions-item label="主机名">{{ capability.hostname || '—' }}</n-descriptions-item>
    </n-descriptions>
  </template>
</template>

<style scoped>
.empty {
  color: #8a8f99;
  padding: 12px 0;
}
.modules {
  margin-top: 14px;
}
.mod-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  font-size: 13px;
  color: #8a8f99;
  margin-bottom: 8px;
}
.raw {
  margin-top: 14px;
}
.mono {
  font-family: monospace;
  font-size: 12px;
}
</style>
