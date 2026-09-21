# lov-evo

**进化酒馆的人格语音角色**：Jev（System One）做结构化情感判断与异步门控，LLM 做长期规划，OpenAI 双工语音（gpt-live）做对话。

为 EvoMap 进化酒馆场景而生：一个能感知情绪、贴着人设说话、有长期方向、也会把关系慢慢养出来的桌面角色。

## 架构

        ┌─────────────────────────────────────────────────────────┐
        │                     tavernbot run                        │
        │                                                         │
        │  ┌──────────┐   WebRTC PCMU ↑   ┌──────────────────┐    │
        │  │  mic /   │ ───────────────▶  │ new-api gateway  │    │
        │  │  silence │                   │  POST /realtime/ │    │
        │  │  uplink  │   WS events ↓     │  calls + /live/  │    │
        │  └──────────┘ ◀───────────────  │  {call_id}       │    │
        │       │            │            └──────────────────┘    │
        │       ▼            ▼                                    │
        │  ┌──────────────────────┐                               │
        │  │  turn loop           │                               │
        │  │  turn.done ─────────▶│ Jev /v1/systemone             │
        │  │                      │  valence / arousal / emotion   │
        │  │                      │  engagement / safety           │
        │  │                      │  persona_fit                   │
        │  │   judgment ─────────▶│ steering.Build → session.update│
        │  └──────────────────────┘                               │
        │            │                                            │
        │            ▼  (every N turns)                           │
        │  ┌──────────────────────┐    ┌──────────────────┐       │
        │  │  planner.Tick        │──▶ │ Jev gate:        │       │
        │  │                      │    │ natural_pause?   │       │
        │  │                      │    │ goals_remaining? │       │
        │  │                      │    └────────┬─────────┘       │
        │  │   async goroutine    │◀────────────┘ pass            │
        │  │                      │    LLM chat.completions        │
        │  │                      │    → plan note → steering      │
        │  └──────────────────────┘                               │
        └─────────────────────────────────────────────────────────┘

- **Jev（System One）**只做判断不生成文本：每轮一次 `/v1/systemone` 调用，问 valence/arousal/emotion/engagement/safety/persona_fit，代码把结果组合成 steering mode（comfort / de_escalate / celebrate / re_engage / goal_push / safety / continue）。
- **LLM 长期规划**是异步的：Jev 先做门控判断（是否自然停顿 + 目标是否未达成），通过后才发 chat.completions 刷新 plan note；refine 跑在 goroutine 里，不阻塞语音。
- **人设**是 YAML，换文件即换角色：`personas/tavern_keeper.yaml`（老板娘）和 `personas/lore_bard.yaml`（吟游诗人）。
- **短期记忆**是 EMA 情感状态 + 最近 N 轮文本，给 Jev 和 planner 当 state。
- **关系记忆**是持久化账本：`runs/relationship-<persona>.json` 记关系阶段、未结事项、共同经历、约定和内部梗；每轮只把最相关的一小段注入 steering，避免把数据库念给双工。

## 快速开始

    go build ./cmd/tavernbot

    # 本地环境自检（无网络）
    ./tavernbot doctor

    # 链路探测：call create → session.started → RTP echo
    ./tavernbot probe

    # 单句 TTS（speakable channel → WAV）
    ./tavernbot speak --text "欢迎来到进化酒馆" --output hello.wav

    # 离线判断一段文本（不走语音）
    ./tavernbot judge --persona personas/tavern_keeper.yaml --text "今天真的累坏了"

    # 启动双工会话（同时打开 Haru Live2D 查看器，Jev 每轮推表情）
    ./tavernbot run --persona personas/tavern_keeper.yaml -v

    # 只看 Live2D：循环 Jev steering mode，并合成口型
    ./tavernbot live2d --demo --lipsync

凭证：`OPENAI_API_KEY` > `LOVBROWSER_API_KEY` > `~/.config/akasha/credentials.env`。
网关默认 `https://newapi.1234bot.com/v1`，可用 `NEW_API_BASE_URL` 或 `--base-url` 覆盖。

`run` 里的键盘指令：`/say <文本>`（让人设念出来）、`/steer <文本>`（改 session 指令）、`/goal <文本>`（手动设长期目标）、`/status`、`/quit`。

默认在 `http://127.0.0.1:8787` 打开 Haru 查看器。`--live2d=off` 关闭；`judge --live2d http://127.0.0.1:8787` 可以把单次 Jev 判断推到已经开着的查看器。

## 真实麦克风

默认构建上行走静音 RTP（协议要求上行必须活跃，speakable 文本才会被念出来）。要真麦克风：

    go build -tags tavern_mic ./cmd/tavernbot   # 需要 cgo / miniaudio

设备选择：`TAVERN_MIC=<设备名片段>` 可强制指定；否则枚举所有采集设备，
短暂探测输入 RMS 后用 `micPickScore` 挑最可能的一个（系统默认输入可能是
只出数字静音的虚拟驱动，比如远控软件的音频管道；无内置麦克风的机器尤其如此）。
上行持续静音 6 秒会打出 `[voice warning]`；macOS 首次采集会弹麦克风权限。

下行音频：Windows 走 winmm 流式播放，macOS `afplay`，Linux `aplay`/`ffplay`。播放前会滤掉网关连续时间线里的句外空闲静音，保留短句内停顿；播放器队列有界，突发下发时丢弃旧帧而不阻塞 WebSocket，避免历史音频累积成多秒延迟。Live2D 口型跟随同一批实际送入播放器的 PCM，避免嘴巴明显领先声音；页面只跟口型和表情，避免和第二路扬声器叠成回声。

## 协议事实（已验证）

- 上行：`POST /v1/realtime/calls`（multipart sdp + session），SDP offer 必须保留结尾 CRLF；`session.model = gpt-live-1-boulder-alpha`。
- 事件：`GET wss://…/v1/live/{call_id}`；`session.context.append` + `channel:"speakable"` 念文本；`session.context.append` + `channel:"developer"` 推 steering（上游静默接受；`session.update` 初始化后不可改 instructions）。
- 下行：`session.output_audio.delta` 是连续 PCM s16le 24kHz mono（含静音填充）。
- 上行音频只走 WebRTC RTP（PCMU 8000），WS 音频事件会被上游拒绝。
- 用户语音 transcript 依赖网关事件形态；当前实现对 user/assistant transcript 事件做了宽容解析，若网关不提供用户转录，Jev 退化为从最近 exchange 推断用户状态。

## 判断与 steering

每轮 `turn.done` 后：

1. `judge.JudgeTurn` 发一次 `/v1/systemone`，题型固定（score×3 + choice + noul×2），全部并行。
2. `memory.UpdateAffect` 把 valence/arousal 折进 EMA，safety 置 sticky 标志。
3. `judge.DecideMode` 选模式（safety > comfort/de_escalate/celebrate > re_engage > goal_push > continue）。
4. `steering.BuildWithScene` 组合当前场景、角色圣经、她自己的 `self_emotion` 和关系账本摘要，`session.context.append` + `channel:"developer"` 注入语音会话。
5. `planner.Consider` 复用本轮 Jev 的 `need_llm` 分数决定是否异步刷新 plan note，不再为门控额外打一次 System One。
6. 同一帧 `avatar.DriveWithRelationship` 把 **小春自己的情绪 + steering mode + 关系阶段** 映射成 Haru 表情/动作，经 WebSocket 推到查看器；用户的情绪是输入，不是她要镜像的目标。

阈值在 persona YAML 的 `judge.*` 和 `planner.*` 里改。

## Live2D

查看器在 `web/live2d`，默认模型是官方样本 **Haru**（来自 [CubismWebSamples](https://github.com/Live2D/CubismWebSamples)），现在前台展示的是“小春 · 桌面陪伴”而不是调试面板。角色按 Jev 的 persona 反应驱动：客人难过 → `comfort` → 小春自己的情绪先落到脸上；关系越深，界面里的“关系/心情/正在做”越有连续性。下行 PCM 的能量映射到 `ParamMouthOpenY` 做口型，情感参数在 Idle 动作之后每帧叠上去。`live2d --lipsync` 可在没语音时看张嘴。

Cubism Core 从 Live2D CDN 加载，首次需要能上网。Haru / Mao 素材受 [Live2D 免费素材协议](https://www.live2d.jp/en/terms/live2d-free-material-license-agreement/) 约束，不在 Apache-2.0 范围内。

## 目录

    cmd/tavernbot/          CLI
    internal/
      config/               base URL + 凭证解析
      jev/                  System One 客户端（/v1/systemone）
      judge/                每轮情感/人设判断 + mode 决策
      memory/               短期记忆 + EMA 情感状态 + 持久化关系账本
      planner/              Jev 门控 + 异步 LLM 规划
      steering/             judgment → instructions 组装
      llm/                  chat.completions 客户端
      audio/                μ-law 编解码、重采样、WAV、分块播放、麦克风
      livevoice/            WebRTC + WS 双工会话
      agent/                编排 loop
    personas/               人设 YAML + 角色圣经
    web/live2d/             Haru 查看器 + 官方样本模型
    internal/avatar/        Jev → 表情/动作映射 + WS hub

## 已知边界

- `delegation.type` 只支持 `client`；`responses` 委派不可用。
- `session.context.append` channel 只接受三种：
  - `speakable`：逐字念出（TTS），用于问候语和 `/say`
  - `developer`：静默注入，模型参考但不念、不主动回复——Jev 判断后的 steering
  - `commentary`：注入后模型会主动开口回应——planner 的长期引导走这里
- `response.create` 需要 Responses delegation，不可用；turn.done 带 role 可区分 user/assistant。
- `session.output_audio.delta` 是连续时间线，不是只在说话时才发。
- 麦克风采集默认关闭（`-tags tavern_mic` 开启）。
- 安全判断是启发式：safety noul 超过阈值就进 comfort 模式，但不会替代真人介入。

## License

Apache-2.0
