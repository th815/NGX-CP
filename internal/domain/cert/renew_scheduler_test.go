// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package cert

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	entcert "github.com/th/ngxcp/ent/certificate"
	entcertdep "github.com/th/ngxcp/ent/certdeployment"
	"github.com/th/ngxcp/internal/acme"
	"github.com/th/ngxcp/internal/crypto"
	"github.com/th/ngxcp/internal/repo"
)

// —— 调度器单测（不依赖真实 ACME / 数据库） ——

type fakeRenewer struct {
	calls  []int
	failID map[int]bool // 指定 certID 续期失败
}

func (f *fakeRenewer) Renew(_ context.Context, certID int) error {
	f.calls = append(f.calls, certID)
	if f.failID[certID] {
		return errTestRenew
	}
	return nil
}

type fakeLister struct{ due []int }

func (f *fakeLister) ListDue(_ context.Context, _ time.Duration) ([]int, error) { return f.due, nil }

type fakeRecorder struct {
	records []rec
}

type rec struct {
	id     int
	ok     bool
	detail string
}

func (f *fakeRecorder) RecordRenewal(_ context.Context, certID int, ok bool, detail string) error {
	f.records = append(f.records, rec{id: certID, ok: ok, detail: detail})
	return nil
}

var errTestRenew = &testErr{"renew failed"}

type testErr struct{ m string }

func (e *testErr) Error() string { return e.m }

func TestScheduler_RenewDue_Triggers(t *testing.T) {
	r := &fakeRenewer{}
	l := &fakeLister{due: []int{10, 20}}
	rec := &fakeRecorder{}
	s := NewScheduler(r, l, rec, 30*24*time.Hour, nil)

	renewed, failed := s.RenewDue(context.Background())
	if len(renewed) != 2 || len(failed) != 0 {
		t.Fatalf("renewed=%v failed=%v", renewed, failed)
	}
	if len(r.calls) != 2 {
		t.Fatalf("expected 2 renew calls, got %v", r.calls)
	}
	if len(rec.records) != 2 {
		t.Fatalf("expected 2 recorder calls, got %v", rec.records)
	}
	for _, rec0 := range rec.records {
		if !rec0.ok {
			t.Fatalf("expected ok=true, got %+v", rec0)
		}
	}
}

func TestScheduler_RenewDue_FailStreakEscalates(t *testing.T) {
	r := &fakeRenewer{failID: map[int]bool{7: true}}
	l := &fakeLister{due: []int{7}}
	rec := &fakeRecorder{}

	var critID, critFails int
	var critCalled bool
	s := NewScheduler(r, l, rec, 30*24*time.Hour, func(id, fails int) {
		critCalled = true
		critID, critFails = id, fails
	})

	// 连续 3 次失败 → 第 3 次触发 onCritical。
	for i := 0; i < 3; i++ {
		s.RenewDue(context.Background())
	}
	if !critCalled {
		t.Fatal("onCritical 未在第 3 次连续失败时触发")
	}
	if critID != 7 || critFails != 3 {
		t.Fatalf("onCritical 参数错误：id=%d fails=%d", critID, critFails)
	}
	// 记录应全部标记失败。
	for _, rec0 := range rec.records {
		if rec0.ok {
			t.Fatalf("预期全部失败，但记录 ok=true: %+v", rec0)
		}
	}
}

func TestScheduler_RenewDue_SuccessResetsStreak(t *testing.T) {
	r := &fakeRenewer{failID: map[int]bool{7: true}}
	l := &fakeLister{due: []int{7}}
	var critCalled bool
	s := NewScheduler(r, l, &fakeRecorder{}, 30*24*time.Hour, func(int, int) { critCalled = true })

	s.RenewDue(context.Background()) // 失败 1
	s.RenewDue(context.Background()) // 失败 2
	if critCalled {
		t.Fatal("不应在 2 次失败时触发")
	}
	// 改为成功：清零 streak，且不再触发。
	r.failID = map[int]bool{}
	s.RenewDue(context.Background())
	if critCalled {
		t.Fatal("清零后不应触发 onCritical")
	}
}

// —— cert.Service 续期单测（sqlite 内存库 + fake issuer） ——

// fakeIssuer 返回预设证书（不接触真实 LE）。
type fakeIssuer struct {
	certPEM, keyPEM, chainPEM []byte
	calls                     int
}

func (f *fakeIssuer) Issue(_ context.Context, req acme.IssueRequest) (*acme.IssueResult, error) {
	f.calls++
	return &acme.IssueResult{
		Domain:   req.Domains[0],
		CertPEM:  f.certPEM,
		KeyPEM:   f.keyPEM,
		ChainPEM: f.chainPEM,
		Issuer:   "Test CA",
	}, nil
}

// genCertPEM 生成一张自签名证书（仅测试用），返回 certPEM/keyPEM/serial。
func genCertPEM(t *testing.T, notAfter time.Time, serial int64) (certPEM, keyPEM []byte, serialHex string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: "example.com"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		DNSNames:              []string{"example.com"},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	serialHex = hex.EncodeToString(tmpl.SerialNumber.Bytes())
	return
}

func TestService_ListDueForRenewal(t *testing.T) {
	ctx := context.Background()
	client, err := repo.Open("sqlite", "file:certduelist?mode=memory&cache=shared&_fk=1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer client.Close()
	kek := make([]byte, 32)
	rand.Read(kek)
	kms, _ := crypto.NewKMS(kek)
	svc := New(client, kms)

	_, _ = client.Node.Create().SetName("n1").Save(ctx)
	// 到期内的 ACME 证书 → 应被列出。
	due, _, _ := genCertPEM(t, time.Now().Add(10*24*time.Hour), 101)
	_, err = client.Certificate.Create().
		SetDomain("due.example.com").SetSan([]string{"due.example.com"}).
		SetIssuer("Test CA").SetSerialNumber("deadbeef01").
		SetFingerprintSha(hex.EncodeToString(sha256Sum(due))).
		SetNotBefore(time.Now().Add(-time.Hour)).SetNotAfter(time.Now().Add(10*24*time.Hour)).
		SetKeyAlg("RSA-2048").SetSource(entcert.SourceAcme).SetStatus(entcert.StatusValid).
		SetEncPrivateKey([]byte("x")).SetEncFullChain([]byte("y")).Save(ctx)
	if err != nil {
		t.Fatalf("seed due cert: %v", err)
	}
	// 远未到期的 ACME 证书 → 不应列出。
	far, _, _ := genCertPEM(t, time.Now().Add(300*24*time.Hour), 102)
	_, err = client.Certificate.Create().
		SetDomain("far.example.com").SetSan([]string{"far.example.com"}).
		SetIssuer("Test CA").SetSerialNumber("deadbeef02").
		SetFingerprintSha(hex.EncodeToString(sha256Sum(far))).
		SetNotBefore(time.Now().Add(-time.Hour)).SetNotAfter(time.Now().Add(300*24*time.Hour)).
		SetKeyAlg("RSA-2048").SetSource(entcert.SourceAcme).SetStatus(entcert.StatusValid).
		SetEncPrivateKey([]byte("x")).SetEncFullChain([]byte("y")).Save(ctx)
	if err != nil {
		t.Fatalf("seed far cert: %v", err)
	}
	// upload 来源 → 不应列出。
	_, err = client.Certificate.Create().
		SetDomain("up.example.com").SetSan([]string{"up.example.com"}).
		SetIssuer("Upload").SetSerialNumber("deadbeef03").
		SetFingerprintSha(hex.EncodeToString(sha256Sum(due))).
		SetNotBefore(time.Now().Add(-time.Hour)).SetNotAfter(time.Now().Add(10*24*time.Hour)).
		SetKeyAlg("RSA-2048").SetSource(entcert.SourceUpload).SetStatus(entcert.StatusValid).
		SetEncPrivateKey([]byte("x")).SetEncFullChain([]byte("y")).Save(ctx)
	if err != nil {
		t.Fatalf("seed upload cert: %v", err)
	}

	ids, err := svc.ListDueForRenewal(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("ListDueForRenewal: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("预期 1 张待续期，实际 %v", ids)
	}
}

func TestService_Renew(t *testing.T) {
	ctx := context.Background()
	client, err := repo.Open("sqlite", "file:certrenew?mode=memory&cache=shared&_fk=1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer client.Close()
	kek := make([]byte, 32)
	rand.Read(kek)
	kms, _ := crypto.NewKMS(kek)
	svc := New(client, kms)
	fd := &fakeDeployer{}
	svc.SetDeployer(fd)

	// 账户密钥（PKCS1 RSA PEM）+ provider token，KMS 加密后落库。
	acctKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	acctPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(acctKey)})
	encAcct, _ := kms.Encrypt(acctPEM)
	encToken, _ := kms.Encrypt([]byte("cf-token-xyz"))

	node, err := client.Node.Create().
		SetName("n1").SetAddress("10.0.0.1").SetRole("real_server").SetStatus("online").Save(ctx)
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}
	old, _, _ := genCertPEM(t, time.Now().Add(10*24*time.Hour), 201)
	c, err := client.Certificate.Create().
		SetDomain("r.example.com").SetSan([]string{"r.example.com"}).
		SetIssuer("Test CA").SetSerialNumber("old-serial").
		SetFingerprintSha(hex.EncodeToString(sha256Sum(old))).
		SetNotBefore(time.Now().Add(-time.Hour)).SetNotAfter(time.Now().Add(10*24*time.Hour)).
		SetKeyAlg("RSA-2048").SetSource(entcert.SourceAcme).SetStatus(entcert.StatusValid).
		SetEncPrivateKey([]byte("oldkey")).SetEncFullChain([]byte("oldchain")).
		SetEncAcmeAccountKey(encAcct).SetEncAcmeProviderToken(encToken).
		SetAcmeProviderType("cloudflare").SetAcmeEmail("a@e.com").SetAcmeKeyAlg("rsa2048").
		Save(ctx)
	if err != nil {
		t.Fatalf("seed cert: %v", err)
	}
	// 历史分发记录（续期后重分发到该节点）。
	_, _ = client.CertDeployment.Create().SetNodeID(node.ID).SetCertificateID(c.ID).SetStatus(entcertdep.StatusDeployed).Save(ctx)

	// fake issuer 返回一张新证书（不同序列号、更远到期）。
	newCertPEM, newKeyPEM, newSerial := genCertPEM(t, time.Now().Add(90*24*time.Hour), 202)
	fi := &fakeIssuer{certPEM: newCertPEM, keyPEM: newKeyPEM, chainPEM: nil}
	svc.SetIssuer(fi)

	if err := svc.Renew(ctx, c.ID); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if fi.calls != 1 {
		t.Fatalf("预期 issuer 被调用 1 次，实际 %d", fi.calls)
	}
	// 库内证书已更新为新序列号与更远到期。
	got, _ := client.Certificate.Get(ctx, c.ID)
	if got.SerialNumber != newSerial {
		t.Fatalf("序列号未更新：%s", got.SerialNumber)
	}
	if !got.NotAfter.After(time.Now().Add(80 * 24 * time.Hour)) {
		t.Fatalf("到期时间未刷新：%v", got.NotAfter)
	}
	// 重分发被触发（fakeDeployer 捕获）。
	if fd.lastTask == nil {
		t.Fatal("续期后未触发重分发")
	}
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}
