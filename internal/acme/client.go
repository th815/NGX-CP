// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package acme 封装 Let's Encrypt 的 DNS-01 签发流程（M4 T042），支持通配符证书。
//
// 本项目只走 DNS-01（Cloudflare 前置拦截 HTTP-01），故底层用 lego 的 DNS-01 provider 机制，
// 并把内部 dns.DNSProvider（T041，自实现 Cloudflare）适配为 lego 的 challenge.Provider。
// 生产用 LE 默认目录；调试可传 CADirURL 指向 staging / pebble（配合 ratelimit 防自锁）。
package acme

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/registration"
	"github.com/th/ngxcp/internal/dns"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// User 实现 lego 的 registration.User 接口（账户身份 + 私钥）。
type User struct {
	Email        string
	Registration *registration.Resource
	key          *rsa.PrivateKey
}

func (u *User) GetEmail() string                         { return u.Email }
func (u *User) GetRegistration() *registration.Resource { return u.Registration }
func (u *User) GetPrivateKey() crypto.PrivateKey        { return u.key }

// legoSolver 把 dns.DNSProvider 适配为 lego 的 challenge.Provider（DNS-01）。
type legoSolver struct {
	provider dns.DNSProvider
}

// Present 写入 _acme-challenge.<domain> TXT 记录。通配符 *.example.com 的 zone 取 example.com。
func (s *legoSolver) Present(domain, _ /*token*/, keyAuth string) error {
	info := dns01.GetChallengeInfo(domain, keyAuth)
	name := strings.TrimSuffix(info.EffectiveFQDN, ".")
	zone := strings.TrimPrefix(domain, "*.")
	return s.provider.SetRecord(context.Background(), zone, name, info.Value)
}

// CleanUp 挑战完成后清理 TXT 记录，避免残留。
func (s *legoSolver) CleanUp(domain, _ /*token*/, keyAuth string) error {
	info := dns01.GetChallengeInfo(domain, keyAuth)
	name := strings.TrimSuffix(info.EffectiveFQDN, ".")
	zone := strings.TrimPrefix(domain, "*.")
	return s.provider.DeleteRecord(context.Background(), zone, name)
}

// Client 是 ACME 签发客户端（T042），内置本地限流。
type Client struct {
	rl *RateLimiter
}

// NewClient 构造 ACME 客户端。
func NewClient() *Client {
	return &Client{rl: NewRateLimiter(6 * time.Second)}
}

// IssueRequest 签发请求。
type IssueRequest struct {
	Domains    []string        // 含 *.example.com 等通配符
	Provider   dns.DNSProvider // T041 的 DNS provider（Cloudflare）
	KeyAlg     string          // "rsa2048"（默认）/ "ecdsa256"
	CADirURL   string          // 默认 LE 生产；调试传 staging / pebble
	Email      string          // 账户邮箱（LE 必填）
	AccountKey *rsa.PrivateKey // 可选：复用账户私钥
}

// IssueResult 签发结果（PEM 编码，可直接入库/落盘）。
type IssueResult struct {
	Domain   string
	CertPEM  []byte
	KeyPEM   []byte
	ChainPEM []byte // issuer / 中间证书
	Issuer   string
}

// Issue 走 DNS-01 流程签发证书，成功后返回 PEM。通配符只能用 DNS-01。
func (c *Client) Issue(ctx context.Context, req IssueRequest) (*IssueResult, error) {
	if len(req.Domains) == 0 {
		return nil, apperr.New(apperr.CodeInvalid, "未指定域名")
	}
	if req.Provider == nil {
		return nil, apperr.New(apperr.CodeInvalid, "未指定 DNS provider")
	}
	if req.Email == "" {
		return nil, apperr.New(apperr.CodeInvalid, "ACME 账户邮箱必填")
	}
	// 限流：避免触发 LE 50 张/域名/周 自锁。
	if c.rl != nil {
		if err := c.rl.Wait(ctx, req.Domains[0]); err != nil {
			return nil, err
		}
	}

	user := &User{Email: req.Email}
	if req.AccountKey != nil {
		user.key = req.AccountKey
	} else {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "生成账户密钥失败", err)
		}
		user.key = k
	}

	config := lego.NewConfig(user)
	if req.CADirURL != "" {
		config.CADirURL = req.CADirURL
	}
	config.Certificate.KeyType = keyType(req.KeyAlg)

	client, err := lego.NewClient(config)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "创建 ACME 客户端失败", err)
	}
	reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeUnavailable, "ACME 账户注册失败", err)
	}
	user.Registration = reg

	// 把内部 DNS provider 接入 lego 的 DNS-01 挑战。
	client.Challenge.SetDNS01Provider(&legoSolver{provider: req.Provider})

	certs, err := client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: req.Domains,
		Bundle:  true,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeUnavailable, "ACME 签发失败", err)
	}
	return &IssueResult{
		Domain:   req.Domains[0],
		CertPEM:  certs.Certificate,
		KeyPEM:   certs.PrivateKey,
		ChainPEM: certs.IssuerCertificate,
		Issuer:   "Let's Encrypt",
	}, nil
}

func keyType(alg string) certcrypto.KeyType {
	switch strings.ToLower(alg) {
	case "ecdsa256", "ecdsa-p256", "ecdsa":
		return certcrypto.EC256
	default:
		return certcrypto.RSA2048
	}
}
