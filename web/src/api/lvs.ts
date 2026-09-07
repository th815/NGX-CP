import client from './client'

export interface DirectorNode {
  id: number
  node_id: number
  address: string
  role: string
  status: string
  state: string
  holding_vip: boolean
  priority: number
  vrid: number
}

export interface VirtualServiceView {
  id: number
  director_id: number
  director_state: string
  vip: string
  port: number
  protocol: string
  scheduler: string
  enabled: boolean
}

export interface RealServerNode {
  id: number
  rip: string
  rport: number
  weight: number
  enabled: boolean
  state: string
  node_id: number
  address: string
  vs_key: string
}

// 摘除：将该 RS 在其所属全部 VS 上的 LVS 权重置 0（流量不再命中），模型标记禁用。
export async function drainRealServer(id: number): Promise<void> {
  await client.post(`/lvs/real-servers/${id}/drain`)
}
// 恢复：下发基线权重并启用。
export async function restoreRealServer(id: number): Promise<void> {
  await client.post(`/lvs/real-servers/${id}/restore`)
}
// 设置基线权重并下发（>0 启用，=0 等效摘除）。
export async function setRealServerWeight(id: number, weight: number): Promise<void> {
  await client.post(`/lvs/real-servers/${id}/weight`, { weight })
}

export interface Topology {
  vip: string
  directors: DirectorNode[]
  vs: VirtualServiceView[]
  rs: RealServerNode[]
}

export async function getTopology(): Promise<Topology> {
  const r = await client.get<{ data: Topology }>('/lvs/topology')
  return r.data.data
}
