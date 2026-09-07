// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package dns 抽象 ACME DNS-01 校验所需的 TXT 记录操作（M4 T041）。
//
// 本项目只走 DNS-01（Cloudflare 前置会拦截 HTTP-01），故 DNS Provider 的唯一职责是
// 在 _acme-challenge.<domain> 上增删 TXT 记录。v1 实现 Cloudflare，预留 Aliyun。
// 接口刻意精简：调用方（ACME 客户端）只关心「写一条挑战值 / 清掉它 / 校验凭据」。
package dns

import "context"

// DNSProvider 是 DNS-01 挑战所需的 TXT 记录操作抽象。
//
// 约定：
//   - name 为完整挑战域名 _acme-challenge.<domain>，不是根域名；
//   - value 为 ACME 服务端要求的挑战值；
//   - SetRecord 必须幂等：重复写入同一 (zone,name,value) 不应产生重复记录；
//   - DeleteRecord 清理挑战完成后的残留 TXT，避免被误当作永久记录。
type DNSProvider interface {
	// Name 返回 provider 标识（如 "cloudflare"），用于注册表与配置选择。
	Name() string
	// SetRecord 写入/更新一条 TXT 记录（name -> value）。
	SetRecord(ctx context.Context, zone, name, value string) error
	// DeleteRecord 删除匹配 name 的 TXT 记录。
	DeleteRecord(ctx context.Context, zone, name string) error
	// Validate 校验 API 凭据有效性（最小权限是否满足），不满足返回明确错误。
	Validate(ctx context.Context) error
}
