// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package handler

import (
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/domain/cert"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/server/response"
)

// CertHandler 证书相关处理器（M4 T043/T044：只读列表/详情 + 上传校验 + 删除 + 分发）。
// 安全红线：任何响应都不含私钥（service 层已隔离）。
type CertHandler struct {
	svc      *cert.Service
	deployer cert.Deployer // T044 下发通道（transport.Server），可为 nil
}

// NewCertHandler 构造证书处理器。deployer 为 T044 证书下发通道，可为 nil（仅查询可用）。
func NewCertHandler(svc *cert.Service, deployer cert.Deployer) *CertHandler {
	return &CertHandler{svc: svc, deployer: deployer}
}

type certUploadBody struct {
	Domain   string   `json:"domain"`
	CertPEM  string   `json:"cert_pem"`
	KeyPEM   string   `json:"key_pem"`
	ChainPEM string   `json:"chain_pem"`
	SANs     []string `json:"sans"`
	Issuer   string   `json:"issuer"`
}

// List 列出全部证书（只读，无需鉴权）。
func (h *CertHandler) List(c *gin.Context) {
	items, err := h.svc.List(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, items, len(items))
}

// Get 获取单张证书元数据（只读，无私钥）。
func (h *CertHandler) Get(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	v, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, v)
}

// Upload 上传证书：走 6 项校验，通过则信封加密入库（写操作，需鉴权）。
func (h *CertHandler) Upload(c *gin.Context) {
	var b certUploadBody
	if err := c.ShouldBindJSON(&b); err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "请求体解析失败："+err.Error()))
		return
	}
	if b.CertPEM == "" {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "cert_pem 不能为空"))
		return
	}
	view, err := h.svc.Upload(c.Request.Context(), cert.UploadRequest{
		Domain:   b.Domain,
		CertPEM:  []byte(b.CertPEM),
		KeyPEM:   []byte(b.KeyPEM),
		ChainPEM: []byte(b.ChainPEM),
		SANs:     b.SANs,
		Issuer:   b.Issuer,
	})
	if err != nil {
		// 校验失败：结构化返回逐条错误与警告，前端逐条展示。
		var ve *cert.ValidationError
		if errors.As(err, &ve) {
			response.FailData(c, apperr.New(apperr.CodeInvalid, err.Error()), ve.Result)
			return
		}
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Delete 删除证书（写操作，需鉴权）。级联清理分发记录在 service 内完成。
func (h *CertHandler) Delete(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if err := h.svc.Delete(c.Request.Context(), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"deleted": id})
}

type certDistributeBody struct {
	NodeIDs          []int  `json:"node_ids"`
	SSLDir           string `json:"ssl_dir"`
	NginxPath        string `json:"nginx_path"`
	Reload           *bool  `json:"reload"`
	ObserveWindowSec int64  `json:"observe_window_sec"`
	ProbeURL         string `json:"probe_url"`
}

// Distribute 把证书分发到一组节点（写操作，需鉴权，T044）。
// 逐节点结果返回：成功/失败及原因；私钥明文仅在本请求作用域内经 mTLS 下发。
func (h *CertHandler) Distribute(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	var b certDistributeBody
	if err := c.ShouldBindJSON(&b); err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "请求体解析失败："+err.Error()))
		return
	}
	res, err := h.svc.Distribute(c.Request.Context(), id, cert.DistributeRequest{
		NodeIDs:          b.NodeIDs,
		SSLDir:           b.SSLDir,
		NginxPath:        b.NginxPath,
		Reload:           b.Reload,
		ObserveWindowSec: b.ObserveWindowSec,
		ProbeURL:         b.ProbeURL,
	}, h.deployer)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}

// certIssueBody ACME 签发请求体（POST /api/v1/certs/issue）。
type certIssueBody struct {
	Domains       []string `json:"domains"`        // 含 *.example.com 等通配符
	Email         string   `json:"email"`          // ACME 账户邮箱（LE 必填）
	ProviderType  string   `json:"provider_type"`  // DNS-01 provider 类型，默认 cloudflare
	ProviderToken string   `json:"provider_token"` // DNS-01 provider API Token（经 KMS 加密存储，绝不回传）
	KeyAlg        string   `json:"key_alg"`        // rsa2048（默认）/ ecdsa256
	CADirURL      string   `json:"ca_dir_url"`     // 默认 LE 生产；staging/pebble 调试用
}

// IssueACME 经 DNS-01 签发证书并入库（写操作，需鉴权，T042/T045）。
// provider_token 仅在请求作用域内经 KMS 加密存储，API 响应绝不含任何私钥/Token。
func (h *CertHandler) IssueACME(c *gin.Context) {
	var b certIssueBody
	if err := c.ShouldBindJSON(&b); err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "请求体解析失败："+err.Error()))
		return
	}
	view, err := h.svc.IssueACME(c.Request.Context(), cert.IssueACMERequest{
		Domains:       b.Domains,
		Email:         b.Email,
		ProviderType:  b.ProviderType,
		ProviderToken: b.ProviderToken,
		KeyAlg:        b.KeyAlg,
		CADirURL:      b.CADirURL,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Renew 手动触发一张 ACME 证书的续期（写操作，需鉴权，T045）。
// 续期复用存储的 ACME 账户密钥 + provider Token 重新签发，并走 T044 原子流水线重分发到历史节点。
func (h *CertHandler) Renew(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if err := h.svc.Renew(c.Request.Context(), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"renewed": id})
}

// Deployments 返回某证书在各节点的分发记录（只读）。
func (h *CertHandler) Deployments(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	items, err := h.svc.GetDeployments(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, items, len(items))
}
