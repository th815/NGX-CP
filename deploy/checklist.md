# 部署强制清单（vCenter / 虚拟化层）

> LVS-DR 在 vSphere + 万兆 LACP 环境下的「虚拟化层」前置项。
> 这些项 **Agent 在 Guest 内无法探测**（见 `internal/agent/health/compliance.go` 的
> `director_promisc` / `lacp_ip_hash` 两项标注"Agent 不可探，由部署清单保证"），
> 因此必须由部署/上线人员在 vCenter 侧逐项确认，并作为上线前的强制钩子。
> 任一未满足，LVS-DR 会出现「`ipvsadm` 计数正常但 RS 收不到包」「时通时断」
> 这类**极度误导且无报错**的故障。

## 0. 拓扑与隔离（部署前）

- [ ] 2 台 Keepalived Director 为**独立虚拟机**（非同一物理机），并设 vCenter **反亲和性规则**（DRS 规则：分离虚拟机），防止同物理机宕机导致双 Director 同时失联。
- [ ] 2 台 Nginx RS 与 Director **分别位于不同物理机**；Director 设 **全部资源预留**（100% 预留 CPU/内存），避免资源争抢导致的 VRRP 抖动。
- [ ] 控制面（ngxcp-server）部署在本地 TH-D2110 虚拟化层，不与 RS/Director 争抢关键资源；控制面本身不跑 VRRP，仅需与 Agent 建立 mTLS。
- [ ] 时钟：关闭 VMware Tools 时间同步（`timesync.disable-host-to-guest = "TRUE"`），改由 chrony 主动同步；两两时钟偏差 **>1s** 即告警（见 `clock_sync` 合规项），否则 TraceID/日志时序错乱、合规漂移误判。

## 1. 端口组安全策略（三项必须全部「接受」）⚠️ 最关键

对承载 VIP 的 **Director 端口组**（以及 RS 所在端口组，DR 模式 RS 也需回包伪装）设置：

| 项 | vCenter 位置 | 必须值 | 不设置的后果 |
| --- | --- | --- | --- |
| **混杂模式** (Promiscuous Mode) | 端口组 → 安全 → 混杂模式 | **接受 (Accept)** | RS 收不到发往 VIP 的入向包（表象：ipvsadm 计数正常但 RS 零流量） |
| **MAC 地址更改** (MAC Address Changes) | 端口组 → 安全 → MAC 地址更改 | **接受 (Accept)** | Director 绑定 VIP 的 MAC 被拒绝，ARP 不通 |
| **伪传输** (Forged Transmits) | 端口组 → 安全 → 伪传输 | **接受 (Accept)** | RS 以 VIP 为源 MAC 回包被丢弃（LVS-DR 回包必须伪造源 MAC） |

> 经验坑：这项没开时，`ipvsadm -Ln` 转发计数正常、Director 上 `tcpdump` 能看到包，
> 但 RS 侧 `tcpdump` 收不到——故障**无任何报错**，排错成本极高。上线前必须确认。

## 2. VRRP / Keepalived（控制面 + Agent 双校验）

- [ ] VRRP 使用 **unicast**（`unicast_src_ip` / `unicast_peer`），**禁用 multicast**（云网络禁组播）。对应合规项 `keepalived_unicast`。
- [ ] 不使用 AH 认证（Keepalived 2.x 已移除 `auth_type AH`）。对应合规项 `no_ah_auth`。
- [ ] `vrrp_instance` 的 `virtual_router_id` 在同二层唯一（0–255）；双 Director 仅 `state`/`priority`/`unicast_*` 三项不同，其余必须完全一致（渲染器 `internal/lvs/render.go` 已做 VRID 冲突检测）。
- [ ] `rp_filter` 严格模式（=1）会让回包源 VIP 被内核丢弃，DR **必设 0**（合规项 `vip_on_lo` 之外的内核参数，部署脚本 `deploy-agent.sh` 预置）。

## 3. 上行链路 LACP

- [ ] ESXi 上行链路 LACP 哈希模式改为 **基于 IP**（IP hash / 源目的 IP），否则单条 TCP 流无法用满万兆、且 Director 双活流量不均。对应合规项 `lacp_ip_hash`（Agent 不可探，此处人工保证）。
- [ ] Director 端口组开启**混杂模式**后，确认 vDS 的**负载均衡**策略与 LACP 哈希一致，避免单上行拥塞。

## 4. 上线自检（控制面侧可观测）

- [ ] 节点在线、`vip_on_lo` 合规项通过（VIP 绑在 `lo:/32`）。
- [ ] Director `holding_vip` 经合规上报填充（拓扑页 `holding_vip` 取真实值，而非按 `state==MASTER` 推断）。
- [ ] `GET /api/v1/lvs/split-brain` 返回 `detected=false`；正常运行仅 1 个 Director `holding_vip=true`。
- [ ] 双 Director **同时** `holding_vip=true` 时，控制面 `StartSplitBrainWatch` 必须打出 **CRITICAL** 日志（脑裂告警）。

## 5. 回滚与应急

- [ ] 端口组三项改动可在 vCenter 一键恢复为「拒绝」；任何一项临时改回「拒绝」都会**立即**导致 LVS-DR 异常，须先排空流量再操作。
- [ ] 脑裂发生时的人工处置：确认真正 MASTER，将备机 `state` 置 `BACKUP` 并 `systemctl restart keepalived`，观察 `split-brain` 端点恢复 `detected=false`。
