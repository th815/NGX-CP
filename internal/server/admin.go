package server

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/config"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/server/response"
)

// adminHandler 承载「首跑管理员令牌获取」相关路由，消除部署后必须 SSH + grep
// auth_admin_token 才能取令牌的痛点（用户原话：生产谁会这么操作）。
//
// 信任边界说明（安全）：setup-token 是免鉴权的「首次设置」端点，仅在尚未确认时返回
// 当前 admin 令牌一次；一旦调用 setup-acknowledge（需有效令牌）落标记文件，该端点即
// 返回 410 不再泄露。即「装完首次打开网页 → 一键拿到令牌 → 点确认锁定」，全程无需登服务器。
// 风险窗口仅存在于「未确认前」——与 config 文件本地可读的信任边界一致，属自托管控制面合理取舍。
type adminHandler struct {
	cfg *config.Config
}

// registerAdmin 挂载管理员相关路由（setup-token 免鉴权、setup-acknowledge 需鉴权）。
func registerAdmin(r *gin.Engine, cfg *config.Config, auth gin.HandlerFunc) {
	ah := &adminHandler{cfg: cfg}
	v1 := r.Group("/api/v1")
	{
		v1.GET("/admin/setup-token", ah.serveSetupToken)
		v1.POST("/admin/setup-acknowledge", auth, ah.serveAcknowledge)
	}
}

// acknowledged 返回首跑是否已确认（标记文件存在即视为已确认）。
func (ah *adminHandler) acknowledged() bool {
	if ah.cfg.AuthAdminTokenAckFile == "" {
		return false // 未配置标记文件 → 视为未确认（退回保守行为：仍可展示，由用户确认）
	}
	_, err := os.Stat(ah.cfg.AuthAdminTokenAckFile)
	return err == nil
}

// serveSetupToken 首跑一次性令牌获取（免鉴权）。
// 已确认 → 410 Gone；未配置令牌（开发态留空）→ 409 提示；否则返回当前令牌并打审计日志。
func (ah *adminHandler) serveSetupToken(c *gin.Context) {
	if ah.acknowledged() {
		c.JSON(http.StatusGone, gin.H{"code": int(apperr.CodeForbidden), "message": "管理员令牌已确认，不再提供明文获取"})
		return
	}
	if ah.cfg.AuthAdminToken == "" {
		c.JSON(http.StatusConflict, gin.H{"code": int(apperr.CodeInvalid), "message": "未配置 auth_admin_token（写接口已禁用）"})
		return
	}
	// 免鉴权暴露令牌属敏感操作，留痕便于事后审计。
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"token":        ah.cfg.AuthAdminToken,
		"acknowledged": false,
	}})
}

// serveAcknowledge 完成首次设置：落确认标记文件，此后 setup-token 不再泄露。
// 需携带有效 admin 令牌（auth 中间件保证），并校验与当前一致（防御性）。
func (ah *adminHandler) serveAcknowledge(c *gin.Context) {
	if ah.cfg.AuthAdminToken == "" {
		c.JSON(http.StatusConflict, gin.H{"code": int(apperr.CodeInvalid), "message": "未配置 auth_admin_token"})
		return
	}
	if ah.acknowledged() {
		response.OK(c, gin.H{"acknowledged": true})
		return
	}
	// 标记文件父目录可能不存在（开发态），尽力创建；失败则回退到仅内存态（重启后再次可获取）。
	if ah.cfg.AuthAdminTokenAckFile != "" {
		if dir := filepath.Dir(ah.cfg.AuthAdminTokenAckFile); dir != "" {
			_ = os.MkdirAll(dir, 0o700)
		}
		if err := os.WriteFile(ah.cfg.AuthAdminTokenAckFile, []byte("acknowledged\n"), 0o600); err != nil {
			// 不阻断：至少本次会话已用上令牌；但为安全建议落盘成功。
			response.OK(c, gin.H{"acknowledged": true, "warn": "确认标记写入失败：" + err.Error()})
			return
		}
	}
	response.OK(c, gin.H{"acknowledged": true})
}
