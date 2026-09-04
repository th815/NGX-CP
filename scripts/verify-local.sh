#!/usr/bin/env bash
# verify-local.sh —— 在本地 docker 沙箱跑通「生产部署」整条链路（控制面自举 CA →
# 建节点 → 签发 Join Token → 取 CA → Agent 注册 → 节点上线），证明部署包可用，
# 再推真实裸金属（TH-D2110）。不依赖任何外部主机 / 凭据，纯本地。
#
# 前置：docker 可用；bin/ngxcp-server、bin/ngxcp-agent 已由 `make build` 生成。
# 用法：
#   bash scripts/verify-local.sh            # 跑完自动清理容器
#   bash scripts/verify-local.sh --keep     # 保留容器，便于人工 inspect
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

NET="ngxcp-verify"
CP_IMG="ngxcp-server:local"
AG_IMG="ngxcp-agent:local"
CP_NAME="ngxcp-cp"
AG_NAME="ngxcp-agent1"
ADMIN_TOKEN="local-verify-secret"
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

cleanup() {
  if [ "$KEEP" = "1" ]; then
    echo "（--keep）保留容器 $CP_NAME / $AG_NAME，可 docker logs 查看；清理运行：bash $0"
    return
  fi
  echo "== 清理 =="
  docker rm -f "$CP_NAME" "$AG_NAME" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# 0) 二进制就绪？
for b in bin/ngxcp-server bin/ngxcp-agent; do
  [ -x "$b" ] || { echo "缺少 $b，先运行 make build"; exit 1; }
done

# 1) 构建镜像
echo "== 构建镜像 =="
docker build -f Dockerfile.server -t "$CP_IMG" . >/dev/null
docker build -f Dockerfile.agent  -t "$AG_IMG" . >/dev/null

# 2) 网络
docker network create "$NET" >/dev/null 2>&1 || true

# 3) 起控制面（自举 CA + sqlite 自动建表）
echo "== 起控制面 =="
docker run -d --name "$CP_NAME" --network "$NET" --network-alias ngxcp-server \
  -v "$REPO_ROOT/configs/server.local.yaml:/etc/ngxcp/config.yaml:ro" \
  -v ngxcp-cp-data:/var/lib/ngxcp \
  -p 8080:8080 -p 9443:9443 \
  "$CP_IMG" --config /etc/ngxcp/config.yaml >/dev/null

# 4) 等健康检查
echo -n "等待控制面 /health"
for i in $(seq 1 30); do
  if curl -fsS http://127.0.0.1:8080/health >/dev/null 2>&1; then echo " OK"; break; fi
  sleep 1
  [ "$i" = "30" ] && { echo " 超时"; docker logs "$CP_NAME" --tail 20; exit 1; }
done

# 5) 建节点 + 签发 Join Token（复刻 Web 一键自注册流程）
echo "== 建节点 + 签 Join Token =="
RESP=$(curl -fsS -X POST http://127.0.0.1:8080/api/v1/nodes \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"rs1","role":"real_server"}')
NID=$(printf '%s' "$RESP" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["id"])')
echo "  节点 id=$NID"

TOKRESP=$(curl -fsS -X POST "http://127.0.0.1:8080/api/v1/nodes/$NID/join-token?ttl=24h" \
  -H "Authorization: Bearer $ADMIN_TOKEN")
JOIN=$(printf '%s' "$TOKRESP" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["token"])')
echo "  取得 Join Token（前缀 ${JOIN:0:8}…）"

# 6) 取 CA 证书（Agent 引导信任用，对应生产 deploy-agent.sh 的 ca.crt 下发）
curl -fsS http://127.0.0.1:8080/agent/ca.crt -o /tmp/ngxcp-ca.crt
echo "  取得 ca.crt（$(wc -c < /tmp/ngxcp-ca.crt) 字节）"

# 7) 起 Agent（等价于 deploy-agent.sh 落地的 ngxcp-agent.service）
echo "== 起 Agent =="
docker run -d --name "$AG_NAME" --network "$NET" \
  -v /tmp/ngxcp-ca.crt:/etc/ngxcp/ca.crt:ro \
  -v ngxcp-ag1-data:/var/lib/ngxcp \
  "$AG_IMG" \
  -control-plane ngxcp-server:9443 -server-name ngxcp-server \
  -ca-cert /etc/ngxcp/ca.crt -join-token "$JOIN" -hostname rs1 >/dev/null

# 8) 轮询节点上线（等价于部署后 `curl /api/v1/nodes` 确认上线）
echo -n "等待节点上线"
ST=""
for i in $(seq 1 30); do
  ST=$(curl -fsS "http://127.0.0.1:8080/api/v1/nodes/$NID" \
       | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["status"])' 2>/dev/null || true)
  [ "$ST" = "online" ] && { echo " 节点=$NID status=online"; break; }
  sleep 1
  [ "$i" = "30" ] && echo " 超时（当前 status=$ST）"
done

if [ "$ST" = "online" ]; then
  echo
  echo "✅ VERIFY PASS —— 部署包端到端可用：控制面自举 CA / Join Token 自注册 / Agent mTLS 上线 全部通过。"
  echo "   生产推送前可执行：bash scripts/deploy.sh 与 bash scripts/deploy-agent.sh"
else
  echo
  echo "❌ VERIFY FAIL —— 节点未上线（status=$ST）。诊断："
  docker logs "$AG_NAME" --tail 20 2>/dev/null
  exit 1
fi
