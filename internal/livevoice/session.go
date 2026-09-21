// Package livevoice is the OpenAI-compatible gpt-live duplex voice client
// over the new-api gateway: WebRTC RTP uplink (PCMU 8kHz) + WebSocket
// event channel for transcripts, downlink audio, and session control.
package livevoice

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/jingx8885/lov-evo/internal/audio"
	"github.com/jingx8885/lov-evo/internal/config"
)

// Event kinds delivered on Session.Events.
const (
	EventStarted    = "started"    // session.started
	EventInputEcho  = "input_echo" // uplink RTP acknowledged
	EventTurnDone   = "turn_done"  // a conversational turn completed
	EventTranscript = "transcript" // incremental text (user or assistant)
	EventUsage      = "usage"
	EventError      = "error"
	EventClosed     = "closed"
)

// Event is one thing the session wants the agent to know.
type Event struct {
	Kind    string
	Speaker string // for EventTranscript: "user" | "assistant"
	Text    string
	Usage   map[string]any
	Err     error
}

// Turn carries a completed turn's text.
type Turn struct {
	Role          string
	UserText      string
	AssistantText string
}

// Session is one duplex voice call.
type Session struct {
	baseURL string
	apiKey  string
	model   string
	voice   string

	pc      *webrtc.PeerConnection
	track   *webrtc.TrackLocalStaticSample
	ws      *websocket.Conn
	cancel  context.CancelFunc
	events  chan Event
	player  *audio.Player
	micStop func()

	sendMu     sync.Mutex
	started    chan struct{}
	startedOne sync.Once
	closeOnce  sync.Once

	turnUser      strings.Builder
	turnAssistant strings.Builder
	interim       strings.Builder
	pcmBytes      atomic.Int64
	pcmMu         sync.Mutex
	pcmAll        []byte
	onPCMMu       sync.Mutex
	onPCM         func([]byte)
	echoN         atomic.Int64
	duckMicUntil  atomic.Int64

	Verbose bool
}

// Connect establishes WebRTC + WS and returns a live session.
// instructions becomes the session-level persona prompt.
func Connect(ctx context.Context, baseURL, apiKey, instructions, voice string) (*Session, error) {
	if voice == "" {
		voice = "cove"
	}
	s := &Session{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   config.DefaultLiveModel,
		voice:   voice,
		events:  make(chan Event, 256),
		started: make(chan struct{}),
		player:  audio.NewPlayer(),
	}
	inner, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("peer connection: %w", err)
	}
	s.pc = pc

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: 8000,
			Channels:  1,
		},
		"audio", "tavernbot")
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("track: %w", err)
	}
	s.track = track
	sender, err := pc.AddTrack(track)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("add track: %w", err)
	}
	// RTCP receiver reports must be drained so the buffer does not stall.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) {
		fmt.Printf("[livevoice] ice %s\n", st)
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		fmt.Printf("[livevoice] pc %s\n", st)
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			buf := make([]byte, 1500)
			for {
				if _, _, err := tr.Read(buf); err != nil {
					return
				}
			}
		}()
	})
	// Keep the OpenAI-style data channel in the SDP so the gateway
	// is happy, but do not parse its messages. Events already arrive
	// on GET /live/{call_id}; handling both replays every PCM chunk
	// and double-fires turn.done.
	if _, err := pc.CreateDataChannel("oai-events", nil); err != nil {
		s.logf("data channel: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return nil, fmt.Errorf("set local: %w", err)
	}
	<-webrtc.GatheringCompletePromise(pc)

	sdp := pc.LocalDescription().SDP
	if !strings.HasSuffix(sdp, "\n") {
		sdp += "\n"
	}
	callID, answer, err := s.postCall(sdp, instructions)
	if err != nil {
		pc.Close()
		return nil, err
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: answer,
	}); err != nil {
		pc.Close()
		return nil, fmt.Errorf("set remote: %w", err)
	}

	wsURL := strings.Replace(s.baseURL, "http", "ws", 1) + "/live/" + url.PathEscape(callID)
	hdr := http.Header{"Authorization": {"Bearer " + apiKey}}
	ws, _, err := websocket.DefaultDialer.DialContext(inner, wsURL, hdr)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("ws dial: %w", err)
	}
	s.ws = ws
	s.logf("call %s joined", callID)

	go s.wsReader(inner)
	go s.uplink(inner)
	return s, nil
}

// postCall does POST /realtime/calls (multipart sdp + session).
func (s *Session) postCall(sdp, instructions string) (callID, answer string, err error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	sdpPart, _ := w.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {"form-data; name=\"sdp\""},
		"Content-Type":        {"application/sdp"},
	})
	sdpPart.Write([]byte(sdp))
	sessPart, _ := w.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {"form-data; name=\"session\""},
		"Content-Type":        {"application/json"},
	})
	sess, _ := json.Marshal(map[string]any{
		"model":        s.model,
		"instructions": instructions,
		"audio":        map[string]any{"output": map[string]any{"voice": s.voice}},
	})
	sessPart.Write(sess)
	w.Close()

	req, err := http.NewRequest(http.MethodPost, s.baseURL+"/realtime/calls", &body)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Accept", "application/sdp")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("call create: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("call create HTTP %d: %.400s", resp.StatusCode, raw)
	}
	loc := resp.Header.Get("Location")
	callID = loc[strings.LastIndex(loc, "/")+1:]
	if callID == "" {
		return "", "", fmt.Errorf("call create: missing Location header")
	}
	return callID, string(raw), nil
}

// duckMouthMin is MouthOpen of real speech, not comfort-noise padding on
// the continuous downlink timeline. A low threshold latches ducking and
// the gateway only ever hears silence.
const duckMouthMin = 0.18

// uplink streams PCMU frames at 20ms cadence: mic frames when available,
// silence otherwise (the gateway needs active RTP to play speakable text).
func (s *Session) uplink(ctx context.Context) {
	micFrames, micStop, err := audio.OpenMic(audio.PCMUUplinkRate)
	if err != nil {
		s.logf("uplink: %v", err)
	} else {
		s.logf("uplink: microphone %s", audio.MicFormat())
	}
	s.micStop = micStop
	silenceFrame := bytes.Repeat([]byte{audio.SilenceByte}, audio.PCMUFrameBytes)
	write := func(frame []byte) {
		if len(frame) == 0 {
			frame = silenceFrame
		}
		_ = s.track.WriteSample(media.Sample{
			Data:     frame,
			Duration: audio.PCMUFrameDur,
		})
	}
	if micFrames != nil {
		s.uplinkMic(ctx, micFrames, write, silenceFrame)
		return
	}

	stop := make(chan struct{})
	defer close(stop)
	silence := audio.SilenceFrames(stop)
	ticker := time.NewTicker(audio.PCMUFrameDur)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var frame []byte
			select {
			case f := <-silence:
				frame = f
			default:
				frame = silenceFrame
			}
			write(frame)
		}
	}
}

// uplinkMic paces RTP from capture itself. The old ticker + non-blocking
// read stuffed silence whenever a mic frame was 1ms late, which shreds ASR.
func (s *Session) uplinkMic(ctx context.Context, micFrames <-chan []byte, write func([]byte), silence []byte) {
	var n, ducked int
	var maxRMS float64
	lastLog := time.Now()
	stall := time.NewTimer(80 * time.Millisecond)
	defer stall.Stop()
	resetStall := func() {
		if !stall.Stop() {
			select {
			case <-stall.C:
			default:
			}
		}
		stall.Reset(80 * time.Millisecond)
	}
	for {
		resetStall()
		select {
		case <-ctx.Done():
			return
		case f, ok := <-micFrames:
			if !ok {
				return
			}
			if r := audio.UlawRMS(f); r > maxRMS {
				maxRMS = r
			}
			frame := f
			if time.Now().UnixNano() < s.duckMicUntil.Load() {
				frame = silence
				ducked++
			}
			n++
			write(frame)
			if s.Verbose && time.Since(lastLog) >= 2*time.Second {
				s.logf("uplink: frames=%d max_rms=%.4f ducked=%d echo=%d format=%s",
					n, maxRMS, ducked, s.echoN.Swap(0), audio.MicFormat())
				if maxRMS < 0.002 {
					s.logf("uplink: mic looks silent; speech will not be recognized")
				}
				n, ducked, maxRMS = 0, 0, 0
				lastLog = time.Now()
			}
		case <-stall.C:
			write(silence)
		}
	}
}

// wsReader is the event pump.
func (s *Session) wsReader(ctx context.Context) {
	for {
		_, data, err := s.ws.ReadMessage()
		if err != nil {
			s.emit(Event{Kind: EventClosed, Err: err})
			return
		}
		s.handleEvent(data)
	}
}

// handleEvent parses one gateway event.
func (s *Session) handleEvent(data []byte) {
	var ev map[string]any
	if err := json.Unmarshal(data, &ev); err != nil {
		return
	}
	etype, _ := ev["type"].(string)
	if s.Verbose && etype != "session.output_audio.delta" && etype != "session.input_audio.append" {
		if tx := transcriptText(ev); tx != "" {
			s.logf("event %s text=%q", etype, clipRunes(tx, 80))
		} else {
			s.logf("event %s", etype)
		}
	}
	switch {
	case etype == "session.started":
		s.markStarted()
	case etype == "session.input_audio.append":
		s.markStarted()
		s.echoN.Add(1)
		s.emit(Event{Kind: EventInputEcho})
	case etype == "session.output_audio.delta":
		chunk := strField(ev, "delta", "audio", "data")
		if chunk == "" {
			return
		}
		pcm, err := base64.StdEncoding.DecodeString(chunk)
		if err != nil {
			return
		}
		s.pcmBytes.Add(int64(len(pcm)))
		s.pcmMu.Lock()
		s.pcmAll = append(s.pcmAll, pcm...)
		s.pcmMu.Unlock()
		mouth := audio.MouthOpen(pcm)
		if mouth > duckMouthMin {
			s.duckMicUntil.Store(time.Now().Add(400 * time.Millisecond).UnixNano())
		}
		// Comfort-noise padding is a continuous timeline; playing it
		// leaks into the mic, VAD never ends, and the model never replies.
		if s.player != nil && mouth > 0 {
			s.player.WritePCM(pcm)
		}
		s.onPCMMu.Lock()
		fn := s.onPCM
		s.onPCMMu.Unlock()
		if fn != nil {
			fn(pcm)
		}
	case etype == "turn.created":
		// First transcript chunks often arrive *before* turn.created.
		// Resetting here dropped the opening words ("欢迎来到").
	case isTranscriptEvent(etype):
		text := transcriptText(ev)
		if text == "" {
			return
		}
		speaker := detectSpeaker(etype, ev)
		dst := &s.turnAssistant
		if speaker == "user" {
			dst = &s.turnUser
		}
		applyTranscript(dst, text)
		if speaker != "user" {
			applyTranscript(&s.interim, text)
			s.duckMicUntil.Store(time.Now().Add(400 * time.Millisecond).UnixNano())
		}
		s.emit(Event{Kind: EventTranscript, Speaker: speaker, Text: dst.String()})
	case etype == "turn.done" || etype == "turn.completed" || etype == "response.done":
		s.finishTurn(eventRole(ev), transcriptText(ev))
	case etype == "session.usage.updated":
		s.markStarted()
		usage, _ := ev["usage"].(map[string]any)
		s.emit(Event{Kind: EventUsage, Usage: usage})
	case etype == "error":
		msg, _ := ev["error"].(map[string]any)
		s.emit(Event{Kind: EventError,
			Err: fmt.Errorf("%v", msg["message"])})
	}
}

func (s *Session) markStarted() {
	s.startedOne.Do(func() {
		close(s.started)
		s.emit(Event{Kind: EventStarted})
	})
}

func (s *Session) finishTurn(role, eventText string) {
	user := s.turnUser.String()
	assistant := s.turnAssistant.String()
	if assistant == "" {
		assistant = s.interim.String()
	}
	if eventText != "" {
		if role == "user" {
			if len([]rune(eventText)) >= len([]rune(user)) {
				user = eventText
			}
		} else if len([]rune(eventText)) >= len([]rune(assistant)) {
			assistant = eventText
		}
	}
	s.turnUser.Reset()
	s.turnAssistant.Reset()
	s.interim.Reset()
	s.emit(Event{Kind: EventTurnDone, Speaker: role, Text: user,
		Usage: map[string]any{"assistant": assistant}})
}

func isTranscriptEvent(t string) bool {
	switch t {
	case "output_transcript.added", "output_transcript.delta",
		"input_transcript.added", "input_transcript.delta",
		"turn.delta":
		return true
	default:
		return strings.Contains(t, "transcription")
	}
}

func eventRole(ev map[string]any) string {
	if role, _ := ev["role"].(string); role != "" {
		return role
	}
	if tt, ok := ev["turn"].(map[string]any); ok {
		if role, _ := tt["role"].(string); role != "" {
			return role
		}
	}
	if item, ok := ev["item"].(map[string]any); ok {
		if role, _ := item["role"].(string); role != "" {
			return role
		}
	}
	return ""
}

// applyTranscript merges one gateway transcript event into the turn buffer.
// Events are sometimes single-character deltas and sometimes growing
// snapshots; replacing on every event kept only the last character ("?").
func applyTranscript(dst *strings.Builder, text string) {
	if text == "" {
		return
	}
	cur := dst.String()
	switch {
	case cur == "":
		dst.WriteString(text)
	case text == cur:
	case strings.HasSuffix(cur, text):
		// added + delta often carry the same chunk
	case strings.HasPrefix(text, cur):
		dst.Reset()
		dst.WriteString(text)
	case strings.HasPrefix(cur, text):
	default:
		dst.WriteString(text)
	}
}

// transcriptText extracts delta text from the event, tolerating the
// several shapes the gateway has used.
func transcriptText(ev map[string]any) string {
	for _, k := range []string{"delta", "text", "transcript"} {
		if s, ok := ev[k].(string); ok && s != "" {
			return s
		}
	}
	for _, k := range []string{"delta", "item", "transcript", "turn"} {
		m, ok := ev[k].(map[string]any)
		if !ok {
			continue
		}
		for _, ck := range []string{"text", "transcript", "delta"} {
			if s, ok := m[ck].(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// detectSpeaker distinguishes user vs assistant transcript events.
func detectSpeaker(etype string, ev map[string]any) string {
	if role := eventRole(ev); role != "" {
		return role
	}
	if strings.Contains(etype, "input") {
		return "user"
	}
	return "assistant"
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	return ""
}

// WaitStarted blocks until session.started or ctx expires.
func (s *Session) WaitStarted(ctx context.Context) error {
	select {
	case <-s.started:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Speak makes the assistant say text verbatim via the speakable channel.
// Uplink RTP must already be flowing.
func (s *Session) Speak(text string) error {
	// Server VAD will hold speakable TTS while the uplink looks busy.
	s.duckMicUntil.Store(time.Now().Add(4 * time.Second).UnixNano())
	return s.sendJSON(map[string]any{
		"type":    "session.context.append",
		"channel": "speakable",
		"content": []map[string]string{{"type": "input_text", "text": text}},
	})
}

// AppendContext appends text to a context channel. The only channel
// confirmed by the gateway contract is "speakable" (verbatim TTS);
// other channels are experimental - see the ctxprobe command.
func (s *Session) AppendContext(channel, text string) error {
	return s.sendJSON(map[string]any{
		"type":    "session.context.append",
		"channel": channel,
		"content": []map[string]string{{"type": "input_text", "text": text}},
	})
}

// Nudge injects a proactive cue through the commentary channel:
// the model sees it and may respond aloud, incorporating it.
// Use it for planner-driven conversation moves.
func (s *Session) Nudge(cue string) error {
	return s.AppendContext("commentary", cue)
}

// Respond asks the model to produce a response turn.
func (s *Session) Respond() error {
	return s.sendJSON(map[string]any{"type": "response.create"})
}

// Steer pushes behavioral guidance mid-conversation through the
// developer context channel. Upstream rejects session.update for
// instructions after initialization, so we inject the steering note
// as silent developer context instead.
func (s *Session) Steer(guidance string) error {
	return s.AppendContext("developer", guidance)
}

func (s *Session) sendJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.ws.WriteMessage(websocket.TextMessage, data)
}

// Events returns the event channel.
func (s *Session) Events() <-chan Event { return s.events }

// OnPCM registers a callback for each downlink PCM chunk (s16le 24kHz).
// Used to drive Live2D lip sync from the same stream the player hears.
func (s *Session) OnPCM(fn func([]byte)) {
	s.onPCMMu.Lock()
	s.onPCM = fn
	s.onPCMMu.Unlock()
}

// PCM returns all downlink PCM so far (s16le 24kHz mono).
func (s *Session) PCM() []byte {
	s.pcmMu.Lock()
	defer s.pcmMu.Unlock()
	out := make([]byte, len(s.pcmAll))
	copy(out, s.pcmAll)
	return out
}

// CallID is exposed for logging.
func (s *Session) logf(format string, args ...any) {
	if s.Verbose {
		fmt.Printf("[livevoice] "+format+"\n", args...)
	}
}

func (s *Session) emit(ev Event) {
	select {
	case s.events <- ev:
	default:
	}
}

// Close ends the session.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		if s.ws != nil {
			_ = s.ws.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
			s.ws.Close()
		}
		if s.micStop != nil {
			s.micStop()
		}
		if s.pc != nil {
			s.pc.Close()
		}
		if s.player != nil {
			s.player.Flush()
			s.player.Close()
		}
	})
}
