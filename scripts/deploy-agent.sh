#!/usr/bin/env bash
# deploy-agent.sh —— 把 ngxcp-agent 部署到一批 Nginx / Keepalived 节点（rollback-safe）。
#
# 目标节点与接入凭据完全由本地环境变量 / 令牌文件提供，脚本不内置任何主机或 IP，可安全入库。
#
# 必填环境变量：
#   NGXCP_AGENT_HOSTS          空格分隔的 SSH 目标，如 "root@rs1 root@rs2"
#   NGXCP_AGENT_CONTROL_PLANE  控制面 gRPC 地址，如 "cp.internal:9443"
#   NGXCP_AGENT_CA_CERT        本地 CA 证书路径（引导期信任，会传到 /etc/ngxcp/ca.crt）
#   NGXCP_AGENT_TOKENS_FILE    每行一条 "<ssh目标>=<一次性接入令牌>"（含机密，不入库）
# 可选：
#   NGXCP_AGENT_ROLLBACK=1     回滚模式：恢复到最近一次备份，不做部署
#
# 流程：构建 → 传文件 → 备份(快照) → stop → 落位 → start → 健康校验。
#       任一节点校验失败即自动回滚该节点到备份二进制，避免停在「Agent 半死」状态。
#       失败即中止，不继续推后续节点（灰度场景下一台一台来）。
#
# 用法：
#   NGXCP_AGENT_HOSTS="root@rs1 root@rs2" \
#   NGXCP_AGENT_CONTROL_PLANE="cp.internal:9443" \
#   NGXCP_AGENT_CA_CERT="./pki/ca.crt" \
#   NGXCP_AGENT_TOKENS_FILE="./.agent-tokens" \
#   bash scripts/deploy-agent.sh
set -euo pipefail

HOSTS="${NGXCP_AGENT_HOSTS:?请设置 NGXCP_AGENT_HOSTS（空格分隔的 SSH 目标，如 root@rs1 root@rs2）}"
CP_ADDR="${NGXCP_AGENT_CONTROL_PLANE:?请设置 NGXCP_AGENT_CONTROL_PLANE（控制面 gRPC 地址，如 cp.internal:9443）}"
CA_CERT="${NGXCP_AGENT_CA_CERT:?请设置 NGXCP_AGENT_CA_CERT（本地 CA 证书路径）}"
TOKENS_FILE="${NGXCP_AGENT_TOKENS_FILE:?请设置 NGXCP_AGENT_TOKENS_FILE（每行 host=token）}"
ROLLBACK="${NGXCP_AGENT_ROLLBACK:-0}"

BIN_DIR="/opt/ngxcp"
ENV_DIR="/etc/ngxcp"
SSH_OPTS="-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

[ -f "$CA_CERT" ] || { echo "CA 证书不存在: $CA_CERT" >&2; exit 1; }
[ -f "$TOKENS_FILE" ] || { echo "令牌文件不存在: $TOKENS_FILE" >&2; exit 1; }

# token_for <ssh目标> —— 从令牌文件取出该主机的一次性接入令牌。
token_for() {
  local want="$1" line
  while IFS= read -r line || [ -n "$line" ]; do
    [ -z "$line" ] && continue
    case "$line" in \#*) continue ;; esac
    if [ "${line%%=*}" = "$want" ]; then
      printf '%s' "${line#*=}"
      return 0
    fi
  done < "$TOKENS_FILE"
  return 1
}

# restore_last_backup <ssh目标> —— 回滚到该主机最近一次备份二进制并重启。
restore_last_backup() {
  ssh $SSH_OPTS "$1" 'bash -s' <<'REMOTE'
set -e
B=/opt/ngxcp
LAST=$(ls -1t "$B"/backups/ngxcp-agent.* 2>/dev/null | head -n 1 || true)
if [ -z "$LAST" ]; then
  echo "  无可用备份，保持现状待人工处理"
  exit 0
fi
systemctl stop ngxcp-agent 2>/dev/null || true
cp -f "$LAST" "$B/ngxcp-agent"
systemctl start ngxcp-agent
echo "  已回滚到 $LAST"
REMOTE
}

# ── 回滚模式 ────────────────────────────────────────────────
if [ "$ROLLBACK" = "1" ]; then
  echo "回滚模式：恢复各节点最近一次备份"
  for HOST in $HOSTS; do
    echo "──────── $HOST ────────"
    restore_last_backup "$HOST"
  done
  echo "回滚完成。"
  exit 0
fi

# ── [1/5] 构建 ──────────────────────────────────────────────
echo "[1/5] 交叉编译 linux/amd64 静态二进制 ..."
cd "$REPO_ROOT"
command -v go >/dev/null 2>&1 || export PATH="$HOME/.workbuddy/binaries/go/current/bin:$PATH"
V=$(git describe --tags --always 2>/dev/null || echo dev)
C=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
BT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS="-X github.com/th/ngxcp/internal/pkg/version.Version=$V \
-X github.com/th/ngxcp/internal/pkg/version.Commit=$C \
-X github.com/th/ngxcp/internal/pkg/version.BuildTime=$BT"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$LDFLAGS" -o bin/ngxcp-agent ./cmd/ngxcp-agent
echo "  构建完成: bin/ngxcp-agent ($V / $C)"

# ── [2..5] 逐节点部署 ───────────────────────────────────────
for HOST in $HOSTS; do
  echo "──────── $HOST ────────"
  if ! TOKEN="$(token_for "$HOST")"; then
    echo "令牌文件缺少 $HOST 的条目，中止（请先从控制面为该节点生成接入令牌）" >&2
    exit 1
  fi

  echo "[2/5] 远端建目录 + 传二进制 / unit / CA 证书 ..."
  ssh $SSH_OPTS "$HOST" "mkdir -p $BIN_DIR/backups $ENV_DIR /var/lib/ngxcp"
  scp $SSH_OPTS bin/ngxcp-agent "$HOST:$BIN_DIR/ngxcp-agent.new" >/dev/null
  scp $SSH_OPTS scripts/ngxcp-agent.service "$HOST:/etc/systemd/system/ngxcp-agent.service" >/dev/null
  scp $SSH_OPTS "$CA_CERT" "$HOST:$ENV_DIR/ca.crt" >/dev/null

  echo "[3/5] 写入环境文件（仅首次；enroll token 一次性，绝不覆盖已有凭据）..."
  if ssh $SSH_OPTS "$HOST" "test -f $ENV_DIR/agent.conf"; then
    echo "  保留现有 $ENV_DIR/agent.conf（Agent 已用持久化客户端证书，无需重新注册）"
  else
    TMPENV="$(mktemp)"
    {
      printf 'NGXCP_AGENT_CONTROL_PLANE=%s\n' "$CP_ADDR"
      printf 'NGXCP_AGENT_CA_CERT=%s/ca.crt\n' "$ENV_DIR"
      printf 'NGXCP_AGENT_ENROLL_TOKEN=%s\n' "$TOKEN"
      printf 'NGXCP_AGENT_DATA_DIR=/var/lib/ngxcp\n'
    } > "$TMPENV"
    scp $SSH_OPTS "$TMPENV" "$HOST:$ENV_DIR/agent.conf" >/dev/null
    ssh $SSH_OPTS "$HOST" "chmod 600 $ENV_DIR/agent.conf"
    rm -f "$TMPENV"
    echo "  已写入 $ENV_DIR/agent.conf（600）"
  fi

  echo "[4/5] 安装：备份旧二进制 → stop → 落位 → start ..."
  ssh $SSH_OPTS "$HOST" 'bash -s' <<'REMOTE'
set -e
B=/opt/ngxcp
if [ -f "$B/ngxcp-agent" ]; then
  TS=$(date +%Y%m%d-%H%M%S)
  cp -f "$B/ngxcp-agent" "$B/backups/ngxcp-agent.$TS"
  echo "  已备份旧二进制 -> $B/backups/ngxcp-agent.$TS"
fi
systemctl stop ngxcp-agent 2>/dev/null || true
mv -f "$B/ngxcp-agent.new" "$B/ngxcp-agent"
chmod +x "$B/ngxcp-agent"
systemctl daemon-reload
systemctl enable ngxcp-agent >/dev/null 2>&1 || true
systemctl start ngxcp-agent
REMOTE

  echo "[5/5] 校验服务状态 ..."
  if ! ssh $SSH_OPTS "$HOST" 'systemctl is-active --quiet ngxcp-agent'; then
    echo "  !! 启动校验失败，自动回滚" >&2
    restore_last_backup "$HOST"
    exit 1
  fi
  ssh $SSH_OPTS "$HOST" 'systemctl show ngxcp-agent -p ActiveState -p SubState --value | tr "\n" " "; echo; journalctl -u ngxcp-agent -n 3 --no-pager | tail -n 3'
done

echo
echo "全部节点部署完成。控制面侧确认节点上线："
echo "  curl -s http://<控制面>:8080/api/v1/nodes"
