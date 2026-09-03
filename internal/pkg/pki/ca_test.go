package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeCSR 生成给定主机名的 CSR（模拟 Agent 本地生成密钥 + 提交 CSR）。
func makeCSR(t *testing.T, hostname string) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: hostname},
		DNSNames: []string{hostname},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), key
}

func mustKeyPEM(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	b, err := EncodePrivateKey(key)
	if err != nil {
		t.Fatalf("encode key: %v", err)
	}
	return b
}

func TestLoadOrCreateCA(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	if ca.Cert.Subject.CommonName != caCommonName {
		t.Errorf("CA CN = %q", ca.Cert.Subject.CommonName)
	}
	// 重新加载应复用同一 CA
	ca2, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("reload CA: %v", err)
	}
	if ca2.Cert.SerialNumber.Cmp(ca.Cert.SerialNumber) != 0 {
		t.Error("reload 后 CA 序列号不一致（应复用已有 CA）")
	}
	// 权限：key 必须 0600，crt 0644
	checkPerm(t, filepath.Join(dir, caKeyFilename), 0o600)
	checkPerm(t, filepath.Join(dir, caCertFilename), 0o644)
	checkPerm(t, filepath.Join(dir, serverKeyFilename), 0o600)
	checkPerm(t, filepath.Join(dir, serverCertFilename), 0o644)
}

func checkPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm() != want {
		t.Errorf("%s perm = %v, want %v", path, info.Mode().Perm(), want)
	}
}

// TestMutualTLSHandshake 验证：签发 → Agent 持客户端证书 → 双向握手成功，且服务端从证书 Serial 反查到 nodeID。
func TestMutualTLSHandshake(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("CA: %v", err)
	}

	csr, key := makeCSR(t, "rs-nginx-01")
	const nodeID = 42
	agentCert, agentPEM, err := ca.IssueAgentCert(csr, nodeID, "rs-nginx-01", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if agentCert.SerialNumber.Int64() != nodeID {
		t.Fatalf("Serial = %d, want %d", agentCert.SerialNumber.Int64(), nodeID)
	}
	if len(agentCert.DNSNames) == 0 || agentCert.DNSNames[0] != "rs-nginx-01" {
		t.Errorf("DNS SAN 缺失: %v", agentCert.DNSNames)
	}

	observed := make(chan int, 1)
	ca.NodeValidator = func(id int) error { observed <- id; return nil }

	srvCfg, err := ca.ServerTLSConfig()
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4)
		conn.Read(buf)
		conn.Write([]byte("pong"))
	}()

	cliCfg, err := ClientTLSConfig(ca.CACertPEM(), agentPEM, mustKeyPEM(t, key), "ngxcp-server")
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}
	conn, err := tls.Dial("tcp", ln.Addr().String(), cliCfg)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "pong" {
		t.Errorf("echo = %q, want pong", buf)
	}

	select {
	case id := <-observed:
		if id != nodeID {
			t.Errorf("服务端反查 nodeID = %d, want %d", id, nodeID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("服务端未观察到 nodeID（VerifyPeerCertificate 未触发）")
	}
}

// TestWrongCARejected 验证：用另一个 CA 签发的客户端证书 → 服务端握手必须失败。
//
// 注意（TLS 1.3 行为）：服务端在收到客户端证书、执行 VerifyPeerCertificate 之前，
// 已将 Finished 发回，因此客户端 tls.Dial 会「抢先」完成握手。正确的拒绝判据是
// 服务端握手是否失败，而非客户端 Dial 是否返回错误。
func TestWrongCARejected(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("CA: %v", err)
	}

	rogueDir := t.TempDir()
	rogue, err := LoadOrCreateCA(rogueDir)
	if err != nil {
		t.Fatalf("rogue CA: %v", err)
	}
	csr, key := makeCSR(t, "rogue")
	_, roguePEM, err := rogue.IssueAgentCert(csr, 7, "rogue", time.Hour)
	if err != nil {
		t.Fatalf("rogue issue: %v", err)
	}

	cliCfg, err := ClientTLSConfig(ca.CACertPEM(), roguePEM, mustKeyPEM(t, key), "ngxcp-server")
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}
	srvCfg, _ := ca.ServerTLSConfig()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer conn.Close()
		tc, ok := conn.(*tls.Conn)
		if !ok {
			srvErr <- fmt.Errorf("非 tls.Conn")
			return
		}
		srvErr <- tc.Handshake() // 服务端在 VerifyPeerCertificate 中拒绝 rogue 证书
	}()

	// 客户端侧可能抢先成功（TLS 1.3），忽略其返回值，以服务端结论为准。
	_, _ = tls.Dial("tcp", ln.Addr().String(), cliCfg)

	select {
	case err := <-srvErr:
		if err == nil {
			t.Fatal("期望服务端拒绝 rogue CA 签发的证书，但服务端握手成功")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("服务端未在预期时间内给出握手结论（可能挂起）")
	}
}

// signExpiredCert 用 CA 密钥显式签发一张已过期的客户端证书（NotAfter 在过去），
// 用于验证「过期证书被拒」。注意：不能走 IssueAgentCert 的 ttl 参数（负数会被钳制为默认值）。
func signExpiredCert(t *testing.T, ca *CA, csrPEM []byte, nodeID int, hostname string) []byte {
	t.Helper()
	csr, err := parseCSR(csrPEM)
	if err != nil {
		t.Fatalf("parse csr: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(int64(nodeID)),
		Subject:               pkix.Name{CommonName: hostname},
		DNSNames:              []string{hostname},
		NotBefore:             time.Now().Add(-2 * time.Hour),
		NotAfter:              time.Now().Add(-1 * time.Hour), // 已过期
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		t.Fatalf("sign expired: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestExpiredCertRejected 验证：过期客户端证书 → 服务端握手必须失败。
func TestExpiredCertRejected(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("CA: %v", err)
	}
	csr, key := makeCSR(t, "expired-host")
	expPEM := signExpiredCert(t, ca, csr, 9, "expired-host")

	cliCfg, err := ClientTLSConfig(ca.CACertPEM(), expPEM, mustKeyPEM(t, key), "ngxcp-server")
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}
	srvCfg, _ := ca.ServerTLSConfig()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer conn.Close()
		tc, ok := conn.(*tls.Conn)
		if !ok {
			srvErr <- fmt.Errorf("非 tls.Conn")
			return
		}
		srvErr <- tc.Handshake() // 服务端在 VerifyPeerCertificate 中拒绝过期证书
	}()

	_, _ = tls.Dial("tcp", ln.Addr().String(), cliCfg)

	select {
	case err := <-srvErr:
		if err == nil {
			t.Fatal("期望服务端拒绝过期证书，但服务端握手成功")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("服务端未在预期时间内给出握手结论（可能挂起）")
	}
}

// TestRenewAgentCert 验证续签保留 Serial=nodeID 与主机名。
func TestRenewAgentCert(t *testing.T) {
	dir := t.TempDir()
	ca, _ := LoadOrCreateCA(dir)
	csr, _ := makeCSR(t, "renew-host")
	old, _, err := ca.IssueAgentCert(csr, 11, "renew-host", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	renewed, _, err := ca.RenewAgentCert(old)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.SerialNumber.Cmp(big.NewInt(11)) != 0 {
		t.Errorf("续签后 Serial = %v, want 11", renewed.SerialNumber)
	}
	if renewed.Subject.CommonName != "renew-host" {
		t.Errorf("续签后 CN = %q", renewed.Subject.CommonName)
	}
	if !renewed.NotAfter.After(old.NotAfter) {
		t.Error("续签后有效期未延长")
	}
}
