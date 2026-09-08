import client from '@/api/client'

// ---- 类型：与控制面 internal/logstore.Entry 对齐 ----
// 注意：Entry 结构体无 json tag，Go 序列化用字段原名（TS/Node/RemoteAddr …）。

export interface LogEntry {
  TS: string // RFC3339 时间
  Node: string
  RID: string // TraceID / request_id
  RemoteAddr: string
  Server: string
  URI: string
  Status: number
  UpstreamAddr: string
  UpstreamStatus: string
  UpstreamRT: number
  RequestRT: number
  Bytes: number
  UA: string
  Raw: string
}

export interface SearchResp {
  items: LogEntry[]
  total: number
  took_ms: number
}

export interface TraceResult {
  rid: string
  spans: LogEntry[]
  nodes: string[]
  first_hop: string
  bottleneck: string
  took_ms: number
}

export interface AggRow {
  Key: string
  Count: number
  Err: number
  P50: number
  P95: number
  P99: number
}

export interface AggResp {
  rows: AggRow[]
  total: number
  took_ms: number
}

export type AggMetric = 'top_uri' | 'top_ip' | 'top_ua' | 'status_dist' | 'rt_percentile'

export interface SearchReq {
  time_from?: string
  time_to?: string
  nodes?: string[]
  status?: number[]
  uri?: string
  ip?: string
  rid?: string
  rt_min?: number
  regex?: boolean
  page?: number
  size?: number
}

export interface AggReq {
  metric: AggMetric
  window?: string
  nodes?: string[]
  status?: number[]
  uri?: string
  ip?: string
  rid?: string
  rt_min?: number
  regex?: boolean
  top_n?: number
}

// ---- 接口：日志检索 / 追踪 / 聚合（T060/T063/T064/T065） ----

export async function searchLogs(req: SearchReq): Promise<SearchResp> {
  const r = await client.post<{ data: SearchResp }>('/logs/search', req)
  return r.data.data
}

export async function traceLog(requestId: string): Promise<TraceResult> {
  const r = await client.get<{ data: TraceResult }>(`/logs/trace/${encodeURIComponent(requestId)}`)
  return r.data.data
}

export async function aggregateLogs(req: AggReq): Promise<AggResp> {
  const r = await client.post<{ data: AggResp }>('/logs/aggregate', req)
  return r.data.data
}
