// Command ngxcp-agent 是部署在每台 Nginx/Keepalived 节点上的常驻代理。
// M0 阶段为占位 main，仅打印版本；M1 实现 gRPC 注册 / 心跳 / mTLS 外连。
package main

import (
	"flag"
	"fmt"

	"github.com/th/ngxcp/internal/pkg/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String())
		return
	}
	fmt.Println("ngxcp-agent: M0 skeleton (gRPC client lands in M1)")
}
