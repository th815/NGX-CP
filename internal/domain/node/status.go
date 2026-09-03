// Package node 实现节点域的核心逻辑：CRUD、能力基线占位、一次性接入令牌。
// M1 阶段 Agent 尚未常驻，能力上报（T016/T017）与心跳（T015）后续里程碑填充。
package node

import (
	entnode "github.com/th/ngxcp/ent/node"
)

// 节点状态对齐 ent node.status 枚举值（online/offline/degraded/enrolling/decommissioned）。
const (
	StatusEnrolling      = string(entnode.StatusEnrolling)
	StatusOnline         = string(entnode.StatusOnline)
	StatusOffline        = string(entnode.StatusOffline)
	StatusDegraded       = string(entnode.StatusDegraded)
	StatusDecommissioned = string(entnode.StatusDecommissioned)
)

// fsm 是节点状态机（见 docs/tasks/M1-skeleton.md T015）：
//
//	enrolling --(能力上报成功 / 首跳心跳)--> online --(超时无心跳)--> offline
//	   |                                      |
//	   +--(注册后超时未上报)--> failed         +--(合规不通过)--> degraded
//
// 注：failed / degraded 为后续里程碑（T018/T019）落地的目标态，这里只声明合法转移，
// 实际跳转由各里程碑的处理函数触发。
var fsm = map[string][]string{
	StatusEnrolling:      {StatusOnline, StatusOffline}, // 能力上报/首跳→online；超时未上报→offline
	StatusOnline:         {StatusOffline, StatusDegraded},
	StatusOffline:        {StatusOnline, StatusEnrolling}, // 重连心跳→online；重新注册→enrolling
	StatusDegraded:       {StatusOnline, StatusOffline},
	StatusDecommissioned: {}, // 终态
}

// CanTransition 校验 from -> to 是否为合法状态转移。
func CanTransition(from, to string) bool {
	for _, t := range fsm[from] {
		if t == to {
			return true
		}
	}
	return false
}

// IsValidStatus 校验状态字符串是否合法（供入参校验复用）。
func IsValidStatus(s string) bool {
	switch entnode.Status(s) {
	case entnode.StatusOnline, entnode.StatusOffline,
		entnode.StatusDegraded, entnode.StatusEnrolling, entnode.StatusDecommissioned:
		return true
	default:
		return false
	}
}
