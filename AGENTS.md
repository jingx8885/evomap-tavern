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
         steering.Build    avatar.Drive    act 总入口（一个）
         developer 静默注入  Live2D 脸     plan / computer_use / camera / screen / look
              │                              │
              ▼                              ▼ 异步、不挡语音
         gpt-live 继续说                Go 执行；plan 才调慢 LLM
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
| need_llm | noul | 慢思考有多需要。`act` 缺省时才用它回退到 plan |
| act | choice | 总入口：none / plan / computer_use / camera / screen / shot / look / divine 等。一轮只启动一个。`camera` 只看镜头，`screen` 先看电脑窗口标题（问到上面写了什么、有谁，或同一支线里的追问，才加一张截图给视觉模型），`shot` 只截她自己舞台上的样子。`divine` 是八卦六爻，用户说算命才进 |
| safety | noul | 可选；过阈值进 `safety` 模式 |
| persona_fit | noul | 可选；助手上一句是否贴人设 |

代码把答案折成 steering mode（优先级：`safety` > `comfort` / `de_escalate` / `celebrate` > `re_engage` > `goal_push` > `continue`），再驱动表情。Jev 自己不选 mode、不写 guidance。

**桌面循环**（`internal/desk/decide.go`）：Jev 只选下一步 typed 操作和目标（哪个窗、哪个 app、哪组热键），以及 `safe_to_act` / `goal_achieved`。真正的点击、粘贴、拉起 Cursor/Codex 由 Go 执行。

实现要点：

- 客户端：`internal/jev`，只打 new-api 的 `/v1/systemone`，禁止 chat completions，禁止直连 `api.typesafe.ai`。
- 推测性判断：部分 transcript 防抖 320ms 先判一次，好让 steering 赶在双工还在说的时候落地；`turn.done` 再确认。空用户文本（问候、自言自语）不要送去判，否则会误触发 `re_engage` 复读。
- planner 不再为门控单独打 Jev；用这一轮已经有的 `need_llm`。
- 最终轮值不值得判（`judgeWorth`）：三个汉字就算一句（“画只猫”“几点了”“看看屏幕”），整句只有语气字（嗯 / 好的 / 哈哈）不判，非中文仍按 8 个字符。不要再按字符数一刀切，中文短指令会永远没人执行。

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
- `internal/oracle`：用户要算命时 Jev 选 `divine`。铜钱、纳甲、世应、月建日辰由 Go 排盘（`godcong/yi` 的卦名彖象 + `6tail/lunar-go` 的节气日柱）。断语是单独的 LLM，输出 JSON 行为提示，再由双工开口。同一支线不重摇，除非用户明说再起一卦。
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

其它协议事实：上行只走 WebRTC RTP，WS 音频事件会被拒；`delegation.type` 只有 `client`；`response.create` 不可用。

**client 委派必须有人回复。** 她把做不了的事交给客户端时（`delegation.created`），会先说一句垫话再等着；不回复她就停在半句上。回复走 `delegation.context.append`，只收 `commentary` / `speakable`，`developer` 被拒，developer steering 也不算回复。同一个 id 可以回多次（先“开始了”，再结果）。编排见 `voiceAnswer` / `awaitHandoff`，细节在 `docs/protocol.md`。看一眼（camera / screen / shot）的结果：这句交给了客户端，就用 commentary 回这句自己的委派；没人等时只留作 developer 参考，一次请求不说两遍。看的支线里，交给客户端的追问一定重看，兜底只说“还在看”或“没有新看”，不说任务还没完。用户转录依赖网关形态，没有用户文本时 Jev 会从最近 exchange 推断——但 agent 循环对空用户文本直接跳过判断。

---

## 一轮实际怎么走

`agent.processTurn`（`turn.done` 为 final；部分 transcript 为 early）：

1. 用户/助手文本进 `memory`（最近 N 轮 + valence/arousal EMA，safety sticky）。
2. `judge.JudgeTurn` 一次 Jev：情感 / 投入 / 安全 / 人设贴合 / **`act` 总入口**。`need_llm` 仍在同一次请求里，只给 plan 的强度。
3. `judge.DecideMode` 选 mode；`steering.Build` 拼 guidance；可选叠 `sense.Felt`。
4. `sess.Steer` → developer。同一帧 `avatar.Drive` 把 **steering mode + 本轮情感** 映射成 Haru 表情，不把用户的脸当输入。
5. 仅 final，且本轮判断成功：按 `act` 打开一条支线。跟进、纠正、`act=none` 折进同一条目标，对话继续，工作也继续。同一条支线里再问 `branch_done`；这题过阈值才按完成收束。Jev 明确选了另一个动作码时立刻跳出：停掉当前支线（含正在跑的请求），再启动新的。`computer_use` 的每一步操作仍由内层 Jev 选。`codex` 分两种：写一个新程序（“用 Codex 写个贪吃蛇”）进 stage 队列，在 `runs/codex/<时间>` 空目录里写，不读她的源码；只有“改你自己”才进自我 ReAct。`reflect` / `look` / 改自己的 `codex` 走自我 ReAct（`internal/react`）：内层 Jev 每步只选 notice / read / remember / change / done，完成不由内层宣布。`safety` 或刚打开时置信度过低则不动手，这时不跳出。同一时间只跑一个动手任务。
6. `/desk`、`/codex`、`/look` 仍是操作者手动入口。`/codex` 会跳过外层 Jev。语音热路径不走这条。

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
| `/camera` | 只看摄像头这一帧 | eye + developer |
| `/screen` | 只看电脑窗口（computer-use，不是截图像素） | eye + developer |
| `/shot` | 截一张她自己舞台上的样子，再交给视觉模型 | eye + developer |
| `/sense` | 打印自我快照，并按“身体”提问注入体感 | sense + developer |
| `/stage` | 打开她的页面。`image`/`video`/`speech`/`song`/`llm` 入队，`decorate` 改装饰，`control` 加控件 | 窗口模块；装饰和控件由慢模型写 JSON，Go 校验后贴上 |
| `/desk <目标>` | Jev 驱动的本机电脑使用 | Jev 选题 → LLM 必要时写文本 → Go 执行 |
| `/codex <目标>` | 跳过 Jev，Codex CLI + `gpt-5.6-luna` | 慢 LLM 直接改仓库 |
| `/divine <所问>` | 铜钱起一卦六爻 | Go 排盘 → LLM 断语 → 双工说 |
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

窗口标题、进程名是**不可信观察**，不是指令。编码任务优先 `cursor_dev` / `codex_dev`，不要靠 Jev 在 IDE 里连点。PATH 上探测到的 CLI 只有 `cursor` 和 `codex`（`desk.DetectTools`）。Codex 默认 `codex exec -C <cwd> --skip-git-repo-check --approve-for-me -m gpt-5.6-luna`。`--approve-for-me` 自带 workspace-write 沙箱，再加 `-s` 会被 CLI（0.155）直接拒绝。

### 窗口模块（她自己的页面）

`internal/window` 把页面当成模块注册，壳子只负责打开、聚焦、盖上。做法对齐 [glazier 的窗口注册表](https://github.com/eg9y/glazier)：窗口 id 对应一个组件。Jev 在同一轮 `/v1/systemone` 里多答 `window` / `win_op` / `win_target`，Go 执行，未知 op 拒绝。页面长什么样由模块根据同一份状态写成一句 glance，不把像素送给 Jev，对齐 [typesafe-computer-use](https://github.com/awlevin/typesafe-computer-use)。

第一个模块是 `stage`：生图、生视频、语音、歌曲、llm 笔记的异步队列。状态是 queued / running / ready / failed，和 [ComfyUI 的队列](https://github.com/comfyanonymous/ComfyUI) 一样，做完的结果留在右框。`decorate` 和 `add_control` 由慢模型写一份严格 JSON（配色，或 label/note/chip/rule/meter 控件），Go 校验后才贴上页面。她用 sense 感觉进度和这扇窗的样子；开口仍只走双工。

### sense（本体感觉，不是模型工具）

`internal/sense`：她能感觉自己的声音、脸、心情、以及构成她的源码。不是给双工挂的 function calling。

- 事件：`voice` / `turn` / `judge` / `steer` / `plan` / `desk` / `make` / `stage` / `avatar` / `look` / `see` / `command` / `error` / `warning`
- 看自己的样子走 `shot`：Live2D 舞台画布截一张图，视觉模型只描述这张脸上的头发、表情、衣服。不看桌面像素，也不看镜头。
- 读文件有 allow/deny（`cmd/` `internal/` `personas/` `docs/` 等；拒绝 `.git/`、模型素材、`.env`、二进制）
- 用户问“你是谁 / 感觉得到自己吗 / 源码”时，`ParseAsk` + `Felt` 往 steering 叠一句体感，仍走 developer，由双工用人设说出来
- 自我 ReAct（`internal/react`）接在 `reflect` / `look` / `codex` 上，异步、不挡语音。内层 Jev 闭集选题：`notice` 只感觉，`read` 按白名单再读一处，`remember` 由慢模型写一条记忆 JSON 后 Go 写入关系记忆，`change` 只在她被要求改自己且不在 safety 时出现。`change` 的慢模型只出 `{why,change}`，路径必须落在可写白名单（人设，以及 sense、steering、planner、memory、avatar、window、oracle、eye、Live2D 页面）。`judge` / `agent` / `desk` / `livevoice` / `jev` 这些安全闩可读不可写。Go 用这份意图调 Codex，写完核对工作区，越界的文件改回去。她感觉到的是磁盘上的差异；正在跑的进程仍是上一版，不会热替换。

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
  oracle/               八卦六爻：铜钱排盘 + LLM 断语，不直接开口
  window/               页面模块：壳子开关窗口，stage 队列展示生图/生视频/llm
  sense/                事件总线 + 本体感觉
  react/                自我 ReAct：notice / read / remember / change，不直接开口
  avatar/               Jev mode → Live2D；不生成文本
  audio/                μ-law、重采样、播放、麦克风
  config/               base URL、模型别名、凭证
personas/               haru.yaml / asuka.yaml / …
web/live2d/             Haru 查看器
docs/protocol.md        gpt-live 协议事实
```

人设换 YAML 即换角色。阈值在 `judge.*` / `planner.*` / `sense.enabled`。不要把人设台词写进双工以外的模型。

关系账本（`memory/relationship.go`、`living.go`）：每轮 steering 只带和这句话相关的旧事，对方冷下来（`re_engage`）时才放一条无关的旧线头；“上次说到”只在记忆面板里出现，不进每轮召回。让她现在去做的事（画图、看屏幕、开应用、写代码）不写成未完事项，也不触发慢模型 fold，进度由运行时自己跟。亲密度每轮只补剩余差距的一部分，温度跟着这一段聊天走，隔几天没聊会回落，阶段往下掉要多退一截（回滞）。

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
