// Command ngxcp-pki 是控制面 PKI 的运维小工具。
// 当前支持 init：生成 CA + 服务端证书（缺失时自动创建，已存在则复用）。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/th/ngxcp/internal/pkg/pki"
)

func main() {
	out := flag.String("out", "pki", "PKI 输出目录（ca.crt/ca.key/server.crt/server.key）")
	flag.Parse()

	if flag.NArg() == 0 || flag.Arg(0) != "init" {
		fmt.Fprintln(os.Stderr, "usage: ngxcp-pki init --out DIR")
		os.Exit(2)
	}

	if _, err := pki.LoadOrCreateCA(*out); err != nil {
		fmt.Fprintln(os.Stderr, "init PKI failed:", err)
		os.Exit(1)
	}
	fmt.Printf("✔ PKI 初始化完成: %s\n", *out)
	fmt.Println("  ca.crt / server.crt → 0644")
	fmt.Println("  ca.key / server.key → 0600（务必备份，丢失需全部 Agent 重新注册）")
}
