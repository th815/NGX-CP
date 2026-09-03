# M0 · 项目地基（W1 前 2 天）

> **目标**：搭出可编译、可测试、可运行的骨架。本里程碑不写业务逻辑，只建立工程基础。
> **完成后**：`make test` 全绿，PG 与 SQLite 双路径都能跑通，`go run ./cmd/ngxcp-server` 能启动。

---

## T001 · 初始化 Go 模块与目录骨架

**目标**：建立 Go 模块、目录结构、go.mod 依赖。

**依赖**：无

**涉及文件**：
```
go.mod
go.sum
Makefile
.golangci.yml
.gitignore
cmd/ngxcp-server/main.go          （占位 main，只打印版本号）
cmd/ngxcp-agent/main.go           （占位 main）
internal/pkg/version/version.go
```

**契约**：

```go
// internal/pkg/version/version.go
package version

var (
    Version   = "dev"      // 编译时注入
    Commit    = "unknown"
    BuildTime = "unknown"
)

func String() string {
    return fmt.Sprintf("ngxcp %s (commit %s, built %s)", Version, Commit, BuildTime)
}
```

**核心依赖**（go.mod）：
```
github.com/gin-gonic/gin          HTTP 框架（或 hertz）
entgo.io/ent                      ORM
github.com/jackc/pgx/v5           PG 驱动
modernc.org/sqlite                纯 Go SQLite 驱动（避免 CGO，保证交叉编译）
github.com/rs/zerolog             结构化日志
github.com/spf13/viper            配置
github.com/stretchr/testify       测试
google.golang.org/grpc            Agent 通信
github.com/testcontainers/testcontainers-go   集成测试
```

**验收命令**：
```bash
go mod tidy
go build ./...          # 必须无错误
go vet ./...
make build
./bin/ngxcp-server --version     # 输出 ngxcp dev (commit unknown...)
./bin/ngxcp-agent --version
```

**陷阱**：
- ⚠️ **SQLite 驱动必须用 `modernc.org/sqlite`（纯 Go）**，不要用 `mattn/go-sqlite3`（需要 CGO，会破坏交叉编译与静态链接）
- ⚠️ Agent 必须能 `CGO_ENABLED=0` 编译，所以**任何依赖都不能引入 CGO**

---

## T002 · Makefile 与开发工具链

**目标**：建立统一的命令入口。

**依赖**：T001

**涉及文件**：`Makefile`

**契约**：Makefile 至少包含这些 target（AI 后续任务会用到）：

```makefile
.PHONY: dev build test lint fmt proto ent migrate-dev e2e backup clean

dev:            ## 起全套依赖 + 控制面 + 前端
build:          ## 编译控制面 + Agent（静态，CGO_ENABLED=0）
test:           ## go test ./... （PKG=xxx 可指定单包）
lint:           ## golangci-lint run
fmt:            ## gofmt -w + goimports -w -local github.com/th/ngxcp
proto:          ## protoc 生成 gRPC 代码
ent:            ## go generate ./ent
migrate-dev:    ## 应用迁移到开发库
e2e:            ## 端到端测试（需要 Docker）
backup:         ## 手动触发备份
clean:          ## 清理产物
```

**验收命令**：
```bash
make help        # 列出所有 target（需要在 Makefile 里加 help target）
make build
make lint
```

**陷阱**：
- ⚠️ Makefile 里用 tab 缩进，不是空格
- ⚠️ `build` 必须设 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64`

---

## T003 · 配置加载（viper）

**目标**：统一配置加载，环境变量优先。

**依赖**：T001

**涉及文件**：
```
internal/config/config.go
internal/config/config_test.go
configs/config.example.yaml
```

**契约**：

```go
type Config struct {
    Server   ServerConfig
    Database DatabaseConfig
    PKI      PKIConfig
    Storage  StorageConfig
    ClickHouse ClickHouseConfig `mapstructure:"clickhouse"`
    VictoriaMetrics VMConfig    `mapstructure:"victoria_metrics"`
    Log      LogConfig
    Security SecurityConfig
}

type DatabaseConfig struct {
    Driver string   // "postgres" | "sqlite"
    DSN    string
    MaxOpenConns int `mapstructure:"max_open_conns"`
    MaxIdleConns int `mapstructure:"max_idle_conns"`
}

type PKIConfig struct {
    CACert string `mapstructure:"ca_cert"`   // /etc/ngxcp/pki/ca.crt
    CAKey  string `mapstructure:"ca_key"`
    AgentCertTTL time.Duration `mapstructure:"agent_cert_ttl"`  // 默认 8760h (1年)
}

func Load(path string) (*Config, error)   // 读文件 + 环境变量覆盖
```

**环境变量映射**（前缀 `NGXCP_`，`.` → `_`）：
```
NGXCP_DB_DRIVER=postgres
NGXCP_DB_DSN=postgres://ngxcp:pwd@127.0.0.1:5432/ngxcp?sslmode=disable
NGXCP_LISTEN=:8080
NGXCP_AGENT_GRPC=:9443
NGXCP_PKI_CA_CERT=/etc/ngxcp/pki/ca.crt
NGXCP_MASTER_KEY_FILE=/etc/ngxcp/master.key
NGXCP_LOG_LEVEL=info
```

**验收命令**：
```bash
NGXCP_DB_DRIVER=sqlite NGXCP_DB_DSN="file:./dev.db" go run ./cmd/ngxcp-server --check-config
# 期望：打印解析后的配置（敏感字段打码），退出码 0

go test ./internal/config/... -v
# 覆盖：无配置文件（全用默认值）、配置文件+环境变量覆盖、非法值报错
```

**陷阱**：
- ⚠️ viper 的 `AutomaticEnv` + `SetEnvKeyReplacer(strings.NewReplacer(".", "_"))` 必须配，否则嵌套 key 读不到
- ⚠️ 敏感字段（DSN 密码、Token）打印时必须打码，**不要打到日志里**

---

## T004 · 错误处理与 API 响应封装

**目标**：建立统一的错误类型与 HTTP 响应格式。

**依赖**：T001

**涉及文件**：
```
internal/pkg/apperr/errors.go
internal/pkg/apperr/errors_test.go
internal/server/response.go
internal/server/middleware/error_handler.go
```

**契约**：

```go
package apperr

type Code int
const (
    CodeOK            Code = 0
    CodeInvalid       Code = 4001
    CodeUnauthorized  Code = 4003
    CodeForbidden     Code = 4005
    CodeNotFound      Code = 4004
    CodeConflict      Code = 4009
    CodePrecondition  Code = 4012   // nginx -t 失败
    CodeUnavailable   Code = 4103   // Agent 离线
    CodeInternal      Code = 5000
)

type Error struct {
    Code    Code
    Message string   // 面向用户的中文消息
    Detail  string   // 技术细节（错误日志、命令输出）
    Cause   error
}

func New(c Code, msg string) *Error
func Wrap(c Code, msg string, cause error) *Error
func (e *Error) WithDetail(d string) *Error
func (e *Error) Error() string
func (e *Error) Unwrap() error

// 判定辅助
func IsNotFound(err error) bool
func CodeOf(err error) Code    // 从任意 error 提取 Code，未识别返回 CodeInternal
```

```go
// internal/server/response.go
func OK(c *gin.Context, data any)
func List(c *gin.Context, items any, total int)
func Fail(c *gin.Context, err error)     // 自动从 err 提取 Code/Message/Detail
```

**验收命令**：
```bash
go test ./internal/pkg/apperr/... -v
# 覆盖：Wrap + Unwrap 链、errors.Is 判定、CodeOf 未识别降级为 Internal

# 手动验证响应格式
curl -s http://localhost:8080/api/v1/nonexistent | jq
# 期望：{"code":4004,"message":"资源不存在","detail":"..."}
```

**陷阱**：
- ⚠️ 必须实现 `Unwrap()`，否则 `errors.Is/As` 失效
- ⚠️ `Detail` 里可能含命令输出（如 nginx -t 的报错），**要做长度截断**（最多 2000 字符）

---

## T005 · 日志（zerolog 结构化）

**目标**：统一日志，支持 trace 上下文字段。

**依赖**：T001

**涉及文件**：
```
internal/pkg/logging/logging.go
internal/pkg/logging/context.go
internal/server/middleware/logging.go
```

**契约**：

```go
func Init(level string, pretty bool) error    // pretty=true 时开发态彩色输出
func Ctx(ctx context.Context) *zerolog.Logger // 从 ctx 提取带上下文字段的 logger

// 上下文字段注入（后续各模块用）
func WithNode(ctx context.Context, id int, name string) context.Context
func WithChange(ctx context.Context, id int) context.Context
func WithTask(ctx context.Context, id int) context.Context
func WithTrace(ctx context.Context, traceID string) context.Context
```

**日志格式**：
```json
{"level":"info","node_id":2,"node_name":"rs-nginx-01","change_id":15,"time":"2026-09-03T12:00:00Z","message":"config deployed"}
```

**验收命令**：
```bash
go test ./internal/pkg/logging/... -v
# 覆盖：上下文字段正确注入、Pretty/JSON 两种格式切换

NGXCP_LOG_LEVEL=debug go run ./cmd/ngxcp-server 2>&1 | head -5
# 期望：结构化 JSON 输出
```

**陷阱**：
- ⚠️ **禁止在日志里输出私钥、证书内容、Token、密码**（评审重点）
- ⚠️ zerolog 的 `Caller()` 会增加开销，生产默认关闭，debug 时开

---

## T006 · ent Schema 与数据库迁移

**目标**：定义核心实体，生成代码，跑通 PG 与 SQLite 双路径。

**依赖**：T001

**涉及文件**：
```
ent/generate.go
ent/schema/node.go
ent/schema/config_file.go
ent/schema/config_blob.go
ent/schema/config_revision.go
ent/schema/change_order.go
ent/schema/deploy_task.go
ent/schema/audit_log.go
internal/repo/client.go
```

**契约**（核心实体的关键字段，完整模型见 `docs/ARCHITECTURE.md` §5）：

```go
// ent/schema/node.go
type Node struct{ ent.Schema }

func (Node) Fields() []ent.Field {
    return []ent.Field{
        field.String("name").Unique(),              // rs-nginx-01
        field.String("address"),                     // 10.0.1.11
        field.Enum("role").
            Values("real_server", "director", "director_and_rs", "unknown"),
        field.Enum("status").
            Values("online", "offline", "degraded", "enrolling", "decommissioned"),
        field.Int("lvs_weight").Default(1),
        field.Bool("lvs_enabled").Default(true),
        field.Time("last_heartbeat_at").Optional(),
        field.Time("created_at").Default(time.Now).Immutable(),
        field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
        field.Time("deleted_at").Optional(),         // 软删除
    }
}

func (Node) Edges() []ent.Edge {
    return []ent.Edge{
        edge.To("capabilities", NodeCapability.Type),
        edge.To("snapshots", ConfigSnapshot.Type),
        edge.To("deploy_tasks", DeployTask.Type),
        edge.To("real_servers", RealServer.Type),
        edge.From("cluster", Cluster.Type).Ref("nodes").Unique(),
    }
}
```

**通用约定（所有实体）**：
- 必须有 `created_at` / `updated_at`
- 配置、节点、证书等用 `deleted_at` 软删除
- 枚举值用小写下划线

**验收命令**：
```bash
go generate ./ent                    # 生成代码，必须无错误
make migrate-dev                     # 应用迁移

# 双路径验证（关键！）
NGXCP_DB_DRIVER=sqlite NGXCP_DB_DSN="file:./dev.db?cache=shared&_fk=1" go test ./internal/repo/...
NGXCP_DB_DRIVER=postgres NGXCP_DB_DSN="postgres://ngxcp:pwd@localhost:5432/ngxcp?sslmode=disable" go test ./internal/repo/...
# 两条都必须通过
```

**陷阱**：
- ⚠️ **每个数据库相关任务都要验证 SQLite + PG 双路径**，不要只测一个
- ⚠️ SQLite 要开 WAL 与外键：`file:xxx.db?cache=shared&_fk=1`，并在 client 初始化时执行 `PRAGMA journal_mode=WAL`
- ⚠️ ent 的 `field.Enum` 在 SQLite 下是 TEXT 类型，在 PG 下是 varchar，迁移生成时要两边都验证
- ⚠️ `go generate` 会覆盖 `ent/` 下的生成文件，**手写代码放 `internal/repo/`，不要放 `ent/`**

---

## M0 集成验收

```bash
make build && make lint && make test
docker compose up -d postgres          # 起 PG
make migrate-dev                        # 迁移
NGXCP_DB_DRIVER=sqlite NGXCP_DB_DSN="file:./dev.db?cache=shared&_fk=1" make migrate-dev
go run ./cmd/ngxcp-server --check-config
```

**全部通过后才进入 M1。**
