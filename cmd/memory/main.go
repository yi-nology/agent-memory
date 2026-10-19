// Command memory — 长期记忆内核的命令行入口。
//
// 薄壳：全部逻辑在 adapters/cli 里（可单测），本文件只负责接上 os.Args。
package main

import "github.com/yi-nology/agent-memory/adapters/cli"

func main() { cli.Main() }
