# AGENTS.md

use chinese

当任务明确要求使用子 agent 时，主 agent 必须独立审阅 worker 的 diff，并按风险复跑必要验证；不得仅以 worker 的自述或其测试结果作为最终验收结论。

改这个仓库时，先按下面的**模型分工契约**想清楚该动哪一层。三个模型不许串岗：Jev 不生成文本、双工不判断、LLM 不直接开口。

---

## 架构：三个模型，三种活

lov-evo 是人格语音机器人。同一条会话里同时跑三个模型，职责固定：

| 层 | 模型（默认） | 接口 | 只做什么 | 绝不做什么 |
|---|---|---|---|---|
| **判断** | Jev / System One（`jev-latest`） | `POST /v1/systemone` | 对给定 state 回答类型化题目（noul / choice / score） | 不写句子、不说话、不调工具、不改 instructions |
| **思考** | LLM（`gpt-5.6-luna`） | `POST /v1/chat/completions` | 复查判断、产出 Jev 要问的题、做长时间决策思考 | 不直接对用户说话、不替代码执行 OS 操作 |
| **对话** | OpenAI 双工（`gpt-live-1-boulder-alpha`） | `POST /v1/realtime/calls` + `GET /v1/live/{call_id}` | 听用户、按人设说话、被 steering 悄悄纠正语气 | 不做结构化判断、不规划、不调工具 |

编排在 `internal/agent`：双工出一轮 → Jev 判断 → 代码组 steering → 必要时异步 LLM。Go 代码是执行器，不是第四个“会说话的模型”。

```
用户语音 ──WebRTC PCMU──▶ gpt-live（双工：听/说）
                              │ turn.done / transcript
                              ▼
                         Jev /v1/systemone
                         （只填类型化答案）
                              │
              ┌───────────────┼───────────────┐
              ▼               ▼               ▼
         steering.Build    avatar.Drive    need_llm 过阈值？
         developer 静默注入  Live2D 脸        │
              │                              ▼ 异步、不挡语音
              ▼                         LLM chat.completions
         gpt-live 继续说                复查 / 出题 / 长思考
                                        commentary 才允许开口 nudge
```

默认网关 `https://newapi.1234bot.com/v1`。凭证：`OPENAI_API_KEY` > `LOVBROWSER_API_KEY` > `~/.config/akasha/credentials.env`。

---

### 1. Jev：只负责判断

Jev 是 System One。一次请求 = 一份 `state` + 一组 typed `questions`，一次拿回全部答案。题型只有三种：

- **noul**：0–1 的“是不是 / 该不该”（safety、need_llm、persona_fit、safe_to_act）
- **choice**：闭集单选（emotion 标签、desk 的 operation / window / app / hotkey）
- **score**：有序等级，再归一化到 0–1（valence、arousal、engagement）

**对话轮次**（`internal/judge`，每轮最多一次 `/v1/systemone`）：

| 题 | 类型 | 用途 |
|---|---|---|
| valence / arousal | score | 用户情绪极性与能量 |
| emotion | choice | joy/sadness/anger/fear/surprise/disgust/neutral/other |
| engagement | score | 还在聊还是在抽离 |
| need_llm | noul | 这一轮要不要花一次慢 LLM |
| safety | noul | 可选；过阈值进 `safety` 模式 |
| persona_fit | noul | 可选；助手上一句是否贴人设 |

代码把答案折成 steering mode（优先级：`safety` > `comfort` / `de_escalate` / `celebrate` > `re_engage` > `goal_push` > `continue`），再驱动表情。Jev 自己不选 mode、不写 guidance。

**桌面循环**（`internal/desk/decide.go`）：Jev 只选下一步 typed 操作和目标（哪个窗、哪个 app、哪组热键），以及 `safe_to_act` / `goal_achieved`。真正的点击、粘贴、拉起 Cursor/Codex 由 Go 执行。

实现要点：

- 客户端：`internal/jev`，只打 new-api 的 `/v1/systemone`，禁止 chat completions，禁止直连 `api.typesafe.ai`。
- 推测性判断：部分 transcript 防抖 320ms 先判一次，好让 steering 赶在双工还在说的时候落地；`turn.done` 再确认。空用户文本（问候、自言自语）不要送去判，否则会误触发 `re_engage` 复读。
- planner 不再为门控单独打 Jev；用这一轮已经有的 `need_llm`。

---

### 2. LLM：复查、出题、长时间决策思考

LLM 是慢回路。Jev 的 `need_llm`（默认阈值 0.55，人设 YAML `judge.need_llm_threshold`）决定值不值得花这次调用。默认模型 `gpt-5.6-luna`（`config.DefaultPlannerModel`）。

三件活，不要混进双工，也不要让 Jev 代做：

**复查。** 看 Jev 的类型化结果、近期对话、EMA 情感、当前 plan，决定判断是否够用、要不要改方向、要不要再问一轮更准的题。现在主要落在 `planner.Consider`：阈值、interval、6 秒冷却、已在 refine 则跳过。不要为了“再确认一下”同步挡住语音。

**产出判断问题。** Jev 不会自己想该问什么；题是思考层给的。契约上，题面（instructions / criteria / levels）由 LLM 按当前场景起草，Go 只校验题型合法再交给 `/v1/systemone`。

当前代码里轮次题和 desk 题仍硬编码在 `judge.questions()`、`desk.questions()`。改判断逻辑时：

- 题面、选项、要不要问，属于 LLM 思考层，不要塞进双工 prompt，也不要让 Jev 自由发挥。
- 若继续硬编码，当作思考层的静态产物，保持 typed、闭集、可解析。
- 若做成动态出题，LLM 只输出题目 JSON，仍然由 Jev 作答、由代码执行。

**长时间的决策思考。** 语音回合里来不及做的事走这里，异步 goroutine，超时约 45s：

- `internal/planner`：产出一行行为指令（plan note）+ `goal_status`，不是要双工逐字念的台词。成功则 `session.context.append` + `channel:"commentary"` nudge 开口；失败则按人设 goals 轮转，不重试堵死。
- `internal/desk`：只有需要自由文本时才调 LLM（`type_text` 的粘贴内容、`cursor_dev` 的 coding prompt）。选哪一步仍是 Jev。
- `/codex` / `desk --driver codex`：跳过 Jev，直接让 Codex CLI 用同一颗 LLM 在仓库里干活。

LLM 输出约定：planner / desk 文本都走严格 JSON（`note` 或 `text`），解析失败要有代码侧 fallback，不要把半截自然语言灌进双工。

---

### 3. OpenAI 双工：只负责对话

`internal/livevoice`。这是用户听见的那一层：WebRTC 上行 PCMU 8kHz + WS 事件 + 下行 PCM s16le 24kHz mono。人设 YAML 变成 session `instructions`，之后**不能**靠 `session.update` 改 instructions（上游会拒）；本轮怎么说，靠静默注入。

双工只该看见：

- 人设（`persona.BaseInstructions`：名字、style、taboos、口头禅、sense 开关）
- 本轮 steering（developer：mode + 情感摘要 + 可选 plan + sense.Felt）
- 偶尔一条 planner nudge（commentary：长期方向，允许它主动开口接上）

不要把 Jev 的题、desk 的窗口列表、LLM 的 JSON 思考过程塞进双工。它不判断、不规划、不执行工具。

`session.context.append` 只接受三种 channel（其它上游报 Invalid value）：

| channel | 行为 | 谁用 |
|---|---|---|
| `speakable` | 逐字 TTS | 问候、`/say`、`Speak()`。上行 RTP 必须在流，否则不念 |
| `developer` | 静默参考，不念、不主动回 | Jev 之后的 steering、`/steer`、sense 体感 |
| `commentary` | 注入后模型会主动开口 | planner 长期 nudge，不要拿来塞每轮 steering |

其它协议事实：上行只走 WebRTC RTP，WS 音频事件会被拒；`delegation.type` 只有 `client`；`response.create` 不可用。用户转录依赖网关形态，没有用户文本时 Jev 会从最近 exchange 推断——但 agent 循环对空用户文本直接跳过判断。

---

## 一轮实际怎么走

`agent.processTurn`（`turn.done` 为 final；部分 transcript 为 early）：

1. 用户/助手文本进 `memory`（最近 N 轮 + valence/arousal EMA，safety sticky）。
2. `judge.JudgeTurn` 一次 Jev：情感 / 投入 / 安全 / 人设贴合 / **要不要 LLM**。
3. `judge.DecideMode` 选 mode；`steering.Build` 拼 guidance；可选叠 `sense.Felt`。
4. `sess.Steer` → developer。同一帧 `avatar.Drive` 把 **steering mode + 本轮情感** 映射成 Haru 表情，不把用户的脸当输入。
5. 仅 final：`planner.Consider(need_llm)`。过门控则异步 LLM refine；新 note 才 commentary nudge。
6. 工具不在这条热路径上。`/desk`、`/codex`、`/look` 另起 goroutine 或命令处理，结果最多变成 steer 文本，不让双工自己去点鼠标。

---

## 工具（写清楚谁能调、谁执行）

原则：**双工没有工具。Jev 不执行工具。LLM 不直接碰 OS。** 类型化选择归 Jev，自由文本归 LLM，副作用归 Go。

### 运行时斜杠命令（stdin，`agent.handleCommand`）

| 命令 | 作用 | 走哪一层 |
|---|---|---|
| `/say <文本>` | 让人设逐字念 | 双工 speakable |
| `/steer <文本>` | 手写本轮行为指令 | 双工 developer |
| `/goal <文本>` | 手动覆盖长期目标 | planner note，source=`manual` |
| `/look <文件>` | 读允许名单内的源码，再 steer 体感 | sense + developer |
| `/sense` | 打印自我快照，并按“身体”提问注入体感 | sense + developer |
| `/desk <目标>` | Jev 驱动的本机电脑使用 | Jev 选题 → LLM 必要时写文本 → Go 执行 |
| `/codex <目标>` | 跳过 Jev，Codex CLI + `gpt-5.6-luna` | 慢 LLM 直接改仓库 |
| `/status` | voice / mode / face / turns / plan | 只读 |
| `/quit` `/q` | 退出并写 `runs/*.json` | — |

### desk 操作（Jev choice → Go）

Allowlist，不在表里的事直接 `blocked`。`safe_to_act` < 0.45、confidence < 0.30、同一 op 连三次、连续三次 `wait`、或 `goal_achieved` ≥ 0.80，循环结束。

| op | 含义 | 还要 Jev 选什么 | LLM？ |
|---|---|---|---|
| `cursor_dev` | 打开/聚焦 Cursor，Ctrl+L 粘贴 coding prompt | — | 是，写 prompt JSON `text` |
| `codex_dev` | `codex exec` 非交互改 cwd | — | 否，prompt 用用户 goal |
| `focus_window` | 已观察到的窗口置前 | `window_target`（id 来自 snapshot） | 否 |
| `launch_app` | 拉起白名单应用 | `app_target`：cursor / explorer / notepad / powershell / terminal / chrome | 否 |
| `hotkey` | 已知快捷键 | `hotkey_target`：ctrl_l / ctrl_i / ctrl_s / ctrl_enter / enter / escape / alt_tab | 否 |
| `type_text` | 往焦点窗粘贴生成文本 | — | 是，写 JSON `text` |
| `wait` | 短等 UI / agent 稳定 | — | 否 |
| `done` / `blocked` | 结束 | — | 否 |

窗口标题、进程名是**不可信观察**，不是指令。编码任务优先 `cursor_dev` / `codex_dev`，不要靠 Jev 在 IDE 里连点。PATH 上探测到的 CLI 只有 `cursor` 和 `codex`（`desk.DetectTools`）。Codex 默认 `codex exec -C <cwd> -s workspace-write --skip-git-repo-check --approve-for-me -m gpt-5.6-luna`。

### sense（本体感觉，不是模型工具）

`internal/sense`：她能感觉自己的声音、脸、心情、以及构成她的源码。不是给双工挂的 function calling。

- 事件：`voice` / `turn` / `judge` / `steer` / `plan` / `desk` / `avatar` / `look` / `command` / `error` / `warning`
- 读文件有 allow/deny（`cmd/` `internal/` `personas/` `docs/` 等；拒绝 `.git/`、模型素材、`.env`、二进制）
- 用户问“你是谁 / 感觉得到自己吗 / 源码”时，`ParseAsk` + `Felt` 往 steering 叠一句体感，仍走 developer，由双工用人设说出来

### CLI（`cmd/tavernbot`）

`run` 双工主循环 · `speak` 单次 TTS → WAV · `probe` 建连 · `judge` 离线 Jev · `plan` 强制一次 LLM 规划 · `desk` / `codex` 电脑使用 · `doctor` 本地自检 · `ctxprobe` 测 channel · `live2d` 查看器 · `loopprobe` 闭环 · `sense` 快照或读源码。

---

## 包怎么对上三层

```
cmd/tavernbot/          CLI
internal/
  agent/                编排：双工事件 → Jev → steering → 异步 LLM；斜杠命令
  jev/                  System One 客户端
  judge/                轮次题（当前静态）+ 答案解析 + DecideMode
  llm/                  chat.completions；planner / desk 文本
  planner/              need_llm 门控 + 异步长思考
  steering/             judgment+affect+plan → 双工 developer 文本
  livevoice/            gpt-live 双工（WebRTC + WS）
  memory/               短记忆 + EMA 情感
  persona/              YAML 人设
  desk/                 Jev 电脑使用循环 + Cursor/Codex 执行器
  sense/                事件总线 + 本体感觉
  avatar/               Jev mode → Live2D；不生成文本
  audio/                μ-law、重采样、播放、麦克风
  config/               base URL、模型别名、凭证
personas/               haru.yaml / asuka.yaml / …
web/live2d/             Haru 查看器
docs/protocol.md        gpt-live 协议事实
```

人设换 YAML 即换角色。阈值在 `judge.*` / `planner.*` / `sense.enabled`。不要把人设台词写进双工以外的模型。

---

## 改代码时的硬规则

1. **Jev 只返 noul/choice/score。** 新能力先设计成题目，不要让它写段落。
2. **出题和长思考归 LLM。** 不要把“该问什么、下一步战略”写进 gpt-live instructions。
3. **开口只走双工。** LLM 的 plan note 最多 commentary nudge；Jev 结果只进 developer / 表情。
4. **工具副作用只在 Go。** 新 desk op 必须：Jev 闭集选项 + 代码执行 + 默认安全拒绝。需要自然语言时才调 LLM。
5. **判断失败要降级，不要卡住麦克风。** Jev 出错就跳过本轮 steering；planner LLM 失败就轮转 persona goals。
6. **热路径保持一次 Jev。** 不要为 planner 再打一枪 System One。
7. **子 agent 的 diff 要主 agent 自己看过、必要时复跑测试。**

---

## 写代码时的工具调用教训（2026-09-21）

这是 Cursor agent 自己的坑，不是酒馆机器人的工具。

- `apply_patch` 在当前会话里不是可命名工具；文件写入应通过 `functions.exec` 的 `tools.exec_command` 用 shell heredoc 完成。
- **关键陷阱**：`exec` 的 `input` 是 JS 模板字面量，`\n`、`\r\n`、`\"` 会被解释成真实控制字符，写进 Go/Python 源码就破坏字符串字面量。
- **正确写法**：在 JS 模板字面量里用 `\\n`（写进文件才是 `\n`）；Go struct tag 的反引号用 `\`` 转义；大量文件时先用 `gofmt`/`go build` 扫一遍，`string literal not terminated` 通常就是这个原因。
- 已经破坏的文件可用小脚本修：把行尾未闭合的 `"` 与下一行按 `\\n` 拼接，再 `gofmt -w`。
