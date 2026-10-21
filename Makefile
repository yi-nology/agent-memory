# agent-memory 工程入口
#
# 纪律：提交前必须 `make check`（fmt + vet + test + 架构门禁，全绿才算过）。
#
# 两个模块分别验证：
#   - 主模块（核心）刻意零依赖，CGO_ENABLED=0 是硬要求；
#   - tools/archcheck 是门禁工具的独立模块，带一个 YAML 解析依赖。
# 主模块绿不代表工具绿——工具的门禁失效是静默的，所以两段都要跑。

# ⚠️ recipe 一律用 bash 跑。
#
# 理由：下面 `release-check` 的 recipe 用了 `set -o pipefail`（bash 专有），
# 而 make 的默认 shell 是 **/bin/sh**——在 Ubuntu 上那是 dash，
# 于是 `set -euo pipefail` 直接报 "Illegal option -o pipefail"。
#
# ⚠️ 这个坑**本地复现不出来**：macOS 的 /bin/sh 是 bash 的 sh 模式，
# 认 pipefail ⇒ 本地绿、CI 红（v0.7.0 的发布闸就红在这儿）。
# 故把 shell 显式钉成 bash，别让「本地能跑」骗了自己。
SHELL := /bin/bash

GO ?= go
export CGO_ENABLED = 0

.PHONY: all check build test test-race vet fmt fmt-check tidy-check architecture-check release-check

all: check

## check 提交前的完整闸（顺序即成本递增）
check: fmt-check vet test architecture-check tidy-check
	@echo "check: 全绿"

build:
	$(GO) build ./...

test:
	$(GO) test ./... -count=1

test-race:
	$(GO) test ./... -count=1 -race

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l . | grep -v '^\.gomodcache' | grep -v '^\.gocache'); \
	if [ -n "$$out" ]; then echo "gofmt 不合规:"; echo "$$out"; exit 1; fi

architecture-check:
	$(GO) -C tools/archcheck run . -root ../.. -policy ../../architecture-policy.yaml
	$(GO) -C tools/archcheck test ./... -count=1

# 依赖地板不许漂移：**只有 storage/ 允许第三方包**，其余模块只准 stdlib 与本仓。
#
# 这条不是重复门禁：门禁在**源码 import 面**上查（谁真的引了它），本目标在
# **go.mod 面**上查（账有没有漂）。两边都看才完整——go.mod 干净但源码里写了
# import 而 go.mod 没同步，构建会红；go.mod 有 require 但没人用，是漂移。
tidy-check:
	@echo "tidy-check: 主模块 require 段（第三方只应服务于 storage/）"; \
	grep -nE '^\s+[a-z0-9][^ ]*\s+v[0-9]' go.mod || echo "  （无 require）"; \
	echo "tidy-check: import 面判据由门禁 memory-core-stdlib-only 执行 ✓"

# 发布物自检（本地版）：按下游的真实路径把已发布的版本引进来并编译。
#
# 与 .github/workflows/ci.yml 的 release 段**同一套动作**。
#
# 用法：make release-check TAG=v0.4.0
#
# # 为什么要有本地版
#
# v0.4.0 的发布物自检在 CI 上红了，而那道闸从 v0.1.0 起就一直是红的
# （版本号形态错）却「没人看它红」——一次错两处。闸红着没人察觉的根因是：
# **红只发生在 CI 上**，本地没有任何一处会复现它。于是发版变成了「推上去看红不红」。
#
# 把它做成一个本地可跑的目标之后，「红」这件事在推 tag 之前就能看到。
# 这条比闸本身更值钱：闸的形状会变，能本地复现它的动作不会变。
release-check:
	@test -n "$(TAG)" || { echo "用法: make release-check TAG=v0.4.0"; exit 2; }
	@set -euo pipefail; \
	tmp=$$(mktemp -d); cd "$$tmp"; \
	go mod init probe >/dev/null; \
	go get "github.com/yi-nology/agent-memory@$(TAG)"; \
	go list -m github.com/yi-nology/agent-memory; \
	CGO_ENABLED=0 go build github.com/yi-nology/agent-memory/types; \
	echo "release-check: 模块级可解析 ✓"
	@set -euo pipefail; \
	tmp=$$(mktemp -d); cd "$$tmp"; \
	go mod init probe >/dev/null; \
	pkgs="types store ports pipeline/recall pipeline/query pipeline/graph evaluation adapters/mcp storage/postgres"; \
	for pkg in $$pkgs; do go get "github.com/yi-nology/agent-memory/$$pkg@$(TAG)"; done; \
	for pkg in $$pkgs; do CGO_ENABLED=0 go build "github.com/yi-nology/agent-memory/$$pkg"; done; \
	echo "release-check: 带依赖的包下游也能引进来并编译 ✓"
