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
	entnode "github.com/th/ngxcp/ent/node"
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

	// 「web 一键自注册」：新建节点 → 为该节点签发节点绑定 Join Token（持久化于 Agent 侧）。
	r.POST("/api/v1/nodes", auth, ad.createNodeWithToken)
	// 为已存在节点重新签发 Join Token（令牌过期 / 需吊销旧令牌时）。
	r.POST("/api/v1/nodes/:id/join-token", auth, ad.rotateJoinToken)
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

// createNodeWithToken 实现「web 一键自注册」：新建节点（enrolling）→ 为该节点签发
// 节点绑定 Join Token（入库 join_tokens 表，原文持久化于 Agent 侧 /etc/ngxcp/agent.conf）→
// 返回 token + 过期时间。前端据此拼出一行安装命令。无审批：节点随后凭令牌自注册即上线。
func (ad *agentDist) createNodeWithToken(c *gin.Context) {
	var in struct {
		Name string `json:"name" binding:"required"`
		Role string `json:"role"`
		TTL  string `json:"ttl"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "缺少节点名称（name）"}})
		return
	}
	role := in.Role
	if role == "" {
		role = "real_server"
	}
	if err := entnode.RoleValidator(entnode.Role(role)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "非法 role: " + role}})
		return
	}
	ttl := 24 * time.Hour
	if in.TTL != "" {
		if d, perr := time.ParseDuration(in.TTL); perr == nil {
			ttl = d
		}
	}
	n, err := ad.nodeSvc.Create(c.Request.Context(), node.CreateNodeIn{Name: in.Name, Role: role})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}
	tok, exp, err := ad.nodeSvc.IssueJoinToken(c.Request.Context(), n.ID, role, ttl)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"node_id":    n.ID,
		"name":       n.Name,
		"role":       role,
		"token":      tok,
		"expires_at": exp,
	}})
}

// rotateJoinToken 为已存在节点重新签发节点绑定 Join Token（令牌过期 / 需吊销旧令牌时）。
// 查询参数 ttl 指定新有效期（默认 24h）。旧令牌随即失效（签名绑定到新 nodeID 会话，
// 但控制面无状态，实际由「重新注册需新令牌」语义保证——运营商轮换即等于吊销旧令牌）。
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
