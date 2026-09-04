# NGX-CP · AI 任务清单

> **本目录是 AI 全程编写的工作分解。每个里程碑一个文件，单个文件可在一次 AI 会话内读完。**

---

## 0. 如何使用这份清单

### 0.1 给 AI 的会话提示词模板

```
角色：你是 NGX-CP 项目的开发工程师（Go 全栈）。

背景：NGX-CP 是管理「2 Keepalived Director（主备 / LVS-DR）+ 2 Nginx RealServer」
的配置管理平台。核心是把配置变更做成「可校验 → 可灰度 → 可观测 → 可回滚」的流水线。
运行环境：vSphere 虚拟化（vCenter）、全万兆、VDS LACP。

请先按顺序阅读：
1. <项目根>/AGENTS.md                    ← 项目宪法，必读
2. <项目根>/docs/tasks/README.md          ← 本文件
3. <项目根>/docs/tasks/M1-skeleton.md     ← 当前里程碑

然后从任务 T010 开始执行。

执行要求：
- 严格按任务里的「涉及文件」与「接口契约」实现，不要自由发挥架构
- 每个任务完成后运行「验收命令」，把真实输出贴给我
- 遇到需要决策的地方先说明再停下，不要自行猜测
- 一个任务做完再开下一个；如果一个任务要改超过 8 个文件，先停下拆分
```

### 0.2 任务执行流程（AI 每个任务都走一遍）

```
1. 复述目标        → 一句话确认理解一致
2. 列出文件清单    → 要创建/修改哪些文件
3. 读现有代码      → 不许凭空猜测现有实现
4. 实现            → 按契约写
5. 跑验收命令      → 必须真实执行，贴输出
6. 更新状态        → 在任务文件里 [ ] → [x]
7. 总结            → 做了什么 / 验证了什么 / 遗留什么
```

### 0.3 粒度红线

| 信号 | 动作 |
| --- | --- |
| 一个任务要改 > 8 个文件 | **停下，拆任务** |
| 单个文件 > 400 行 | **停下，按职责拆** |
| 单个函数 > 60 行 | 提取子函数 |
| 需要在不确定的设计上做选择 | **停下问用户** |
| 发现架构文档与实现冲突 | **停下报告**，不改架构 |

---

## 1. 全局契约（三条，必须先定死）

**契约先行是这个项目能被 AI 并行开发的前提。** 三条契约定了之后，不同模块可以在不同会话里并行实现而不打架。

### 1.1 控制面 ↔ Agent：gRPC

- 文件：`proto/agent/v1/agent.proto`
- 生成：`make proto` → `gen/agent/v1/*.pb.go`
- 传输：gRPC 双向流 + mTLS，Agent 主动外连（节点不开放入站端口）
- **安全红线：只暴露预定义指令，绝不提供 `ExecCommand(cmd string)`**

指令清单（完整定义见 `docs/ARCHITECTURE.md` §4）：

```protobuf
service AgentService {
  rpc Register(RegisterRequest) returns (RegisterResponse);        // 一次性令牌换取 mTLS 证书
  rpc Heartbeat(stream HeartbeatRequest) returns (stream HeartbeatResponse);  // 双向流
  rpc ReportCapability(CapabilityReport) returns (Ack);            // 能力上报
  rpc SyncConfig(SyncConfigRequest) returns (stream Progress);     // 原子落盘 + reload
  rpc ValidateConfig(ValidateRequest) returns (ValidateResponse);  // nginx -t
  rpc CreateSnapshot(SnapshotRequest) returns (SnapshotResponse);
  rpc RestoreSnapshot(RestoreRequest) returns (Progress);
  rpc DeployCert(DeployCertRequest) returns (Progress);
  rpc SetRealServerWeight(WeightRequest) returns (Ack);            // LVS 摘除/加回
  rpc CheckDRCompliance(Empty) returns (ComplianceReport);
  rpc UpgradeBinary(UpgradeRequest) returns (stream Progress);     // 热升级
  rpc StreamLogs(Empty) returns (stream LogBatch);                 // 日志上传
}
```

### 1.2 内部 REST API：OpenAPI

- 文件：`docs/api/openapi.yaml`
- 前端代码由它生成：`web/src/api/types.ts` + 客户端
- 响应统一格式：`{ code, message, detail, data }`（见 AGENTS.md §4.1）

### 1.3 数据库 Schema：ent

- 文件：`ent/schema/*.go`（唯一事实来源）
- 生成：`go generate ./ent`
- 迁移：`make migrate-dev`
- **不裸写 SQL**；复杂查询放 `internal/repo/`

---

## 2. 里程碑总览

| 里程碑 | 内容 | 任务数 | 周期 | 依赖 | 完成标志 |
| --- | --- | --- | --- | --- | --- |
| **M0** 项目地基 | 骨架、配置、数据库、错误处理 | 6 | W1 前 2 天 | — | `make test` 全绿，PG + SQLite 双跑通 |
| **M1** 骨架与接入 | gRPC、mTLS、Agent 注册/心跳/能力发现 | 11 | W1–W2 | M0 | **4 个节点全部在线，掉线 30s 内感知** |
| **M2** 配置中心 | 配置树、版本链、Diff、校验、漂移 | 9 | W3–W4 | M1 | 改一个 conf 能看到 diff 与校验结果；写错被拦下 |
| **M3** 发布引擎 | 变更单、快照、原子落盘、探活、回滚、LVS 灰度 | 10 | W5–W6 | M2 | **发错误配置 → 零节点污染 → 自动回滚** |
| **M4** 证书管理 | ACME/CF、上传校验、分发、自动续期 | 7 | W7 | M3 | 测试证书自动续期分发 |
| **M5** LVS 管理 | Keepalived 渲染、DR 巡检、拓扑、权重 | 8 | W8 | M3 | 手工改坏 `arp_ignore` 被检出并阻断发布 |
| **M6** 日志与安全 | 采集、ClickHouse、检索、TraceID、规则引擎、封禁 | 11 | W9–W10 | M3 | 输入 request_id 查到全链路；注入触发封禁且可回滚 |
| **M7** 监控 | Agent /metrics、VM、Grafana、告警汇聚 | 8 | W11 | M1 | Grafana 有数据；告警汇总到平台告警中心 |
| **M8** 构建与升级 | 版本矩阵、容器化构建、热升级、LVS 联动 | 9 | W12 | M3 | nginx 补丁版升级全程零 5xx |
| **M9** 备份与运维 | 备份脚本、恢复演练、设置页、审计 | 6 | W13 | M3 | 恢复演练通过（PITR） |

**关键路径**：M0 → M1 → M2 → M3 → （M4/M5/M6/M7/M8 可并行）→ M9

**建议的最小可用闭环**：M0+M1+M2+M3 —— 做完这四个，平台已经能安全地管理配置了。剩下的是增值模块，**做完 M3 就应该先投入实际使用，再迭代**。

---

## 3. 进度看板

> AI 每完成一个任务，把对应的 `- [ ]` 改成 `- [x]`，并在后面标注完成日期。

| 里程碑 | 任务 | 状态 |
| --- | --- | --- |
| M0 | T001–T006 | ⬜ 未开始 |
| M1 | T010–T020 | ⬜ 未开始 |
| M2 | T020–T028 | ⬜ 未开始 |
| M3 | T030–T039 | ✅ 完成（T030–T039 全部 ✅，含 T039 发布页面与集成验收 → 最小可用闭环达成） |
| M4 | T040–T046 | ⬜ 未开始 |
| M5 | T050–T057 | ⬜ 未开始 |
| M6 | T060–T070 | ⬜ 未开始 |
| M7 | T071–T078 | ⬜ 未开始 |
| M8 | T080–T088 | ⬜ 未开始 |
| M9 | T090–T095 | ⬜ 未开始 |

---

## 4. 里程碑间的集成验收

每个里程碑结束时跑一次，全部通过才进入下一个。

```bash
# M0
make test && make lint && go run ./cmd/ngxcp-server --check-config

# M1（4 个节点全部在线）
curl -s http://localhost:8080/api/v1/nodes | jq '.data.items | length'   # 期望 4
curl -s http://localhost:8080/api/v1/nodes | jq -r '.data.items[].role'  # 期望 2 director + 2 real_server
# 停掉一个 Agent，30 秒内检查状态变为 offline

# M2
curl -s -X POST http://localhost:8080/api/v1/nodes/1/config/validate -d @bad.conf
# 期望：返回语法错误，且不落盘

# M3（最关键）
make e2e
# 期望：故意下发语法错误的配置 → 零节点被污染 → 自动回滚 → 有审计记录

# M4
# 手动触发一次测试证书签发 → 分发 → 检查 4 个节点文件落盘且 nginx -t 通过

# M5
# 在 RS 上 sysctl -w net.ipv4.conf.all.arp_ignore=0
# 期望：5 分钟内节点标红，且新建 LVS 发布被阻断

# M6
curl -s "http://localhost:8080/api/v1/logs/trace/8f3c1a9b" | jq
# 期望：返回该 request_id 的全链路日志

# M7
curl -s "http://localhost:8428/api/v1/query?query=up" | jq '.data.result | length'  # 期望 ≥ 4

# M8
# 下发一个补丁版 nginx 升级 → 全程 zero 5xx → 检查 nginx -V 已更新

# M9
./scripts/backup.sh && ./scripts/restore.sh --dry-run
```

---

## 5. 关键提醒

### 5.1 动手前必须确认的两件事

1. **vCenter 侧的端口组安全策略已配置**（Director 端口组开「混杂模式 + MAC 地址更改 + 伪传输」）
   —— 没配的话 M5 阶段 LVS 完全不通，而且表象极具误导性（`ipvsadm` 计数正常但 RS 收不到包）
2. **物理交换机是否堆叠 / 支持 MLAG** —— 决定了 VDS 的 LACP 能否正常工作

详见 `docs/DECISIONS.md` §16。

### 5.2 测试数据的真实环境样例

`testdata/` 必须包含用户真实环境的输出样例（AI 不要编造）：

```
testdata/nginx_V_1.30.0.txt          ← 用户提供的 nginx -V 输出
testdata/nginx_T_dump.txt            ← nginx -T 完整输出（含文件边界标记）
testdata/keepalived_master.conf      ← 主 Director 配置
testdata/keepalived_backup.conf      ← 备 Director 配置
testdata/ipvsadm_Ln.txt              ← ipvsadm -Ln 输出
testdata/access_log_sample.jsonl     ← JSON 格式访问日志样例
```

### 5.3 常见 AI 失误（这个项目的特有陷阱）

| 失误 | 正确做法 |
| --- | --- |
| 用 `cp` 直接替换配置文件 | 必须 staging → `nginx -t` → 同分区 rename |
| 只看 `reload` 返回码判断成功 | reload 失败时老进程继续跑，**必须探活** |
| 让 Agent 提供 `ExecCommand(cmd)` | **绝对禁止**，只暴露预定义指令 |
| 单独对 conf.d/xxx.conf 做 `nginx -t` | 必须用完整上下文 `nginx -t -p <prefix> -c <conf>` |
| 日志采集忘记处理 logrotate | 监控 inode 变化，轮转后重开文件 |
| 把私钥放进配置版本历史 | 私钥存独立表，**不进 config_blob** |
| ClickHouse 单条插入 | 必须攒批（async_insert，1000 条 / 5s） |
| 忘记设 ClickHouse `max_memory_usage` | 默认吃 90% 系统内存，必须设 6G |
| Agent 用 CGO 编译 | `CGO_ENABLED=0` 静态编译，否则目标机 glibc 不匹配 |
| 时间用本地时区存储 | **统一 UTC 存储，展示时转本地** |

完整陷阱清单见 `AGENTS.md` §9。
