// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package backup 定义发布前快照的领域模型与保留策略（T031）。
//
// 快照是"一次变更可回滚"的根基：变更前在节点上抓取配置（及可选证书）的 tar.gz，
// 并记录每个文件的权限与属主，恢复时才能原样还原——只存内容会导致 nginx 读不了配置。
package backup

import (
	"sort"
	"time"
)

// SnapshotFile 记录单个被快照文件的元数据（恢复时用于还原权限/属主）。
// Path 为节点上的绝对路径；Mode 是 os.FileMode 的底层 bits（含类型位）。
// Owner 优先存 "name:name"（如 nginx:nginx），解析失败回退 "uid:gid"。
type SnapshotFile struct {
	Path   string
	SHA256 string
	Size   int64
	Mode   int64
	Owner  string
}

// ConfigSnapshot 一次发布前快照的元信息（与 Agent 侧 / 控制面契约对齐）。
type ConfigSnapshot struct {
	ID            int
	NodeID        int
	ChangeOrderID *int
	Type          string // pre_deploy | manual | scheduled
	Path          string // 生成的 tar.gz 绝对路径，如 /var/lib/ngxcp/snapshots/<node>/<ts>.tar.gz
	Files         []SnapshotFile
	Size          int64
	CreatedAt     time.Time
}

// SnapshotPolicy 快照保留策略（来自配置 snapshot: 块）。
type SnapshotPolicy struct {
	KeepDays       int  // 保留天数，默认 90
	KeepMaxPerNode int  // 每节点最多保留份数，默认 200
	IncludeSSL     bool // 是否包含 /etc/nginx/ssl，默认 false（证书有独立生命周期）
}

// DefaultSnapshotPolicy 返回默认保留策略。
func DefaultSnapshotPolicy() SnapshotPolicy {
	return SnapshotPolicy{KeepDays: 90, KeepMaxPerNode: 200, IncludeSSL: false}
}

// PathsForRole 依据节点角色返回要快照的目录集合：
//   - 所有角色都快照 /etc/nginx；
//   - director / director_and_rs 额外快照 /etc/keepalived；
//   - includeSSL 为真时额外快照 /etc/nginx/ssl。
func PathsForRole(role string, includeSSL bool) []string {
	paths := []string{"/etc/nginx"}
	switch role {
	case "director", "director_and_rs":
		paths = append(paths, "/etc/keepalived")
	}
	if includeSSL {
		paths = append(paths, "/etc/nginx/ssl")
	}
	return paths
}

// FilterExpired 返回已过保留期的快照（CreatedAt 早于 now-KeepDays）。
// KeepDays<=0 表示永不过期，返回 nil。
func (p SnapshotPolicy) FilterExpired(snaps []ConfigSnapshot, now time.Time) []ConfigSnapshot {
	if p.KeepDays <= 0 {
		return nil
	}
	cutoff := now.AddDate(0, 0, -p.KeepDays)
	var expired []ConfigSnapshot
	for i := range snaps {
		if snaps[i].CreatedAt.Before(cutoff) {
			expired = append(expired, snaps[i])
		}
	}
	return expired
}

// ExcessPerNode 返回超出 KeepMaxPerNode 的旧快照（按 CreatedAt 升序保留最新 N 份）。
// KeepMaxPerNode<=0 或未超限返回 nil。
func (p SnapshotPolicy) ExcessPerNode(snaps []ConfigSnapshot) []ConfigSnapshot {
	if p.KeepMaxPerNode <= 0 || len(snaps) <= p.KeepMaxPerNode {
		return nil
	}
	sorted := append([]ConfigSnapshot{}, snaps...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
	})
	excess := len(sorted) - p.KeepMaxPerNode
	return sorted[:excess]
}
