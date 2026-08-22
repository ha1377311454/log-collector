SHELL := /bin/sh

APP_NAME := log-collector
MAIN_PACKAGE := ./cmd/log-collector
DIST_DIR := dist

.DEFAULT_GOAL := help

.PHONY: help fmt tidy test vet build clean run \
	build-linux build-linux-amd64 build-linux-arm64 \
	release-check release-snapshot release-local

help: ## 显示可用命令
	@awk 'BEGIN {FS = ":.*## "; printf "用法: make <target>\n\n"} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

fmt: ## 格式化 Go 源码
	gofmt -w cmd internal

tidy: ## 整理 Go 模块依赖
	go mod tidy

test: ## 运行全部测试
	go test ./...

vet: ## 执行 Go 静态检查
	go vet ./...

build: ## 构建当前系统和架构的二进制文件
	mkdir -p $(DIST_DIR)
	go build -trimpath -o $(DIST_DIR)/$(APP_NAME) $(MAIN_PACKAGE)

build-linux: build-linux-amd64 build-linux-arm64 ## 构建全部支持的 Linux 架构

build-linux-amd64: ## 构建 Linux AMD64 可执行文件
	mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o $(DIST_DIR)/$(APP_NAME)-linux-amd64 $(MAIN_PACKAGE)

build-linux-arm64: ## 构建 Linux ARM64 可执行文件
	mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o $(DIST_DIR)/$(APP_NAME)-linux-arm64 $(MAIN_PACKAGE)

run: ## 使用项目根目录的 config.yaml 启动采集器
	go run $(MAIN_PACKAGE) -config ./config.yaml

release-check: ## 检查 GoReleaser 配置
	goreleaser check

release-snapshot: ## 生成本地快照制品，不发布到远程仓库
	goreleaser release --snapshot --clean

release-local: ## 生成正式格式制品，但跳过远程发布
	goreleaser release --clean --skip=publish

clean: ## 删除本地构建和发布产物
	rm -rf $(DIST_DIR)
