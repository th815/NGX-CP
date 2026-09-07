import client from '@/api/client'

// ---- 类型：与控制面 internal/domain/node 的 JSON 视图对齐 ----

export type NodeStatus = 'enrolling' | 'online' | 'offline' | 'degraded'
export type NodeRole = 'unknown' | 'real_server' | 'director'

export interface NodeComplianceView {
  passed: boolean
  checked_at: number
  critical_failed: string[]
}

export interface NodeFsProbeView {
  passed: boolean
  checked_at: number
  critical_failed: string[]
}

export interface NodeOut {
  id: number
  name: string
  address: string
  role: NodeRole
  status: NodeStatus
  lvs_weight: number
  lvs_enabled: boolean
  last_heartbeat_at?: string
  clock_skew_seconds?: number
  compliance?: NodeComplianceView
  fs_probe?: NodeFsProbeView
  created_at: string
  updated_at: string
}

export interface CreateNodeIn {
  name: string
  address: string
  role: NodeRole
  lvs_weight?: number
}

export interface NginxCapabilityView {
  version?: string
  prefix?: string
  conf_path?: string
  sbin_path?: string
  modules: string[]
  raw_args?: string
  config_hash?: string
}

export interface SystemInfoView {
  os?: string
  kernel?: string
  nginx_managed_by?: string
  selinux_status?: string
  ulimit_nofile?: number
  timezone?: string
  ntp_synced?: boolean
  logrotate_conf?: string
  disk_free?: Record<string, number>
  warnings?: string[]
}

export interface ConfigFileView {
  path: string
  sha256: string
  size: number
  mod_time?: string
  captured_at: string
}

export interface LogTargetView {
  path: string
  type: string
  format?: string
  level?: string
  is_syslog: boolean
  is_off: boolean
  has_variable: boolean
  skip_reason?: string
  size: number
  inode: number
  stat_err?: string
  captured_at: string
}

export interface CapabilityView {
  node_id: number
  hostname?: string
  os?: string
  kernel?: string
  has_keepalived: boolean
  has_ipvsadm: boolean
  nginx?: NginxCapabilityView
  checksum?: string
  captured_at?: string
  system?: SystemInfoView
  config_files?: ConfigFileView[]
  log_targets?: LogTargetView[]
}

export interface EnrollTokenOut {
  token: string
  node_id: number
  expires_at: string
}

// 列表信封：{code,message,data:[...],total}
interface ListEnvelope<T> {
  code: number
  message: string
  data: T[]
  total: number
}

// ---- 接口 ----

export async function listNodes(opts: { role?: string; status?: string } = {}): Promise<{
  items: NodeOut[]
  total: number
}> {
  const r = await client.get<ListEnvelope<NodeOut>>('/nodes', { params: opts })
  // r.data 是信封；envelope.data 才是节点数组。
  return { items: r.data.data ?? [], total: r.data.total ?? 0 }
}

export async function getNode(id: number): Promise<NodeOut> {
  const r = await client.get<{ data: NodeOut }>(`/nodes/${id}`)
  return r.data.data
}

export async function getCapability(id: number): Promise<CapabilityView> {
  const r = await client.get<{ data: CapabilityView }>(`/nodes/${id}/capability`)
  return r.data.data
}

export async function getConfigFiles(id: number): Promise<ConfigFileView[]> {
  const r = await client.get<{ data: ConfigFileView[] }>(`/nodes/${id}/config-files`)
  return r.data.data ?? []
}

export async function getLogTargets(id: number): Promise<LogTargetView[]> {
  const r = await client.get<{ data: LogTargetView[] }>(`/nodes/${id}/log-targets`)
  return r.data.data ?? []
}

export async function createNode(inp: CreateNodeIn): Promise<NodeOut> {
  const r = await client.post<{ data: NodeOut }>('/nodes', inp)
  return r.data.data
}

export async function deleteNode(id: number): Promise<void> {
  await client.delete(`/nodes/${id}`)
}

// 局部更新：仅传需要改的字段（后端 UpdateNodeIn 全为指针字段）。
export interface UpdateNodeIn {
  name?: string
  address?: string
  role?: NodeRole
  status?: NodeStatus
  lvs_weight?: number
  lvs_enabled?: boolean
}

export async function updateNode(id: number, inp: UpdateNodeIn): Promise<NodeOut> {
  const r = await client.patch<{ data: NodeOut }>(`/nodes/${id}`, inp)
  return r.data.data
}

export async function refreshCapability(id: number): Promise<void> {
  await client.post(`/nodes/${id}/refresh`)
}

export async function issueEnrollToken(id: number, ttl?: string): Promise<EnrollTokenOut> {
  const params = ttl ? { ttl } : undefined
  const r = await client.post<{ data: EnrollTokenOut }>(`/nodes/${id}/enroll-token`, undefined, {
    params
  })
  return r.data.data
}

export interface JoinTokenOut {
  token: string
  expires_at: string
}

// 引导信息：前端拼装一键安装命令所需（控制面基址 / Agent 可达 gRPC 地址 / 二进制分发是否就绪）。
export interface BootstrapInfo {
  origin: string
  grpc_addr: string
  binary_ready: boolean
}

// 签发节点绑定 Join Token（一个 Agent 一个 Token，可复用重建证书；轮换即吊销旧令牌）。
export async function issueJoinToken(id: number, ttl?: string): Promise<JoinTokenOut> {
  const params = ttl ? { ttl } : undefined
  const r = await client.post<{ data: JoinTokenOut }>(`/nodes/${id}/join-token`, undefined, {
    params
  })
  return r.data.data
}

export async function revokeJoinToken(id: number): Promise<void> {
  await client.post(`/nodes/${id}/join-token/revoke`)
}

export async function getBootstrapInfo(): Promise<BootstrapInfo> {
  const r = await client.get<{ data: BootstrapInfo }>('/agent/bootstrap-info')
  return r.data.data
}

// ---- 首跑管理员令牌获取（免 SSH+grep） ----

export interface SetupToken {
  token: string
  acknowledged: boolean
}

// 首跑一次性获取管理员令牌（免鉴权，仅未确认时服务端返回）。
// 已确认(410)/未配置令牌(409) → 返回 null（无可展示）。
export async function getSetupToken(): Promise<SetupToken | null> {
  try {
    const r = await client.get<{ data: SetupToken }>('/admin/setup-token')
    return r.data.data
  } catch (e: unknown) {
    const err = e as { response?: { status?: number } }
    if (err?.response?.status === 410 || err?.response?.status === 409) return null
    return null
  }
}

// 完成首次设置：携带（已写入 store 的）有效令牌确认，锁定 setup-token。
export async function acknowledgeSetup(): Promise<void> {
  await client.post('/admin/setup-acknowledge')
}
