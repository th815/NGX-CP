//go:build webui

package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

//go:embed all:dist
var distFS embed.FS

// RegisterWebUI 将构建后的前端（dist）以 SPA 形式托管：
// 命中真实静态资源则直接返回，否则回退到 index.html；/api 与 /health 不回退。
func RegisterWebUI(r *gin.Engine) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return
	}
	fileServer := http.FileServer(http.FS(sub))
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if strings.HasPrefix(p, "/api") || p == "/health" {
			c.Status(http.StatusNotFound)
			return
		}
		if f, err := sub.Open(strings.TrimPrefix(p, "/")); err == nil {
			if st, e := f.Stat(); e == nil && !st.IsDir() {
				_ = f.Close()
				fileServer.ServeHTTP(c.Writer, c.Request)
				return
			}
			_ = f.Close()
		}
		data, e := distFS.ReadFile("dist/index.html")
		if e != nil {
			c.Status(http.StatusNotFound)
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", data)
	})
}
