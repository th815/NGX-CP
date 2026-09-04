// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package deploy 实现发布引擎的领域模型：变更单状态机与持久化转换（T030）。
package deploy

import (
	"context"
	"fmt"
	"time"

	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/ent/changeorder"
	"github.com/th/ngxcp/ent/schema"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// updateFunc 是 Transition 的可选副作用修饰符（在状态更新之外附带写字段）。
type updateFunc func(*ent.ChangeOrderUpdate) *ent.ChangeOrderUpdate

// Service 封装变更单的持久化与状态机转换。
// 所有状态迁移都走数据库乐观锁（见 Transition），控制面重启后可从库里恢复。
//
// 回滚与告警依赖为可选项，经 Set* 注入（不破坏既有 New(client) 调用方）：
//   - rbClient：触发某节点执行回滚（控制面经 Agent 接线，单测用 fake）
//   - alert：   告警出口，rollback_failed 必发 CRITICAL
//   - eventSink：实时事件出口（T037 SSE Hub），nil 时静默丢弃
type Service struct {
	client      *ent.Client
	rbClient    NodeRollbackClient
	alert       AlertSink
	approvalCfg *ApprovalConfig // 审批配置；nil 时用 DefaultApprovalConfig()
	eventSink   EventSink       // 事件出口；nil 时静默丢弃（单测/未接线）
}

// New 构造发布服务。
func New(client *ent.Client) *Service {
	return &Service{client: client}
}

// SetEventSink 注入实时事件出口（T037 SSE）。nil 合法，表示暂不推送。
func (s *Service) SetEventSink(es EventSink) { s.eventSink = es }

// emit 发布一条发布事件；未注入 eventSink 时静默丢弃。
// 调用方只需填业务字段，ID/时间戳由 Hub 补齐。
func (s *Service) emit(evt DeployEvent) {
	if s.eventSink == nil {
		return
	}
	if evt.Timestamp == 0 {
		evt.Timestamp = time.Now().UnixMilli()
	}
	s.eventSink.Emit(evt)
}

// Transition 执行一次状态转换，使用数据库乐观锁：
//
//	UPDATE change_orders SET status=? WHERE id=? AND status=?
//
// 影响行数 0 → 并发冲突（另一协程已改写）或起始状态不匹配，返回 CodeConflict。
// 非法迁移（不在 transitions 表中）直接返回 CodeInvalid，不落库。
func (s *Service) Transition(ctx context.Context, orderID int, from, to string, mods ...updateFunc) error {
	fromS, toS := OrderStatus(from), OrderStatus(to)
	if !CanTransition(fromS, toS) {
		return apperr.New(apperr.CodeInvalid, fmt.Sprintf("非法的状态转换：%s → %s", from, to))
	}
	upd := s.client.ChangeOrder.Update().
		Where(changeorder.ID(orderID), changeorder.StatusEQ(changeorder.Status(fromS))).
		SetStatus(changeorder.Status(toS))
	for _, m := range mods {
		upd = m(upd)
	}
	n, err := upd.Save(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "状态转换写库失败", err)
	}
	if n == 0 {
		return apperr.New(apperr.CodeConflict, "状态冲突：该变更单已被其他操作修改，或起始状态不匹配")
	}
	return nil
}

// CreateInput 创建变更单（draft）的输入。
type CreateInput struct {
	Title             string
	Type              string
	Source            string
	TargetNodes       []int
	ConfigRevisionIDs []int
	Strategy          schema.DeployStrategy
	CreatedBy         string
	Comment           string
}

// CreateDraft 插入一条 draft 状态的变更单。type 非法会在落库时被 ent 枚举校验拦截。
func (s *Service) CreateDraft(ctx context.Context, in CreateInput) (*ent.ChangeOrder, error) {
	if in.Title == "" {
		return nil, apperr.New(apperr.CodeInvalid, "变更单标题不能为空")
	}
	src := in.Source
	if src == "" {
		src = string(changeorder.DefaultSource) // manual
	}
	b := s.client.ChangeOrder.Create().
		SetTitle(in.Title).
		SetType(changeorder.Type(in.Type)).
		SetSource(changeorder.Source(src)).
		SetTargetNodes(in.TargetNodes).
		SetConfigRevisionIds(in.ConfigRevisionIDs).
		SetStrategy(in.Strategy).
		SetCreatedBy(in.CreatedBy)
	if in.Comment != "" {
		b = b.SetComment(in.Comment)
	}
	co, err := b.Save(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalid, "创建变更单失败", err)
	}
	return co, nil
}

// Get 取单条变更单（不存在返回 CodeNotFound）。
func (s *Service) Get(ctx context.Context, id int) (*ent.ChangeOrder, error) {
	co, err := s.client.ChangeOrder.Get(ctx, id)
	if ent.IsNotFound(err) {
		return nil, apperr.New(apperr.CodeNotFound, "变更单不存在")
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "读取变更单失败", err)
	}
	return co, nil
}

// List 列出变更单（status 为空表示全部），按创建时间倒序。
func (s *Service) List(ctx context.Context, statusFilter string) ([]*ent.ChangeOrder, error) {
	q := s.client.ChangeOrder.Query().Order(ent.Desc(changeorder.FieldCreatedAt))
	if statusFilter != "" {
		q = q.Where(changeorder.StatusEQ(changeorder.Status(OrderStatus(statusFilter))))
	}
	items, err := q.All(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "列出变更单失败", err)
	}
	return items, nil
}

// ListByStatus 返回处于指定状态的所有变更单（用于控制面重启后恢复 running 任务）。
func (s *Service) ListByStatus(ctx context.Context, status string) ([]*ent.ChangeOrder, error) {
	items, err := s.client.ChangeOrder.Query().
		Where(changeorder.StatusEQ(changeorder.Status(OrderStatus(status)))).
		All(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "按状态查询变更单失败", err)
	}
	return items, nil
}

// —— 语义化状态转换（供 handler / 后续 worker 调用）——

// Submit draft → 按审批规则决定进入 pending_approval 或直达 pending。
// 命中规则（或显式声明需审批）：落一条待审 Approval 记录后转入 pending_approval。
// 免审批：直接 draft → pending，不创建审批记录。
func (s *Service) Submit(ctx context.Context, orderID int) error {
	co, err := s.Get(ctx, orderID)
	if err != nil {
		return err
	}
	need, rule := s.cfg().RequiresApproval(co)
	if !need {
		if err := s.Transition(ctx, orderID, string(StatusDraft), string(StatusPending)); err != nil {
			return err
		}
		s.emit(DeployEvent{OrderID: orderID, Step: "submit", Status: string(StatusPending), Message: "已提交（免审批）"})
		return nil
	}
	if err := s.createApproval(ctx, orderID, rule); err != nil {
		return err
	}
	if err := s.Transition(ctx, orderID, string(StatusDraft), string(StatusPendingApproval)); err != nil {
		return err
	}
	s.emit(DeployEvent{OrderID: orderID, Step: "submit", Status: string(StatusPendingApproval), Message: "已提交，等待审批：" + rule})
	return nil
}

// Approve pending_approval → pending，做自审批拦截并记录审批人。
// 当 allow_self_approval=false 且审批人与提交人相同时拒绝（审计合规硬约束）。
func (s *Service) Approve(ctx context.Context, orderID int, approver string) error {
	co, err := s.Get(ctx, orderID)
	if err != nil {
		return err
	}
	if !s.cfg().AllowSelfApproval && approver != "" && co.CreatedBy != "" && approver == co.CreatedBy {
		return apperr.New(apperr.CodeInvalid, "不允许审批人审批自己提交的变更单")
	}
	if err := s.markApproval(ctx, orderID, "approved", approver, ""); err != nil {
		return err
	}
	if err := s.Transition(ctx, orderID, string(StatusPendingApproval), string(StatusPending),
		func(u *ent.ChangeOrderUpdate) *ent.ChangeOrderUpdate { return u.SetApprovedBy(approver) }); err != nil {
		return err
	}
	s.emit(DeployEvent{OrderID: orderID, Step: "approve", Status: string(StatusPending), Message: "已批准（" + approver + "）"})
	return nil
}

// Reject pending_approval → rejected，并记录拒绝动作。
func (s *Service) Reject(ctx context.Context, orderID int) error {
	if err := s.markApproval(ctx, orderID, "rejected", "", ""); err != nil {
		return err
	}
	if err := s.Transition(ctx, orderID, string(StatusPendingApproval), string(StatusRejected)); err != nil {
		return err
	}
	s.emit(DeployEvent{OrderID: orderID, Step: "reject", Status: string(StatusRejected), Message: "已拒绝"})
	return nil
}

// Cancel 从任何可取消的状态 → canceled（乐观锁：WHERE status IN (可取消集合)）。
func (s *Service) Cancel(ctx context.Context, orderID int) error {
	cancelable := []changeorder.Status{
		changeorder.StatusDraft,
		changeorder.StatusPendingApproval,
		changeorder.StatusPending,
		changeorder.StatusRunning,
		changeorder.StatusFailed,
		changeorder.StatusPartialSuccess,
		changeorder.StatusRollbackFailed,
	}
	n, err := s.client.ChangeOrder.Update().
		Where(changeorder.ID(orderID), changeorder.StatusIn(cancelable...)).
		SetStatus(changeorder.StatusCanceled).
		Save(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "取消变更单写库失败", err)
	}
	if n == 0 {
		return apperr.New(apperr.CodeConflict, "该变更单当前状态不可取消，或已被其他操作修改")
	}
	s.emit(DeployEvent{OrderID: orderID, Step: "cancel", Status: string(StatusCanceled), Message: "已取消"})
	return nil
}

// Start pending → running，并记录 started_at（执行流水线入口，后续任务实现）。
func (s *Service) Start(ctx context.Context, orderID int) error {
	now := time.Now()
	if err := s.Transition(ctx, orderID, string(StatusPending), string(StatusRunning),
		func(u *ent.ChangeOrderUpdate) *ent.ChangeOrderUpdate { return u.SetStartedAt(now) }); err != nil {
		return err
	}
	s.emit(DeployEvent{OrderID: orderID, Step: "start", Status: string(StatusRunning), Message: "开始执行"})
	return nil
}
