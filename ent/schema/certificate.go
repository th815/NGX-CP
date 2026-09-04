// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package schema 定义证书子系统的 ent 实体（M4）。
package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// Certificate 一张证书的元数据 + 信封加密的私钥/链。
//
// 安全红线（DECISIONS §5 / AGENTS §9.2）：
//   - 私钥（enc_private_key）与全链（enc_full_chain）以 AES-GCM 信封加密存储，
//     绝不进 config_blob 版本历史；
//   - API 只返回本实体元数据字段，永不回传私钥明文。
type Certificate struct {
	ent.Schema
}

// Fields 证书元数据。私钥/链为加密 blob，单独存储。
func (Certificate) Fields() []ent.Field {
	return []ent.Field{
		field.String("domain").NotEmpty().
			Comment("主域名，如 example.com"),
		field.JSON("san", []string{}).
			Optional().
			SchemaType(map[string]string{"postgres": "jsonb", "sqlite": "json"}).
			Comment("所有 subjectAltName"),
		field.String("issuer").NotEmpty().
			Comment("Let's Encrypt / Upload / 内部 CA"),
		field.String("serial_number").NotEmpty().Unique().
			Comment("证书序列号（十六进制），全局唯一"),
		field.String("fingerprint_sha").NotEmpty().
			Comment("SHA-256 指纹（十六进制）"),
		field.Time("not_before").
			Comment("生效时间（UTC）"),
		field.Time("not_after").
			Comment("到期时间（UTC）；<7天标红，<30天标黄"),
		field.String("key_alg").NotEmpty().
			Comment("RSA-2048 / ECDSA-P256"),
		field.Enum("source").
			Values("acme", "upload").
			Default("upload").
			Comment("acme=自动签发，upload=手动上传"),
		field.Enum("status").
			Values("valid", "expired", "revoked", "error").
			Default("valid").
			Comment("证书生命周期状态"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		// 敏感材料：信封加密（AES-GCM，见 internal/crypto/kms.go）。
		field.Bytes("enc_private_key").Optional().
			Comment("AES-GCM(envelope) 加密的私钥，绝不回传浏览器"),
		field.Bytes("enc_full_chain").Optional().
			Comment("AES-GCM(envelope) 加密的全链（leaf+intermediate）"),
	}
}

// Edges 一张证书可分发到多个节点。
func (Certificate) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("deployments", CertDeployment.Type).
			Comment("该证书在各节点的分发记录"),
	}
}
