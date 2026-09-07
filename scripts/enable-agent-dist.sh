#!/usr/bin/env bash
# enable-agent-dist.sh —— 修复「节点自注册下载 Agent 二进制 404」。
#
# 症状：节点执行一键安装命令卡在 [2/5] 下载 Agent 二进制 ... curl (22) 404。
# 根因：控制面 agent_dist_dir 未配置 / dist 目录无二进制（deploy.sh 早期版本未启用分发）。
#
# 本脚本做三件事（幂等，可重复执行）：
#   1) 本地交叉编译 Agent 二进制到 dist/agent/（amd64 + arm64）
#   2) 上传到控制面 /opt/ngxcp/dist/agent/
#   3) 幂等补齐 config.yaml 的 agent_dist_dir，重启控制面并验证下载可用
#
# 用途：存量控制面快速补救，无需重跑全量 deploy.sh（新部署已由 deploy.sh 内置同样逻辑）。
#
# 用法： NGXCP_DEPLOY_HOST=root@your-host bash scripts/enable-agent-dist.sh
set -euo pipefail

HOST="${NGXCP_DEPLOY_HOST:?请设置环境变量 NGXCP_DEPLOY_HOST（控制面主机，如 root@192.168.5.50）}"
REMOTE_DIR="/opt/ngxcp"
DIST_SUBDIR="dist/agent"
SSH_OPTS="-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

command -v go >/dev/null 2>&1 || export PATH="$HOME/.workbuddy/binaries/go/current/bin:$PATH"
cd "$REPO_ROOT"

echo "[1/5] 交叉编译 Agent 分发产物 ..."
mkdir -p "$DIST_SUBDIR"
V=$(git describe --tags --always 2>/dev/null || echo dev)
C=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
BT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS="-X github.com/th/ngxcp/internal/pkg/version.Version=$V -X github.com/th/ngxcp/internal/pkg/version.Commit=$C -X github.com/th/ngxcp/internal/pkg/version.BuildTime=$BT"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$LDFLAGS" -o "$DIST_SUBDIR/ngxcp-agent-linux-amd64" ./cmd/ngxcp-agent
CGO_ENABLED=0 GOOS=linux GOARCH=arm64  go build -ldflags "$LDFLAGS" -o "$DIST_SUBDIR/ngxcp-agent-linux-arm64" ./cmd/ngxcp-agent
echo "  dist/agent/ngxcp-agent-linux-{amd64,arm64} 就绪"

echo "[2/5] 上传分发产物 ..."
ssh $SSH_OPTS "$HOST" "mkdir -p $REMOTE_DIR/$DIST_SUBDIR"
scp $SSH_OPTS "$DIST_SUBDIR/ngxcp-agent-linux-amd64" "$DIST_SUBDIR/ngxcp-agent-linux-arm64" \
  "$HOST:$REMOTE_DIR/$DIST_SUBDIR/"
ssh $SSH_OPTS "$HOST" "chmod 0755 $REMOTE_DIR/$DIST_SUBDIR/ngxcp-agent-linux-*"

echo "[3/5] 幂等补齐 agent_dist_dir 配置 ..."
ssh $SSH_OPTS "$HOST" 'bash -s' <<'REMOTE'
set -e
if ! grep -q '^agent_dist_dir:' /opt/ngxcp/config.yaml; then
  echo 'agent_dist_dir: "/opt/ngxcp/dist/agent"' >> /opt/ngxcp/config.yaml
  echo "  已追加 agent_dist_dir"
else
  echo "  已有配置：$(grep '^agent_dist_dir:' /opt/ngxcp/config.yaml)"
fi
REMOTE

echo "[4/5] 重启控制面使配置生效 ..."
ssh $SSH_OPTS "$HOST" 'systemctl restart ngxcp-server; sleep 2; systemctl is-active --quiet ngxcp-server && echo "  ngxcp-server 已重启"'

echo "[5/5] 验证二进制可下载 ..."
ssh $SSH_OPTS "$HOST" 'curl -sS -o /dev/null -w "  /agent/bin/ngxcp-agent-linux-amd64 -> HTTP %{http_code}\n" http://127.0.0.1:8080/agent/bin/ngxcp-agent-linux-amd64'
echo
echo "完成。现在到目标节点重新执行一键安装命令即可（[2/5] 下载应返回 200）。"
