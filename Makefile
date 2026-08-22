SHELL := /bin/sh

APP_NAME := log-collector
MAIN_PACKAGE := ./cmd/log-collector
DIST_DIR := dist

.DEFAULT_GOAL := help

.PHONY: help fmt tidy test vet build clean run \
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
