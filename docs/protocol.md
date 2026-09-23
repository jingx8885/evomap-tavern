# new-api gpt-live 协议事实

来源：akasha-grimoire `skills/codex-live-voice/references/new-api-contract.md`。

| 能力 | 方法与路径 | 说明 |
|---|---|---|
| 创建双工呼叫 | `POST /v1/realtime/calls` (multipart) | `sdp` + `session` 两个字段 |
| 事件通道 | `GET /v1/live/{call_id}` (WS) | upgrade 到上游 |

- `session` JSON: `model`, `instructions`, `audio.output.voice`；网关把 `gpt-live-1-boulder-alpha` 映射成 `gpt-live-1-codex` 并补 `delegation:{type:"client"}`。
- SDP offer 必须保留结尾 CRLF。
- 上行 PCMU 8000Hz / 20ms 帧；上行 RTP 不流动时 speakable 不会被念。
- `session.context.append` + `channel:"speakable"` → assistant 逐字念 content。
- 下行 `session.output_audio.delta` = base64 PCM s16le 24kHz mono，连续时间线。
- `session.update` 改 instructions；`session.close` 结束。
- `session.usage.updated` 累计 `audio_duration_ms`。
- 边界：纯 WS 只能收；不做音频重编码；`responses` 委派不可用。

## context.append channels（实测）

| channel | 行为 | 用途 |
|---|---|---|
| speakable | 逐字念出（TTS） | 问候语、/say |
| developer | 静默注入，不念、不主动回复 | Jev steering note |
| commentary | 注入后模型会主动开口回应 | planner 长期引导 nudge |

其他 channel（context/ephemeral/instructions/memory）上游报 Invalid value。

## client 委派（实测 2026-09-23）

用户提一件她做不了的事（画图、看屏幕、跑任务）时，上游不会当场编答案，而是先把这一轮交给客户端：

1. 下行 `delegation.created`，常常早于用户那一轮的 `turn.done`：
   `{"item":{"id":"item_...","type":"delegation","content":[{"type":"input_text","text":"<用户原话>"}],"handoff_id":"handoff_1","target":"client","user_bidi_turn_id":"turn_..."},"offset_ms":...}`
2. 她先说一句垫话（“嗯，我看看啊”“别催，我这就给你弄”），然后**等客户端回复**。没人回复，这一轮就停在半句上。
3. 客户端回复：`{"type":"delegation.context.append","delegation_item_id":"<item.id>","channel":"commentary","content":[{"type":"input_text","text":"..."}]}`，上游回 `delegation.context.appended`。
   - `commentary`：她用自己的话转述；`speakable`：逐字念。
   - `developer` 会被拒：`Invalid value: 'developer'. Supported values are: 'speakable' and 'commentary'.`
   - 等委派期间走 `session.context.append` 的 developer steering 不算回复，她照样等着。
4. 同一个 id 可以回复多次，每次都会开口：先回“开始了”，任务完成后再回结果。12 秒后才回复也照样被接受并说出来。

客户端做法（`internal/agent`）：任务自己的“开始了 / 做好了 / 失败了”那句话走这个 id；这一轮什么都不跑时立刻用 commentary 让她自己接话；对已开任务的追问约 2 秒后回队列里的真实进度；12 秒还没人回复就兜底一次。委派 id 10 分钟后失效；在没有交出来的轮次里新开任务时，已回复过的旧 id 退役，那项任务的结果只静默参考，不让她主动开口。
