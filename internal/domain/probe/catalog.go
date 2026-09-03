// Package probe 实现控制面侧的日志/文件系统（FS）健康探测目录与判定（T018）。
//
// 实际探测由 Agent 在主机上执行（df / 证书有效期 / 文件权限 / 日志目录可写性 /
// error.log 错误速率 / pid 文件存在性等），控制面只持有「规则目录（单一事实来源）+
// 聚合判定」，并据此驱动节点状态机：online --(关键项不通过)--> degraded。
//
// 设计镜像 internal/domain/compliance（DR 合规自检）：两者都是「Agent 上报结构化检查项，
// 控制面判定 critical 失败即降级」。节点是否 degraded 由两维度聚合决定（见 node.Service.recomputeHealth）。
package probe

import agentv1 "github.com/th/ngxcp/gen/agent/v1"

// 严重级别（与 compliance 保持一致语义）。关键（critical）项不通过即判定整体不健康 → 节点降级。
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
)

// 规则分类（便于前端分组展示与按类聚合）。
const (
	CatDisk     = "disk"     // 磁盘使用率
	CatCert     = "cert"     // 证书有效期
	CatSecurity = "security" // 文件权限 / 越权
	CatLog      = "log"      // 日志可写性 / 错误速率 / pid 态
)

// RuleDef 是一条 FS/日志健康规则的定义（控制面侧目录，不随节点变化）。
type RuleDef struct {
	Name     string
	Title    string
	Severity string // SeverityCritical / SeverityWarning
	Category string
	Expected string // 期望状态（人类可读）
	FixCmd   string // 修复建议（运维参考）
}

// Catalog 是日志/FS 健康探测的完整规则目录（单一事实来源）。
//
// 覆盖：磁盘使用率（nginx 路径 < 90%）、证书剩余有效期（> 14 天，与 PKI 续签联动）、
// 配置/证书文件权限（非全局可写，防越权篡改，对应 Agent 路径白名单 /etc/nginx、/etc/nginx/ssl）、
// 日志目录可写、error.log 错误速率未雪崩、pid 文件存在。
var Catalog = []RuleDef{
	{
		Name:     "disk_usage_nginx_paths",
		Title:    "nginx 路径所在挂载点使用率 < 90%",
		Severity: SeverityCritical,
		Category: CatDisk,
		Expected: "df 对 prefix/conf/log 挂载点使用率 < 90%",
		FixCmd:   "清理日志/缓存或扩容磁盘",
	},
	{
		Name:     "cert_expiry",
		Title:    "证书剩余有效期 > 14 天",
		Severity: SeverityCritical,
		Category: CatCert,
		Expected: "ssl 证书 NotAfter - now > 14d",
		FixCmd:   "触发证书续签（ACME 自动或手动上传）",
	},
	{
		Name:     "config_world_writable",
		Title:    "/etc/nginx 及 ssl 文件权限非全局可写",
		Severity: SeverityCritical,
		Category: CatSecurity,
		Expected: "配置与私钥文件权限为 0644/0600，无全局可写",
		FixCmd:   "chmod 收紧 /etc/nginx 与 /etc/nginx/ssl 权限",
	},
	{
		Name:     "log_dir_writable",
		Title:    "日志目录可写",
		Severity: SeverityWarning,
		Category: CatLog,
		Expected: "nginx 日志目录可写（否则启动失败或丢日志）",
		FixCmd:   "修正日志目录属主/权限",
	},
	{
		Name:     "error_log_growth",
		Title:    "error.log 近窗口错误速率未雪崩",
		Severity: SeverityWarning,
		Category: CatLog,
		Expected: "错误行速率 < 阈值（无 upstream/5xx 雪崩）",
		FixCmd:   "排查 upstream / 后端异常",
	},
	{
		Name:     "pid_file_present",
		Title:    "nginx pid 文件存在且可读",
		Severity: SeverityWarning,
		Category: CatLog,
		Expected: "/var/run/nginx.pid 或配置 pid 路径存在",
		FixCmd:   "确认 nginx 运行态正常",
	},
}

// CatalogByName 便于按规则名快速索引。
func CatalogByName() map[string]RuleDef {
	m := make(map[string]RuleDef, len(Catalog))
	for _, r := range Catalog {
		m[r.Name] = r
	}
	return m
}

// FindItem 在 Agent 上报的报告里按 name 找对应项（找不到返回 nil）。
func FindItem(report *agentv1.FsProbeReport, name string) *agentv1.ComplianceItem {
	if report == nil {
		return nil
	}
	for _, it := range report.GetItems() {
		if it.GetName() == name {
			return it
		}
	}
	return nil
}
