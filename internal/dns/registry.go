// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package dns 的 Provider 注册表（M4 T041）。
//
// 控制面按配置里的 type 选择 provider；各实现包通过 init() 自注册。
// 新增 Aliyun 等只需实现 DNSProvider 并在其包内 Register，无需改此处。
package dns

import "fmt"

// Config 是构造一个 provider 所需的配置（来自控制面配置/密钥库）。
type Config struct {
	Type  string // "cloudflare" 等，对应注册名
	Token string // API Token（经 KMS 解密后注入）
}

// Constructor 根据 Config 构造一个 DNSProvider。
type Constructor func(cfg Config) (DNSProvider, error)

var registry = map[string]Constructor{}

// Register 注册一个 provider 构造器（通常由实现包的 init() 调用）。
func Register(name string, c Constructor) {
	registry[name] = c
}

// New 按 type 构造 provider。
func New(cfg Config) (DNSProvider, error) {
	c, ok := registry[cfg.Type]
	if !ok {
		return nil, fmt.Errorf("未知 DNS provider 类型：%s（已注册：%v）", cfg.Type, Registered())
	}
	return c(cfg)
}

// Registered 返回所有已注册 provider 名称。
func Registered() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	return names
}

// init 自注册 Cloudflare。
func init() {
	Register("cloudflare", func(cfg Config) (DNSProvider, error) {
		if cfg.Token == "" {
			return nil, fmt.Errorf("cloudflare provider 需要 API Token")
		}
		return NewCloudflare(cfg.Token), nil
	})
}
