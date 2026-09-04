# M4 · 证书管理（W7）

> **目标**：统一管理证书生命周期——ACME 自动签发（Cloudflare DNS-01）+ 手动上传校验 + 安全分发 + 自动续期。
> **完成标志**：测试证书自动续期分发到 4 个节点，且 `nginx -t` 全通过；手动上传一张缺 intermediate 的证书被 6 项校验拦截。
>
> 决策依据：`docs/DECISIONS.md`（证书来源 / DNS Provider）、`docs/ARCHITECTURE.md` §证书子系统。
> 安全红线（AGENTS §9.2）：**私钥永不下发浏览器、不进配置版本历史**；下载私钥需二次确认 + 审计。

---

## T040 · 证书数据模型与加密存储

> **状态：✅ 已完成**
> 交付：`ent/schema/certificate.go`（证书实体，元数据与私钥分离，`enc_private_key`/`enc_full_chain` 为信封加密 blob，API 永不回传私钥）、`ent/schema/cert_deployment.go`（逐节点分发状态，外键 `certificate` Unique+Required）；`internal/crypto/kms.go` 信封加密（AES-256-GCM，随机 DEK + KEK 包装，主密钥取 `NGXCP_MASTER_KEY` 或 `/etc/ngxcp/master.key` hex，绝硬编码）+ `kms_test.go`（round-trip/错误密钥失败/nonce 唯一/非法长度拒绝/环境变量载入 共 5 例）。`go generate ./ent` 已重新生成运行时；`go vet`/`go test ./internal/crypto/...`/全仓 `go build` 通过。

**目标**：定义证书实体，私钥与元数据分离存储，密钥信封加密。

**依赖**：T006

**涉及文件**：
```
ent/schema/certificate.go
ent/schema/cert_deployment.go
internal/crypto/kms.go            # 信封加密：AES-GCM + 主密钥
internal/crypto/kms_test.go
```

**契约**：
```go
// 元数据表：API 只返回这些字段，绝不返回私钥
type Certificate struct {
    ID              int
    Domain          string        // 主域名，如 example.com
    SAN             []string      // 所有 subjectAltName
    Issuer          string        // "Let's Encrypt" / "Upload"
    SerialNumber    string
    FingerprintSHA  string        // SHA-256 指纹
    NotBefore, NotAfter time.Time
    KeyAlg          string        // RSA-2048 / ECDSA-P256
    Source          string        // "acme" | "upload"
    Status          string        // "valid" | "expired" | "revoked" | "error"
    CreatedAt       time.Time
    // 私钥与链：单独存 blob 表，按 envelope 加密，不进 config_revision 历史
    EncPrivateKey   []byte        // AES-GCM(envelope)
    EncFullChain    []byte
}

// 每个节点上的分发状态
type CertDeployment struct {
    ID            int
    CertificateID int
    NodeID        int
    Status        string          // "pending" | "deployed" | "failed"
    DeployedAt    *time.Time
    Error         string
}
```

**验收命令**：
```bash
make generate && go test ./ent/... ./internal/crypto/...
# 期望：全部通过；用测试主密钥加密/解密一个样本私钥，round-trip 一致
```

**AI 陷阱**：
- 私钥绝不能进 `config_blob` 版本历史（见 AGENTS §9.2）
- 主密钥从 `NGXCP_MASTER_KEY` 环境变量或 `/etc/ngxcp/master.key`（0600）读取，**不要硬编码**
- 加密用 AES-GCM 需随机 nonce，每条记录独立 nonce，不要复用

---

## T041 · DNS Provider 抽象 + Cloudflare 实现

**目标**：抽象 DNS-01 校验所需的 TXT 记录操作，v1 实现 Cloudflare，预留 Aliyun。

**依赖**：T006

**涉及文件**：
```
internal/dns/provider.go        # DNSProvider 接口
internal/dns/cloudflare.go
internal/dns/cloudflare_test.go # 用 httptest 模拟 CF API
internal/dns/registry.go        # provider 注册表
```

**契约**：
```go
type DNSProvider interface {
    Name() string
    SetRecord(ctx context.Context, zone, name, value string) error   // _acme-challenge.<domain> TXT
    DeleteRecord(ctx context.Context, zone, name string) error
    Validate(ctx context.Context) error                               // 测试 API 凭据有效性
}

// Cloudflare API Token 最小权限：Zone.Zone:Read + Zone.DNS:Edit，限定到具体 zone
// Token 加密存 SQLite；master key 同 T040
```

**验收命令**：
```bash
go test ./internal/dns/...
# 期望：用 httptest 桩验证 SetRecord 发了正确请求、Validate 正确解析权限错误
```

**AI 陷阱**：
- CF 前置会拦截 HTTP-01，本项目**只走 DNS-01**（见 DECISIONS 证书来源）
- `name` 是 `_acme-challenge.<domain>`，不是根域名
- Token 权限过大是安全事故面，严格按最小权限

---

## T042 · ACME 客户端（DNS-01 + 通配符）

**目标**：对接 Let's Encrypt，完成 order → CSR → 校验 → 签发，存入信封加密的证书表。

**依赖**：T040, T041

**涉及文件**：
```
internal/acme/client.go          # 封装 lego 或 autocert 的 DNS-01 流程
internal/acme/client_test.go     # 用 pebble (ACME 测试 CA) 做 e2e
internal/acme/ratelimit.go       # 本地限流，避免触发 LE 50 张/周 限制
```

**契约**：
```go
func (c *Client) Issue(ctx context.Context, req IssueRequest) (*Certificate, error)
// req: Domains []string（含 *.example.com）, Provider DNSProvider, KeyAlg string

// 关键：签发成功后立即走 T045 的续期登记；调试时 ratelimit 拦截重复签发
```

**验收命令**：
```bash
# 起一个本地 pebble CA，跑签发 e2e
docker run -d -p 14000:14000 pebble   # 测试 CA
go test ./internal/acme/... -tags e2e
# 期望：成功签发一张测试证书，写库，fingerprint 非空
```

**AI 陷阱**：
- Let's Encrypt 生产限流 50 张/域名/周，**调试必须走 pebble/ staging**，否则自锁
- 通配符证书只能用 DNS-01，不能退化到 HTTP-01
- 签发失败的 order 要清理 challenge 记录，否则 TXT 残留

---

## T043 · 手动上传 + 6 项校验

**目标**：上传 PEM 时做完整性/安全性校验，任一不过即拒绝并给出明确原因。

**依赖**：T040

**涉及文件**：
```
internal/cert/validate.go
internal/cert/validate_test.go   # 构造 6 种失败样本
internal/server/handler/cert.go
```

**契约**：
```go
type ValidationResult struct {
    OK      bool
    Errors  []string   // 人类可读，前端直接展示
}
func ValidateUpload(certPEM, keyPEM, chainPEM []byte) ValidationResult
// 6 项：私钥/证书模数匹配、链完整、链顺序(leaf→inter→不含root)、
//       SAN 覆盖引用它的 server_name、有效期(已过期/剩余<7天标红)、签名算法(SHA-1/MD5 拒绝)
```

**验收命令**：
```bash
go test ./internal/cert/...
# 期望：6 个失败样本各自被精确拦截，错误信息明确
```

**AI 陷阱**：
- 链顺序校验时 root CA 不应出现在文件里（只发 leaf+intermediate）
- 模数比对用公钥，不是私钥
- "剩余 < 7 天"是**警告**不是拒绝，区分 OK=false 与 warning

---

## T044 · 证书分发到节点

**目标**：通过 Agent `DeployCert` 指令安全落盘，复用发布流水线的原子语义。

**依赖**：T040, M1(gRPC)

**涉及文件**：
```
proto/agent/v1/agent.proto       # DeployCert 已在全局契约定义
internal/agent/dispatch.go       # 下发逻辑
internal/server/handler/cert.go  # 触发分发，写 CertDeployment
```

**Agent 端执行序列（原子）**：
```
mTLS 拉取 → 内存解密 envelope → 写 /etc/nginx/ssl/<domain>.{crt,key}
  → chmod 0644 crt / 0600 key → chown root:root
  → 同分区 rename 原子切换 → nginx -t → reload → 探活(443 TLS 握手)
```

**验收命令**：
```bash
# 在节点侧观察
ls -l /etc/nginx/ssl/example.com.*   # 期望 crt 0644 key 0600 root:root
nginx -t && echo OK
openssl s_client -connect 127.0.0.1:443 </dev/null 2>/dev/null | openssl x509 -noout -subject
```

**AI 陷阱**：
- 私钥**永不下发浏览器**：API 只返回元数据，下载私钥需二次确认 + 审计留痕
- 用变量引用证书路径（`ssl_certificate $host.crt`）会导致每次握手读盘，**用静态路径 + reload**
- 不同分区 rename 不是原子的，必须同分区

---

## T045 · 自动续期调度

**目标**：到期前 30 天起每日检查，续期成功则创建 `source='cert-renew'` 变更单走完整发布流水线。

**依赖**：T042, T044, M3(变更单)

**涉及文件**：
```
internal/cert/renew_scheduler.go
internal/cert/renew_scheduler_test.go
```

**契约**：
```go
// 每日 03:00 触发
func (s *Scheduler) Run(ctx context.Context) {
    for _, c := range dueCerts(30 * 24 * time.Hour) {
        if renewed, err := acme.Renew(c); err == nil {
            createChangeOrder(source="cert_renew", certID=c.ID) // 走 M3 流水线
        } else {
            failStreak[c.ID]++
            if failStreak[c.ID] >= 3 { alert(CRITICAL) }   // 连续 3 天失败升级
        }
    }
}
```

**验收命令**：
```bash
go test ./internal/cert/... -run Renew
# 期望：模拟到期证书触发续期并创建变更单；连续失败达阈值触发 CRITICAL
```

**AI 陷阱**：
- 续期**必须走发布流水线**（校验/灰度/探活/回滚），不能旁路直接写盘
- 注意 LE 限流，续期也受 50 张/周 约束，平台侧本地限流

---

## T046 · 证书管理 UI

**目标**：列表 / 详情 / 上传 / 手动续期 / nginx 1.30 QUIC 配置提示。

**依赖**：T040–T045

**涉及文件**：
```
web/src/views/certs/{List,Detail,Upload}.vue
web/src/components/cert/ExpiryBadge.vue
web/src/api/cert.ts
```

**要点**：
- 列表展示域名/SAN/签发者/到期日（<7天红 / <30天黄）/ 来源徽标；私钥状态用锁图标
- 详情只展示元数据 + 指纹 + 分发节点状态；「下载私钥」按钮需二次确认弹窗
- 上传表单实时显示 6 项校验结果
- 对 nginx 1.30 给出 QUIC 配置片段建议：
```nginx
listen 443 quic reuseport;  listen 443 ssl;  http2 on;
ssl_certificate     /etc/nginx/ssl/example.com.crt;
ssl_certificate_key /etc/nginx/ssl/example.com.key;
add_header Alt-Svc 'h3=":443"; ma=86400' always;
```

**验收命令**：
```bash
cd web && npm run build && npm run typecheck
# 期望：构建通过；上传一张坏证书时 6 项错误逐条显示
```

**AI 陷阱**：
- 详情页 **绝不** 回传私钥内容，前端也不要预留「显示私钥」接口
- 过期计算务必用 UTC，展示再转本地（见 AGENTS §9.3）
