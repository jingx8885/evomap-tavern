# lov-evo

**进化酒馆的人格对话机器人**：Jev（System One）做结构化情感判断与异步门控，LLM 做长期规划，OpenAI 双工语音（gpt-live）做对话。

为 EvoMap 进化酒馆场景而生：一个能感知情绪、贴着人设说话、并且有长期方向的酒馆角色。

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

    # 启动双工会话
    ./tavernbot run --persona personas/tavern_keeper.yaml -v

凭证：`OPENAI_API_KEY` > `LOVBROWSER_API_KEY` > `~/.config/akasha/credentials.env`。
网关默认 `https://newapi.1234bot.com/v1`，可用 `NEW_API_BASE_URL` 或 `--base-url` 覆盖。

`run` 里的键盘指令：`/say <文本>`（让人设念出来）、`/steer <文本>`（改 session 指令）、`/goal <文本>`（手动设长期目标）、`/status`、`/quit`。

## 真实麦克风

默认构建上行走静音 RTP（协议要求上行必须活跃，speakable 文本才会被念出来）。要真麦克风：

    go build -tags tavern_mic ./cmd/tavernbot   # 需要 cgo / miniaudio

下行音频通过 `afplay`（macOS）/`aplay`（Linux）分块播放；找不到播放器时只落盘 WAV。

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
4. `steering.Build` 组合成 guidance，`session.context.append` + `channel:"developer"` 注入语音会话。
5. `planner.Tick` 每 N 轮问 Jev 是否到自然停顿且目标未尽；过了就异步让 LLM 刷新 plan note。

阈值在 persona YAML 的 `judge.*` 和 `planner.*` 里改。

## 目录

    cmd/tavernbot/          CLI
    internal/
      config/               base URL + 凭证解析
      jev/                  System One 客户端（/v1/systemone）
      judge/                每轮情感/人设判断 + mode 决策
      memory/               短期记忆 + EMA 情感状态
      planner/              Jev 门控 + 异步 LLM 规划
      steering/             judgment → instructions 组装
      llm/                  chat.completions 客户端
      audio/                μ-law 编解码、重采样、WAV、分块播放、麦克风
      livevoice/            WebRTC + WS 双工会话
      agent/                编排 loop
    personas/               人设 YAML

## 已知边界

- `delegation.type` 只支持 `client`；`responses` 委派不可用。
- `session.context.append` channel 只接受 speakable / commentary / developer；developer 静默注入不念出，是 steering 通道；`response.create` 需要 Responses delegation，不可用。
- `session.output_audio.delta` 是连续时间线，不是只在说话时才发。
- 麦克风采集默认关闭（`-tags tavern_mic` 开启）。
- 安全判断是启发式：safety noul 超过阈值就进 comfort 模式，但不会替代真人介入。

## License

Apache-2.0
