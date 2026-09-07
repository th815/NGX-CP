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
