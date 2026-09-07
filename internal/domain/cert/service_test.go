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

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	entcert "github.com/th/ngxcp/ent/certificate"
	entcertdep "github.com/th/ngxcp/ent/certdeployment"
	entnode "github.com/th/ngxcp/ent/node"
	"github.com/th/ngxcp/internal/crypto"
	"github.com/th/ngxcp/internal/repo"
)

// fakeDeployer 是 cert.Deployer 的测试替身：捕获下发任务并可控返回成功/失败。
type fakeDeployer struct {
	lastTask *agentv1.DeployCertTask
	fail     bool
	failMsg  string
}

func (f *fakeDeployer) DeployCert(ctx context.Context, nodeID int, task *agentv1.DeployCertTask) (*agentv1.DeployCertResult, error) {
	f.lastTask = task
	if f.fail {
		return &agentv1.DeployCertResult{TaskId: task.GetTaskId(), Ok: false, Error: f.failMsg}, nil
	}
	return &agentv1.DeployCertResult{TaskId: task.GetTaskId(), Ok: true, DeployedAt: time.Now().Unix()}, nil
}

func TestService_Distribute(t *testing.T) {
	ctx := context.Background()
	client, err := repo.Open("sqlite", "file:certdist?mode=memory&cache=shared&_fk=1")
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
	v, err := svc.Upload(ctx, UploadRequest{CertPEM: certPEM, KeyPEM: keyPEM})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	node, err := client.Node.Create().
		SetName("rs-01").SetAddress("192.168.5.8").
		SetRole(entnode.RoleRealServer).SetStatus(entnode.StatusOnline).
		Save(ctx)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	fd := &fakeDeployer{}
	res, err := svc.Distribute(ctx, v.ID, DistributeRequest{
		NodeIDs: []int{node.ID},
		SSLDir:  "/etc/nginx/ssl",
	}, fd)
	if err != nil {
		t.Fatalf("distribute: %v", err)
	}
	if res.Total != 1 || res.Deployed != 1 || res.Failed != 0 {
		t.Fatalf("分发结果期望 1 成功 0 失败，实际 total=%d deployed=%d failed=%d", res.Total, res.Deployed, res.Failed)
	}

	// 下发的私钥/证书必须与原始一致（经 KMS 解密回放）。
	if fd.lastTask == nil {
		t.Fatal("deployer 未收到下发任务")
	}
	if fd.lastTask.GetKeyPem() != string(keyPEM) {
		t.Fatal("下发的私钥与原始不符（解密回放错误）")
	}
	if fd.lastTask.GetCertPem() != string(certPEM) {
		t.Fatal("下发的证书与原始不符")
	}
	if fd.lastTask.GetDomain() != "test.example.com" {
		t.Fatalf("domain 不符：%s", fd.lastTask.GetDomain())
	}

	// cert_deployments 应写入 deployed 记录。
	dep, err := client.CertDeployment.Query().
		Where(entcertdep.HasCertificateWith(entcert.ID(v.ID)), entcertdep.NodeID(node.ID)).
		Only(ctx)
	if err != nil {
		t.Fatalf("查询分发记录：%v", err)
	}
	if dep.Status != entcertdep.StatusDeployed {
		t.Fatalf("分发记录状态期望 deployed，实际 %s", dep.Status)
	}
	if dep.Error != "" {
		t.Fatalf("成功分发不应有 error：%s", dep.Error)
	}

	// 失败路径：另一个节点，deployer 返回失败。
	node2, err := client.Node.Create().
		SetName("rs-02").SetAddress("192.168.5.9").
		SetRole(entnode.RoleRealServer).SetStatus(entnode.StatusOnline).
		Save(ctx)
	if err != nil {
		t.Fatalf("create node2: %v", err)
	}
	fd2 := &fakeDeployer{fail: true, failMsg: "nginx -t 失败"}
	if _, err := svc.Distribute(ctx, v.ID, DistributeRequest{NodeIDs: []int{node2.ID}}, fd2); err != nil {
		t.Fatalf("distribute fail path: %v", err)
	}
	dep2, err := client.CertDeployment.Query().
		Where(entcertdep.HasCertificateWith(entcert.ID(v.ID)), entcertdep.NodeID(node2.ID)).
		Only(ctx)
	if err != nil {
		t.Fatalf("查询失败分发记录：%v", err)
	}
	if dep2.Status != entcertdep.StatusFailed {
		t.Fatalf("失败路径状态期望 failed，实际 %s", dep2.Status)
	}
	if dep2.Error != "nginx -t 失败" {
		t.Fatalf("失败原因不符：%s", dep2.Error)
	}
}

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
