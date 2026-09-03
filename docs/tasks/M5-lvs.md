# M5 · LVS / Keepalived 管理（W8）

> **目标**：把 LVS-DR + Keepalived 主备纳入同一控制台，渲染配置、巡检合规、拓扑可视、权重灰度。
> **完成标志**：在 RS 上手工改坏 `arp_ignore` → 5 分钟内节点标红 → 新建 LVS/配置发布被阻断。
>
> 决策依据：`docs/ARCHITECTURE.md` §7（LVS 子系统）、`docs/DECISIONS.md` §16（vSphere 环境）。
> 关键约束：**LVS 调度层与 Nginx 七层是两套模型、一个控制台**；DR 合规不通过直接阻断发布。

---

## T050 · LVS / Keepalived 数据模型

**目标**：建模 Director、VirtualService、RealServer，双机仅 3 项差异。

**依赖**：T006

**涉及文件**：
```
ent/schema/director.go
ent/schema/virtual_service.go
ent/schema/real_server.go
```

**契约**：
```go
type Director struct {
    ID             int
    NodeID         int        // 关联 M1 的 Node
    State          string     // "MASTER" | "BACKUP"
    Priority       int        // MASTER 高
    VirtualRouterID int       // 0-255，同二层唯一
    UnicastSrcIP   string
    UnicastPeerIP  string     // 云环境禁 VRRP 组播，必须 unicast
    Interface      string     // 如 eth0
    Mode           string     // "DR" | "NAT" | "TUN"
}
type VirtualService struct {
    ID, DirectorID int
    VIP            string
    Port           int
    Protocol       string     // tcp
    Scheduler      string     // rr | wrr | lc
}
type RealServer struct {
    ID, VirtualServiceID int
    NodeID               int
    Port                 int
    Weight               int   // 0 = 摘除
    HealthCheck          string // 复用 nginx_upstream_check_module 语义
}
// 双机一致性：两台 Director 除 state/priority/unicast_src_ip 外必须完全一致
```

**验收命令**：
```bash
make generate && go test ./ent/...
# 期望：schema 通过；用 testdata/keepalived_master.conf + backup.conf 校验解析
```

**AI 陷阱**：
- 云厂商 VPC 禁 VRRP 组播 → **必须 unicast_src_ip + unicast_peer**
- Keepalived 2.x 已移除 AH 认证，只剩 `auth_type PASS`（明文），别指望它做安全
- DR 模式不支持端口映射，VS 端口必须等于 RS 端口

---

## T051 · Keepalived 配置渲染器

**目标**：从模型渲染 master/backup 配置，仅 3 项不同，减少人为漂移。

**依赖**：T050

**涉及文件**：
```
internal/lvs/render.go
internal/lvs/render_test.go
```

**契约**：
```go
func RenderKeepalived(d Director, vs []VirtualService, rs map[int][]RealServer) string
// 输出标准 keepalived.conf：
//   global_defs + vrrp_instance(VI_1) + 每个 VS 的 virtual_server + real_server
// MASTER/BACKUP 仅 state/priority/unicast_src_ip 三处不同
```

**验收命令**：
```bash
go test ./internal/lvs/... -run Render
# 期望：渲染结果与 testdata/keepalived_master.conf 语义一致（忽略注释）
```

**AI 陷阱**：
- `virtual_router_id` 同二层必须唯一，渲染时做冲突检测
- `vrrp_script` 健康检查必须能检出 nginx 故障，否则主备切换无意义

---

## T052 · DR 合规自检（Agent 指令）

**目标**：Agent 每 5 分钟跑 6 项硬约束 + Keepalived 7 检查，上报 ComplianceReport。

**依赖**：T050, M1(Heartbeat)

**涉及文件**：
```
proto/agent/v1/agent.proto       # CheckDRCompliance 已在全局契约
internal/agent/compliance.go
```

**契约**：
```go
type ComplianceItem struct {
    Name   string  // "vip_on_lo32" | "arp_ignore" | "arp_announce" | "rp_filter"
                  // | "vip_not_on_eth0" | "port_match" | "l2_reachable"
    OK     bool
    Detail string
}
// 6 硬约束（见 ARCHITECTURE §7.2）：
//  1. VIP 绑 lo:0 且 /32
//  2. net.ipv4.conf.{all,lo}.arp_ignore=1 / arp_announce=2
//  3. net.ipv4.conf.{all,default,eth0}.rp_filter=0   （严格模式会丢回包）
//  4. VIP 绝不在物理网卡
//  5. VS 端口 == RS 端口
//  6. Director 与 RS 二层可达（arping）
```

**验收命令**：
```bash
# 在 RS 上特意改坏，看 Agent 上报
sysctl -w net.ipv4.conf.all.arp_ignore=0
# 期望：下一轮心跳的 ComplianceReport 中 arp_ignore=FAIL
```

**AI 陷阱**：
- `rp_filter` 严格模式（=1）会让回包源 VIP 被内核丢弃，DR 必设 0
- ARP 抑制是最易被忽略且最难排查的项（时通时断）
- 这些检查是**运行时**的；vCenter 端口组安全策略（混杂/伪传输）Agent 测不到，见 T056/§16

---

## T053 · LVS 拓扑聚合 API

**目标**：把模型 + Agent 上报聚合成前端拓扑图数据。

**依赖**：T050, T052

**涉及文件**：
```
internal/server/handler/lvs.go
internal/lvs/topology.go
```

**契约**：
```go
type Topology struct {
    Directors []DirectorNode   // 2 个，标主备与 VIP 持有
    VIP       string
    RS        []RealServerNode // 2 个，含 weight / 健康
}
// 前端据此画：Client → VIP → Director(主/备) → RS×2
```

**验收命令**：
```bash
curl -s localhost:8080/api/v1/lvs/topology | jq '.data.directors | length'   # 期望 2
curl -s localhost:8080/api/v1/lvs/topology | jq '.data.rs | length'          # 期望 2
```

**AI 陷阱**：
- 拓扑必须反映**实时** VIP 持有方（主备可能已切换），不能只存静态模型

---

## T054 · 权重摘除式灰度发布

**目标**：复用 M3 发布引擎，发布前先 `SetRealServerWeight=0` 排空，再发配置。

**依赖**：T050, M3(T030/T034)

**涉及文件**：
```
internal/deploy/strategy/lvs_graceful.go
proto/agent/v1/agent.proto       # SetRealServerWeight 已在全局契约
```

**序列**：
```
w=0 → 等待连接排空(keepalive 超时) → 下发配置 → 双层探活(80+443)
     → w=1 → 观测 60s → 再动第二台
```

**验收命令**：
```bash
make e2e TEST=lvs_graceful
# 期望：发布期间客户端零 5xx（摘除期间流量只在另一台）
```

**AI 陷阱**：
- 摘除靠 LVS 权重，不是 nginx reload；reload 不移除 upstream 成员
- 排空要等 keepalive 连接自然结束，不能硬切

---

## T055 · 发布前合规门禁

**目标**：DR 合规任一项 FAIL → 阻断该节点参与 LVS/配置发布。

**依赖**：T052, M3

**涉及文件**：
```
internal/deploy/gate.go
internal/deploy/gate_test.go
```

**契约**：
```go
func (g *Gate) Check(nodeID int) error {
    if !complianceOK(nodeID) { return ErrComplianceBlocked }
    return nil
}
// 阻断的是"发布"，不是业务；节点仍正常服务
```

**验收命令**：
```bash
go test ./internal/deploy/... -run Gate
# 期望：合规 FAIL 的节点在发布准备阶段被拦截
```

**AI 陷阱**：
- 阻断发布 ≠ 摘流量，语义要分清，别误伤在线业务

---

## T056 · 脑裂检测 + vCenter 前置清单

**目标**：两 Director 同时持 VIP → CRITICAL；并固化 vCenter 端口组三项安全策略为部署清单。

**依赖**：T050, T053

**涉及文件**：
```
internal/lvs/split_brain.go
deploy/checklist.md              # 部署强制项
```

**契约**：
```go
// 两节点都上报 holding_vip=true 且间隔 < 3s → 脑裂
// 容忍窗口 > 3s：vMotion 期间有几百毫秒中断，避免误报
func DetectSplitBrain(reports []ComplianceReport) (bool, Alert)
```

**vCenter 端口组强制项（部署清单，不可运行时修复）**：
1. Director 端口组开「**伪传输**」——LVS-DR 改目标 MAC 后源 MAC 仍是上游，被判伪造会被静默丢包（`ipvsadm` 计数正常但 RS 收不到包）
2. 开「**MAC 地址更改**」+「**混杂模式**」——VRRP 用虚拟 MAC `00:00:5E:00:01:<VRID>`，不开会双主脑裂
3. 以上只在 Director 单独端口组开，别全局开

**验收命令**：
```bash
# 模拟双主：两台都写 holding_vip
# 期望：平台 CRITICAL 告警，且 checklist.md 三项被标记为部署前置
```

**AI 陷阱**：
- 脑裂检测容忍窗口必须 > vMotion 中断时间，否则频繁误报
- vCenter 三项策略 Agent 检测不到，只能做成清单强制项

---

## T057 · LVS 管理 UI

**目标**：拓扑 SVG + VS/RS 表格 + 权重编辑 + 合规徽标 + keepalived 配置编辑器。

**依赖**：T051–T056

**涉及文件**：
```
web/src/views/lvs/{Topology,VSList,Config}.vue
web/src/components/lvs/TopoSvg.vue
web/src/components/lvs/ComplianceBadge.vue
```

**要点**：
- 拓扑图：VIP 居中，左右两 Director（主绿/备灰），下接 2 个 RS（含 weight 与圆点健康色）
- RS 行内可拖拽/输入 weight，保存即触发 T054 灰度
- 合规徽标红时，该节点「发布」按钮禁用并提示原因
- keepalived 配置编辑器复用 Monaco，保存走渲染器 + 发布流水线

**验收命令**：
```bash
cd web && npm run build && npm run typecheck
# 期望：构建通过；点击 RS 权重编辑触发灰度发布任务
```

**AI 陷阱**：
- 拓扑 SVG 节点状态来自实时上报，不是静态模型
- 合规红的节点禁用发布按钮，前端也要拦（纵深防御）
