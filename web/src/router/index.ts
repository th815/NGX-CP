import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'

// M1：节点管理页（T020）为真实实现；其余视图为占位骨架，随各里程碑落地逐步填充。
const routes: RouteRecordRaw[] = [
  { path: '/', redirect: '/dashboard' },
  {
    path: '/',
    component: () => import('@/layouts/MainLayout.vue'),
    children: [
      { path: 'dashboard', name: 'dashboard', component: () => import('@/views/Placeholder.vue'), meta: { title: '总览', group: '运维' } },
      { path: 'clusters', name: 'clusters', component: () => import('@/views/Placeholder.vue'), meta: { title: '集群分组', group: '运维' } },
      { path: 'nodes', name: 'nodes', component: () => import('@/views/Nodes.vue'), meta: { title: '节点管理', group: '运维' } },
      { path: 'configs', name: 'configs', component: () => import('@/views/Placeholder.vue'), meta: { title: '配置中心', group: '配置' } },
      { path: 'deploy', name: 'deploy', component: () => import('@/views/Placeholder.vue'), meta: { title: '发布任务', group: '配置' } },
      { path: 'certs', name: 'certs', component: () => import('@/views/Placeholder.vue'), meta: { title: '证书管理', group: '配置' } },
      { path: 'backup', name: 'backup', component: () => import('@/views/Placeholder.vue'), meta: { title: '备份恢复', group: '配置' } },
      { path: 'lvs', name: 'lvs', component: () => import('@/views/Placeholder.vue'), meta: { title: 'LVS 管理', group: '网络' } },
      { path: 'logs', name: 'logs', component: () => import('@/views/Placeholder.vue'), meta: { title: '日志中心', group: '观测' } },
      { path: 'security', name: 'security', component: () => import('@/views/Placeholder.vue'), meta: { title: '安全预警', group: '观测' } },
      { path: 'monitor', name: 'monitor', component: () => import('@/views/Placeholder.vue'), meta: { title: '监控中心', group: '观测' } },
      { path: 'build', name: 'build', component: () => import('@/views/Placeholder.vue'), meta: { title: '构建与升级', group: '运维' } },
      { path: 'audit', name: 'audit', component: () => import('@/views/Placeholder.vue'), meta: { title: '审计日志', group: '运维' } },
      { path: 'settings', name: 'settings', component: () => import('@/views/Placeholder.vue'), meta: { title: '系统设置', group: '运维' } }
    ]
  },
  { path: '/:pathMatch(.*)*', redirect: '/dashboard' }
]

const router = createRouter({
  history: createWebHistory(),
  routes
})

export default router
