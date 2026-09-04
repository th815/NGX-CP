// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package deploy 发布事件模型（T037 SSE 推送的数据单元）。
// 域层只负责「产生事件」，传输与订阅由控制面 Hub 实现（见 internal/server/hub.go）。
package deploy

// DeployEvent 是发布过程中的实时事件，经 SSE 推送给前端。
// 控制面在生命周期迁移（提交/批准/拒绝/取消/开始）与单节点执行步骤
// （transfer/validate/...，待 T039 执行器接线后补齐）时发出。
type DeployEvent struct {
	ID        int64  `json:"id"`       // 全局单调事件号，供 SSE Last-Event-ID 重放
	OrderID   int    `json:"order_id"` // 所属变更单
	NodeID    int    `json:"node_id,omitempty"`
	NodeName  string `json:"node_name,omitempty"`
	Step      string `json:"step,omitempty"`     // "submit" | "approve" | "transfer" | "validate" | ...
	Status    string `json:"status,omitempty"`   // "running" | "success" | "failed" | 生命周期态
	Progress  int    `json:"progress,omitempty"` // 0-100
	Message   string `json:"message,omitempty"`
	Timestamp int64  `json:"timestamp"` // unix 毫秒
}

// EventSink 是事件出口（由控制面 Hub 注入实现；单测可注入 fake）。
// 域层只产生事件、不关心传输细节——符合 T030「依赖经 Set* 注入」的约定。
type EventSink interface {
	Emit(evt DeployEvent)
}
