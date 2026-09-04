// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package crypto 信封加密单元测试。
package crypto

import (
	"bytes"
	"testing"
)

// testKey 返回确定性的 32 字节测试主密钥（仅测试用，非生产密钥）。
func testKey() []byte {
	k := make([]byte, keyLen)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestKMS_RoundTrip(t *testing.T) {
	kms, err := NewKMS(testKey())
	if err != nil {
		t.Fatalf("NewKMS: %v", err)
	}
	samples := [][]byte{
		[]byte("-----BEGIN PRIVATE KEY-----\nMIIE...\n-----END PRIVATE KEY-----"),
		{},
		bytes.Repeat([]byte("x"), 4096),
	}
	for _, s := range samples {
		b, err := kms.Encrypt(s)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		out, err := kms.Decrypt(b)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if !bytes.Equal(out, s) {
			t.Fatalf("round-trip mismatch: len %d vs %d", len(out), len(s))
		}
	}
}

func TestKMS_WrongKeyFails(t *testing.T) {
	a, _ := NewKMS(testKey())
	wrong := make([]byte, keyLen)
	wrong[0] = 0xff
	b, err := a.Encrypt([]byte("data"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	bad, err := NewKMS(wrong)
	if err != nil {
		t.Fatalf("NewKMS(wrong): %v", err)
	}
	if _, err := bad.Decrypt(b); err == nil {
		t.Fatal("期望用错误主密钥解密失败，但成功")
	}
}

func TestKMS_NonceUniqueness(t *testing.T) {
	kms, _ := NewKMS(testKey())
	b1, _ := kms.Encrypt([]byte("same"))
	b2, _ := kms.Encrypt([]byte("same"))
	if bytes.Equal(b1, b2) {
		t.Fatal("相同明文两次加密应得到不同密文（随机 DEK/nonce 未生效）")
	}
}

func TestNewKMS_RejectsBadLen(t *testing.T) {
	if _, err := NewKMS([]byte("short")); err == nil {
		t.Fatal("期望拒绝错误长度的主密钥")
	}
	if _, err := NewKMS(make([]byte, 16)); err == nil {
		t.Fatal("期望拒绝 16 字节主密钥")
	}
}

func TestNewKMSFromEnv_Success(t *testing.T) {
	hexKey := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	t.Setenv(masterKeyEnv, hexKey)
	kms, err := NewKMSFromEnv()
	if err != nil {
		t.Fatalf("NewKMSFromEnv: %v", err)
	}
	b, err := kms.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if out, err := kms.Decrypt(b); err != nil || !bytes.Equal(out, []byte("secret")) {
		t.Fatalf("round-trip failed: %v", err)
	}
}
