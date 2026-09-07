// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package cert 证书域服务（M4 T043）：上传校验 + 信封加密入库 + 列表/详情/删除。
//
// 安全红线（DECISIONS §5 / AGENTS §9.2）：
//   - 私钥经 KMS 信封加密后以 enc_private_key 存储，API 绝不回传；
//   - 全链经 KMS 加密后以 enc_full_chain 存储，与 config_blob 版本历史隔离；
//   - 详情/列表 API 只返回元数据视图 CertificateView。
package cert

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/th/ngxcp/ent"
	entcert "github.com/th/ngxcp/ent/certificate"
	entcertdep "github.com/th/ngxcp/ent/certdeployment"
	"github.com/th/ngxcp/internal/cert"
	"github.com/th/ngxcp/internal/crypto"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// Service 证书域服务。kms 为 nil 时 Upload 返回明确错误（主密钥未配置）。
type Service struct {
	client *ent.Client
	kms    *crypto.KMS
}

// New 构造证书服务。kms 可为 nil（仅查询可用，上传/解密不可用）。
func New(client *ent.Client, kms *crypto.KMS) *Service {
	return &Service{client: client, kms: kms}
}

// UploadRequest 上传证书请求。CertPEM 可含 leaf+inter（fullchain 风格）。
// Domain / SANs / Issuer 为空时从证书自动推断。
type UploadRequest struct {
	Domain   string
	CertPEM  []byte
	KeyPEM   []byte
	ChainPEM []byte
	SANs     []string
	Issuer   string
}

// CertificateView API 返回的证书元数据（绝不含私钥）。
type CertificateView struct {
	ID           int       `json:"id"`
	Domain       string    `json:"domain"`
	SANs         []string  `json:"sans"`
	Issuer       string    `json:"issuer"`
	SerialNumber string    `json:"serial_number"`
	Fingerprint  string    `json:"fingerprint_sha"`
	NotBefore    time.Time `json:"not_before"`
	NotAfter     time.Time `json:"not_after"`
	KeyAlg       string    `json:"key_alg"`
	Source       string    `json:"source"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	Warnings     []string  `json:"warnings,omitempty"`
}

// Upload 校验并入库一张上传证书。校验失败返回 CodeInvalid 并附带逐条原因。
func (s *Service) Upload(ctx context.Context, req UploadRequest) (*CertificateView, error) {
	// 1) 6 项校验（T043）。
	res := cert.ValidateUpload(req.CertPEM, req.KeyPEM, req.ChainPEM, nil)
	if !res.OK {
		return nil, &ValidationError{Result: res}
	}

	// 2) 提取 leaf 元数据。
	certs, err := cert.ParseCertificateChain(req.CertPEM)
	if err != nil || len(certs) == 0 {
		return nil, apperr.Wrap(apperr.CodeInvalid, "证书解析失败", err)
	}
	leaf := certs[0]

	domain := req.Domain
	if domain == "" {
		if len(leaf.DNSNames) > 0 {
			domain = leaf.DNSNames[0]
		} else {
			domain = leaf.Subject.CommonName
		}
	}
	sans := req.SANs
	if len(sans) == 0 {
		sans = append([]string{}, leaf.DNSNames...)
		if leaf.Subject.CommonName != "" {
			sans = append(sans, leaf.Subject.CommonName)
		}
	}
	issuer := req.Issuer
	if issuer == "" {
		issuer = leaf.Issuer.CommonName
		if issuer == "" {
			issuer = leaf.Issuer.String()
		}
	}
	fingerprint := sha256.Sum256(leaf.Raw)

	// 3) 信封加密敏感材料。
	if s.kms == nil {
		return nil, apperr.New(apperr.CodeUnavailable, "主密钥未配置（NGXCP_MASTER_KEY 或 /etc/ngxcp/master.key），无法加密存储证书")
	}
	fullChain := append(append([]byte{}, req.CertPEM...), req.ChainPEM...)
	encKey, err := s.kms.Encrypt(req.KeyPEM)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "加密私钥失败", err)
	}
	encChain, err := s.kms.Encrypt(fullChain)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "加密证书链失败", err)
	}

	// 4) 入库（序列号唯一约束，重复上传返回 CodeConflict）。
	c, err := s.client.Certificate.Create().
		SetDomain(domain).
		SetSan(sans).
		SetIssuer(issuer).
		SetSerialNumber(hex.EncodeToString(leaf.SerialNumber.Bytes())).
		SetFingerprintSha(hex.EncodeToString(fingerprint[:])).
		SetNotBefore(leaf.NotBefore).
		SetNotAfter(leaf.NotAfter).
		SetKeyAlg(keyAlgName(leaf.PublicKey)).
		SetSource("upload").
		SetStatus("valid").
		SetEncPrivateKey(encKey).
		SetEncFullChain(encChain).
		Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return nil, apperr.New(apperr.CodeConflict, "该证书（序列号）已存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "写入证书失败", err)
	}

	view := toView(c)
	view.Warnings = res.Warnings
	return view, nil
}

// List 列出全部证书（按到期时间升序，最快到期的在前）。
func (s *Service) List(ctx context.Context) ([]*CertificateView, error) {
	cs, err := s.client.Certificate.Query().Order(entcert.ByNotAfter()).All(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "查询证书列表失败", err)
	}
	views := make([]*CertificateView, 0, len(cs))
	for _, c := range cs {
		views = append(views, toView(c))
	}
	return views, nil
}

// Get 获取单张证书元数据（不含私钥）。
func (s *Service) Get(ctx context.Context, id int) (*CertificateView, error) {
	c, err := s.client.Certificate.Get(ctx, id)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "证书不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "查询证书失败", err)
	}
	return toView(c), nil
}

// Delete 删除证书：先级联清理各节点分发记录，再删证书本体（外键 RESTRICT 保护）。
func (s *Service) Delete(ctx context.Context, id int) error {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "开启事务失败", err)
	}
	// 先清子表 cert_deployment（FK -> certificate Required）。
	if _, err := tx.CertDeployment.Delete().
		Where(entcertdep.HasCertificateWith(entcert.ID(id))).
		Exec(ctx); err != nil {
		_ = tx.Rollback()
		return apperr.Wrap(apperr.CodeInternal, "清理证书分发记录失败", err)
	}
	if err := tx.Certificate.DeleteOneID(id).Exec(ctx); err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return apperr.New(apperr.CodeNotFound, "证书不存在")
		}
		return apperr.Wrap(apperr.CodeInternal, "删除证书失败", err)
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "提交事务失败", err)
	}
	return nil
}

// toView 把 ent 实体投影为 API 视图（不含任何私钥字段）。
func toView(c *ent.Certificate) *CertificateView {
	return &CertificateView{
		ID:           c.ID,
		Domain:       c.Domain,
		SANs:         c.San,
		Issuer:       c.Issuer,
		SerialNumber: c.SerialNumber,
		Fingerprint:  c.FingerprintSha,
		NotBefore:    c.NotBefore,
		NotAfter:    c.NotAfter,
		KeyAlg:       c.KeyAlg,
		Source:       string(c.Source),
		Status:       string(c.Status),
		CreatedAt:    c.CreatedAt,
	}
}

// keyAlgName 把公钥生成为可读算法名（RSA-2048 / ECDSA-P256）。
func keyAlgName(pub any) string {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		return fmt.Sprintf("RSA-%d", k.N.BitLen())
	case *ecdsa.PublicKey:
		return fmt.Sprintf("ECDSA-%s", k.Curve.Params().Name)
	default:
		return "UNKNOWN"
	}
}

// ValidationError 携带 6 项校验结果，供 handler 以 FailData 结构化返回前端逐条展示。
type ValidationError struct {
	Result cert.ValidationResult
}

func (e *ValidationError) Error() string {
	if len(e.Result.Errors) == 0 {
		return "证书校验未通过"
	}
	return "证书校验未通过：" + strings.Join(e.Result.Errors, "；")
}
