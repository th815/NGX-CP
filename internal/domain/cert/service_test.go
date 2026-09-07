// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package cert

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	entcert "github.com/th/ngxcp/ent/certificate"
	entcertdep "github.com/th/ngxcp/ent/certdeployment"
	"github.com/th/ngxcp/internal/crypto"
	"github.com/th/ngxcp/internal/repo"
)

func genSelfSigned(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test.example.com"},
		DNSNames:              []string{"test.example.com"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		SignatureAlgorithm:    x509.SHA256WithRSA,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return
}

func TestService_UploadListGetDelete(t *testing.T) {
	ctx := context.Background()
	client, err := repo.Open("sqlite", "file:certsvc?mode=memory&cache=shared&_fk=1")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer client.Close()

	kek := make([]byte, 32)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("rand kek: %v", err)
	}
	kms, err := crypto.NewKMS(kek)
	if err != nil {
		t.Fatalf("new kms: %v", err)
	}
	svc := New(client, kms)

	certPEM, keyPEM := genSelfSigned(t)

	// 上传：校验通过 + 信封加密入库。
	v, err := svc.Upload(ctx, UploadRequest{CertPEM: certPEM, KeyPEM: keyPEM})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if v.Domain != "test.example.com" {
		t.Fatalf("domain 推断错误：%s", v.Domain)
	}
	if v.Fingerprint == "" {
		t.Fatal("指纹不应为空")
	}

	// 私钥必须经 KMS 加密，绝不明文入库。
	c, err := client.Certificate.Get(ctx, v.ID)
	if err != nil {
		t.Fatalf("get raw: %v", err)
	}
	if len(c.EncPrivateKey) == 0 {
		t.Fatal("enc_private_key 不应为空")
	}
	if bytes.Equal(c.EncPrivateKey, keyPEM) {
		t.Fatal("安全红线被违反：私钥明文入库")
	}

	// 列表与详情。
	list, err := svc.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list 期望 1 条，实际 %d (err=%v)", len(list), err)
	}
	g, err := svc.Get(ctx, v.ID)
	if err != nil || g.ID != v.ID {
		t.Fatalf("get 失败：%v", err)
	}

	// 删除级联：先造一条分发记录，删除证书应一并清掉。
	if _, err := client.CertDeployment.Create().SetNodeID(1).SetCertificate(c).Save(ctx); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	if err := svc.Delete(ctx, v.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(ctx, v.ID); err == nil {
		t.Fatal("删除后仍能 Get，期望 not found")
	}
	n, err := client.CertDeployment.Query().Where(entcertdep.HasCertificateWith(entcert.ID(v.ID))).Count(ctx)
	if err != nil {
		t.Fatalf("count deployment: %v", err)
	}
	if n != 0 {
		t.Fatalf("分发记录未级联删除，残留 %d 条", n)
	}
}
