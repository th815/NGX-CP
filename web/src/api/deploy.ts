import client from '@/api/client'

// ---- 类型：与控制面 internal/domain/deploy 的 JSON 视图对齐 ----

export type OrderStatus =
  | 'draft'
  | 'pending_approval'
  | 'pending'
  | 'running'
  | 'success'
  | 'failed'
  | 'rolling_back'
  | 'rolled_back'
  | 'partial_success'
  | 'rejected'
  | 'canceled'

export interface DeployStrategyView {
  mode: string
  batch_size: number
  observe_window: number
  failure_threshold: number
  auto_rollback: boolean
  approval_required: boolean
}

export interface ChangeOrderView {
  id: number
  title: string
  type: string
  source: string
  status: OrderStatus
  target_nodes: number[]
  strategy: DeployStrategyView
  created_by: string
  started_at: string
  finished_at: string
  created_at: string
  updated_at: string
}

export interface CreateChangeOrderIn {
  title: string
  type?: string
  source?: string
  target_nodes?: number[]
  config_revision_ids?: number[]
  strategy?: Partial<DeployStrategyView>
  created_by?: string
  comment?: string
}

export interface ApprovalView {
  order_id: number
  required_by?: string
  status: string
  approver?: string
  comment?: string
  created_at: string
  updated_at: string
  expires_at?: string
}

interface Envelope<T> {
  code: number
  message: string
  data: T
  total?: number
}

// ---- 接口 ----

export async function listChangeOrders(status?: string): Promise<ChangeOrderView[]> {
  const r = await client.get<Envelope<ChangeOrderView[]>>('/change-orders', {
    params: status ? { status } : undefined
  })
  return r.data.data ?? []
}

export async function getChangeOrder(id: number): Promise<ChangeOrderView> {
  const r = await client.get<Envelope<ChangeOrderView>>(`/change-orders/${id}`)
  return r.data.data
}

export async function createChangeOrder(inp: CreateChangeOrderIn): Promise<ChangeOrderView> {
  const r = await client.post<Envelope<ChangeOrderView>>('/change-orders', inp)
  return r.data.data
}

export async function submitChangeOrder(
  id: number
): Promise<{ id: number; status: string; approval_required: boolean; required_by?: string }> {
  const r = await client.post<
    Envelope<{ id: number; status: string; approval_required: boolean; required_by?: string }>
  >(`/change-orders/${id}/submit`)
  return r.data.data
}

export async function approveChangeOrder(id: number, approved_by?: string): Promise<void> {
  await client.post(`/change-orders/${id}/approve`, { approved_by })
}

export async function rejectChangeOrder(id: number): Promise<void> {
  await client.post(`/change-orders/${id}/reject`, {})
}

export async function cancelChangeOrder(id: number): Promise<void> {
  await client.post(`/change-orders/${id}/cancel`)
}

export async function rollbackChangeOrder(id: number): Promise<void> {
  await client.post(`/change-orders/${id}/rollback`)
}

export async function getApprovalForOrder(id: number): Promise<ApprovalView | null> {
  const r = await client.get<Envelope<ApprovalView>>(`/change-orders/${id}/approval`)
  return r.data.data ?? null
}

export async function listApprovals(status?: string): Promise<ApprovalView[]> {
  const r = await client.get<Envelope<ApprovalView[]>>('/approvals', {
    params: status ? { status } : undefined
  })
  return r.data.data ?? []
}
