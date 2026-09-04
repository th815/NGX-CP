# 生产环境 LVS + Nginx 只读体检报告

> **范围**：192.0.2.6 / 192.0.2.7（LVS Director，主备，VIP `192.0.2.5`）+ 192.0.2.8 / 192.0.2.9（Nginx RealServer）
> **日期**：2026-09-04
> **性质**：**只读体检**。未执行任何写操作（`ipvsadm -e` / `nginx -s reload` / 配置文件修改 / 服务重启均未执行）。
> **方法**：`scripts/prod-lvs-probe.sh`（严格只读，单条 SSH 命令 15s 硬上限，绝不改动生产）。
> 原始证据见 `testdata/ipvsadm_Ln.txt`、`testdata/keepalived_master.conf`、`testdata/nginx_V_1.30.0.txt`。

---

## 1. 结果摘要

| 指标 | 数值 |
| --- | --- |
| PASS | 26 |
| FLAG | 3 |
| 拓扑 | 2 Director + 2 RS，LVS-DR，wrr，persistent 60，VIP `192.0.2.5` |
| Nginx | 1.30.0（编译模块齐全，`nginx -t` 通过） |
| RS 约束 | VIP 落 `lo` + `arp_ignore=1/arp_announce=2` 正确 |

**3 个 FLAG（均为配置/架构层面的真实风险，非误报）：**

| # | 严重度 | 问题 | 位置 |
| --- | --- | --- | --- |
| P1 | 🔴 高 | keepalived 使用 **multicast**，未配 `unicast_peer` | .6 / .7 |
| P2 | 🔴 高 | **备机 .7 持有与 .6 完全一致的 ipvs 转发表**——双 MASTER / split-brain 表象 | .7 |
| P3 | 🟡 中 | 同一 RS 在 `:80` / `:443(tcp)` / `:443(udp)` 是**三条独立 VS**，灰度必须三处同步摘除 | 全部 |

---

## 2. 标注问题详解

### P1 · keepalived 组播（违反 vSphere/vSwitch 铁律）

**证据**（`keepalived_master.conf`）：配置中**无 `unicast_peer { … }` 块**，`vrrp_instance VI_1` 直接依赖默认组播；仅用 `auth_type PASS / auth_pass example`。

```text
vrrp_instance VI_1 {
    state MASTER
    interface ens33
    virtual_router_id 100
    priority 100
    advert_int 1
    authentication { auth_type PASS; auth_pass example }
    virtual_ipaddress { 192.0.2.5 }
}
# 无 unicast_peer —— 走组播
```

**影响**：在 vSphere 环境，标准 vSwitch **默认拦截 / 不转发 VRRP 组播**（IGMP snooping + 无 IGMP querier），备机收不到 MASTER 的 advert，会持续认为 MASTER 已死而抢占 → **双 MASTER 脑裂**。这正是 P2 的根因。

**建议整改（未执行）**：在两端 `vrrp_instance` 内加 `unicast_peer { src_ip <本机>; peer_ip <对端>; }` 并 `interface` 保持 `ens33`，关闭对组播的依赖。优先级/状态保持现状即可。

---

### P2 · 备机 .7 持有完整 ipvs 转发表（split-brain 表象）

**证据**（`testdata/ipvsadm_Ln.txt`）：`.6`（MASTER，持 VIP，有真实 ActiveConn）与 `.7`（BACKUP，**不持 VIP**）的 `ipvsadm -Ln` 输出**逐行相同**——三条 VS、两个 RS、权重均为 1。

```text
# .6 MASTER（持 VIP，.9:443 有 ActiveConn 1/2）
TCP  192.0.2.5:443 wrr persistent 60
  -> 192.0.2.9:443   Route   1   2   1

# .7 BACKUP（不持 VIP，但 ipvs 规则一成不变地存在，全部 ActiveConn=0）
TCP  192.0.2.5:443 wrr persistent 60
  -> 192.0.2.9:443   Route   1   0   0
```

**影响**：正常 keepalived DR 下，备机**不持有** ipvs 规则（规则随 MASTER 角色生效时才建立）。备机静态持有完整规则，说明两台 Director 实际都进入了 MASTER 角色——与 P1 互为因果：组播被拦 → .7 收不到 .6 的 advert → .7 自认 MASTER → 建立 ipvs 规则。当前因 VIP 仅落在 .6，尚未造成双 VIP 冲突，但**一旦 .6 抖动，脑裂会瞬间放大为双 VIP 双写/双响应**，是高危隐患。

**建议整改（未执行）**：先落地 P1（unicast_peer），再确认：仅 MASTER 持 VIP 且唯一持有 ipvs 规则；可在维护窗口对 .7 执行 `ipvsadm -C` 验证其规则是否随角色正确收敛。

---

### P3 · 同一 RS 在 3 条 VS 上（灰度必须三处同步摘除）

**证据**（`ipvsadm -Ln`）：VIP `192.0.2.5` 下存在**三条独立 VS 条目**：

```text
TCP  192.0.2.5:80    wrr persistent 60   → .8:80 / .9:80
TCP  192.0.2.5:443   wrr persistent 60   → .8:443 / .9:443     （HTTPS）
UDP  192.0.2.5:443   wrr persistent 60   → .8:443 / .9:443     （QUIC/HTTPS3）
```

**影响**：对 NGX-CP T035「LVS 权重摘除式灰度」是**硬约束**——摘除某 RS 时必须对**同一 RS 的 `:80`、`:443/tcp`、`:443/udp` 三条 VS 同时 `ipvsadm -e … -w 0`**。只摘 `:80` 会让 443（含 QUIC）继续把流量打向正在变更的 RS，灰度失效。

**设计校验**：T035 的 `GracefulDeploy.DeployOne` 已采用 `BackendRef{Address}` 枚举该 backend 地址在全部 VS 上的条目统一置权（而非单条 VS），与本条一致，设计正确。

**附加注意**：`persistent 60` 意味着已建连接会被持久化 60s。灰度「排空」阶段必须等 `ActiveConn → 0` 或容忍 60s 持久窗口，否则旧连接仍会打到被摘 RS（T035 `waitDrain` 已覆盖）。

---

## 3. 已通过的合规项（无风险）

- ✅ LVS-DR 模式（`Forward=Route`）、调度器 `wrr`
- ✅ Nginx 1.30.0，`nginx -t` 通过；编译模块齐全（`--with-stream` / `stream_ssl_preread` / `http_v3` / `http_realip` / `upstream_check`）
- ✅ RS 上 VIP `192.0.2.5/32` 落 `lo`；`arp_ignore=1 / arp_announce=2` 正确（DR 硬约束）
- ✅ Nginx 监听 80/443；`/etc/nginx/ssl` 存在（匹配 Agent 路径白名单）
- ✅ `ip_vs` 内核模块已加载

---

## 4. 对 NGX-CP / T035 的结论

1. **T035 灰度算法正确**：`BackendRef` 枚举多 VS 置权 + `defer` 必加回权重 + 排空等待，与现场「3 条 VS + persistent 60」完全契合，无需改动。
2. **接管前必须先解决 P1/P2**：NGX-CP 的 Director Agent 接管 LVS 权重编排前，现场 keepalived 必须改为 unicast，否则控制面下发权重摘除时会落在一个脑裂环境中，结果不可预期。
3. **节点健康判定**：备机持有 ipvs 规则本身即异常信号，可作为 NGX-CP DR 合规巡检（T031/T05x）的一条强校验规则：「持 VIP 的节点才允许有 ActiveConn；不持 VIP 却有 ipvs 规则 → 标红」。

---

## 5. 复现

```bash
# 只读体检（不改任何生产配置）
bash scripts/prod-lvs-probe.sh
# 期望输出结尾： PASS=26  FLAG=3

# 查看原始证据
cat testdata/ipvsadm_Ln.txt
cat testdata/keepalived_master.conf
cat testdata/nginx_V_1.30.0.txt
```

> 报告中所列整改项均为**标注建议**，本次体检未在生产执行任何写操作。
