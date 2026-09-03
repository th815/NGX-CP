// Package node 实现节点域的核心逻辑：CRUD、能力基线占位、一次性接入令牌。
// M1 阶段 Agent 尚未常驻，能力上报（T016/T017）与心跳（T015）后续里程碑填充。
package node

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/ent"
	entnode "github.com/th/ngxcp/ent/node"
	entnodecap "github.com/th/ngxcp/ent/nodecapability"
	entncf "github.com/th/ngxcp/ent/nodeconfigfile"
	entnlt "github.com/th/ngxcp/ent/nodelogtarget"
	"github.com/th/ngxcp/internal/domain/compliance"
	"github.com/th/ngxcp/internal/domain/config"
	"github.com/th/ngxcp/internal/domain/probe"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// Service 持有 ent 客户端与接入令牌内存表（令牌持久化随 T014 落地）。
type Service struct {
	client *ent.Client

	// cfgStore 是 T021 配置版本化内容寻址存储（可空：未注入时 SaveConfigTree 仅维护
	// T018-A 元数据快照，不写入版本链；早期单测与 gRPC 单测据此跳过版本化路径）。
	cfgStore *config.ConfigStore

	// drift 是 T026 配置漂移检测器（可空：未注入时 SaveConfigTree 仅同步版本链，不做漂移检测）。
	drift *config.DriftDetector

	mu     sync.RWMutex
	tokens map[string]*enrollToken

	// compMu / compReports 缓存各节点最近一次合规自检报告（M1 内存态，无独立表；
	// 与 clock_skew 同理，真实持久化随 T018/T019 后续里程碑）。
	compMu      sync.RWMutex
	compReports map[int]*agentv1.ComplianceReport

	// fsMu / fsReports 缓存各节点最近一次日志/FS 健康探测报告（T018，内存态，同源）。
	fsMu      sync.RWMutex
	fsReports map[int]*agentv1.FsProbeReport
}

// New 构造节点服务。cfgStore 为 T021 配置版本化存储（可传 nil，见 Service.cfgStore 说明）。
func New(client *ent.Client, cfgStore *config.ConfigStore) *Service {
	return &Service{
		client:      client,
		cfgStore:    cfgStore,
		tokens:      make(map[string]*enrollToken),
		compReports: make(map[int]*agentv1.ComplianceReport),
		fsReports:   make(map[int]*agentv1.FsProbeReport),
	}
}

// SetDriftDetector 注入 T026 漂移检测器（可空：未注入时 SaveConfigTree 不做漂移检测）。
func (s *Service) SetDriftDetector(d *config.DriftDetector) {
	s.drift = d
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
	ClockSkewSeconds *float64            `json:"clock_skew_seconds,omitempty"` // T015：仅在线且上报过时间戳时存在
	Compliance       *NodeComplianceView `json:"compliance,omitempty"`        // T019：最近一次 DR 合规自检
	FsProbe          *NodeFsProbeView    `json:"fs_probe,omitempty"`          // T018：最近一次日志/FS 健康探测
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
}

// NodeComplianceView 是节点最近一次合规自检的对外视图（来自 Agent 上报 + 控制面判定）。
type NodeComplianceView struct {
	Passed         bool     `json:"passed"`
	CheckedAt      int64    `json:"checked_at"`
	CriticalFailed []string `json:"critical_failed,omitempty"` // 未通过的关键项名（驱动 degraded 的依据）
}

// NodeFsProbeView 是节点最近一次日志/FS 健康探测的对外视图（来自 Agent 上报 + 控制面判定）。
type NodeFsProbeView struct {
	Passed         bool     `json:"passed"`
	CheckedAt      int64    `json:"checked_at"`
	CriticalFailed []string `json:"critical_failed,omitempty"` // 未通过的关键项名（驱动 degraded 的依据）
}

// CapabilityView 是节点能力基线的对外视图（T016/T017/T018 采集结果）。
// Nginx 为 nil 表示非 nginx 节点（如纯 LVS Director）。
type CapabilityView struct {
	NodeID        int                  `json:"node_id"`
	Hostname      string               `json:"hostname,omitempty"`
	OS            string               `json:"os,omitempty"`
	Kernel        string               `json:"kernel,omitempty"`
	HasKeepalived bool                 `json:"has_keepalived"`
	HasIPVS       bool                 `json:"has_ipvsadm"`
	Nginx         *NginxCapabilityView `json:"nginx,omitempty"`
	Checksum      string               `json:"checksum,omitempty"` // 双机一致性 diff 用
	CapturedAt    *time.Time           `json:"captured_at,omitempty"`
	System        *SystemInfoView      `json:"system,omitempty"`      // T018：主机运行底座画像
	ConfigFiles   []*ConfigFileView    `json:"config_files,omitempty"` // T018：配置树（仅元数据）
	LogTargets    []*LogTargetView     `json:"log_targets,omitempty"`  // T018：日志采集目标
}

// SystemInfoView 是主机运行底座画像的对外视图（T018，源自 Agent 上报的 capability.system）。
type SystemInfoView struct {
	OS             string           `json:"os,omitempty"`
	Kernel         string           `json:"kernel,omitempty"`
	NginxManagedBy string           `json:"nginx_managed_by,omitempty"` // systemd | manual
	SELinuxStatus  string           `json:"selinux_status,omitempty"`   // enforcing | permissive | disabled | unknown
	UlimitNofile   int              `json:"ulimit_nofile,omitempty"`
	Timezone       string           `json:"timezone,omitempty"`
	NTPSynced      bool             `json:"ntp_synced,omitempty"`
	LogRotateConf  string           `json:"logrotate_conf,omitempty"`
	DiskFree       map[string]int64 `json:"disk_free,omitempty"`
	Warnings       []string         `json:"warnings,omitempty"`
}

// NginxCapabilityView 是 nginx 侧的能力基线（来自 `nginx -V` 与 `nginx -T`）。
type NginxCapabilityView struct {
	Version    string   `json:"version,omitempty"`
	Prefix     string   `json:"prefix,omitempty"`
	ConfPath   string   `json:"conf_path,omitempty"`
	SbinPath   string   `json:"sbin_path,omitempty"`
	Modules    []string `json:"modules"`
	RawArgs    string   `json:"raw_args,omitempty"`
	ConfigHash string   `json:"config_hash,omitempty"` // nginx -T 全量配置哈希
}

// ConfigFileView 是配置树中单个文件的元数据（不含内容，内容按需向 Agent 请求）。
type ConfigFileView struct {
	Path       string     `json:"path"`
	SHA256     string     `json:"sha256"`
	Size       int64      `json:"size"`
	ModTime    *time.Time `json:"mod_time,omitempty"`
	CapturedAt time.Time  `json:"captured_at"`
}

// LogTargetView 是一个日志采集目标的对外视图。
type LogTargetView struct {
	Path        string    `json:"path"`
	Type        string    `json:"type"`
	Format      string    `json:"format,omitempty"`
	Level       string    `json:"level,omitempty"`
	IsSyslog    bool      `json:"is_syslog"`
	IsOff       bool      `json:"is_off"`
	HasVariable bool      `json:"has_variable"`
	SkipReason  string    `json:"skip_reason,omitempty"`
	Size        int64     `json:"size"`
	Inode       uint64    `json:"inode"`
	StatErr     string    `json:"stat_err,omitempty"`
	CapturedAt  time.Time `json:"captured_at"`
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
	SystemInfo    *SystemInfoView // T018：主机运行底座画像（可为 nil，非 nginx 节点或采集失败）
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

	// 系统信息 JSON 序列化（T018）；nil 时存空串，表示未采集。
	var sysJSON string
	if in.SystemInfo != nil {
		if b, e := json.Marshal(in.SystemInfo); e == nil {
			sysJSON = string(b)
		}
	}

	existing, err := s.client.NodeCapability.Query().
		Where(entnodecap.HasNodeWith(entnode.ID(id))).
		Exist(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "查询已有能力基线失败", err)
	}

	// 主机身份与角色判定依据此前只存在于 CapabilityIn 而从未落库，
	// 导致节点详情页拿不到 hostname/OS/内核，也无法据 has_keepalived 判定 Director 角色。
	now := time.Now()
	if existing {
		_, err = s.client.NodeCapability.Update().
			Where(entnodecap.HasNodeWith(entnode.ID(id))).
			SetHostname(in.Hostname).
			SetOs(in.OS).
			SetKernel(in.Kernel).
			SetHasKeepalived(in.HasKeepalived).
			SetHasIpvsadm(in.HasIPVS).
			SetVersion(in.NginxVersion).
			SetPrefix(in.NginxPrefix).
			SetConfPath(in.NginxConfPath).
			SetSbinPath(in.NginxSbinPath).
			SetModules(modules).
			SetRawArgs(in.NginxRawArgs).
			SetConfigHash(in.ConfigHash).
			SetChecksum(checksum).
			SetSystemInfo(sysJSON).
			SetCapturedAt(now).
			Save(ctx)
	} else {
		_, err = s.client.NodeCapability.Create().
			SetNodeID(id).
			SetHostname(in.Hostname).
			SetOs(in.OS).
			SetKernel(in.Kernel).
			SetHasKeepalived(in.HasKeepalived).
			SetHasIpvsadm(in.HasIPVS).
			SetVersion(in.NginxVersion).
			SetPrefix(in.NginxPrefix).
			SetConfPath(in.NginxConfPath).
			SetSbinPath(in.NginxSbinPath).
			SetModules(modules).
			SetRawArgs(in.NginxRawArgs).
			SetConfigHash(in.ConfigHash).
			SetChecksum(checksum).
			SetSystemInfo(sysJSON).
			SetCapturedAt(now).
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

// ---- T019/T018：健康维度聚合判定（合规 + 日志/FS）----

// SetCompliance 处理 Agent 上报的 DR 合规自检报告：缓存最新报告，并聚合两个健康维度
// （合规 + 日志/FS）重新计算节点 degraded/online 态（见 recomputeHealth）。
// report 为 nil 时直接忽略（不驱动任何流转）。
func (s *Service) SetCompliance(ctx context.Context, id int, report *agentv1.ComplianceReport) error {
	if report == nil {
		return nil
	}
	s.compMu.Lock()
	s.compReports[id] = report
	s.compMu.Unlock()
	return s.recomputeHealth(ctx, id)
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

// SetFsProbe 处理 Agent 上报的日志/FS 健康探测报告（T018）：缓存最新报告，并聚合两个健康维度
// （合规 + 日志/FS）重新计算节点 degraded/online 态（见 recomputeHealth）。
// report 为 nil 时直接忽略（不驱动任何流转）。
func (s *Service) SetFsProbe(ctx context.Context, id int, report *agentv1.FsProbeReport) error {
	if report == nil {
		return nil
	}
	s.fsMu.Lock()
	s.fsReports[id] = report
	s.fsMu.Unlock()
	return s.recomputeHealth(ctx, id)
}

// GetFsProbe 返回节点最近一次日志/FS 健康探测报告（内存态；无则 nil）。
func (s *Service) GetFsProbe(ctx context.Context, id int) (*agentv1.FsProbeReport, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	s.fsMu.RLock()
	defer s.fsMu.RUnlock()
	if r, ok := s.fsReports[id]; ok {
		return r, nil
	}
	return nil, nil
}

// recomputeHealth 聚合合规与日志/FS 两个健康维度，重新计算节点 degraded/online 态。
//
// 规则：
//   - 任一维度存在未通过的 critical 项 → 节点 degraded（两个维度的失败都会把 online 拉到 degraded）。
//   - 两个维度都无未通过 critical 项（其余维度未上报视为不阻断）→ 节点 online。
//
// 仅对 online/degraded 做翻转，不触碰 enrolling/offline；仅在状态实际变化时才写库 + 打 WARN，
// 避免重复 UPDATE 与日志刷屏。某维度从未上报（nil）按"不阻断"处理（未知 ≠ 失败），
// 因此单维度先行上报即可驱动对应翻转，且另一维度恢复不会误把仍存在问题的维度翻转回 online。
func (s *Service) recomputeHealth(ctx context.Context, id int) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	s.compMu.RLock()
	comp := s.compReports[id]
	s.compMu.RUnlock()
	s.fsMu.RLock()
	fs := s.fsReports[id]
	s.fsMu.RUnlock()

	compFails := comp != nil && !compliance.Evaluate(comp).Passed
	fsFails := fs != nil && !probe.Evaluate(fs.GetItems()).Passed
	degraded := compFails || fsFails

	cur, err := s.client.Node.Query().Where(entnode.ID(id)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return apperr.New(apperr.CodeNotFound, "节点不存在")
		}
		return apperr.Wrap(apperr.CodeInternal, "查询节点失败", err)
	}
	switch cur.Status {
	case entnode.StatusOnline:
		if degraded && cur.Status != entnode.StatusDegraded {
			if _, err := s.client.Node.UpdateOneID(id).SetStatus(entnode.StatusDegraded).Save(ctx); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "标记 degraded 失败", err)
			}
			slog.Default().Warn("node health degraded by probe/compliance", "node_id", id,
				"compliance_failed", compFails, "fs_failed", fsFails)
		}
	case entnode.StatusDegraded:
		if !degraded && cur.Status != entnode.StatusOnline {
			if _, err := s.client.Node.UpdateOneID(id).SetStatus(entnode.StatusOnline).Save(ctx); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "恢复 online 失败", err)
			}
		}
	}
	return nil
}

// ---- T018-C：配置树 / 日志目标持久化与 API 暴露 ----

// SaveConfigTree 用 Agent 上报的 nginx -T 配置树**整体替换**该节点的配置文件快照（快照语义）。
// 仅持久化元数据（路径/大小/哈希），不存内容（见 ent NodeConfigFile 设计说明）。
// 若注入了 cfgStore（T021 版本化存储），则同时把带内容的配置树同步进内容寻址版本链。
func (s *Service) SaveConfigTree(ctx context.Context, id int, files []*agentv1.ConfigFile) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "开启事务失败", err)
	}
	// 先清空旧快照再写入新快照：保证配置被删除后视图同步消失，避免孤儿记录。
	if _, err := tx.NodeConfigFile.Delete().Where(entncf.HasNodeWith(entnode.ID(id))).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return apperr.Wrap(apperr.CodeInternal, "清理旧配置树失败", err)
	}
	now := time.Now()
	for _, f := range files {
		if f == nil {
			continue
		}
		if err := tx.NodeConfigFile.Create().
			SetNodeID(id).
			SetPath(f.GetPath()).
			SetSha256(f.GetSha256()).
			SetSize(f.GetSize()).
			SetCapturedAt(now).
			Exec(ctx); err != nil {
			_ = tx.Rollback()
			return apperr.Wrap(apperr.CodeInternal, "写入配置树失败", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "提交配置树事务失败", err)
	}
	// T021：同步进版本化内容寻址存储（带内容）。cfgStore 为可空依赖，未注入时跳过。
	if s.cfgStore != nil {
		if _, err := s.cfgStore.SyncFromAgent(ctx, id, files); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "同步配置版本化存储失败", err)
		}
	}
	// T026：配置树上报即触发漂移检测（契合"在心跳/上报时检测，不每次跑 nginx -T"）。
	// 以 Agent 上报的实际内容为 actual，与平台期望版本做 SHA 级比对。
	// 检测失败降级为告警而非阻断同步（配置树已落库，漂移下次巡检仍可捕获）。
	if s.drift != nil {
		reported := make([]config.ReportedConfigFile, 0, len(files))
		for _, f := range files {
			if f == nil {
				continue
			}
			reported = append(reported, config.ReportedConfigFile{
				Path:    f.GetPath(),
				SHA:     f.GetSha256(),
				Content: f.GetContent(),
			})
		}
		if _, derr := s.drift.RecordActual(ctx, id, reported); derr != nil {
			slog.Default().Warn("drift detection skipped after config sync", "node_id", id, "err", derr)
		}
	}
	return nil
}

// GetConfigFiles 返回节点最近一次配置树的文件元数据列表（不含内容）。
func (s *Service) GetConfigFiles(ctx context.Context, id int) ([]*ConfigFileView, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.client.NodeConfigFile.Query().
		Where(entncf.HasNodeWith(entnode.ID(id))).
		Order(ent.Asc("path")).
		All(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "查询配置树失败", err)
	}
	out := make([]*ConfigFileView, 0, len(rows))
	for _, r := range rows {
		v := &ConfigFileView{
			Path:       r.Path,
			SHA256:     r.Sha256,
			Size:       r.Size,
			CapturedAt: r.CapturedAt,
		}
		if !r.ModTime.IsZero() {
			t := r.ModTime
			v.ModTime = &t
		}
		out = append(out, v)
	}
	return out, nil
}

// SaveLogTargets 用 Agent 上报的日志采集目标**整体替换**该节点的日志目标快照。
func (s *Service) SaveLogTargets(ctx context.Context, id int, items []*agentv1.LogTarget) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "开启事务失败", err)
	}
	if _, err := tx.NodeLogTarget.Delete().Where(entnlt.HasNodeWith(entnode.ID(id))).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return apperr.Wrap(apperr.CodeInternal, "清理旧日志目标失败", err)
	}
	now := time.Now()
	for _, t := range items {
		if t == nil {
			continue
		}
		if err := tx.NodeLogTarget.Create().
			SetNodeID(id).
			SetPath(t.GetPath()).
			SetType(entnlt.Type(t.GetType())).
			SetFormat(t.GetFormat()).
			SetLevel(t.GetLevel()).
			SetIsSyslog(t.GetIsSyslog()).
			SetIsOff(t.GetIsOff()).
			SetHasVariable(t.GetHasVariable()).
			SetSkipReason(t.GetSkipReason()).
			SetSize(t.GetSize()).
			SetInode(t.GetInode()).
			SetStatErr(t.GetStatErr()).
			SetCapturedAt(now).
			Exec(ctx); err != nil {
			_ = tx.Rollback()
			return apperr.Wrap(apperr.CodeInternal, "写入日志目标失败", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "提交日志目标事务失败", err)
	}
	return nil
}

// GetLogTargets 返回节点最近一次日志采集目标列表。
func (s *Service) GetLogTargets(ctx context.Context, id int) ([]*LogTargetView, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.client.NodeLogTarget.Query().
		Where(entnlt.HasNodeWith(entnode.ID(id))).
		Order(ent.Asc("type"), ent.Asc("path")).
		All(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "查询日志目标失败", err)
	}
	out := make([]*LogTargetView, 0, len(rows))
	for _, r := range rows {
		out = append(out, &LogTargetView{
			Path:        r.Path,
			Type:        r.Type.String(),
			Format:      r.Format,
			Level:       r.Level,
			IsSyslog:    r.IsSyslog,
			IsOff:       r.IsOff,
			HasVariable: r.HasVariable,
			SkipReason:  r.SkipReason,
			Size:        r.Size,
			Inode:       r.Inode,
			StatErr:     r.StatErr,
			CapturedAt:  r.CapturedAt,
		})
	}
	return out, nil
}

// GetCapability 返回节点能力基线真实视图（T016/T017/T018 采集结果已落库）：
// nginx 编译画像、主机系统信息、配置树与日志采集目标快照。
func (s *Service) GetCapability(ctx context.Context, id int) (*CapabilityView, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	v := &CapabilityView{NodeID: id}
	cap, err := s.client.NodeCapability.Query().
		Where(entnodecap.HasNodeWith(entnode.ID(id))).
		Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return nil, apperr.Wrap(apperr.CodeInternal, "查询能力基线失败", err)
	}
	if cap != nil {
		v.Hostname = cap.Hostname
		v.OS = cap.Os
		v.Kernel = cap.Kernel
		v.HasKeepalived = cap.HasKeepalived
		v.HasIPVS = cap.HasIpvsadm
		v.Checksum = cap.Checksum
		if !cap.CapturedAt.IsZero() {
			t := cap.CapturedAt
			v.CapturedAt = &t
		}
		if cap.Version != "" {
			v.Nginx = &NginxCapabilityView{
				Version:    cap.Version,
				Prefix:     cap.Prefix,
				ConfPath:   cap.ConfPath,
				SbinPath:   cap.SbinPath,
				Modules:    cap.Modules,
				RawArgs:    cap.RawArgs,
				ConfigHash: cap.ConfigHash,
			}
		}
		if cap.SystemInfo != "" {
			var si SystemInfoView
			if json.Unmarshal([]byte(cap.SystemInfo), &si) == nil {
				v.System = &si
			}
		}
	}
	if v.ConfigFiles, err = s.GetConfigFiles(ctx, id); err != nil {
		return nil, err
	}
	if v.LogTargets, err = s.GetLogTargets(ctx, id); err != nil {
		return nil, err
	}
	return v, nil
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
