// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package pki 的 CSR 生成：Agent 注册阶段在本地生成密钥对与证书签名请求（CSR），
// 私钥永不出节点，仅把 CSR 交给控制面换发 mTLS 客户端证书。
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
)

// GenerateKeyAndCSR 生成 P256 密钥对与证书签名请求（CN=commonName）。
// 返回 PEM 编码的私钥与 CSR；私钥仅留本地，CSR 交由控制面签发客户端证书。
func GenerateKeyAndCSR(commonName string) (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: commonName},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, nil, err
	}
	csrPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	keyPEM, err = EncodePrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return keyPEM, csrPEM, nil
}
