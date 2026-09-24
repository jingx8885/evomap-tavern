# lov-evo

**进化酒馆的人格语音角色**：同一条会话里三个模型各干一件事。Jev（System One）做结构化判断，慢 LLM 做复查和长时间思考，OpenAI 双工（gpt-live）按人设说话。Go 负责执行，不充当第四个会开口的模型。

为 EvoMap 进化酒馆场景而生：一个能感知情绪、贴着人设说话、有长期方向，也会看、做、画、写，并把关系慢慢养出来的桌面角色。默认人设是小春（`personas/haru.yaml`）。

## 架构

```
用户语音 ──WebRTC PCMU──▶ gpt-live（双工：听 / 说）
        │ turn.done / transcript
        ▼
   Jev /v1/systemone          一次请求，类型化答案
   情感 · 投入 · 安全 · act
        │
        ├─ steering.Build ──developer──▶ 双工继续说（静默注入，不改 instructions）
        ├─ avatar.Drive ──────────────▶ Haru 表情
        └─ act 一条支线 ──────────────▶ Go 执行
              plan / 看 / 桌面 / Codex / 舞台队列 / 六爻
              慢思考异步，不挡语音
```

- **Jev** 只填 noul / choice / score，不写句子。每轮最多一次 `/v1/systemone`：valence、arousal、emotion、engagement、safety、persona_fit，外加总入口 `act`。`need_llm` 跟这次一起回来，只用来决定要不要开慢思考。
- **慢 LLM**（默认 `gpt-5.6-luna`）做三件事：复查判断够不够、起草判断题、在语音回合里来不及做的决策。输出是严格 JSON（plan note 或一段文本），失败就按人设 goals 轮转，不把半截话灌进双工。
- **双工**只看见人设、本轮 steering、偶尔一条 planner nudge。它不判断、不规划、不调工具。初始化之后不能再 `session.update` 改 instructions；本轮怎么说，靠 `session.context.append`。
- **人设**是 YAML，换文件即换角色：`personas/haru.yaml`（小春，默认）、`personas/asuka.yaml`（明日香）、`personas/tavern_keeper.yaml`、`personas/lore_bard.yaml`（吟游诗人洛尔）。
- **短期记忆**是 EMA 情感状态加最近 N 轮文本。**关系账本**写在 `runs/relationship-<persona>.json`：阶段、共同经历、约定。每轮只把和这句话相关的一小段注入 steering。

## 她现在能做的事

一轮里 Jev 只选一个 `act`，Go 打开对应支线。跟进和纠正留在同一条支线上；明确换了动作码就停掉当前支线再开新的。同一时间只跑一个动手任务。

| act | 做什么 |
|---|---|
| `none` | 只聊天 |
| `plan` | 异步写一条行为指令，再用 commentary 轻轻推她开口 |
| `reflect` / `look` | 感觉自己，或按白名单读源码。问到改自己时，自我 ReAct 才改允许的文件；跑着的进程不会热替换 |
| `camera` | 看镜头这一帧 |
| `screen` | 看电脑窗口标题；问到上面写了什么或有谁，才加一张截图 |
| `shot` | 截她自己舞台上的样子，只描述脸上的头发、表情、衣服 |
| `computer_use` | 本机桌面：聚焦窗口、拉起白名单应用、热键、往焦点窗粘贴。编码优先 Cursor / Codex，不在 IDE 里连点 |
| `codex` | 写一个新程序：在 `runs/codex/<时间>` 的空目录里写，不读她的源码。写出 `index.html`（或任一顶层 html）后，舞台右框用沙箱 iframe 打开，可以直接玩。只有“改你自己”才走自我 ReAct |
| `image` / `video` / `speech` / `song` | 进舞台队列，做完留在右框 |
| `picture` / `watch` / `listen` | 再看或再听刚刚做好的图、视频、声音 |
| `divine` | 用户说算命才进。铜钱、纳甲、世应由 Go 排盘，断语交给慢 LLM，开口仍走双工 |

`/play/` 只提供 Codex 那次写出来的页面和它自己的脚本。响应带 `sandbox`，页面即使在新标签页打开，也叫不到本机的 `/api`。

## 快速开始

```
go build ./cmd/tavernbot

# 本地环境自检（无网络）
./tavernbot doctor

# 链路探测：call create → session.started → RTP echo
./tavernbot probe

# 单句 TTS（speakable channel → WAV）
./tavernbot speak --text "欢迎来到进化酒馆" --output hello.wav

# 离线判断一段文本（不走语音）
./tavernbot judge --persona personas/haru.yaml --text "今天真的累坏了"

# 启动双工会话（同时打开 Haru Live2D，Jev 每轮推表情）
./tavernbot run --persona personas/haru.yaml -v

# 只看 Live2D：循环 Jev steering mode，并合成口型
./tavernbot live2d --demo --lipsync
```

其它子命令：`plan`（强制一次慢规划）、`desk`（Jev 驱动的本机电脑使用）、`codex`（跳过 Jev，Codex CLI 直接改当前仓库）、`divine`（离线起一卦）、`sense`（本体快照或读白名单源码）、`ctxprobe`、`loopprobe`。

凭证：`OPENAI_API_KEY` > `LOVBROWSER_API_KEY` > `~/.config/akasha/credentials.env`。
网关默认 `https://newapi.1234bot.com/v1`，可用 `NEW_API_BASE_URL` 或 `--base-url` 覆盖。

`run` 的键盘指令：

| 命令 | 作用 |
|---|---|
| `/say <文本>` | 让人设逐字念 |
| `/steer <文本>` | 手写本轮行为指令（developer，不念出来） |
| `/goal <文本>` | 手动覆盖长期目标 |
| `/look <文件>` | 读白名单源码，再注入体感 |
| `/camera` `/screen` `/shot` | 镜头、电脑窗口、她自己的脸，三件分开 |
| `/sense` | 打印自我快照并注入体感 |
| `/stage` | 打开她的页面；`image` / `video` / `speech` / `song` / `llm` 入队 |
| `/desk <目标>` | Jev 驱动的本机电脑使用 |
| `/codex <目标>` | 跳过外层 Jev，Codex 改当前仓库 |
| `/divine <所问>` | 铜钱起一卦 |
| `/on` `/off` | 打开或按住她的动手能力 |
| `/status` `/quit` | 只读状态，或退出并写 `runs/*.json` |

语音里说“用 Codex 写个贪吃蛇”走舞台队列里的新程序，不走 `/codex` 这条改仓库的手动入口。

默认在 `http://127.0.0.1:8787` 打开 Haru 查看器。`--live2d=off` 关闭；`judge --live2d http://127.0.0.1:8787` 可以把单次 Jev 判断推到已经开着的查看器。`run -vision=both|camera|screen|off` 控制镜头和屏幕；`/shot` 始终是她自己脸上的一张截图。

## 真实麦克风

默认构建上行走静音 RTP（协议要求上行必须活跃，speakable 文本才会被念出来）。要真麦克风：

```
go build -tags tavern_mic ./cmd/tavernbot   # 需要 cgo / miniaudio
```

设备选择：`TAVERN_MIC=<设备名片段>` 可强制指定；否则枚举所有采集设备，短暂探测输入 RMS 后挑最可能的一个（系统默认输入可能是只出数字静音的虚拟驱动；无内置麦克风的机器尤其如此）。上行持续静音会打出 `[voice warning]`；macOS 首次采集会弹麦克风权限。

下行音频：Windows 走 winmm 流式播放，macOS `afplay`，Linux `aplay`/`ffplay`。播放前会滤掉网关连续时间线里的句外空闲静音，保留短句内停顿；播放器队列有界，突发下发时丢弃旧帧而不阻塞 WebSocket。Live2D 口型跟随同一批实际送入播放器的 PCM。手机宽度下页面只留 Haru，收起桌面侧栏。

## 协议事实（已验证）

- 上行：`POST /v1/realtime/calls`（multipart sdp + session），SDP offer 必须保留结尾 CRLF；`session.model = gpt-live-1-boulder-alpha`。
- 事件：`GET wss://…/v1/live/{call_id}`。`session.context.append` 只接受三种 channel：
  - `speakable`：逐字念出，用于问候和 `/say`
  - `developer`：静默参考，不念、不主动回。steering 和体感走这里
  - `commentary`：注入后模型会主动开口。planner 的长期 nudge，以及回复她交给客户端的那一句，走这里
- 初始化之后 `session.update` 不能改 instructions。
- 下行：`session.output_audio.delta` 是连续 PCM s16le 24kHz mono（含静音填充）。
- 上行音频只走 WebRTC RTP（PCMU 8000），WS 音频事件会被上游拒绝。
- `delegation.type` 只有 `client`。她把做不了的事交出来时会先垫一句再等着；不回复就停在半句上。回复走 `delegation.context.append`，`developer` 不算回复。
- 用户语音 transcript 依赖网关事件形态。没有用户文本时，空问候不会送去判断，以免误触发 `re_engage`。

## 判断与 steering

`turn.done` 是最终轮；说话过程中的部分 transcript 会先防抖判一次，好让 steering 赶在她还在说的时候落地。

1. 文本进短期记忆。空用户文本、整句只有语气字，直接跳过判断。
2. `judge.JudgeTurn` 一次拿回情感、投入、安全、人设贴合和 `act`。
3. `judge.DecideMode` 选模式：`safety` > `comfort` / `de_escalate` / `celebrate` > `re_engage` > `goal_push` > `continue`。
4. `steering.Build` 拼场景、人设和关系摘要，经 `developer` 注入。问到“你是谁 / 感觉得到自己吗”时再叠一句体感。
5. `planner.Consider` 用这一轮已有的 `need_llm` 决定是否异步刷新 plan note。不再为门控额外打一次 System One。
6. 同一帧把 **她自己的情绪 + steering mode + 关系阶段** 映射成 Haru 表情。用户的情绪是输入，不是要镜像到脸上的目标。
7. 仅最终轮、且判断成功时，按 `act` 打开一条支线。

阈值在 persona YAML 的 `judge.*` 和 `planner.*` 里改。三个汉字就算一句（“画只猫”“看看屏幕”）；整句只有“嗯 / 好的 / 哈哈”不判。

## Live2D 与舞台

查看器在 `web/live2d`，默认模型是官方样本 **Haru**（来自 [CubismWebSamples](https://github.com/Live2D/CubismWebSamples)），前台是“小春 · 桌面陪伴”。下行 PCM 的能量映射到 `ParamMouthOpenY`，情感参数在 Idle 之后每帧叠上去。已注册的身体动作都可以被一轮选中，不限于六段 idle。

舞台窗口（`internal/window`）是另一块页面：生图、生视频、语音、歌曲、llm 笔记和 Codex 小程序的队列。状态是 queued / running / ready / failed，做完的结果留在右框。Codex 写出的页面在右框里直接跑，队列上也可以“打开玩”。`decorate` 和控件由慢模型写严格 JSON，Go 校验后才贴上。

Cubism Core 从 Live2D CDN 加载，首次需要能上网。Haru / Mao 素材受 [Live2D 免费素材协议](https://www.live2d.jp/en/terms/live2d-free-material-license-agreement/) 约束，不在 Apache-2.0 范围内。

## 目录

```
cmd/tavernbot/          CLI
internal/
  agent/                编排：双工事件 → Jev → steering → 异步支线
  jev/                  System One 客户端（只打 /v1/systemone）
  judge/                轮次题、答案解析、DecideMode、act
  memory/               短期记忆、EMA、关系账本
  planner/              need_llm 门控 + 异步长思考
  steering/             judgment → developer 文本
  llm/                  chat.completions
  livevoice/            WebRTC + WS 双工
  audio/                μ-law、重采样、播放、麦克风
  avatar/               steering mode → Live2D
  desk/                 Jev 电脑使用循环 + Cursor/Codex 执行
  eye/                  camera / screen / shot
  window/               舞台队列；/play 提供 Codex 页面
  sense/                本体感觉，不是模型工具
  react/                自我 ReAct：notice / read / remember / change
  oracle/               八卦六爻排盘，断语不直接开口
  persona/              YAML 人设
  config/               base URL、模型别名、凭证
personas/               haru / asuka / tavern_keeper / lore_bard
web/live2d/             Haru 查看器
docs/protocol.md        gpt-live 协议事实
```

## 已知边界

- `delegation.type` 只支持 `client`；`response.create` 不可用。
- 麦克风采集默认关闭（`-tags tavern_mic` 开启）。
- 安全判断是启发式：safety 超过阈值就进对应模式，不会替代真人介入。
- 自我修改只写白名单路径，写完核对工作区；正在跑的进程仍是上一版。
- Codex 新程序在 `runs/codex/<时间>`，不读本仓库。`/codex` 和 `desk --driver codex` 才会改当前工作目录。
- desk 不在允许表里的操作直接拒绝。`safe_to_act` 过低、同一操作连做三次、或目标已达成，循环结束。

## License

Apache-2.0
