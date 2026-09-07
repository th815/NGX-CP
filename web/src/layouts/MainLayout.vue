<script setup lang="ts">
import { computed, h, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter, RouterLink } from 'vue-router'
import {
  NLayout,
  NLayoutSider,
  NLayoutHeader,
  NLayoutContent,
  NMenu,
  NButton,
  NSpace,
  NInput,
  NTag,
  NIcon,
  useDialog,
  type MenuOption
} from 'naive-ui'
import { useAppStore } from '@/stores/app'

const router = useRouter()
const route = useRoute()
const app = useAppStore()

const collapsed = ref(false)
const tokenDraft = ref(app.token)

watch(tokenDraft, (v) => app.setToken(v))

const dialog = useDialog()

// 令牌缺失 / 失效时（API 返回 401）直接弹出录入框，免去用户去「系统设置」空页找入口。
function openTokenDialog() {
  const draft = ref(app.token || '')
  dialog.warning({
    title: '需要管理员令牌',
    content: () =>
      h('div', { style: 'margin-top: 8px' }, [
        h(
          'div',
          { style: 'margin-bottom: 10px; font-size: 13px; opacity: .8' },
          '在控制面主机执行：grep auth_admin_token /opt/ngxcp/config.yaml，把值粘贴到下面'
        ),
        h(NInput, {
          value: draft.value,
          'onUpdate:value': (v: string) => (draft.value = v),
          placeholder: 'auth_admin_token 的值'
        })
      ]),
    positiveText: '保存',
    negativeText: '取消',
    onPositiveClick: () => {
      app.setToken(draft.value.trim())
      tokenDraft.value = draft.value.trim()
    }
  })
}

// 由 api/client.ts 在 401 时派发，统一在此弹窗，避免各页面重复处理。
onMounted(() => window.addEventListener('ngxcp:unauthorized', openTokenDialog))
onUnmounted(() => window.removeEventListener('ngxcp:unauthorized', openTokenDialog))

// 导航：按 group 分组，key 即路由 name。
const groups: { label: string; routes: { name: string; title: string }[] }[] = [
  { label: '运维', routes: [
    { name: 'dashboard', title: '总览' },
    { name: 'clusters', title: '集群分组' },
    { name: 'nodes', title: '节点管理' },
    { name: 'build', title: '构建与升级' },
    { name: 'audit', title: '审计日志' },
    { name: 'settings', title: '系统设置' }
  ] },
  { label: '配置', routes: [
    { name: 'configs', title: '配置中心' },
    { name: 'deploy', title: '发布任务' },
    { name: 'certs', title: '证书管理' },
    { name: 'backup', title: '备份恢复' }
  ] },
  { label: '网络', routes: [{ name: 'lvs', title: 'LVS 管理' }] },
  { label: '观测', routes: [
    { name: 'logs', title: '日志中心' },
    { name: 'security', title: '安全预警' },
    { name: 'monitor', title: '监控中心' }
  ] }
]

const menuOptions = computed<MenuOption[]>(() =>
  groups.map((g) => ({
    type: 'group',
    label: g.label,
    key: `group-${g.label}`,
    children: g.routes.map((r) => ({
      label: () =>
        h(
          RouterLink,
          { to: { name: r.name } },
          { default: () => r.title }
        ),
      key: r.name
    }))
  }))
)

const activeKey = computed(() => (route.name as string) || 'dashboard')
const pageTitle = computed(() => (route.meta?.title as string) || 'NGX-CP')
const breadcrumb = computed(() => {
  const g = groups.find((x) => x.routes.some((r) => r.name === route.name))
  return g ? [g.label, pageTitle.value] : [pageTitle.value]
})

function goHome() {
  router.push({ name: 'dashboard' })
}
</script>

<template>
  <n-layout has-sider style="height: 100vh">
    <n-layout-sider
      bordered
      collapse-mode="width"
      :collapsed-width="64"
      :width="220"
      :collapsed="collapsed"
      show-trigger
      @collapse="collapsed = true"
      @expand="collapsed = false"
    >
      <div class="brand" @click="goHome">
        <div class="logo">N</div>
        <span v-if="!collapsed" class="brand-name">NGX-CP</span>
      </div>
      <n-menu :options="menuOptions" :value="activeKey" />
    </n-layout-sider>

    <n-layout>
      <n-layout-header bordered class="topbar">
        <n-space align="center" :size="12">
          <span class="crumb">
            <template v-for="(c, i) in breadcrumb" :key="i">
              <span class="crumb-item">{{ c }}</span>
              <span v-if="i < breadcrumb.length - 1" class="crumb-sep">/</span>
            </template>
          </span>
        </n-space>
        <n-space align="center" :size="10" class="topbar-right">
          <n-input
            v-model:value="tokenDraft"
            size="small"
            placeholder="管理员令牌（Bearer）"
            style="width: 220px"
          />
          <n-tag :bordered="false" :type="tokenDraft ? 'success' : 'warning'" size="small">
            {{ tokenDraft ? '令牌已配置' : '未配置令牌' }}
          </n-tag>
          <n-button size="small" tertiary @click="app.toggleDark()">
            {{ app.dark ? '☀ 亮色' : '🌙 暗色' }}
          </n-button>
        </n-space>
      </n-layout-header>

      <n-layout-content class="content">
        <router-view />
      </n-layout-content>
    </n-layout>
  </n-layout>
</template>

<style scoped>
.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  height: 56px;
  padding: 0 18px;
  cursor: pointer;
  border-bottom: 1px solid var(--n-border-color);
  user-select: none;
}
.logo {
  width: 28px;
  height: 28px;
  border-radius: 8px;
  background: linear-gradient(135deg, #18a058, #2080f0);
  color: #fff;
  font-weight: 700;
  display: flex;
  align-items: center;
  justify-content: center;
}
.brand-name {
  font-weight: 700;
  letter-spacing: 0.5px;
}
.topbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  height: 56px;
  padding: 0 18px;
}
.crumb {
  font-size: 14px;
  color: #8a8f99;
}
.crumb-item:last-child {
  color: #1f2329;
  font-weight: 600;
}
.crumb-sep {
  margin: 0 6px;
  color: #c2c8d1;
}
.topbar-right {
  flex: 0 0 auto;
}
.content {
  padding: 0;
  height: calc(100vh - 56px);
  overflow: auto;
}
</style>
