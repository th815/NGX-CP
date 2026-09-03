#!/usr/bin/env bash
# 生成 gRPC Go 代码：由 proto/agent/v1/agent.proto 生成到 gen/agent/v1。
# 依赖（一次性安装）：
#   go install github.com/bufbuild/buf/cmd/buf@latest
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$PATH:$(go env GOPATH)/bin"

for b in buf protoc-gen-go protoc-gen-go-grpc; do
  command -v "$b" >/dev/null 2>&1 || { echo "ERROR: $b 未安装，见脚本顶部说明"; exit 1; }
done

buf generate proto
echo "✔ 生成完成 -> gen/agent/v1"
