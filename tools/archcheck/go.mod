// 门禁工具独立模块：核心仓的 go.mod 刻意保持 require 段为空，
// 「零核心依赖」必须能被 `go list -m all` 结构性验证，而不是靠嘴说。
module github.com/yi-nology/agent-memory/tools/archcheck

go 1.27

require gopkg.in/yaml.v3 v3.0.1
