#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 tianhao
#
# prod-lvs-probe.sh —— NGX-CP 生产环境「只读」一致性探测脚本（T035 配套）
#
# ⚠️ 本脚本严格只读：
#    - 只允许执行读命令（ipvsadm -Ln / nginx -t / ss / sysctl / cat / systemctl is-active …）
#    - 绝不执行 ipvsadm -e / nginx -s reload / systemctl restart / 写文件 / 改配置
#    - 任何会改变生产状态的操作都不在此脚本内。
#
# 用途：在 Agent/控制面正式接管前，对「LVS + Nginx」生产环境做一次性合规体检，
#       输出 PASS / FLAG 报告。FLAG 项需人工确认，但本脚本本身不修复、不改任何东西。
#
# 用法：bash scripts/prod-lvs-probe.sh
# 前置：~/.ssh/config 已配置 192.168.5.6-9 的 root 免密（本机已配）。

# 注意：故意不使用 set -e / set -u —— 任一检查失败都应继续产出完整报告，
# 且只读探测绝不应因某条命令无输出而中断。
# 统一 locale，避免远程 `ip -br addr` 等输出产生乱码。
export LC_ALL=C LANG=C

# ── 生产环境拓扑（用户提供，只读探测用）─────────────────────────────────
LVS_HOSTS=(192.168.5.6 192.168.5.7)
NGX_HOSTS=(192.168.5.8 192.168.5.9)
VIP="192.168.5.5"

PASS=0
FLAG=0

# 只读执行：ssh 远程跑单条命令；失败不致命（仍继续其它检查）。
# 关键：连接超时(ConnectTimeout=6) + 会话保活(ServerAlive*) + 单条命令硬上限(15s 后 kill)，
# 避免某主机命令挂起导致整脚本卡死。每次调用独立临时文件，不污染生产。
rsh() {
  local host="$1" cmd="$2" out
  out=$(mktemp /tmp/ngxcp-probe.XXXXXX)
  ssh -o BatchMode=yes -o ConnectTimeout=6 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 \
      "$host" "$cmd" >"$out" 2>/dev/null &
  local pid=$!
  local i=0
  while kill -0 "$pid" 2>/dev/null && [ $i -lt 15 ]; do sleep 1; i=$((i+1)); done
  if kill -0 "$pid" 2>/dev/null; then kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; fi
  cat "$out"; rm -f "$out"
}

report_pass() { echo "  [PASS] $1"; PASS=$((PASS+1)); }
report_flag() { echo "  [FLAG] $1"; FLAG=$((FLAG+1)); }
report_info() { echo "  [INFO] $1"; }

echo "================================================================"
echo " NGX-CP 生产环境只读体检  |  VIP=$VIP  LVS=${LVS_HOSTS[*]}  NGX=${NGX_HOSTS[*]}"
echo "================================================================"

# ── 1. LVS Director 检查 ──────────────────────────────────────────────
for h in "${LVS_HOSTS[@]}"; do
  echo "── LVS $h ───────────────────────────────────────────────────────"
  ipvs=$(rsh "$h" "ipvsadm -Ln 2>&1")
  [ -z "$ipvs" ] && { report_flag "无法读取 ipvsadm -Ln（连通性或权限）"; continue; }

  # DR 模式 & 调度器
  if echo "$ipvs" | grep -q "Route"; then report_pass "VS 使用 DR 模式 (Forward=Route)"; else report_flag "未发现 DR(Route) 转发，期望 LVS-DR"; fi
  if echo "$ipvs" | grep -q "wrr"; then report_pass "调度器为 wrr"; else report_info "调度器非 wrr: $(echo "$ipvs" | grep -m1 'Scheduler' || true)"; fi

  # VIP 是否在本节点
  if rsh "$h" "ip -br addr 2>/dev/null" | grep -q "$VIP"; then
    has_vip=1
    report_info "VIP $VIP 当前落在 $h（主）"
  else
    has_vip=0
    report_info "VIP $VIP 不在 $h（备，符合主备）"
  fi

  # RS 权重拓扑（关键：一个 RS 往往挂在多条 VS 上，如 :80/:443tcp/:443udp）
  # 注意：ipvsadm -Ln 的 "->" 行有前导空格，必须用字段匹配 $1=="->"，不能用 /^->/ 锚定。
  vs_count=$(echo "$ipvs" | awk '($1=="TCP"||$1=="UDP"){c++} END{print c+0}')
  echo "$ipvs" | awk '($1=="TCP"||$1=="UDP"){proto=$1;vs=$2;next} ($1=="->" && vs!=""){print proto,vs,$2}' \
    | sort -u | while read -r p v r; do
      report_info "$p $v -> $r"
    done
  report_info "本节点 VS 条目数: $vs_count（LVS-DR 下 :80 / :443(tcp) / :443(udp) 应为 3 条）"

  # 分裂脑(Split-Brain)启发式：备机(不持 VIP)却持有 VS 转发规则，疑似双 MASTER。
  has_vs=$(echo "$ipvs" | grep -cE '^[[:space:]]*->' || true)
  if [ "$has_vip" != "1" ] && [ "$has_vs" -gt 0 ]; then
    report_flag "备机($h)不持 VIP 却持有 $has_vs 条 VS 转发规则（疑似 split-brain / 双 MASTER，组播被 vSwitch 拦截常见）"
  fi

  # keepalived VRRP 传输方式：项目铁律要求 unicast
  ka=$(rsh "$h" "grep -iE 'unicast_peer|vrrp_instance' /etc/keepalived/keepalived.conf 2>/dev/null")
  if echo "$ka" | grep -qi "unicast_peer"; then report_pass "keepalived 使用 unicast_peer（符合 vSphere/云铁律）"; else report_flag "keepalived 未配置 unicast_peer（疑似 multicast，公有云/vSwitch 可能脑裂）"; fi

  # ip_vs 模块
  if rsh "$h" "lsmod | grep -q ip_vs" 2>/dev/null; then report_pass "ip_vs 内核模块已加载"; else report_flag "ip_vs 模块未加载"; fi

  # 转发开关（DR 下非必需，仅信息）
  fwd=$(rsh "$h" "cat /proc/sys/net/ipv4/ip_forward 2>/dev/null")
  report_info "ip_forward=$fwd（DR 模式非必需，无害）"
done

# ── 2. Nginx RS 检查 ──────────────────────────────────────────────────
for h in "${NGX_HOSTS[@]}"; do
  echo "── Nginx $h ─────────────────────────────────────────────────────"
  ver=$(rsh "$h" "nginx -v 2>&1")
  report_info "版本: $ver"

  # nginx -t（只读语法校验，不 reload）
  if rsh "$h" "nginx -t 2>&1" | grep -q "test is successful"; then report_pass "nginx -t 语法通过"; else report_flag "nginx -t 未通过（详见远程输出）"; fi

  # 编译模块是否符合 NGX-CP 能力基线
  nv=$(rsh "$h" "nginx -V 2>&1")
  for m in "--with-stream" "--with-stream_ssl_preread_module" "--with-http_v3_module" "--with-http_realip_module" "--add-module=../nginx_upstream_check_module"; do
    if echo "$nv" | grep -qF -- "$m"; then report_pass "编译含 $m"; else report_flag "缺少编译模块 $m（NGX-CP 能力基线要求）"; fi
  done

  # DR RS 硬约束：VIP 落 lo + arp 抑制
  if rsh "$h" "ip -br addr 2>/dev/null" | grep -q "$VIP/32"; then report_pass "VIP $VIP/32 落在 lo（DR RS 正确）"; else report_flag "VIP $VIP/32 未落在 lo（DR RS 必须，否则 ARP 冲突）"; fi
  ai=$(rsh "$h" "sysctl -n net.ipv4.conf.all.arp_ignore 2>/dev/null" | tr -d '\r')
  aa=$(rsh "$h" "sysctl -n net.ipv4.conf.all.arp_announce 2>/dev/null" | tr -d '\r')
  if [ "$ai" = "1" ] && [ "$aa" = "2" ]; then report_pass "arp_ignore=1 / arp_announce=2（DR 正确）"; else report_flag "arp 抑制异常: $ai/$aa（期望 1/2）"; fi

  # 监听 & ssl 目录（Agent 路径白名单）
  if rsh "$h" "ss -tlnp 2>/dev/null" | grep -qE ":80 |:443 "; then report_pass "nginx 监听 80/443"; else report_flag "nginx 未监听 80/443"; fi
  if rsh "$h" "test -d /etc/nginx/ssl" 2>/dev/null; then report_pass "/etc/nginx/ssl 存在（匹配 Agent 白名单）"; else report_flag "/etc/nginx/ssl 缺失（Agent 白名单要求）"; fi
done

echo "================================================================"
echo " 结果：PASS=$PASS  FLAG=$FLAG  （FLAG 仅标注，本脚本未做任何修改）"
echo "================================================================"
# 仅标注，不阻断；退出码 0 便于作为只读检查接入 CI。
exit 0
