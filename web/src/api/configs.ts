import client from '@/api/client'

// ---- 类型：与控制面 internal/domain/config 的 JSON 视图对齐 ----

// 配置中心文件视图（T021 ListFiles / GetFile）。
export interface ConfigFileItem {
  id: number
  node_id: number
  path: string
  format: string
  current_rev_id: number
  current_sha: string
  current_size: number
  current_content?: string // 仅 GetFile 填充
  source: string
  author: string
  created_at: string
  updated_at: string
}

// 版本视图（T021 ListRevisions）。
export interface RevisionView {
  id: number
  node_id: number
  path: string
  sha256: string
  source: string
  author: string
  message: string
  parent_id: number
  change_order_id: number
  created_at: string
}

// Diff 行（T022）。
export interface DiffLine {
  type: 'add' | 'del' | 'context'
  content: string
  old_no: number
  new_no: number
}

// Diff 区块（T022）。
export interface DiffHunk {
  old_start: number
  old_lines: number
  new_start: number
  new_lines: number
  lines: DiffLine[]
}

// Diff 结果（T022）。
export interface DiffResult {
  from: number
  to: number
  stats: { added: number; deleted: number; changed: number }
  hunks: DiffHunk[]
}

// nginx -t 结构化错误（T024）。
export interface NginxError {
  level: string
  message: string
  file: string
  line: number
}

// 校验结果（T024 + T025 复用）。
export interface ValidateResult {
  ok: boolean
  raw: string
  errors: NginxError[]
}

// 语义校验问题（T025）。
export interface SemanticIssue {
  rule_id: string
  severity: string
  message: string
  file: string
  line: number
  fix: string
}

export interface SemanticResult {
  node_id: number
  issues: SemanticIssue[]
  count: number
}

// 漂移项（T026）。
export interface DriftItem {
  path: string
  expected_sha: string
  actual_sha: string
  diff: DiffResult | null
  detected_at: string
  severity: string
}

export interface DriftReport {
  node_id: number
  items: DriftItem[]
  note?: string
}

// 配置模板（T027）。
export interface ConfigTemplate {
  id: number
  name: string
  content: string
  applies_to: string
  variables: string[]
  created_at: string
  updated_at: string
}

// 变量视图（T027，secret 已打码）。
export interface VariableView {
  scope: string
  target_id: number
  key: string
  value: string
  secret: boolean
}

// ---- 信封辅助 ----

interface ListEnvelope<T> {
  code: number
  message: string
  data: T[]
  total: number
}

// ---- 接口：配置树 / 版本 / diff ----

export async function listConfigFiles(nodeId: number): Promise<ConfigFileItem[]> {
  const r = await client.get<ListEnvelope<ConfigFileItem>>('/configs', { params: { node_id: nodeId } })
  return r.data.data ?? []
}

export async function getConfigFile(id: number): Promise<ConfigFileItem> {
  const r = await client.get<{ data: ConfigFileItem }>(`/configs/${id}`)
  return r.data.data
}

export async function listRevisions(id: number): Promise<RevisionView[]> {
  const r = await client.get<ListEnvelope<RevisionView>>(`/configs/${id}/revisions`)
  return r.data.data ?? []
}

// manualEdit 把编辑器内容存为新版本（来源 manual_edit，T028 编辑→保存）。
export async function manualEdit(
  id: number,
  content: string,
  message: string,
  author?: string
): Promise<RevisionView> {
  const r = await client.post<{ data: RevisionView }>(`/configs/${id}/manual-edit`, {
    content,
    message,
    author
  })
  return r.data.data
}

// diffRevisions 对同文件两版做语义 diff。校验失败（4012）由拦截器抛错，调用方自行捕获。
export async function diffRevisions(fileId: number, from: number, to: number): Promise<DiffResult> {
  const r = await client.get<{ data: DiffResult }>(`/configs/${fileId}/diff`, {
    params: { from, to }
  })
  return r.data.data
}

// ---- 接口：校验（T024 nginx -t + T025 语义） ----

export interface ValidateReq {
  node_id: number
  nginx_path?: string
  prefix?: string
  conf_path: string
  files: { path: string; content: string }[]
}

// validateConfig 触发目标 Agent 跑 nginx -t（T024）。
// 成功返回 {ok:true}；语法错误时后端以 4012 返回结构化错误数组，此处转为 {ok:false, errors}。
export async function validateConfig(req: ValidateReq): Promise<ValidateResult> {
  try {
    const r = await client.post<{ data: ValidateResult }>('/configs/validate', req)
    return r.data.data
  } catch (e: unknown) {
    const data = (e as { response?: { data?: { data?: NginxError[] } } })?.response?.data
    if (data && Array.isArray(data.data)) {
      return { ok: false, raw: '', errors: data.data }
    }
    throw e
  }
}

// semanticCheck 对节点跑语义规则引擎（T025）。
export async function semanticCheck(nodeId: number): Promise<SemanticResult> {
  const r = await client.post<{ data: SemanticResult }>('/configs/semantic-check', { node_id: nodeId })
  return r.data.data
}

// ---- 接口：漂移（T026） ----

export async function listDrift(nodeId?: number): Promise<DriftReport | DriftItem[]> {
  const r = await client.get<{ data: DriftReport | DriftItem[] }>('/configs/drift', {
    params: nodeId ? { node_id: nodeId } : undefined
  })
  return r.data.data
}

// ---- 接口：模板与变量（T027） ----

export async function listTemplates(): Promise<ConfigTemplate[]> {
  const r = await client.get<ListEnvelope<ConfigTemplate>>('/templates')
  return r.data.data ?? []
}

export async function getTemplate(id: number): Promise<ConfigTemplate> {
  const r = await client.get<{ data: ConfigTemplate }>(`/templates/${id}`)
  return r.data.data
}

// renderTemplate 按节点批量渲染模板，返回 node_id -> 配置文本。
export async function renderTemplate(id: number, nodeIds: number[]): Promise<Record<number, string>> {
  const r = await client.post<{ data: Record<number, string> }>(`/templates/${id}/render`, {
    node_ids: nodeIds
  })
  return r.data.data
}

export async function listVariables(opts: { scope?: string; targetId?: number } = {}): Promise<
  VariableView[]
> {
  const params: Record<string, unknown> = {}
  if (opts.scope) params.scope = opts.scope
  if (opts.targetId !== undefined) params.target_id = opts.targetId
  const r = await client.get<ListEnvelope<VariableView>>('/variables', { params })
  return r.data.data ?? []
}

export interface SetVariableReq {
  scope: string
  target_id: number
  key: string
  value: string
  secret?: boolean
}

export async function setVariable(req: SetVariableReq): Promise<void> {
  await client.post('/variables', req)
}
