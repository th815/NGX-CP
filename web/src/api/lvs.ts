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
  rip: string
  rport: number
  weight: number
  enabled: boolean
  state: string
  node_id: number
  address: string
  vs_key: string
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
