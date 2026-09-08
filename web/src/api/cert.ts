import client from '@/api/client'

// 与控制面 internal/domain/cert 的 CertificateView 对齐。
export interface CertView {
  id: number
  domain: string
  sans: string[]
  issuer: string
  serial_number: string
  fingerprint_sha: string
  not_before: string
  not_after: string
  key_alg: string
  source: string
  status: string
  created_at: string
  warnings?: string[]
}

export interface ValidationResult {
  ok: boolean
  errors: string[]
  warnings: string[]
}

export interface UploadCertIn {
  domain?: string
  cert_pem: string
  key_pem?: string
  chain_pem?: string
  sans?: string[]
  issuer?: string
}

export async function listCerts(): Promise<CertView[]> {
  const r = await client.get<{ data: CertView[] }>('/certs')
  return r.data.data
}

export async function getCert(id: number): Promise<CertView> {
  const r = await client.get<{ data: CertView }>(`/certs/${id}`)
  return r.data.data
}

export async function uploadCert(body: UploadCertIn): Promise<CertView> {
  const r = await client.post<{ data: CertView }>('/certs', body)
  return r.data.data
}

export async function deleteCert(id: number): Promise<void> {
  await client.delete(`/certs/${id}`)
}

// 分发相关类型（与 internal/domain/cert 的 DistributeResult / DeploymentResult 对齐）。
export interface DeploymentResult {
  node_id: number
  node_name: string
  status: string // deployed / failed
  error?: string
  deployed_at?: number // unix 秒
}

export interface DistributeResult {
  total: number
  deployed: number
  failed: number
  items: DeploymentResult[]
}

export interface DistributeCertIn {
  node_ids: number[]
  ssl_dir?: string
  nginx_path?: string
  reload?: boolean
  observe_window_sec?: number
  probe_url?: string
}

export async function distributeCert(id: number, body: DistributeCertIn): Promise<DistributeResult> {
  const r = await client.post<{ data: DistributeResult }>(`/certs/${id}/distribute`, body)
  return r.data.data
}

export async function getDeployments(id: number): Promise<DeploymentResult[]> {
  const r = await client.get<{ data: DeploymentResult[] }>(`/certs/${id}/deployments`)
  return r.data.data
}

// T045 ACME 签发 / 续期 API（与控制面 handler.CertHandler.IssueACME / Renew 对齐）。
export interface IssueACMEIn {
  domains: string[]
  email: string
  provider_type?: string
  provider_token: string
  key_alg?: string
  ca_dir_url?: string
}

export async function issueACME(body: IssueACMEIn): Promise<CertView> {
  const r = await client.post<{ data: CertView }>('/certs/issue', body)
  return r.data.data
}

// 仅 ACME 签发的证书可续期（续期复用存储的账户密钥 + provider Token）。
export async function renewCert(id: number): Promise<void> {
  await client.post(`/certs/${id}/renew`, {})
}

// 节点摘要（分发弹窗多选用，复用 nodes 列表的精简字段）。
export interface NodeBrief {
  id: number
  name: string
  address: string
  role: string
  status: string
}

export async function listNodesBrief(): Promise<NodeBrief[]> {
  const r = await client.get<{ data: NodeBrief[] }>('/nodes')
  return r.data.data
}
