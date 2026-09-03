# NGX-CP Makefile —— 统一命令入口（注意：recipe 必须用 tab 缩进）
SHELL := /bin/bash
BINARY_SERVER := bin/ngxcp-server
BINARY_AGENT  := bin/ngxcp-agent
PKG := github.com/th/ngxcp
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_T ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X $(PKG)/internal/pkg/version.Version=$(VERSION) \
           -X $(PKG)/internal/pkg/version.Commit=$(COMMIT) \
           -X $(PKG)/internal/pkg/version.BuildTime=$(BUILD_T)

.PHONY: help dev build test lint fmt proto ent migrate-dev e2e backup clean

help: ## 列出所有 target
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n",$$1,$$2}'

dev: ## 起全套依赖 + 控制面（开发态）
	make build && ./$(BINARY_SERVER) --config configs/config.example.yaml

build: ## 编译控制面 + Agent（静态，CGO_ENABLED=0）
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY_SERVER) ./cmd/ngxcp-server
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY_AGENT)  ./cmd/ngxcp-agent

test: ## 运行全部测试
	go test ./...

testf: ## 按正则运行指定用例： make testf F=TestDualPath
	go test ./... -run "$(F)"

lint: ## golangci-lint
	golangci-lint run ./...

fmt: ## gofmt + goimports
	gofmt -w . && go run golang.org/x/tools/cmd/goimports@latest -w -local $(PKG) .

proto: ## 生成 gRPC 代码
	./scripts/gen_proto.sh

ent: ## 生成 ent 代码
	go generate ./ent

migrate-dev: ## 应用迁移到开发库（读取 NGXCP_DB_DRIVER / NGXCP_DB_DSN 环境变量）
	go run ./cmd/ngxcp-migrate

e2e: ## 端到端测试（需要 Docker）
	go test -tags e2e ./test/e2e/...

backup: ## 手动触发备份
	go run ./cmd/ngxcp-server --backup

clean: ## 清理产物
	rm -rf bin/ dev.db
