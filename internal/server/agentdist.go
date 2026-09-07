package server

import (
	"embed"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/domain/node"
	"github.com/th/ngxcp/internal/pkg/pki"
)

// embed 自注册分发资产：节点侧安装脚本与 web 接入控制台（单一事实来源，运行时不依赖外部文件）。
//
//go:embed install_agent.sh join_console.html
var agentDistFS embed.FS

// agentDist 承载「节点自注册」相关路由：安装脚本、引导 CA、二进制下载、控制台页面、Join Token 签发。
type agentDist struct {
	ca         *pki.CA
	distDir    string // 二进制分发目录（make dist 填充）；空 = 禁用下载
	grpcListen string // 控制面 gRPC 监听（如 :9443），用于推导 Agent 可达的公网地址 host:port
	nodeSvc    *node.Service
}

// registerAgentDistribution 挂载自注册分发路由。
// auth 为 Bearer 鉴权中间件（节点创建 / Join Token 签发需鉴权；安装脚本/CA/二进制/控制台页均公开）。
func registerAgentDistribution(r *gin.Engine, ca *pki.CA, distDir, grpcListen string, nodeSvc *node.Service, auth gin.HandlerFunc) {
	ad := &agentDist{ca: ca, distDir: distDir, grpcListen: grpcListen, nodeSvc: nodeSvc}

	r.GET("/agent/install.sh", ad.serveInstallScript)
	r.GET("/agent/ca.crt", ad.serveCA)
	r.GET("/agent/bin/:file", ad.serveBinary)
	r.GET("/agent/", ad.serveConsole)
	// 前端 SPA 拼装「一键安装命令」所需的引导信息（公开只读，不含机密）。
	r.GET("/api/v1/agent/bootstrap-info", ad.serveBootstrapInfo)

	// 新建节点走 router.go 的 ns.POST("")（node.Create），签发 Join Token 走下方
	// :id/join-token 路由；Web 控制台按「两步」调用，避免与 router 的 /api/v1/nodes 冲突。
	// 为已存在节点重新签发 Join Token（令牌过期 / 需吊销旧令牌时）。
	r.POST("/api/v1/nodes/:id/join-token", auth, ad.rotateJoinToken)
	// 独立吊销节点 Join Token（只吊销、不签发；令牌泄漏 / 节点下线安全响应）。
	r.POST("/api/v1/nodes/:id/join-token/revoke", auth, ad.revokeJoinToken)
}

// serveInstallScript 提供节点侧自安装脚本（公开，引导用）。
func (ad *agentDist) serveInstallScript(c *gin.Context) {
	data, err := agentDistFS.ReadFile("install_agent.sh")
	if err != nil {
		c.String(http.StatusInternalServerError, "install script missing")
		return
	}
	c.Header("Content-Type", "text/x-shellscript; charset=utf-8")
	c.Data(http.StatusOK, "text/x-shellscript; charset=utf-8", data)
}

// serveCA 提供控制面 CA 证书（公开，Agent 注册握手信任用，非机密）。
func (ad *agentDist) serveCA(c *gin.Context) {
	c.Header("Content-Type", "application/x-pem-file")
	c.Data(http.StatusOK, "application/x-pem-file", ad.ca.CACertPEM())
}

// serveBinary 提供预编译 Agent 二进制下载（公开）。按 basename 防目录穿越。
func (ad *agentDist) serveBinary(c *gin.Context) {
	if ad.distDir == "" {
		c.String(http.StatusNotFound, "二进制分发未启用（控制面 agent_dist_dir 为空）")
		return
	}
	name := filepath.Base(c.Param("file")) // 仅取文件名，杜绝 ../../ 穿越
	path := filepath.Join(ad.distDir, name)
	f, err := os.Open(path)
	if err != nil {
		c.String(http.StatusNotFound, "二进制缺失：请先 `make dist`（期望 %s）", path)
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", "attachment; filename="+name)
	c.Header("Content-Length", strconv.FormatInt(info.Size(), 10))
	c.File(path)
}

// serveConsole 提供 web 接入控制台（公开页面；页内提示输入管理员令牌调用签发接口）。
// 把请求来源与 gRPC 地址注入页面占位符，使生成的一行命令自带正确地址。
func (ad *agentDist) serveConsole(c *gin.Context) {
	html, err := agentDistFS.ReadFile("join_console.html")
	if err != nil {
		c.String(http.StatusInternalServerError, "console page missing")
		return
	}
	origin := publicOrigin(c)
	grpc := grpcPublicAddr(c, ad.grpcListen)
	rendered := strings.NewReplacer("__ORIGIN__", origin, "__GRPC__", grpc).Replace(string(html))
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(rendered))
}

// serveBootstrapInfo 返回前端拼装一键安装命令所需的引导信息：控制面基址、Agent 可达
// gRPC 地址、二进制分发是否就绪。与 /agent/ 控制台注入同源（publicOrigin / grpcPublicAddr），
// 公开只读且不含机密——Agent 下载与 CA 本身即为公开引导材料，鉴权由 Join Token 承载。
func (ad *agentDist) serveBootstrapInfo(c *gin.Context) {
	ready := false
	if ad.distDir != "" {
		// 以 amd64 产物存在性代表分发目录已就绪（install.sh 默认按架构取同名文件）。
		if _, err := os.Stat(filepath.Join(ad.distDir, "ngxcp-agent-linux-amd64")); err == nil {
			ready = true
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"origin":       publicOrigin(c),
		"grpc_addr":    grpcPublicAddr(c, ad.grpcListen),
		"binary_ready": ready,
	}})
}

// rotateJoinToken 为已存在节点重新签发节点绑定 Join Token（令牌过期 / 需吊销旧令牌时）。
// 查询参数 ttl 指定新有效期（默认 24h）。轮换即显式吊销旧令牌（RevokeNodeJoinTokens 置
// revoked，即时生效），再签发新令牌——与「只吊销不签发」的 revokeJoinToken 端点互补。
func (ad *agentDist) rotateJoinToken(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "非法节点 ID"}})
		return
	}
	n, err := ad.nodeSvc.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "节点不存在"}})
		return
	}
	ttl := 24 * time.Hour
	if q := c.Query("ttl"); q != "" {
		if d, perr := time.ParseDuration(q); perr == nil {
			ttl = d
		}
	}
	// 轮换即吊销旧令牌：吊销该节点所有未吊销令牌，使旧令牌立即失效（无需等过期）。
	if err := ad.nodeSvc.RevokeNodeJoinTokens(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}
	tok, exp, err := ad.nodeSvc.IssueJoinToken(c.Request.Context(), id, string(n.Role), ttl)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"token": tok, "expires_at": exp}})
}

// revokeJoinToken 吊销该节点所有尚未吊销的 Join Token（只吊销、不签发）。
// 与 rotateJoinToken（吊销旧 + 签发新）互补，用于安全事件响应：令牌疑似泄漏或节点
// 下线时，吊销后旧令牌立即失效（revoked 即时生效），Agent 持旧令牌注册将被拒。
func (ad *agentDist) revokeJoinToken(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "非法节点 ID"}})
		return
	}
	if _, err := ad.nodeSvc.Get(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "节点不存在"}})
		return
	}
	if err := ad.nodeSvc.RevokeNodeJoinTokens(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"revoked": id}})
}

// publicOrigin 还原请求的公网来源（兼容反向代理的 X-Forwarded-* 头）。
func publicOrigin(c *gin.Context) string {
	scheme := c.GetHeader("X-Forwarded-Proto")
	if scheme == "" {
		if c.Request.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := c.GetHeader("X-Forwarded-Host")
	if host == "" {
		host = c.Request.Host
	}
	return scheme + "://" + host
}

// grpcPublicAddr 推导 Agent 可达的 gRPC 地址（host:port）：用请求来源的主机 + 控制面 gRPC 监听端口。
// cfg.AgentGRPC 通常为 :9443（只含端口），Agent 需要完整 host:port 才能拨号，故以请求主机补全。
func grpcPublicAddr(c *gin.Context, grpcListen string) string {
	origin := publicOrigin(c)
	if u, err := url.Parse(origin); err == nil && u.Hostname() != "" {
		host := u.Hostname()
		port := "9443"
		if grpcListen != "" {
			if i := strings.LastIndex(grpcListen, ":"); i >= 0 {
				port = grpcListen[i+1:]
			}
		}
		return host + ":" + port
	}
	return grpcListen
}
