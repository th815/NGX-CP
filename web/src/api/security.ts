import client from '@/api/client'

// ---- 类型：与控制面 internal/security 对齐 ----

// 安全事件（T067 处置状态机视图）。
export interface SecurityEvent {
  id: number
  rule_id: string
  rule_name: string
  level: 'INFO' | 'WARN' | 'CRITICAL'
  node: string
  sample: string // 证据：原始日志片段
  handled: boolean
  action: 'pending' | 'blocked' | 'ignored'
  created_at: string
}

export interface EventListResp {
  items: SecurityEvent[]
  total: number
}

export interface EventListParams {
  level?: string
  handled?: boolean
  page?: number
  size?: number
}

// 攻击检测规则（T066 DefaultRules）。
export interface SecurityRule {
  id: string
  name: string
  level: string
  window: string
  threshold: number
  action: 'auto' | 'semi' | 'alert'
  sql: string
}

// 封禁变更单返回（T068，仅取展示所需字段）。
export interface BlockOrder {
  id: number
  title: string
  type: string
  status: string
}

export interface HandleReq {
  action: 'blocked' | 'ignored'
}

export interface BlockReq {
  ip: string
  reason?: string
  operator?: string
}

// ---- 接口：安全事件 / 封禁 / 规则（T067/T068/T069/T066） ----

export async function listEvents(params: EventListParams = {}): Promise<EventListResp> {
  const r = await client.get<{ data: EventListResp }>('/security/events', { params })
  return r.data.data
}

export async function getEvent(id: number): Promise<SecurityEvent> {
  const r = await client.get<{ data: SecurityEvent }>(`/security/events/${id}`)
  return r.data.data
}

export async function handleEvent(id: number, action: 'blocked' | 'ignored'): Promise<void> {
  await client.post(`/security/events/${id}/handle`, { action } satisfies HandleReq)
}

// blockEvent 从安全事件一键封禁（提取样本来源 IP → security_block 变更单）。
export async function blockEvent(id: number, operator?: string): Promise<BlockOrder> {
  const r = await client.post<{ data: BlockOrder }>(
    `/security/events/${id}/block`,
    operator ? { operator } : {}
  )
  return r.data.data
}

// blockIP 直接封禁一个 IP。
export async function blockIP(ip: string, reason?: string, operator?: string): Promise<BlockOrder> {
  const r = await client.post<{ data: BlockOrder }>('/security/blocklist', {
    ip,
    reason,
    operator
  } satisfies BlockReq)
  return r.data.data
}

// unblockIP 解封一个 IP（同样走变更单）。
export async function unblockIP(ip: string, reason?: string, operator?: string): Promise<BlockOrder> {
  const r = await client.delete<{ data: BlockOrder }>(`/security/blocklist/${encodeURIComponent(ip)}`, {
    data: { reason, operator }
  })
  return r.data.data
}

export async function listRules(): Promise<SecurityRule[]> {
  const r = await client.get<{ data: SecurityRule[] }>('/security/rules')
  return r.data.data
}
