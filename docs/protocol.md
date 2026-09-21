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
