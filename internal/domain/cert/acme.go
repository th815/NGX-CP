// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package cert 的 ACME 签发 / 续期逻辑（M4 T042 + T045）。
//
// 安全红线（AGENTS §9.2）：ACME 账户私钥与 DNS provider Token 经 KMS 信封加密存储，
// API 绝不回传；续期复用存储的账户密钥避免重复注册 LE 账户。
package cert

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"time"

	"github.com/th/ngxcp/ent"
	entcert "github.com/th/ngxcp/ent/certificate"
	entcertdep "github.com/th/ngxcp/ent/certdeployment"
	"github.com/th/ngxcp/internal/acme"
	"github.com/th/ngxcp/internal/cert"
	"github.com/th/ngxcp/internal/dns"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// IssueACMERequest ACME 签发请求（T042 落地：签发并入库）。
type IssueACMERequest struct {
	Domains       []string // 含 *.example.com 等通配符
	Email         string   // ACME 账户邮箱（LE 必填）
	ProviderType  string   // DNS-01 provider 类型，默认 cloudflare
	ProviderToken string   // DNS-01 provider API Token（经 KMS 加密存储）
	KeyAlg        string   // rsa2048（默认）/ ecdsa256
	CADirURL      string   // 默认 LE 生产；staging/pebble 调试用
}

// IssueACME 经 DNS-01 签发证书并信封加密入库，同时持久化续期所需凭据
// （ACME 账户私钥 + provider Token + email + key alg + CA 目录），便于 T045 自动续期。
func (s *Service) IssueACME(ctx context.Context, req IssueACMERequest) (*CertificateView, error) {
	if len(req.Domains) == 0 {
		return nil, apperr.New(apperr.CodeInvalid, "未指定域名")
	}
	if req.Email == "" {
		return nil, apperr.New(apperr.CodeInvalid, "ACME 账户邮箱必填")
	}
	if req.ProviderToken == "" {
		return nil, apperr.New(apperr.CodeInvalid, "DNS-01 provider Token 必填")
	}
	ptype := req.ProviderType
	if ptype == "" {
		ptype = "cloudflare"
	}
	provider, err := dns.New(dns.Config{Type: ptype, Token: req.ProviderToken})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalid, "构造 DNS provider 失败", err)
	}

	// 自生成 ACME 账户密钥并持久化（续期复用，避免重复注册 LE 账户）。
	acctKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "生成 ACME 账户密钥失败", err)
	}
	acctPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(acctKey),
	})

	res, err := s.issuer().Issue(ctx, acme.IssueRequest{
		Domains:    req.Domains,
		Provider:   provider,
		KeyAlg:     req.KeyAlg,
		CADirURL:   req.CADirURL,
		Email:      req.Email,
		AccountKey: acctKey,
	})
	if err != nil {
		return nil, err // Issue 已包装为 apperr
	}

	leaf, err := parseLeaf(res.CertPEM)
	if err != nil {
		return nil, err
	}
	if s.kms == nil {
		return nil, apperr.New(apperr.CodeUnavailable, "主密钥未配置，无法加密存储证书")
	}
	fullChain := append(append([]byte{}, res.CertPEM...), res.ChainPEM...)
	encKey, err := s.kms.Encrypt(res.KeyPEM)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "加密私钥失败", err)
	}
	encChain, err := s.kms.Encrypt(fullChain)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "加密证书链失败", err)
	}
	encAcct, err := s.kms.Encrypt(acctPEM)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "加密 ACME 账户密钥失败", err)
	}
	encToken, err := s.kms.Encrypt([]byte(req.ProviderToken))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "加密 provider Token 失败", err)
	}

	domain := req.Domains[0]
	if len(leaf.DNSNames) > 0 {
		domain = leaf.DNSNames[0]
	}
	created, err := s.client.Certificate.Create().
		SetDomain(domain).
		SetSan(leaf.DNSNames).
		SetIssuer(res.Issuer).
		SetSerialNumber(hex.EncodeToString(leaf.SerialNumber.Bytes())).
		SetFingerprintSha(hex.EncodeToString(fingerprint(leaf.Raw))).
		SetNotBefore(leaf.NotBefore).
		SetNotAfter(leaf.NotAfter).
		SetKeyAlg(keyAlgName(leaf.PublicKey)).
		SetSource(entcert.SourceAcme).
		SetStatus(entcert.StatusValid).
		SetEncPrivateKey(encKey).
		SetEncFullChain(encChain).
		SetEncAcmeAccountKey(encAcct).
		SetEncAcmeProviderToken(encToken).
		SetAcmeProviderType(ptype).
		SetAcmeEmail(req.Email).
		SetAcmeKeyAlg(req.KeyAlg).
		SetAcmeCaDirURL(req.CADirURL).
		Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return nil, apperr.New(apperr.CodeConflict, "该证书（序列号）已存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "写入证书失败", err)
	}
	return toView(created), nil
}

// Renew 续期一张 ACME 证书：用存储的账户密钥 + provider Token 重新签发，
// 更新库内证书行（序列号/指纹/有效期/密文），并重分发到历史节点（走 T044 原子流水线）。
func (s *Service) Renew(ctx context.Context, certID int) error {
	c, err := s.client.Certificate.Get(ctx, certID)
	if err != nil {
		if ent.IsNotFound(err) {
			return apperr.New(apperr.CodeNotFound, "证书不存在")
		}
		return apperr.Wrap(apperr.CodeInternal, "查询证书失败", err)
	}
	if c.Source != entcert.SourceAcme {
		return apperr.New(apperr.CodeInvalid, "仅 ACME 签发的证书可自动续期")
	}
	if len(c.EncAcmeAccountKey) == 0 || len(c.EncAcmeProviderToken) == 0 {
		return apperr.New(apperr.CodeUnavailable, "缺少 ACME 续期凭据（账户密钥/provider Token 未存储）")
	}
	if s.kms == nil {
		return apperr.New(apperr.CodeUnavailable, "主密钥未配置，无法解密续期凭据")
	}

	acctPEM, err := s.kms.Decrypt(c.EncAcmeAccountKey)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "解密 ACME 账户密钥失败", err)
	}
	token, err := s.kms.Decrypt(c.EncAcmeProviderToken)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "解密 provider Token 失败", err)
	}
	acctKey, err := parseRSAPrivateKey(acctPEM)
	if err != nil {
		return err
	}
	provider, err := dns.New(dns.Config{Type: c.AcmeProviderType, Token: string(token)})
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalid, "构造 DNS provider 失败", err)
	}

	domains := append([]string{}, c.San...)
	if len(domains) == 0 {
		domains = []string{c.Domain}
	}
	res, err := s.issuer().Issue(ctx, acme.IssueRequest{
		Domains:    domains,
		Provider:   provider,
		KeyAlg:     c.AcmeKeyAlg,
		CADirURL:   c.AcmeCaDirURL,
		Email:      c.AcmeEmail,
		AccountKey: acctKey,
	})
	if err != nil {
		return err
	}

	leaf, err := parseLeaf(res.CertPEM)
	if err != nil {
		return err
	}
	fullChain := append(append([]byte{}, res.CertPEM...), res.ChainPEM...)
	encKey, err := s.kms.Encrypt(res.KeyPEM)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "加密私钥失败", err)
	}
	encChain, err := s.kms.Encrypt(fullChain)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "加密证书链失败", err)
	}

	if _, err := c.Update().
		SetSerialNumber(hex.EncodeToString(leaf.SerialNumber.Bytes())).
		SetFingerprintSha(hex.EncodeToString(fingerprint(leaf.Raw))).
		SetNotBefore(leaf.NotBefore).
		SetNotAfter(leaf.NotAfter).
		SetKeyAlg(keyAlgName(leaf.PublicKey)).
		SetStatus(entcert.StatusValid).
		SetEncPrivateKey(encKey).
		SetEncFullChain(encChain).
		SetUpdatedAt(time.Now()).
		Save(ctx); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "更新证书失败", err)
	}

	// 重分发到历史节点（T044 原子流水线：校验/灰度/探活/回滚），满足"续期必须走发布流水线"。
	return s.redistributeToPriorNodes(ctx, certID)
}

// ListDueForRenewal 返回在 horizon 内到期、可自动续期的 ACME 证书 ID 列表（T045 调度用）。
func (s *Service) ListDueForRenewal(ctx context.Context, horizon time.Duration) ([]int, error) {
	cs, err := s.client.Certificate.Query().
		Where(
			entcert.SourceEQ(entcert.SourceAcme),
			entcert.StatusEQ(entcert.StatusValid),
			entcert.NotAfterLTE(time.Now().Add(horizon)),
		).
		All(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "查询待续期证书失败", err)
	}
	ids := make([]int, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	return ids, nil
}

// redistributeToPriorNodes 把证书重分发到其历史分发记录中的全部节点（续期后刷新线上证书）。
func (s *Service) redistributeToPriorNodes(ctx context.Context, certID int) error {
	if s.deployer == nil {
		return nil // 无下发通道则仅更新库内证书，跳过重分发
	}
	ds, err := s.client.CertDeployment.Query().
		Where(entcertdep.HasCertificateWith(entcert.ID(certID))).
		All(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "查询历史分发记录失败", err)
	}
	nodeIDs := make([]int, 0, len(ds))
	for _, d := range ds {
		nodeIDs = append(nodeIDs, d.NodeID)
	}
	if len(nodeIDs) == 0 {
		return nil
	}
	_, err = s.Distribute(ctx, certID, DistributeRequest{NodeIDs: nodeIDs}, s.deployer)
	return err
}

// parseLeaf 从 PEM 证书（可含链）解析出 leaf 证书。
func parseLeaf(pemBytes []byte) (*x509.Certificate, error) {
	certs, err := cert.ParseCertificateChain(pemBytes)
	if err != nil || len(certs) == 0 {
		return nil, apperr.Wrap(apperr.CodeInvalid, "签发结果证书解析失败", err)
	}
	return certs[0], nil
}

// parseRSAPrivateKey 解析 PKCS#1 PEM 的 RSA 私钥（lego 账户密钥格式）。
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, apperr.New(apperr.CodeInvalid, "ACME 账户密钥 PEM 解析失败")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalid, "ACME 账户密钥解析失败", err)
	}
	return key, nil
}

// fingerprint 计算 DER 证书的 SHA-256 指纹（返回可 hex 编码的切片）。
func fingerprint(der []byte) []byte {
	sum := sha256.Sum256(der)
	return sum[:]
}
