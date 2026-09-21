# AGENTS.md

use chinese

当任务明确要求使用子 agent 时，主 agent 必须独立审阅 worker 的 diff，并按风险复跑必要验证；不得仅以 worker 的自述或其测试结果作为最终验收结论。

# 工具调用教训（2026-09-21）

- `apply_patch` 在当前会话里不是可命名工具；文件写入应通过 `functions.exec` 的 `tools.exec_command` 用 shell heredoc 完成。
- **关键陷阱**：`exec` 的 `input` 是 JS 模板字面量，`\n`、`\r\n`、`\"` 会被解释成真实控制字符，写进 Go/Python 源码就破坏字符串字面量。
- **正确写法**：在 JS 模板字面量里用 `\\n`（写进文件才是 `\n`）；Go struct tag 的反引号用 `\` ` 转义；大量文件时先用 `gofmt`/`go build` 扫一遍，`string literal not terminated` 通常就是这个原因。
- 已经破坏的文件可用小脚本修：把行尾未闭合的 `"` 与下一行按 `\\n` 拼接，再 `gofmt -w`。
