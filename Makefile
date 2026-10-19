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
check: fmt-check vet test architecture-check
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

# 依赖地板不许漂移：主模块必须**零直接依赖**，多一行 require 就是纪律被破坏。
tidy-check:
	@n=$$(grep -cE '^\s+[a-z0-9].*\sv[0-9]' go.mod || true); \
	if [ "$$n" != "0" ]; then \
		echo "主模块 require 段应为空（核心零依赖），实得 $$n 行:"; \
		grep -nE '^\s+[a-z0-9].*\sv[0-9]' go.mod; exit 1; \
	fi; \
	echo "tidy-check: 主模块零依赖 ✓"
