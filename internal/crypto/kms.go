// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package crypto 提供证书私钥等敏感材料的信封加密（envelope encryption）。
//
// 方案（v1，对齐 DECISIONS §5 与 AGENTS §9.2 安全红线）：
//  1. 随机生成数据密钥 DEK（AES-256）；
//  2. 用 DEK 以 AES-256-GCM 加密明文；
//  3. 用主密钥 KEK 以 AES-256-GCM 加密 DEK；
//  4. 三者（kekNonce ‖ wrappedDEK ‖ dataNonce ‖ ct）一并存储为单个 blob。
//
// 每条记录使用独立随机 nonce，绝不复用。主密钥从环境变量 NGXCP_MASTER_KEY
// （hex 编码）或文件 /etc/ngxcp/master.key（hex 编码，0600）读取，绝不硬编码。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/th/ngxcp/internal/pkg/apperr"
)

const (
	keyLen        = 32 // AES-256 主密钥 / 数据密钥长度
	nonceLen      = 12 // GCM 标准 nonce 长度
	gcmTagSize    = 16 // GCM 认证标签长度
	masterKeyEnv  = "NGXCP_MASTER_KEY"
	masterKeyFile = "/etc/ngxcp/master.key"
)

var errKeyLen = errors.New("crypto: 主密钥长度必须为 32 字节（AES-256）")

// KMS 信封加密器，持有主密钥 KEK（内存副本，使用后应尽早释放）。
type KMS struct {
	kek []byte
}

// NewKMS 用给定主密钥构造 KMS；主密钥必须 32 字节。
func NewKMS(kek []byte) (*KMS, error) {
	if len(kek) != keyLen {
		return nil, errKeyLen
	}
	k := make([]byte, keyLen)
	copy(k, kek)
	return &KMS{kek: k}, nil
}

// NewKMSFromEnv 从环境变量或密钥文件加载主密钥（hex 编码）。两者皆缺则返回错误。
func NewKMSFromEnv() (*KMS, error) {
	raw := strings.TrimSpace(os.Getenv(masterKeyEnv))
	src := "env:" + masterKeyEnv
	if raw == "" {
		b, err := os.ReadFile(masterKeyFile)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalid,
				"未找到主密钥：请设置 "+masterKeyEnv+" 或写入 "+masterKeyFile, err)
		}
		raw = strings.TrimSpace(string(b))
		src = "file:" + masterKeyFile
	}
	kek, err := hex.DecodeString(raw)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalid, "主密钥 hex 解码失败 ("+src+")", err)
	}
	if len(kek) != keyLen {
		return nil, apperr.Wrap(apperr.CodeInvalid,
			"主密钥长度必须为 32 字节 ("+src+")", errKeyLen)
	}
	return NewKMS(kek)
}

// Encrypt 信封加密，返回 blob = kekNonce ‖ wrappedDEK ‖ dataNonce ‖ ciphertext。
func (k *KMS) Encrypt(plaintext []byte) ([]byte, error) {
	dek := make([]byte, keyLen)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "生成数据密钥失败", err)
	}
	defer zeroBytes(dek)

	dataNonce, ct, err := gcmSeal(dek, plaintext)
	if err != nil {
		return nil, err
	}
	kekNonce, wrappedDEK, err := gcmSeal(k.kek, dek)
	if err != nil {
		return nil, err
	}
	blob := make([]byte, 0, nonceLen+len(wrappedDEK)+nonceLen+len(ct))
	blob = append(blob, kekNonce...)
	blob = append(blob, wrappedDEK...)
	blob = append(blob, dataNonce...)
	blob = append(blob, ct...)
	return blob, nil
}

// Decrypt 解出信封，返回原始明文；主密钥不匹配时返回未授权错误。
func (k *KMS) Decrypt(blob []byte) ([]byte, error) {
	if len(blob) < 2*nonceLen+keyLen+gcmTagSize {
		return nil, apperr.New(apperr.CodeInvalid, "crypto: 密文长度非法")
	}
	kekNonce := blob[:nonceLen]
	rest := blob[nonceLen:]
	wrappedLen := keyLen + gcmTagSize
	if len(rest) < wrappedLen+nonceLen {
		return nil, apperr.New(apperr.CodeInvalid, "crypto: 密文长度非法")
	}
	wrappedDEK := rest[:wrappedLen]
	dataNonce := rest[wrappedLen : wrappedLen+nonceLen]
	ct := rest[wrappedLen+nonceLen:]

	dek, err := gcmOpen(k.kek, kekNonce, wrappedDEK)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeUnauthorized, "主密钥解密数据密钥失败（密钥不匹配？）", err)
	}
	defer zeroBytes(dek)
	return gcmOpen(dek, dataNonce, ct)
}

// gcmSeal 用 key 以 AES-256-GCM 加密，返回随机 nonce 与密文。
func gcmSeal(key, plaintext []byte) (nonce, ciphertext []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInternal, "aes.NewCipher", err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInternal, "cipher.NewGCM", err)
	}
	nonce = make([]byte, g.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInternal, "生成 nonce 失败", err)
	}
	return nonce, g.Seal(nil, nonce, plaintext, nil), nil
}

// gcmOpen 用 key + nonce 解密 GCM 密文。
func gcmOpen(key, nonce, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "aes.NewCipher", err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "cipher.NewGCM", err)
	}
	pt, err := g.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeUnauthorized, "GCM 解密失败", err)
	}
	return pt, nil
}

// zeroBytes 原地清空敏感字节（密钥退出作用域前调用）。
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
