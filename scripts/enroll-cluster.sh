#!/usr/bin/env bash
# enroll-cluster.sh —— 一次性纳管一组节点：建节点 + 签发 enroll token + 批量部署 Agent。
#
# 封装「控制面建节点」与「deploy-agent.sh 批量部署」两段，使 2+2 架构一条命令上线。
# 该脚本只做编排（API 调用 + 喂给已验证的 deploy-agent.sh），不重复造轮子。
#
# 前置：
#   - 本机能 SSH 免密到控制面与各节点（BatchMode，不支持密码）
#   - 控制面已部署（deploy.sh 跑过），并取得管理员令牌
#   - 本机有 go + node（deploy-agent.sh 会现场构建 agent 二进制）
#
# 用法：
#   NGXCP_ADMIN_TOKEN=<token> bash scripts/enroll-cluster.sh
#   节点清单 / 控制面地址见脚本顶部「可配置区」，按需修改后重跑即可。
set -euo pipefail

# ── 可配置区 ──────────────────────────────────────────────
CONTROL_PLANE="192.168.5.50"        # 控制面虚机 IP
API_PORT="8080"
GRPC_ADDR="${CONTROL_PLANE}:9443"   # Agent gRPC 接入地址
# 节点清单：每行 "ip 角色"，角色 ∈ {director, real_server, director_and_rs}
NODES=(
  "192.168.5.6 director"
  "192.168.5.7 director"
  "192.168.5.8 real_server"
  "192.168.5.9 real_server"
)
TOKEN_TTL="24h"
# ─────────────────────────────────────────────────────────

# 管理员令牌：优先环境变量；否则自动从控制面读取（用户无需手敲）。
# 无论哪种来源都做去引号/去空白清理，避免 config 里 YAML 引号污染 Bearer 值导致 401。
if [ -n "${NGXCP_ADMIN_TOKEN:-}" ]; then
  ADMIN_TOKEN="${NGXCP_ADMIN_TOKEN}"
else
  echo "未提供 NGXCP_ADMIN_TOKEN，自动从控制面读取..."
  ADMIN_TOKEN="$(ssh -o BatchMode=yes -o StrictHostKeyChecking=no "root@${CONTROL_PLANE}" \
    'grep "^auth_admin_token:" /opt/ngxcp/config.yaml' 2>/dev/null | sed -E 's/.*:[[:space:]]*"?([^"]*)"?.*/\1/' || true)"
  if [ -z "$ADMIN_TOKEN" ]; then
    echo "自动读取失败，请手动提供令牌重试：" >&2
    echo "  NGXCP_ADMIN_TOKEN=<token> bash $0" >&2
    echo "  （token 取自控制面： ssh root@${CONTROL_PLANE} 'grep auth_admin_token /opt/ngxcp/config.yaml' ）" >&2
    exit 1
  fi
  echo "已从控制面读取管理员令牌。"
fi
ADMIN_TOKEN="$(printf '%s' "$ADMIN_TOKEN" | tr -d '"' | tr -d "'" | xargs)"
[ -z "$ADMIN_TOKEN" ] && { echo "错误：管理员令牌为空，无法继续。" >&2; exit 1; }
BASE="http://${CONTROL_PLANE}:${API_PORT}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PKI_DIR="$(pwd)/pki"; mkdir -p "$PKI_DIR"
CA_CERT="$PKI_DIR/ca.crt"
TOKENS_FILE="$(pwd)/.agent-tokens"

echo "控制面: $BASE"

# 1) 拉取引导 CA（Agent 启动校验服务端证书用）
echo "[1/3] 拉取引导 CA -> $CA_CERT"
curl -fsS "$BASE/agent/ca.crt" -o "$CA_CERT"

# 2) 逐台：建节点（name=ip，便于追溯）→ 签发一次性 enroll token → 落令牌文件
> "$TOKENS_FILE"
for entry in "${NODES[@]}"; do
  ip="${entry% *}"; role="${entry#* }"
  echo "[2/3] 建节点 $ip (role=$role)"
  body="{\"name\":\"$ip\",\"role\":\"$role\"}"
  resp="$(curl -fsS -X POST "$BASE/api/v1/nodes" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H "Content-Type: application/json" -d "$body")"
  nid="$(printf '%s' "$resp" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["id"])')"
  echo "      节点 id=$nid"
  tresp="$(curl -fsS -X POST "$BASE/api/v1/nodes/$nid/enroll-token?ttl=$TOKEN_TTL" \
    -H "Authorization: Bearer $ADMIN_TOKEN")"
  token="$(printf '%s' "$tresp" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["token"])')"
  printf 'root@%s=%s\n' "$ip" "$token" >> "$TOKENS_FILE"
  echo "      enroll token 已签发（一次性，注册成功后即失效）"
done
echo "      令牌文件 -> $TOKENS_FILE"

# 3) 交给 deploy-agent.sh 批量部署（自构建 + scp + 回滚安全）
echo "[3/3] 调用 deploy-agent.sh 纳管 ${#NODES[@]} 台..."
hosts="$(printf 'root@%s ' "${NODES[@]%% *}")"
NGXCP_AGENT_HOSTS="$hosts" \
NGXCP_AGENT_CONTROL_PLANE="$GRPC_ADDR" \
NGXCP_AGENT_CA_CERT="$CA_CERT" \
NGXCP_AGENT_TOKENS_FILE="$TOKENS_FILE" \
  bash "$REPO_ROOT/scripts/deploy-agent.sh"

echo
echo "纳管完成。校验节点上线："
echo "  curl -s $BASE/api/v1/nodes -H 'Authorization: Bearer $ADMIN_TOKEN'"
