// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package cert 实现证书子系统的核心逻辑：上传校验（T043）、信封加密入库由
// domain 层调用、续期调度（T045）后续接入。
//
// 校验严格对齐 DECISIONS §5.6 的「6 项校验」：
//  1. 私钥/证书模数匹配；
//  2. 链完整（可构建到可信根，软校验）；
//  3. 链顺序（leaf→inter，最后一张不得是自签根，硬校验）；
//  4. SAN 覆盖引用它的 server_name（调用方传入引用域名时校验）；
//  5. 有效期（已过期/未生效拒绝；<7天/<30天警告）；
//  6. 签名算法（SHA-1/MD5 等弱算法拒绝）。
package cert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ValidationResult 校验结果。
// Errors 为拒绝级（任一存在则 OK=false，证书不得入库）；
// Warnings 为警告级（如临近到期、链未连到系统根），不阻断入库但前端展示。
type ValidationResult struct {
	OK       bool     `json:"ok"`
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

// ValidateUpload 校验上传的证书材料。
//
// 参数：
//   - certPEM：叶子证书 PEM，可同时包含中间证书（fullchain 风格）；
//   - keyPEM：私钥 PEM（PKCS1/PKCS8/EC 均支持）；
//   - chainPEM：单独提供的额外中间证书（可选，可与 certPEM 合并）；
//   - referencedNames：引用该证书的 server_name 列表（可选），用于 SAN 覆盖校验。
func ValidateUpload(certPEM, keyPEM, chainPEM []byte, referencedNames []string) ValidationResult {
	var res ValidationResult

	key, keyErr := parsePrivateKey(keyPEM)
	if keyErr != nil {
		res.Errors = append(res.Errors, "私钥解析失败："+keyErr.Error())
	}

	certs, certErr := parseCertificates(certPEM)
	if certErr != nil {
		res.Errors = append(res.Errors, "证书解析失败："+certErr.Error())
		return res // 没有叶子证书，后续全部无意义
	}
	if len(certs) == 0 {
		res.Errors = append(res.Errors, "未找到任何证书（CERTIFICATE PEM 块）")
		return res
	}
	leaf := certs[0]

	// ① 私钥/证书模数匹配（用公钥比对，绝不用私钥内容）。
	if keyErr == nil && !publicKeyMatches(key, leaf.PublicKey) {
		res.Errors = append(res.Errors, "私钥与证书公钥不匹配（模数/公钥不一致）")
	}

	// 组装完整链：certPEM 中 leaf 之后的块 + 单独传入的 chainPEM。
	chain := certs[1:]
	if len(chainPEM) > 0 {
		extra, e := parseCertificates(chainPEM)
		if e != nil {
			res.Errors = append(res.Errors, "中间证书链解析失败："+e.Error())
		} else {
			chain = append(chain, extra...)
		}
	}

	// ③ 链顺序（leaf→inter，最后不得是自签根）。硬校验。
	if err := checkChainOrder(leaf, chain); err != nil {
		res.Errors = append(res.Errors, err.Error())
	} else if len(chain) > 0 {
		// ② 链完整：尝试构建到可信根。系统根缺失（自托管 CA）时仅警告。
		if err := verifyChainToRoots(leaf, chain); err != nil {
			res.Warnings = append(res.Warnings, "证书链无法构建到系统可信根（自托管 CA 可接受）："+err.Error())
		}
	}

	// ④ SAN 覆盖：仅当调用方传入引用域名时校验。
	if len(referencedNames) > 0 {
		names := certNames(leaf)
		if len(names) == 0 {
			res.Errors = append(res.Errors, "证书不含任何 SAN 或 Common Name")
		} else if missing := sansCover(names, referencedNames); len(missing) > 0 {
			res.Errors = append(res.Errors, fmt.Sprintf("证书 SAN 未覆盖以下引用域名：%v", missing))
		}
	}

	now := time.Now()
	// ⑤ 有效期：已过期/未生效拒绝；<7天、<30天警告（剩余天数展示）。
	if now.After(leaf.NotAfter) {
		res.Errors = append(res.Errors, fmt.Sprintf("证书已过期（到期 %s UTC）", leaf.NotAfter.UTC().Format(time.RFC3339)))
	} else {
		remain := leaf.NotAfter.Sub(now)
		if remain < 7*24*time.Hour {
			res.Warnings = append(res.Warnings, fmt.Sprintf("证书将在 %.1f 天内到期，请尽快续期", remain.Hours()/24))
		} else if remain < 30*24*time.Hour {
			res.Warnings = append(res.Warnings, fmt.Sprintf("证书将在 %.1f 天内到期", remain.Hours()/24))
		}
	}
	if now.Before(leaf.NotBefore) {
		res.Errors = append(res.Errors, fmt.Sprintf("证书尚未生效（生效 %s UTC）", leaf.NotBefore.UTC().Format(time.RFC3339)))
	}

	// ⑥ 弱签名算法拒绝。
	if weakSig(leaf.SignatureAlgorithm) {
		res.Errors = append(res.Errors, fmt.Sprintf("证书使用弱签名算法 %s，已拒绝（存在安全风险）", leaf.SignatureAlgorithm))
	}

	res.OK = len(res.Errors) == 0
	return res
}

// parsePrivateKey 解析 PEM 私钥（PKCS1 / PKCS8 / SEC1 EC），自动跳过非密钥块。
func parsePrivateKey(pemBytes []byte) (crypto.PrivateKey, error) {
	rest := pemBytes
	var lastErr error
	for {
		block, r := pem.Decode(rest)
		if block == nil {
			break
		}
		if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
			return key, nil
		}
		if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
			return key, nil
		}
		if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
			return key, nil
		}
		lastErr = errors.New("无法解析私钥（PKCS1/PKCS8/EC 均失败，请确认是 PEM 格式私钥）")
		rest = r
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("未找到 PRIVATE KEY PEM 块")
}

// parseCertificates 解析一段可能含多个 CERTIFICATE 块的 PEM，顺序返回。
func parseCertificates(pemBytes []byte) ([]*x509.Certificate, error) {
	var ders [][]byte
	rest := pemBytes
	for {
		block, r := pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			ders = append(ders, block.Bytes)
		}
		rest = r
	}
	if len(ders) == 0 {
		return nil, errors.New("未找到 CERTIFICATE PEM 块")
	}
	certs := make([]*x509.Certificate, 0, len(ders))
	for _, d := range ders {
		c, err := x509.ParseCertificate(d)
		if err != nil {
			return nil, fmt.Errorf("解析证书失败：%w", err)
		}
		certs = append(certs, c)
	}
	return certs, nil
}

// publicKeyMatches 用公钥比对私钥与证书是否配对（RSA 比模数 N，ECDSA 比 X/Y）。
func publicKeyMatches(priv crypto.PrivateKey, pub crypto.PublicKey) bool {
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		rpub, ok := pub.(*rsa.PublicKey)
		return ok && k.PublicKey.N.Cmp(rpub.N) == 0
	case *ecdsa.PrivateKey:
		epub, ok := pub.(*ecdsa.PublicKey)
		return ok && k.PublicKey.X.Cmp(epub.X) == 0 && k.PublicKey.Y.Cmp(epub.Y) == 0
	default:
		return false
	}
}

// checkChainOrder 校验 leaf 由 chain[0] 签发、依次向上、且最后一张不是自签根。
func checkChainOrder(leaf *x509.Certificate, chain []*x509.Certificate) error {
	if len(chain) == 0 {
		return nil // 单证书（自签名或依赖系统根），顺序无法判断，交 verifyChainToRoots
	}
	// 最后一张若自签名（Subject==Issuer 且签名自验），视为携带根证书，应拒绝。
	last := chain[len(chain)-1]
	if strings.EqualFold(string(last.RawSubject), string(last.RawIssuer)) {
		if err := last.CheckSignatureFrom(last); err == nil {
			return errors.New("证书链包含根证书（root），应只提供 leaf + intermediate")
		}
	}
	parent := leaf
	for i, c := range chain {
		if err := parent.CheckSignatureFrom(c); err != nil {
			return fmt.Errorf("证书链顺序错误：第 %d 张证书不由前一张签发", i+1)
		}
		parent = c
	}
	return nil
}

// verifyChainToRoots 尝试把 leaf+chain 构建到可信根。系统根缺失时返回错误（调用方降级为警告）。
func verifyChainToRoots(leaf *x509.Certificate, chain []*x509.Certificate) error {
	roots := x509.NewCertPool()
	if sys, err := x509.SystemCertPool(); err == nil && sys != nil {
		roots = sys
	}
	inter := x509.NewCertPool()
	for _, c := range chain {
		inter.AddCert(c)
	}
	// 用证书有效期起点稍后一点作为验证时间，避免过期状态干扰纯链结构校验。
	verifyTime := leaf.NotBefore.Add(time.Minute)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		CurrentTime:   verifyTime,
	}); err != nil {
		return err
	}
	return nil
}

// certNames 返回证书所有可用域名（SAN + Common Name）。
func certNames(c *x509.Certificate) []string {
	names := append([]string{}, c.DNSNames...)
	if cn := c.Subject.CommonName; cn != "" {
		names = append(names, cn)
	}
	return names
}

// sansCover 检查 referenced 中的每个域名是否都被 names 覆盖（支持 *.example.com 通配）。
func sansCover(names, referenced []string) []string {
	var missing []string
	for _, ref := range referenced {
		covered := false
		for _, n := range names {
			if n == ref || wildcardMatch(n, ref) {
				covered = true
				break
			}
		}
		if !covered {
			missing = append(missing, ref)
		}
	}
	return missing
}

// wildcardMatch 判断 name 是否被通配符 pattern（如 *.example.com）覆盖。
// 仅匹配单级子域：a.example.com ✓；a.b.example.com ✗。
func wildcardMatch(pattern, name string) bool {
	if !strings.HasPrefix(pattern, "*.") {
		return false
	}
	base := pattern[2:]
	if base == "" || strings.Contains(base, "..") {
		return false
	}
	if !strings.HasSuffix(name, "."+base) {
		return false
	}
	prefix := strings.TrimSuffix(name, "."+base)
	return prefix != "" && !strings.Contains(prefix, ".")
}

// weakSig 返回证书签名算法是否为已知弱算法（SHA-1 / MD5 / MD2 等）。
func weakSig(algo x509.SignatureAlgorithm) bool {
	switch algo {
	case x509.MD2WithRSA, x509.MD5WithRSA, x509.SHA1WithRSA, x509.ECDSAWithSHA1:
		return true
	default:
		return false
	}
}

// ParseCertificateChain 导出解析：按 PEM 顺序返回证书链（供 domain 层提取元数据）。
func ParseCertificateChain(pemBytes []byte) ([]*x509.Certificate, error) {
	return parseCertificates(pemBytes)
}
