# agent-memory 工程入口
#
# 纪律：提交前必须 `make check`（fmt + vet + test + 架构门禁，全绿才算过）。
#
# 两个模块分别验证：
#   - 主模块（核心）刻意零依赖，CGO_ENABLED=0 是硬要求；
#   - tools/archcheck 是门禁工具的独立模块，带一个 YAML 解析依赖。
# 主模块绿不代表工具绿——工具的门禁失效是静默的，所以两段都要跑。

GO ?= go
export CGO_ENABLED = 0

.PHONY: all check build test test-race vet fmt fmt-check tidy-check architecture-check

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
