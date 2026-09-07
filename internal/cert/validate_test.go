// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package cert

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func newRSA(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	return k
}

func makeCert(t *testing.T, tmpl, parent *x509.Certificate, pub crypto.PublicKey, signer crypto.PrivateKey) *x509.Certificate {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return c
}

func caTemplate(cn string, notAfter time.Time) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		SignatureAlgorithm:    x509.SHA256WithRSA,
	}
}

func leafTemplate(cn string, sans []string, notBefore, notAfter time.Time, sig x509.SignatureAlgorithm) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		DNSNames:              sans,
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		SignatureAlgorithm:    sig,
	}
}

func encCert(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

func encKey(priv interface{}) []byte {
	b, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b})
}

// 构造一棵完整证书树：root(self) -> inter -> leaf，全部 RSA-SHA256。
func buildChain(t *testing.T) (leaf, inter, root *x509.Certificate, leafKey, interKey *rsa.PrivateKey) {
	t.Helper()
	rootKey := newRSA(t, 2048)
	interKey = newRSA(t, 2048)
	leafKey = newRSA(t, 2048)

	notAfter := time.Now().Add(365 * 24 * time.Hour)
	rootTmpl := caTemplate("Test Root CA", notAfter.Add(365*24*time.Hour))
	root = makeCert(t, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey) // 自签：parent 传模板自身
	inter = makeCert(t, caTemplate("Test Inter CA", notAfter), root, &interKey.PublicKey, rootKey)
	leaf = makeCert(t, leafTemplate("example.com", []string{"example.com", "*.example.com"}, time.Now().Add(-time.Hour), notAfter, x509.SHA256WithRSA), inter, &leafKey.PublicKey, interKey)
	return
}

func TestValidateUpload_Success(t *testing.T) {
	leaf, inter, _, leafKey, _ := buildChain(t)
	res := ValidateUpload(encCert(leaf), encKey(leafKey), encCert(inter), []string{"example.com"})
	if !res.OK {
		t.Fatalf("期望 OK=true，实际 Errors=%v Warnings=%v", res.Errors, res.Warnings)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("成功样本不应有 Errors：%v", res.Errors)
	}
}

func TestValidateUpload_KeyMismatch(t *testing.T) {
	leaf, inter, _, _, _ := buildChain(t)
	otherKey := newRSA(t, 2048) // 与 leaf 不匹配的私钥
	res := ValidateUpload(encCert(leaf), encKey(otherKey), encCert(inter), nil)
	if res.OK {
		t.Fatal("期望被拒（私钥不匹配），实际 OK=true")
	}
	if !contains(res.Errors, "私钥与证书公钥不匹配") {
		t.Fatalf("缺少期望错误，实际 Errors=%v", res.Errors)
	}
}

func TestValidateUpload_ChainOrderWrong(t *testing.T) {
	leaf, _, _, leafKey, _ := buildChain(t)
	// 用另一棵树的 intermediate 作为 chain：leaf 不由它签发，且末张非自签。
	_, inter2, _, _, _ := buildChain(t)
	res := ValidateUpload(encCert(leaf), encKey(leafKey), encCert(inter2), nil)
	if res.OK {
		t.Fatal("期望被拒（链顺序错误），实际 OK=true")
	}
	if !contains(res.Errors, "证书链顺序错误") {
		t.Fatalf("缺少期望错误，实际 Errors=%v", res.Errors)
	}
}

func TestValidateUpload_ChainContainsRoot(t *testing.T) {
	leaf, inter, root, leafKey, _ := buildChain(t)
	// 把 root 也传进去（用户误传完整链含根）。
	res := ValidateUpload(encCert(leaf), encKey(leafKey), append(encCert(inter), encCert(root)...), nil)
	if res.OK {
		t.Fatal("期望被拒（链含根证书），实际 OK=true")
	}
	if !contains(res.Errors, "证书链包含根证书") {
		t.Fatalf("缺少期望错误，实际 Errors=%v", res.Errors)
	}
}

func TestValidateUpload_Expired(t *testing.T) {
	_, inter, _, leafKey, interKey := buildChain(t)
	expiredLeaf := makeCert(t,
		leafTemplate("expired.example.com", []string{"expired.example.com"},
			time.Now().Add(-72*time.Hour), time.Now().Add(-48*time.Hour), x509.SHA256WithRSA),
		inter, &leafKey.PublicKey, interKey)
	res := ValidateUpload(encCert(expiredLeaf), encKey(leafKey), encCert(inter), nil)
	if res.OK {
		t.Fatal("期望被拒（已过期），实际 OK=true")
	}
	if !contains(res.Errors, "证书已过期") {
		t.Fatalf("缺少期望错误，实际 Errors=%v", res.Errors)
	}
}

func TestValidateUpload_SANNotCovered(t *testing.T) {
	leaf, inter, _, leafKey, _ := buildChain(t)
	res := ValidateUpload(encCert(leaf), encKey(leafKey), encCert(inter), []string{"other.example.org"})
	if res.OK {
		t.Fatal("期望被拒（SAN 未覆盖引用域名），实际 OK=true")
	}
	if !contains(res.Errors, "证书 SAN 未覆盖") {
		t.Fatalf("缺少期望错误，实际 Errors=%v", res.Errors)
	}
}

func TestWeakSig(t *testing.T) {
	if !weakSig(x509.SHA1WithRSA) {
		t.Error("SHA1WithRSA 应判定为弱算法")
	}
	if !weakSig(x509.MD5WithRSA) {
		t.Error("MD5WithRSA 应判定为弱算法")
	}
	if weakSig(x509.SHA256WithRSA) {
		t.Error("SHA256WithRSA 不应判定为弱算法")
	}
}

func contains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
