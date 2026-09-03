#!/usr/bin/env bash
# 生成控制面 PKI：自签 CA（10 年）+ 服务端证书（ServerAuth）。
# 用法：./scripts/init-pki.sh [OUTPUT_DIR]   （默认 ./pki）
# 依赖：go（用 cmd/ngxcp-pki 生成，逻辑见 internal/pkg/pki）
set -euo pipefail
OUT="${1:-pki}"
cd "$(dirname "$0")/.."
echo "初始化控制面 PKI -> $OUT"
go run ./cmd/ngxcp-pki init --out "$OUT"
echo
echo "== 生成的文件与权限 =="
ls -la "$OUT"
echo
echo "⚠️  ca.key / server.key 权限为 0600，必须纳入异地备份："
echo "    丢失 CA 私钥 = 所有 Agent 客户端证书失效，需全部重新注册。"
