// Package node 实现节点域的核心逻辑：CRUD、能力基线占位、一次性接入令牌。
// M1 阶段 Agent 尚未常驻，能力上报（T016/T017）与心跳（T015）后续里程碑填充。
package node

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/ent"
	entnode "github.com/th/ngxcp/ent/node"
	entnodecap "github.com/th/ngxcp/ent/nodecapability"
	"github.com/th/ngxcp/internal/domain/compliance"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// Service 持有 ent 客户端与接入令牌内存表（令牌持久化随 T014 落地）。
type Service struct {
	client *ent.Client

	mu     sync.RWMutex
	tokens map[string]*enrollToken

	// compMu / compReports 缓存各节点最近一次合规自检报告（M1 内存态，无独立表；
	// 与 clock_skew 同理，真实持久化随 T018/T019 后续里程碑）。
	compMu       sync.RWMutex
	compReports  map[int]*agentv1.ComplianceReport
}

// New 构造节点服务。
func New(client *ent.Client) *Service {
	return &Service{
		client:      client,
		tokens:      make(map[string]*enrollToken),
		compReports: make(map[int]*agentv1.ComplianceReport),
	}
}

// Client 暴露底层 ent 客户端，供审计中间件等复用同一连接（避免重复持有）。
func (s *Service) Client() *ent.Client { return s.client }

// enrollToken 一次性接入令牌记录（只存哈希，原文仅生成时返回一次）。
// nodeID 用于在 T014 Agent 注册时把令牌回绑到具体节点。
type enrollToken struct {
	nodeID    int
	expiresAt time.Time
	used      bool
}

// NodeOut 是节点的对外视图（脱敏后的 DTO）。
type NodeOut struct {
	ID               int        `json:"id"`
	Name             string     `json:"name"`
	Address          string     `json:"address"`
	Role             string     `json:"role"`
	Status           string     `json:"status"`
	LvsWeight        int        `json:"lvs_weight"`
	LvsEnabled       bool       `json:"lvs_enabled"`
	LastHeartbeatAt  *time.Time `json:"last_heartbeat_at,omitempty"`
	ClockSkewSeconds *float64   `json:"clock_skew_seconds,omitempty"` // T015：仅在线且上报过时间戳时存在
	Compliance       *NodeComplianceView `json:"compliance,omitempty"` // T019：最近一次 DR 合规自检
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// NodeComplianceView 是节点最近一次合规自检的对外视图（来自 Agent 上报 + 控制面判定）。
type NodeComplianceView struct {
	Passed         bool     `json:"passed"`
	CheckedAt      int64    `json:"checked_at"`
	CriticalFailed []string `json:"critical_failed,omitempty"` // 未通过的关键项名（驱动 degraded 的依据）
}

func toOut(n *ent.Node) *NodeOut {
	out := &NodeOut{
		ID:          n.ID,
		Name:        n.Name,
		Address:     n.Address,
		Role:        string(n.Role),
		Status:      string(n.Status),
		LvsWeight:   n.LvsWeight,
		LvsEnabled:  n.LvsEnabled,
		CreatedAt:   n.CreatedAt,
		UpdatedAt:   n.UpdatedAt,
	}
	if !n.LastHeartbeatAt.IsZero() {
		t := n.LastHeartbeatAt
		out.LastHeartbeatAt = &t
	}
	return out
}

// ListOpts 列表过滤与分页。
type ListOpts struct {
	Role   string
	Status string
	Limit  int
	Offset int
}

// List 分页列出节点，返回本页数据与总数。
func (s *Service) List(ctx context.Context, o ListOpts) ([]*NodeOut, int, error) {
	q := s.client.Node.Query()
	if o.Role != "" {
		q = q.Where(entnode.RoleEQ(entnode.Role(o.Role)))
	}
	if o.Status != "" {
		q = q.Where(entnode.StatusEQ(entnode.Status(o.Status)))
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.CodeInternal, "统计节点数失败", err)
	}
	if o.Limit <= 0 || o.Limit > 200 {
		o.Limit = 50
	}
	nodes, err := q.Order(ent.Asc("id")).Offset(o.Offset).Limit(o.Limit).All(ctx)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.CodeInternal, "列出节点失败", err)
	}
	out := make([]*NodeOut, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, toOut(n))
	}
	return out, total, nil
}

// CreateNodeIn 创建节点入参。
type CreateNodeIn struct {
	Name      string `json:"name"`
	Address   string `json:"address"`
	Role      string `json:"role"`
	LvsWeight *int   `json:"lvs_weight,omitempty"`
}

// Create 新建节点，初态为 enrolling / 角色 unknown（待能力上报后由 T019 识别）。
func (s *Service) Create(ctx context.Context, in CreateNodeIn) (*NodeOut, error) {
	if in.Name == "" {
		return nil, apperr.New(apperr.CodeInvalid, "节点名称不能为空")
	}
	role := entnode.RoleUnknown
	if in.Role != "" {
		role = entnode.Role(in.Role)
		if err := entnode.RoleValidator(role); err != nil {
			return nil, apperr.New(apperr.CodeInvalid, "非法 role")
		}
	}
	w := 1
	if in.LvsWeight != nil {
		w = *in.LvsWeight
	}
	n, err := s.client.Node.Create().
		SetName(in.Name).
		SetAddress(in.Address).
		SetRole(role).
		SetStatus(entnode.StatusEnrolling).
		SetLvsWeight(w).
		SetLvsEnabled(false).
		Save(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "创建节点失败", err)
	}
	return toOut(n), nil
}

// Get 按 ID 取节点；不存在返回 CodeNotFound。
func (s *Service) Get(ctx context.Context, id int) (*NodeOut, error) {
	n, err := s.client.Node.Query().Where(entnode.ID(id)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "节点不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "查询节点失败", err)
	}
	return toOut(n), nil
}

// UpdateNodeIn 更新节点入参（全部可选，仅更新非空字段）。
type UpdateNodeIn struct {
	Name       *string `json:"name,omitempty"`
	Address    *string `json:"address,omitempty"`
	Role       *string `json:"role,omitempty"`
	Status     *string `json:"status,omitempty"`
	LvsWeight  *int    `json:"lvs_weight,omitempty"`
	LvsEnabled *bool   `json:"lvs_enabled,omitempty"`
}

// Update 局部更新节点。
func (s *Service) Update(ctx context.Context, id int, in UpdateNodeIn) (*NodeOut, error) {
	u := s.client.Node.UpdateOneID(id)
	if in.Name != nil {
		u.SetName(*in.Name)
	}
	if in.Address != nil {
		u.SetAddress(*in.Address)
	}
	if in.Role != nil {
		r := entnode.Role(*in.Role)
		if err := entnode.RoleValidator(r); err != nil {
			return nil, apperr.New(apperr.CodeInvalid, "非法 role")
		}
		u.SetRole(r)
	}
	if in.Status != nil {
		st := entnode.Status(*in.Status)
		if err := entnode.StatusValidator(st); err != nil {
			return nil, apperr.New(apperr.CodeInvalid, "非法 status")
		}
		u.SetStatus(st)
	}
	if in.LvsWeight != nil {
		u.SetLvsWeight(*in.LvsWeight)
	}
	if in.LvsEnabled != nil {
		u.SetLvsEnabled(*in.LvsEnabled)
	}
	n, err := u.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "节点不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "更新节点失败", err)
	}
	return toOut(n), nil
}

// Delete 删除节点。
func (s *Service) Delete(ctx context.Context, id int) error {
	err := s.client.Node.DeleteOneID(id).Exec(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return apperr.New(apperr.CodeNotFound, "节点不存在")
		}
		return apperr.Wrap(apperr.CodeInternal, "删除节点失败", err)
	}
	return nil
}

// IssueEnrollToken 为指定节点生成一次性接入令牌（格式 ngxcp_<24B base62>），仅返回原文一次。
// 库中只存 SHA-256 哈希与 nodeID；默认 1h 有效。节点不存在返回 CodeNotFound。
func (s *Service) IssueEnrollToken(ctx context.Context, id int, ttl time.Duration) (string, time.Time, error) {
	// 令牌必须绑定到真实存在的节点，否则校验时无法回绑。
	if _, err := s.Get(ctx, id); err != nil {
		return "", time.Time{}, err
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	raw, err := newToken()
	if err != nil {
		return "", time.Time{}, apperr.Wrap(apperr.CodeInternal, "生成令牌失败", err)
	}
	sum := hashToken(raw)
	exp := time.Now().Add(ttl)
	s.mu.Lock()
	s.tokens[sum] = &enrollToken{nodeID: id, expiresAt: exp}
	s.mu.Unlock()
	return raw, exp, nil
}

// VerifyEnrollToken 校验接入令牌：存在 + 未使用 + 未过期，校验成功后标记已用（一次性）。
// 返回令牌绑定的 nodeID，供 T014 Agent 注册流程把请求回绑到具体节点。
// 接受 ctx 以便后续令牌持久化（落库）时传递超时/取消。
func (s *Service) VerifyEnrollToken(ctx context.Context, raw string) (int, error) {
	sum := hashToken(raw)
	s.mu.RLock()
	t, ok := s.tokens[sum]
	s.mu.RUnlock()
	if !ok {
		return 0, apperr.New(apperr.CodeUnauthorized, "令牌无效")
	}
	if t.used {
		return 0, apperr.New(apperr.CodeUnauthorized, "令牌已使用")
	}
	if time.Now().After(t.expiresAt) {
		return 0, apperr.New(apperr.CodeUnauthorized, "令牌已过期")
	}
	s.mu.Lock()
	t.used = true
	s.mu.Unlock()
	return t.nodeID, nil
}

// MarkEnrolled 将节点从 enrolling 标记为 online（T014 Agent 注册成功回写）。
// 仅当节点当前处于 enrolling 才允许跳转，避免把已上线节点误置为 online。
func (s *Service) MarkEnrolled(ctx context.Context, id int) error {
	_, err := s.client.Node.UpdateOneID(id).
		Where(entnode.StatusEQ(entnode.StatusEnrolling)).
		SetStatus(entnode.StatusOnline).
		Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return apperr.New(apperr.CodeNotFound, "节点不存在")
		}
		return apperr.Wrap(apperr.CodeInternal, "标记节点已注册失败", err)
	}
	return nil
}

// ---- T015：心跳与会话状态机 ----

// TouchHeartbeat 处理一次心跳到达：刷新 last_heartbeat_at，并把 enrolling/offline 拉回 online。
// 状态转移遵循 FSM：enrolling --(首跳)--> online；offline --(重连)--> online。
// 仅校验节点存在（不存在返回 CodeNotFound），超时判定由 SessionManager 扫描器负责（用控制面本地时间）。
func (s *Service) TouchHeartbeat(ctx context.Context, id int) error {
	now := time.Now()
	if _, err := s.client.Node.UpdateOneID(id).
		SetLastHeartbeatAt(now).
		Save(ctx); err != nil {
		if ent.IsNotFound(err) {
			return apperr.New(apperr.CodeNotFound, "节点不存在")
		}
		return apperr.Wrap(apperr.CodeInternal, "更新心跳时间失败", err)
	}
	// 仅在确需翻转时才发第二句 UPDATE，避免无谓写放大。
	if _, err := s.client.Node.UpdateOneID(id).
		Where(entnode.StatusIn(entnode.StatusEnrolling, entnode.StatusOffline)).
		SetStatus(entnode.StatusOnline).
		Save(ctx); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "翻转节点在线态失败", err)
	}
	return nil
}

// MarkOffline 将节点从 online 标记为 offline（T015 扫描器在超时无心跳时调用）。
// 仅当节点当前为 online 才翻转，避免把 enrolling/degraded 误置为 offline。
func (s *Service) MarkOffline(ctx context.Context, id int) error {
	_, err := s.client.Node.UpdateOneID(id).
		Where(entnode.StatusEQ(entnode.StatusOnline)).
		SetStatus(entnode.StatusOffline).
		Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return apperr.New(apperr.CodeNotFound, "节点不存在")
		}
		return apperr.Wrap(apperr.CodeInternal, "标记节点离线失败", err)
	}
	return nil
}

// CapabilityIn 是 Agent 上报的能力基线入参（由传输层从 proto 映射，避免域层依赖 gRPC 契约）。
type CapabilityIn struct {
	Hostname      string
	OS            string
	Kernel        string
	HasKeepalived bool
	HasIPVS       bool
	NginxVersion  string
	NginxPrefix   string
	NginxConfPath string
	NginxSbinPath string
	NginxModules  []string
	NginxRawArgs  string
	ConfigHash    string
}

// SaveCapability 落库节点能力基线（T016/T017 解析结果）。按 nodeID upsert 到 node_capabilities，
// 并计算整份画像的 checksum 便于双机一致性 diff。能力上报成功后按 FSM 将 enrolling 拉到 online。
func (s *Service) SaveCapability(ctx context.Context, id int, in CapabilityIn) error {
	// 节点必须存在。
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	modules := in.NginxModules
	if modules == nil {
		modules = []string{}
	}
	checksum := capabilityChecksum(in)

	existing, err := s.client.NodeCapability.Query().
		Where(entnodecap.HasNodeWith(entnode.ID(id))).
		Exist(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "查询已有能力基线失败", err)
	}

	if existing {
		_, err = s.client.NodeCapability.Update().
			Where(entnodecap.HasNodeWith(entnode.ID(id))).
			SetVersion(in.NginxVersion).
			SetPrefix(in.NginxPrefix).
			SetConfPath(in.NginxConfPath).
			SetSbinPath(in.NginxSbinPath).
			SetModules(modules).
			SetRawArgs(in.NginxRawArgs).
			SetChecksum(checksum).
			SetCapturedAt(time.Now()).
			Save(ctx)
	} else {
		_, err = s.client.NodeCapability.Create().
			SetNodeID(id).
			SetVersion(in.NginxVersion).
			SetPrefix(in.NginxPrefix).
			SetConfPath(in.NginxConfPath).
			SetSbinPath(in.NginxSbinPath).
			SetModules(modules).
			SetRawArgs(in.NginxRawArgs).
			SetChecksum(checksum).
			SetCapturedAt(time.Now()).
			Save(ctx)
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "落库能力基线失败", err)
	}

	// 能力上报成功：enrolling --(capability)--> online。
	if _, err := s.client.Node.UpdateOneID(id).
		Where(entnode.StatusEQ(entnode.StatusEnrolling)).
		SetStatus(entnode.StatusOnline).
		Save(ctx); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "翻转节点在线态失败", err)
	}
	return nil
}

// capabilityChecksum 对能力画像做内容寻址哈希（双机一致性 diff 用）。
func capabilityChecksum(in CapabilityIn) string {
	modules := in.NginxModules
	if modules == nil {
		modules = []string{}
	}
	h := sha256.New()
	_, _ = h.Write([]byte(in.NginxVersion + "\x00"))
	_, _ = h.Write([]byte(in.NginxPrefix + "\x00"))
	_, _ = h.Write([]byte(in.NginxConfPath + "\x00"))
	_, _ = h.Write([]byte(in.NginxSbinPath + "\x00"))
	for _, m := range modules {
		_, _ = h.Write([]byte(m + "\x00"))
	}
	_, _ = h.Write([]byte(in.NginxRawArgs + "\x00"))
	_, _ = h.Write([]byte(in.ConfigHash + "\x00"))
	return hex.EncodeToString(h.Sum(nil))
}

// ---- T019：DR 合规自检（控制面侧判定 + 状态机流转）----

// SetCompliance 处理 Agent 上报的 DR 合规自检报告：缓存最新报告，并按 FSM 驱动节点状态。
//   - online 且存在未通过的 critical 项 → degraded（合规不通过）
//   - degraded 且报告整体通过 → 恢复 online
//
// report 为 nil 时直接忽略（不驱动任何流转）。
func (s *Service) SetCompliance(ctx context.Context, id int, report *agentv1.ComplianceReport) error {
	if report == nil {
		return nil
	}
	s.compMu.Lock()
	s.compReports[id] = report
	s.compMu.Unlock()

	passed := compliance.Evaluate(report).Passed
	cur, err := s.client.Node.Query().Where(entnode.ID(id)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return apperr.New(apperr.CodeNotFound, "节点不存在")
		}
		return apperr.Wrap(apperr.CodeInternal, "查询节点失败", err)
	}
	switch cur.Status {
	case entnode.StatusOnline:
		if !passed {
			if _, err := s.client.Node.UpdateOneID(id).SetStatus(entnode.StatusDegraded).Save(ctx); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "标记 degraded 失败", err)
			}
			slog.Default().Warn("node compliance degraded", "node_id", id)
		}
	case entnode.StatusDegraded:
		if passed {
			if _, err := s.client.Node.UpdateOneID(id).SetStatus(entnode.StatusOnline).Save(ctx); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "恢复 online 失败", err)
			}
		}
	}
	return nil
}

// GetCompliance 返回节点最近一次合规自检报告（内存态；无则 nil）。
func (s *Service) GetCompliance(ctx context.Context, id int) (*agentv1.ComplianceReport, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	s.compMu.RLock()
	defer s.compMu.RUnlock()
	if r, ok := s.compReports[id]; ok {
		return r, nil
	}
	return nil, nil
}

// GetCapability 返回节点能力基线（真实解析器已在 internal/agent/capability 落地：
// ParseNginxV / ParseConfigTree；待 T015 心跳上报后由 Agent 采集并入库，此处仍返回占位）。
func (s *Service) GetCapability(ctx context.Context, id int) (map[string]any, error) {
	n, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"node_id": n.ID,
		"role":    n.Role,
		"status":  n.Status,
		"note":    "能力上报（nginx -V / -T 解析）已由 internal/agent/capability 实现，待 T015 接入上报",
	}, nil
}

// RefreshCapability 触发一次能力刷新（T015 会话管理落地后下发指令；此处仅校验节点存在）。
func (s *Service) RefreshCapability(ctx context.Context, id int) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	return nil
}

func newToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "ngxcp_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(raw string) string {
	// 轻量哈希即可：库内只存哈希，原文泄露也无法重放（配合一次性 + 有效期）。
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", sum)
}
