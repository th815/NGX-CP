#!/usr/bin/env bash
# deploy.sh —— 将 ngxcp-server（含内嵌前端 SPA）部署到目标主机（由环境变量 NGXCP_DEPLOY_HOST 指定）。
#
# 前置：能用 root 密钥 SSH 到 $NGXCP_DEPLOY_HOST（建议 ssh 免密）。
# 用途：构建前端 → 交叉编译(内嵌 webui) → 传输 → 备份旧二进制 → 落位 →
#       首次生成配置(机密不入库) → 启服务 → 冒烟测试。
# 回滚：保留 /opt/ngxcp/backups/ngxcp-server.<时间戳>，必要时 stop + 换回 + restart 即可。
# 安全：本脚本不硬编码任何目标主机/IP，部署目标完全由本地环境变量决定，可安全入库。
#
# 用法： NGXCP_DEPLOY_HOST=root@your-host bash scripts/deploy.sh
set -euo pipefail

HOST="${NGXCP_DEPLOY_HOST:?请设置环境变量 NGXCP_DEPLOY_HOST（目标主机，如 root@your-host）}"
REMOTE_DIR="/opt/ngxcp"
SSH_OPTS="-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# 确保 go 可用（优先系统，回退到 WorkBuddy 托管 go）
command -v go >/dev/null 2>&1 || export PATH="$HOME/.workbuddy/binaries/go/current/bin:$PATH"

echo "[1/6] 构建前端 SPA（web/dist，需 node+npm）..."
cd "$REPO_ROOT/web"
command -v npm >/dev/null 2>&1 || export PATH="$HOME/.workbuddy/binaries/node/current/bin:$PATH"
npm run build
cd "$REPO_ROOT"

echo "[2/6] 交叉编译 linux/amd64 静态二进制（内嵌 webui）..."
cd "$REPO_ROOT"
export CGO_ENABLED=0 GOOS=linux GOARCH=amd64
V=$(git describe --tags --always 2>/dev/null || echo dev)
C=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
BT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS="-X github.com/th/ngxcp/internal/pkg/version.Version=$V -X github.com/th/ngxcp/internal/pkg/version.Commit=$C -X github.com/th/ngxcp/internal/pkg/version.BuildTime=$BT"
go build -tags webui -ldflags "$LDFLAGS" -o bin/ngxcp-server ./cmd/ngxcp-server

echo "[3/6] 远端建目录 + 放行防火墙(若 firewalld 运行) ..."
ssh $SSH_OPTS "$HOST" 'bash -s' <<'REMOTE'
set -e
mkdir -p /opt/ngxcp /opt/ngxcp/backups /var/lib/ngxcp/snapshots /var/lib/ngxcp/artifacts /var/log/ngxcp
if systemctl is-active --quiet firewalld; then
  firewall-cmd --permanent --add-port=8080/tcp >/dev/null
  firewall-cmd --permanent --add-port=9443/tcp >/dev/null
  firewall-cmd --reload >/dev/null
  echo "firewalld: 已放行 8080/9443"
else
  echo "firewalld: 未运行，跳过"
fi
REMOTE

echo "[4/6] 传输二进制 / 规则 / systemd 单元 ..."
scp $SSH_OPTS bin/ngxcp-server "$HOST:/opt/ngxcp/ngxcp-server.new"
scp $SSH_OPTS configs/rules.yaml "$HOST:/opt/ngxcp/rules.yaml"
scp $SSH_OPTS scripts/ngxcp-server.service "$HOST:/etc/systemd/system/ngxcp-server.service"

echo "[5/6] 安装：备份旧二进制 + 落位 + 首次生成配置 + 启服务 ..."
ssh $SSH_OPTS "$HOST" 'bash -s' <<'REMOTE'
set -e
TS=$(date +%Y%m%d-%H%M%S)
if [ -f /opt/ngxcp/ngxcp-server ]; then
  cp -f /opt/ngxcp/ngxcp-server "/opt/ngxcp/backups/ngxcp-server.$TS"
  echo "已备份旧二进制 -> /opt/ngxcp/backups/ngxcp-server.$TS"
fi
mv -f /opt/ngxcp/ngxcp-server.new /opt/ngxcp/ngxcp-server
chmod +x /opt/ngxcp/ngxcp-server

# 配置仅首次生成（保留已有机密 token，绝不覆盖）
if [ ! -f /opt/ngxcp/config.yaml ]; then
  TOKEN=$(openssl rand -hex 32)
  SECRET=$(openssl rand -hex 32)
  cat > /opt/ngxcp/config.yaml <<YAML
listen: ":8080"
agent_grpc: ":9443"
db_driver: "sqlite"
db_dsn: "file:/var/lib/ngxcp/ngxcp.db?cache=shared&_fk=1"
db_auto_migrate: true
pki_dir: "/opt/ngxcp/pki"
storage_snapshots_dir: "/var/lib/ngxcp/snapshots"
storage_artifacts_dir: "/var/lib/ngxcp/artifacts"
log_level: "info"
log_pretty: false
auth_admin_token: "$TOKEN"
security_session_secret: "$SECRET"
security_totp_required: false
drift_auto_remediate: false
YAML
  chmod 600 /opt/ngxcp/config.yaml
  echo "已生成 /opt/ngxcp/config.yaml（token 见文件，未入库）"
else
  echo "保留现有 /opt/ngxcp/config.yaml"
fi

# 启动前配置自检
/opt/ngxcp/ngxcp-server --config /opt/ngxcp/config.yaml --check-config >/dev/null && echo "config check OK"

systemctl daemon-reload
systemctl enable ngxcp-server
if systemctl is-active --quiet ngxcp-server; then
  systemctl restart ngxcp-server
  echo "service restarted (新二进制已生效)"
else
  systemctl start ngxcp-server
  echo "service started"
fi
REMOTE

echo "[6/6] 冒烟测试 /health + /api/v1/version + SPA 首页 ..."
sleep 2
ssh $SSH_OPTS "$HOST" 'curl -fsS http://127.0.0.1:8080/health && echo && curl -fsS http://127.0.0.1:8080/api/v1/version && echo && curl -fsS -o /dev/null -w "SPA / -> HTTP %{http_code}\n" http://127.0.0.1:8080/'
echo
echo "部署完成。查看 live token： ssh $HOST 'grep auth_admin_token /opt/ngxcp/config.yaml'"
