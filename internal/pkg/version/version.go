// Package version 暴露构建版本信息，由 -ldflags 在编译时注入。
package version

import "fmt"

// 这些变量在 CI / 本地编译时通过 -ldflags "-X github.com/th/ngxcp/internal/pkg/version.Version=..." 注入。
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// String 返回可读的版本串。
func String() string {
	return fmt.Sprintf("ngxcp %s (commit %s, built %s)", Version, Commit, BuildTime)
}
