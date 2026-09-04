#!/usr/bin/env bash
# ngxcp-agent 自安装脚本（由控制面 /agent/install.sh 提供，对应「web 一键安装自注册」）。
#
# 用法（控制台「新建节点并生成接入命令」会给出这条一行命令）：
#   curl -fsSL https://<控制面>/agent/install.sh | sudo bash -s -- \
#       --cp https://<控制面> --grpc <控制面:9443> --token <JOIN_TOKEN>
#
# 行为：
#   1. 校验 root + 架构（linux x86_64 / aarch64）。
#   2. 从 <cp>/agent/ca.crt 拉取引导 CA，落盘到数据目录。
#   3. 从 <cp>/agent/bin/ngxcp-agent-linux-<arch> 拉取二进制到 /usr/local/bin。
#   4. 写 systemd 单元（机密走 EnvironmentFile，不进 unit、不出现于 ps）。
#   5. enable --now。Agent 启动后用 --token 自注册（Join Token + 本地 CSR），
#      控制面复用令牌绑定的既有节点并下发证书，节点无审批直接上线。
#
# 幂等：重复执行仅覆盖二进制 + 重启服务；EnvironmentFile 不被覆盖（凭据已存在则保留）。
set -euo pipefail

CP=""          # 控制面 HTTP 基址，如 https://cp.example.com
GRPC=""        # 控制面 gRPC 地址，如 cp.example.com:9443
TOKEN=""       # 自注册 Join Token
DATA_DIR="/var/lib/ngxcp"
UNIT=/etc/systemd/system/ngxcp-agent.service
ENV_FILE=/etc/ngxcp-agent.env

while [ $# -gt 0 ]; do
  case "$1" in
    --cp)     CP="$2"; shift 2 ;;
    --grpc)   GRPC="$2"; shift 2 ;;
    --token)  TOKEN="$2"; shift 2 ;;
    --data-dir) DATA_DIR="$2"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
done

[ "$(id -u)" -eq 0 ] || { echo "需 root 权限（sudo）" >&2; exit 1; }
[ -n "$CP" ]   || { echo "缺少 --cp" >&2; exit 2; }
[ -n "$GRPC" ] || { echo "缺少 --grpc" >&2; exit 2; }
[ -n "$TOKEN" ]|| { echo "缺少 --token" >&2; exit 2; }

ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) BIN_ARCH=amd64 ;;
  aarch64|arm64) BIN_ARCH=arm64 ;;
  *) echo "不支持的架构: $ARCH" >&2; exit 1 ;;
esac

echo "[1/5] 拉取引导 CA ..."
install -d -m 700 "$DATA_DIR"
curl -fsSL "$CP/agent/ca.crt" -o "$DATA_DIR/ca.crt"
chmod 600 "$DATA_DIR/ca.crt"

echo "[2/5] 下载 Agent 二进制 (linux-$BIN_ARCH) ..."
curl -fsSL "$CP/agent/bin/ngxcp-agent-linux-$BIN_ARCH" -o /usr/local/bin/ngxcp-agent
chmod 0755 /usr/local/bin/ngxcp-agent

echo "[3/5] 写入 systemd 单元 ..."
cat > "$UNIT" <<EOF
[Unit]
Description=NGX-CP Agent (node-side control-plane proxy)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$ENV_FILE
ExecStart=/usr/local/bin/ngxcp-agent \\
  -control-plane \${NGXCP_AGENT_CONTROL_PLANE} \\
  -ca-cert \${NGXCP_AGENT_CA_CERT} \\
  -join-token \${NGXCP_AGENT_JOIN_TOKEN} \\
  -data-dir \${NGXCP_AGENT_DATA_DIR}
Restart=always
RestartSec=2
# 改配置/抓快照/跑 nginx -t 需要写这些路径；CAP_NET_ADMIN 供 ipvsadm 调权。
ProtectSystem=full
ReadWritePaths=/etc/nginx /etc/keepalived /var/lib/ngxcp /var/log/nginx
AmbientCapabilities=CAP_NET_ADMIN
MemoryMax=256M

[Install]
WantedBy=multi-user.target
EOF

# 环境文件：仅在首次写入（Join Token 一次性，且 Agent 已持久化客户端证书，
# 重复部署绝不覆盖已有凭据，避免把已注册节点打回未注册）。
if [ ! -f "$ENV_FILE" ]; then
  echo "[3/5] 写入环境文件 $ENV_FILE (600) ..."
  install -d -m 700 "$(dirname "$ENV_FILE")"
  cat > "$ENV_FILE" <<EOF
NGXCP_AGENT_CONTROL_PLANE=$GRPC
NGXCP_AGENT_CA_CERT=$DATA_DIR/ca.crt
NGXCP_AGENT_JOIN_TOKEN=$TOKEN
NGXCP_AGENT_DATA_DIR=$DATA_DIR
EOF
  chmod 600 "$ENV_FILE"
else
  echo "[3/5] 环境文件已存在，保留既有凭据（如需轮换请手动编辑 $ENV_FILE）"
fi

echo "[4/5] 重载并启动 ..."
systemctl daemon-reload
systemctl enable --now ngxcp-agent

echo "[5/5] 完成。5 秒后查看状态 ..."
sleep 5
systemctl status ngxcp-agent --no-pager --lines=5 || true
echo "节点应已在控制面「节点」列表中出现并上线。若未出现，看日志: journalctl -u ngxcp-agent -e"
