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
	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	entcert "github.com/th/ngxcp/ent/certificate"
	entcertdep "github.com/th/ngxcp/ent/certdeployment"
	"github.com/th/ngxcp/internal/acme"
	"github.com/th/ngxcp/internal/cert"
	"github.com/th/ngxcp/internal/crypto"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// ACMEIssuer 是 ACME 签发能力的抽象（T042 客户端实现；单测可注入 fake）。
type ACMEIssuer interface {
	Issue(ctx context.Context, req acme.IssueRequest) (*acme.IssueResult, error)
}

// Service 证书域服务。kms 为 nil 时 Upload 返回明确错误（主密钥未配置）。
// deployer 为 nil 时 Distribute/Renew 不可用（控制面未接入 Agent 下发通道）。
// acmeIssuer 为 nil 时回落到 acme.NewClient()（真实 LE）；单测可注入 fake。
type Service struct {
	client    *ent.Client
	kms       *crypto.KMS
	deployer  Deployer
	acmeIssuer ACMEIssuer
}

// New 构造证书服务。kms 可为 nil（仅查询可用，上传/解密不可用）。
func New(client *ent.Client, kms *crypto.KMS) *Service {
	return &Service{client: client, kms: kms}
}

// SetDeployer 注入 Agent 证书下发通道（transport.Server），使 Distribute/Renew 可用。
func (s *Service) SetDeployer(d Deployer) { s.deployer = d }

// SetIssuer 注入 ACME 签发器（默认 acme.NewClient()）；单测可注入 fake。
func (s *Service) SetIssuer(i ACMEIssuer) { s.acmeIssuer = i }

// issuer 返回当前签发器，nil 时回落到真实 LE 客户端。
func (s *Service) issuer() ACMEIssuer {
	if s.acmeIssuer != nil {
		return s.acmeIssuer
	}
	return acme.NewClient()
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

// ===== T044：证书分发到节点 =====

// Deployer 是控制面向 Agent 下发证书落盘任务的能力抽象（注入 transport.Server）。
// 私钥明文经 mTLS 下发，绝不进浏览器、绝不入库明文（控制面用 KMS 信封加密存储）。
type Deployer interface {
	DeployCert(ctx context.Context, nodeID int, task *agentv1.DeployCertTask) (*agentv1.DeployCertResult, error)
}

// DistributeRequest 分发请求：把一张证书分发到一组节点。
type DistributeRequest struct {
	NodeIDs          []int  // 目标节点 ID 列表
	SSLDir           string // 落盘目录，默认 /etc/nginx/ssl
	NginxPath        string // nginx 二进制路径，默认 /usr/sbin/nginx
	Reload           *bool  // 落盘后是否 reload，默认 true
	ObserveWindowSec int64  // 落盘后观测窗口（秒），默认 5
	ProbeURL         string // 探活 URL，空则跳过探活
}

// DeploymentResult 单个节点的分发结果。
type DeploymentResult struct {
	NodeID     int    `json:"node_id"`
	NodeName   string `json:"node_name"`
	Status     string `json:"status"` // deployed / failed
	Error      string `json:"error,omitempty"`
	DeployedAt int64  `json:"deployed_at,omitempty"` // unix 秒，UTC
}

// DistributeResult 分发聚合结果。
type DistributeResult struct {
	Total    int                `json:"total"`
	Deployed int                `json:"deployed"`
	Failed   int                `json:"failed"`
	Items    []*DeploymentResult `json:"items"`
}

// Distribute 把证书解密后逐节点下发到 Agent 落盘，并写/更新 cert_deployments 分发记录。
// 任一节点失败不影响其他节点（逐节点独立结果）。
func (s *Service) Distribute(ctx context.Context, certID int, req DistributeRequest, deployer Deployer) (*DistributeResult, error) {
	if deployer == nil {
		return nil, apperr.New(apperr.CodeUnavailable, "控制面未接入 Agent 下发通道（transport.Server 缺失）")
	}
	if s.kms == nil {
		return nil, apperr.New(apperr.CodeUnavailable, "主密钥未配置，无法解密证书")
	}
	if len(req.NodeIDs) == 0 {
		return nil, apperr.New(apperr.CodeInvalid, "未指定目标节点")
	}

	// 1) 取证书 + KMS 解密私钥与全链（明文仅本函数作用域内存在，绝不入库/回传）。
	c, err := s.client.Certificate.Get(ctx, certID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "证书不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "查询证书失败", err)
	}
	keyPEM, err := s.kms.Decrypt(c.EncPrivateKey)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "解密私钥失败", err)
	}
	fullChain, err := s.kms.Decrypt(c.EncFullChain)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "解密证书链失败", err)
	}

	sslDir := req.SSLDir
	if sslDir == "" {
		sslDir = "/etc/nginx/ssl"
	}
	nginxPath := req.NginxPath
	if nginxPath == "" {
		nginxPath = "/usr/sbin/nginx"
	}
	reload := true
	if req.Reload != nil {
		reload = *req.Reload
	}
	observe := req.ObserveWindowSec
	if observe == 0 {
		observe = 5
	}

	res := &DistributeResult{}
	for _, nodeID := range req.NodeIDs {
		item := &DeploymentResult{NodeID: nodeID}
		node, nerr := s.client.Node.Get(ctx, nodeID)
		if nerr != nil {
			item.Status = "failed"
			item.Error = "节点不存在"
		} else {
			item.NodeName = node.Name
			task := &agentv1.DeployCertTask{
				TaskId:           fmt.Sprintf("cert-%d-node-%d-%d", certID, nodeID, time.Now().UnixNano()),
				Domain:           c.Domain,
				CertPem:          string(fullChain),
				KeyPem:           string(keyPEM),
				SslDir:           sslDir,
				NginxPath:        nginxPath,
				Reload:           reload,
				ObserveWindowSec: observe,
				ProbeUrl:         req.ProbeURL,
			}
			dr, derr := deployer.DeployCert(ctx, nodeID, task)
			if derr != nil {
				item.Status = "failed"
				item.Error = derr.Error()
			} else if dr == nil || !dr.GetOk() {
				item.Status = "failed"
				if dr != nil && dr.GetError() != "" {
					item.Error = dr.GetError()
				} else {
					item.Error = "Agent 返回失败（无原因）"
				}
			} else {
				item.Status = "deployed"
				item.DeployedAt = dr.GetDeployedAt()
			}
		}
		// 写/更新分发记录（cert_deployments），便于 UI 展示逐节点状态。
		s.upsertDeployment(ctx, certID, nodeID, item)
		if item.Status == "deployed" {
			res.Deployed++
		} else {
			res.Failed++
		}
		res.Items = append(res.Items, item)
		res.Total++
	}
	return res, nil
}

// upsertDeployment 写或更新单条 cert_deployments 记录（certificate_id + node_id 维度 upsert）。
func (s *Service) upsertDeployment(ctx context.Context, certID, nodeID int, item *DeploymentResult) {
	existing, qerr := s.client.CertDeployment.Query().
		Where(
			entcertdep.HasCertificateWith(entcert.ID(certID)),
			entcertdep.NodeID(nodeID),
		).
		Only(ctx)
	status := entcertdep.Status(item.Status)
	if qerr != nil {
		create := s.client.CertDeployment.Create().
			SetNodeID(nodeID).
			SetCertificateID(certID).
			SetStatus(status)
		if item.DeployedAt != 0 {
			create.SetDeployedAt(time.Unix(item.DeployedAt, 0).UTC())
		}
		if item.Error != "" {
			create.SetError(item.Error)
		}
		_, _ = create.Save(ctx)
		return
	}
	upd := existing.Update().SetStatus(status)
	if item.DeployedAt != 0 {
		upd.SetDeployedAt(time.Unix(item.DeployedAt, 0).UTC())
	}
	if item.Error != "" {
		upd.SetError(item.Error)
	} else {
		upd.ClearError()
	}
	_, _ = upd.Save(ctx)
}

// GetDeployments 返回某证书在各节点的分发记录（供 UI 展示逐节点状态）。
func (s *Service) GetDeployments(ctx context.Context, certID int) ([]*DeploymentResult, error) {
	ds, err := s.client.CertDeployment.Query().
		Where(entcertdep.HasCertificateWith(entcert.ID(certID))).
		Order(entcertdep.ByNodeID()).
		All(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "查询分发记录失败", err)
	}
	out := make([]*DeploymentResult, 0, len(ds))
	for _, d := range ds {
		item := &DeploymentResult{
			NodeID: d.NodeID,
			Status: string(d.Status),
			Error:  d.Error,
		}
		if !d.DeployedAt.IsZero() {
			item.DeployedAt = d.DeployedAt.Unix()
		}
		out = append(out, item)
	}
	return out, nil
}
