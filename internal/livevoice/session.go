// Package livevoice is the OpenAI-compatible gpt-live duplex voice client
// over the new-api gateway: WebRTC RTP uplink (PCMU 8kHz) + WebSocket
// event channel for transcripts, downlink audio, and session control.
package livevoice

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	"github.com/pion/interceptor"
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
	EventWarning    = "warning" // non-fatal: silent mic, degraded uplink
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
	playbackGate  audio.DownlinkGate
	onPCMMu       sync.Mutex
	onPCM         func([]byte)
	echoN         atomic.Int64
	writeErrs     atomic.Int64
	duckMicUntil  atomic.Int64
	inject        chan []byte
	skipMic       bool
	offerAudio    string
	answerAudio   string

	Verbose bool

	// Downlink speech. lineOpen stays set until the assistant turn
	// closes; speechUntil covers the playback tail after the last packet.
	lineOpen    atomic.Bool
	speechUntil atomic.Int64

	deferMu    sync.Mutex
	deferred   []deferredNote
	deferTimer *time.Timer
	noteMu     sync.Mutex
	noteFn     func(string)
	// contextSend overrides AppendContext in tests.
	contextSend func(channel, text string) error
}

// deferredNote is one context append waiting until she is quiet.
type deferredNote struct {
	channel string
	text    string
}

// ErrHeld means a commentary nudge was not injected into the live line.
// It waits until she is quiet, because that channel asks her to speak.
// Developer steering is quiet context and is never held.
var ErrHeld = errors.New("not injected into the live line")

// speechTail is how long after the latest downlink voice the line
// still counts as in progress. Packets lead the speaker by a little.
const speechTail = 900 * time.Millisecond

// speechStuck is how long a missing assistant turn.done may keep the
// line open after packets stop, so a dropped event cannot wedge steering.
const speechStuck = 5 * time.Second

// Connect establishes WebRTC + WS and returns a live session.
// instructions becomes the session-level persona prompt.
func Connect(ctx context.Context, baseURL, apiKey, instructions, voice string) (*Session, error) {
	return connect(ctx, baseURL, apiKey, instructions, voice, false)
}

// ConnectScripted is Connect without microphone capture. Uplink is
// silence plus optional InjectUlaw frames (for closed-loop ASR tests).
func ConnectScripted(ctx context.Context, baseURL, apiKey, instructions, voice string) (*Session, error) {
	return connect(ctx, baseURL, apiKey, instructions, voice, true)
}

func connect(ctx context.Context, baseURL, apiKey, instructions, voice string, skipMic bool) (*Session, error) {
	if voice == "" {
		voice = "cove"
	}
	s := &Session{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   config.DefaultLiveModel,
		voice:   voice,
		events:  make(chan Event, 2048),
		started: make(chan struct{}),
		player:  audio.NewPlayer(),
		inject:  make(chan []byte, 1024),
		skipMic: skipMic,
	}
	inner, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	pc, err := newPCMUPeerConnection()
	if err != nil {
		return nil, err
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
		if st == webrtc.PeerConnectionStateFailed {
			s.emit(Event{Kind: EventClosed, Err: fmt.Errorf("peer connection failed")})
		}
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
	s.offerAudio = sdpAudioLines(sdp)
	callID, answer, err := s.postCall(sdp, instructions)
	if err != nil {
		pc.Close()
		return nil, err
	}
	s.answerAudio = sdpAudioLines(answer)
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

// newPCMUPeerConnection negotiates only G.711 μ-law PT 0. The default
// MediaEngine prefers Opus; writing PCMU into an Opus sender is received
// as RTP (echo) but never looks like speech to ASR.
func newPCMUPeerConnection() (*webrtc.PeerConnection, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: 8000,
			Channels:  1,
		},
		PayloadType: 0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("register PCMU: %w", err)
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, fmt.Errorf("interceptors: %w", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("peer connection: %w", err)
	}
	return pc, nil
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

// duckHangover covers the winmm jitter buffer plus a beat of speaker
// tail so echo does not look like a barge-in to server VAD.
const duckHangover = 900 * time.Millisecond

func (s *Session) duckMic() {
	s.duckMicUntil.Store(time.Now().Add(duckHangover).UnixNano())
}

// uplink streams PCMU frames at 20ms cadence: mic frames when available,
// silence otherwise (the gateway needs active RTP to play speakable text).
func (s *Session) uplink(ctx context.Context) {
	silenceFrame := bytes.Repeat([]byte{audio.SilenceByte}, audio.PCMUFrameBytes)
	write := func(frame []byte) {
		if len(frame) == 0 {
			frame = silenceFrame
		}
		if err := s.track.WriteSample(media.Sample{
			Data:     frame,
			Duration: audio.PCMUFrameDur,
		}); err != nil {
			n := s.writeErrs.Add(1)
			if s.Verbose && n <= 3 {
				s.logf("uplink write: %v", err)
			}
		}
	}
	if !s.skipMic {
		micFrames, micStop, err := audio.OpenMic(audio.PCMUUplinkRate)
		if err != nil {
			s.logf("uplink: %v", err)
			s.emit(Event{Kind: EventWarning, Err: err})
		} else {
			s.logf("uplink: microphone %s", audio.MicFormat())
		}
		s.micStop = micStop
		if micFrames != nil {
			s.uplinkMic(ctx, micFrames, write, silenceFrame)
			return
		}
	} else {
		s.logf("uplink: scripted (no mic)")
	}

	ticker := time.NewTicker(audio.PCMUFrameDur)
	defer ticker.Stop()
	var n int
	var maxRMS float64
	lastLog := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var frame []byte
			select {
			case f := <-s.inject:
				frame = f
			default:
				frame = silenceFrame
			}
			if r := audio.UlawRMS(frame); r > maxRMS {
				maxRMS = r
			}
			n++
			write(frame)
			if s.Verbose && time.Since(lastLog) >= 2*time.Second {
				s.logf("uplink: frames=%d max_rms=%.4f echo=%d write_err=%d queued=%d",
					n, maxRMS, s.echoN.Load(), s.writeErrs.Load(), len(s.inject))
				n, maxRMS = 0, 0
				lastLog = time.Now()
			}
		}
	}
}

// uplinkLiveFrames is the most mic audio RTP may hold (6×20ms).
// A deeper queue is a stale backlog. Flushing it later is a second
// uplink, and server VAD treats that copy as a barge-in.
const uplinkLiveFrames = 6

func clipUplinkQueue(q [][]byte) (kept [][]byte, dropped int) {
	if len(q) <= uplinkLiveFrames {
		return q, 0
	}
	dropped = len(q) - uplinkLiveFrames
	return q[dropped:], dropped
}

// uplinkMic sends one PCMU frame per 20ms of wall clock. Capture can
// clump frames, and writing that clump immediately makes RTP timestamps
// run ahead of arrival time. The gateway then plays the clump about two
// seconds later and cuts the live utterance. Silence is sent only when
// the queue is actually empty, so a frame that is 1ms late is not replaced.
func (s *Session) uplinkMic(ctx context.Context, micFrames <-chan []byte, write func([]byte), silence []byte) {
	var n, ducked, dropped int
	var maxRMS, peakRMS float64
	start := time.Now()
	warned := false
	lastLog := time.Now()
	var queued [][]byte
	next := time.Now()
	timer := time.NewTimer(audio.PCMUFrameDur)
	defer timer.Stop()
	for {
		next = next.Add(audio.PCMUFrameDur)
		if wait := time.Until(next); wait > 0 {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(wait)
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
		} else if wait < -audio.PCMUFrameDur {
			// Fell behind the wall clock. Do not catch up with a burst.
			next = time.Now()
		}

		for {
			select {
			case <-ctx.Done():
				return
			case f, ok := <-micFrames:
				if !ok {
					return
				}
				queued = append(queued, f)
				var drop int
				queued, drop = clipUplinkQueue(queued)
				dropped += drop
			default:
				goto send
			}
		}
	send:
		frame := silence
		live := len(queued) > 0
		if live {
			frame = queued[0]
			queued = queued[1:]
			if r := audio.UlawRMS(frame); r > maxRMS {
				maxRMS = r
			}
			if maxRMS > peakRMS {
				peakRMS = maxRMS
			}
		}
		if !warned && time.Since(start) > 6*time.Second && peakRMS < 0.002 {
			warned = true
			s.emit(Event{Kind: EventWarning, Err: fmt.Errorf(
				"mic has produced only silence for 6s (%s); speech will not be recognized "+
					"- check the input device (TAVERN_MIC can force one) and mic permission", audio.MicFormat())})
		}
		if live && time.Now().UnixNano() < s.duckMicUntil.Load() {
			frame = silence
			ducked++
		}
		n++
		write(frame)
		if s.Verbose && time.Since(lastLog) >= 2*time.Second {
			s.logf("uplink: frames=%d max_rms=%.4f ducked=%d dropped=%d echo=%d format=%s",
				n, maxRMS, ducked, dropped, s.echoN.Swap(0), audio.MicFormat())
			if maxRMS < 0.002 {
				s.logf("uplink: mic looks silent; speech will not be recognized")
			}
			n, ducked, dropped, maxRMS = 0, 0, 0, 0
			lastLog = time.Now()
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
		} else if etype == "session.started" {
			s.logf("event %s %s", etype, clipRunes(string(data), 400))
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
		if audio.ChunkHasVoice(pcm) {
			s.duckMic()
			s.markSpeaking()
		}
		playbackPCM := s.playbackGate.Filter(pcm)
		if s.player != nil {
			// Keep pauses inside an utterance, but do not queue the gateway's
			// continuous idle timeline. A burst of old silence otherwise sits
			// in front of fresh speech and grows into seconds of downlink lag.
			s.player.WritePCM(playbackPCM)
		}
		s.onPCMMu.Lock()
		fn := s.onPCM
		s.onPCMMu.Unlock()
		if fn != nil {
			// Use the same gated samples sent to the player. Feeding the
			// raw gateway timeline here made the avatar move before sound
			// whenever the speaker queue had accumulated stale audio.
			fn(playbackPCM)
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
		changed := applyTranscript(dst, text)
		if speaker != "user" && applyTranscript(&s.interim, text) {
			changed = true
		}
		// The gateway resends the same cumulative transcript on a timer.
		// Emitting each copy flooded the agent loop, so turn.done was
		// dropped and the voice model stayed in a reply.
		if !changed {
			return
		}
		if speaker != "user" {
			s.duckMic()
			s.markSpeaking()
		}
		s.emit(Event{Kind: EventTranscript, Speaker: speaker, Text: dst.String()})
	case etype == "turn.done" || etype == "turn.completed" || etype == "response.done":
		role := eventRole(ev)
		if role == "assistant" || etype == "response.done" {
			s.markLineClosed()
		}
		s.finishTurn(role, transcriptText(ev))
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
	if strings.Contains(t, "input_audio") {
		return false
	}
	// turn.delta duplicates input/output_transcript.added and often
	// has no role, which used to append assistant speech onto the user.
	if t == "turn.delta" {
		return false
	}
	return strings.Contains(t, "transcript")
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
// It reports whether the buffer changed. A repeated snapshot must not
// look like a new utterance.
func applyTranscript(dst *strings.Builder, text string) bool {
	if text == "" {
		return false
	}
	cur := dst.String()
	switch {
	case cur == "":
		dst.WriteString(text)
		return true
	case text == cur:
		return false
	case strings.HasSuffix(cur, text):
		// added + delta often carry the same chunk
		return false
	case strings.HasPrefix(text, cur):
		dst.Reset()
		dst.WriteString(text)
		return true
	case strings.HasPrefix(cur, text):
		return false
	default:
		dst.WriteString(text)
		return true
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
	if strings.Contains(etype, "output") {
		return "assistant"
	}
	if etype == "turn.delta" {
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

// EchoCount is how many session.input_audio.append echoes the gateway sent.
func (s *Session) EchoCount() int64 {
	return s.echoN.Load()
}

// SDPAudio returns negotiated audio lines from the local offer and remote answer.
func (s *Session) SDPAudio() (offer, answer string) {
	return s.offerAudio, s.answerAudio
}

func sdpAudioLines(sdp string) string {
	var parts []string
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "m=audio") || strings.HasPrefix(line, "a=rtpmap:") || strings.HasPrefix(line, "a=fmtp:") {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, " | ")
}

// InjectUlaw queues 20ms PCMU frames onto the uplink (scripted ASR tests).
func (s *Session) InjectUlaw(frames [][]byte) error {
	if s.inject == nil {
		return fmt.Errorf("uplink inject not ready")
	}
	for _, f := range frames {
		cp := append([]byte(nil), f...)
		select {
		case s.inject <- cp:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("inject stalled")
		}
	}
	return nil
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
	fitted := FitHead(text)
	if fitted != strings.TrimSpace(text) {
		s.logf("context append clipped channel=%s tokens~%d", channel, estimateTokens(fitted))
	}
	return s.sendJSON(map[string]any{
		"type":    "session.context.append",
		"channel": channel,
		"content": []map[string]string{{"type": "input_text", "text": fitted}},
	})
}

// Nudge injects a proactive cue through the commentary channel:
// the model sees it and may respond aloud, incorporating it.
// Use it for planner-driven conversation moves.
// A line already in progress is not cut; the cue waits until it ends.
func (s *Session) Nudge(cue string) error {
	if s.hold("commentary", cue) {
		return ErrHeld
	}
	return s.sendContext("commentary", cue)
}

// Respond asks the model to produce a response turn.
func (s *Session) Respond() error {
	return s.sendJSON(map[string]any{"type": "response.create"})
}

// Steer pushes behavioral guidance through the developer channel.
// Upstream rejects session.update for instructions after initialization.
// This channel is quiet context: the model may use it on a later reply,
// and appending it does not ask her to speak or open another turn.
// Send it while she is already talking. Holding it until the line ends
// just drops the note she needed for this exchange.
func (s *Session) Steer(guidance string) error {
	if strings.TrimSpace(guidance) == "" {
		return nil
	}
	return s.sendContext("developer", guidance)
}

// Speaking reports whether downlink speech is still in progress.
func (s *Session) Speaking() bool {
	now := time.Now().UnixNano()
	until := s.speechUntil.Load()
	if now < until {
		return true
	}
	if s.lineOpen.Load() && now < until+int64(speechStuck) {
		return true
	}
	return false
}

// SetNote receives lines the session wants on the agent log,
// such as a commentary nudge delivered after the line.
// Developer steering is not deferred.
func (s *Session) SetNote(fn func(string)) {
	s.noteMu.Lock()
	s.noteFn = fn
	s.noteMu.Unlock()
}

func (s *Session) note(msg string) {
	s.noteMu.Lock()
	fn := s.noteFn
	s.noteMu.Unlock()
	if fn != nil {
		fn(msg)
	}
}

func (s *Session) markSpeaking() {
	s.lineOpen.Store(true)
	s.speechUntil.Store(time.Now().Add(speechTail).UnixNano())
	s.rescheduleDeferred()
}

func (s *Session) markLineClosed() {
	if !s.lineOpen.Swap(false) {
		return
	}
	s.speechUntil.Store(time.Now().Add(speechTail).UnixNano())
	s.rescheduleDeferred()
}

func (s *Session) rescheduleDeferred() {
	s.deferMu.Lock()
	if len(s.deferred) > 0 {
		s.armDeferLocked()
	}
	s.deferMu.Unlock()
}

// hold queues text when a line is in progress or an older note is
// already waiting. The caller must not also send it.
func (s *Session) hold(channel, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return true
	}
	s.deferMu.Lock()
	defer s.deferMu.Unlock()
	if !s.Speaking() && len(s.deferred) == 0 {
		return false
	}
	s.enqueueLocked(channel, text)
	return true
}

func (s *Session) enqueueLocked(channel, text string) {
	for i := range s.deferred {
		if s.deferred[i].channel != channel {
			continue
		}
		if channel == "commentary" {
			s.deferred[i].text = text
		} else {
			s.deferred[i].text = s.deferred[i].text + "\n" + text
		}
		s.armDeferLocked()
		return
	}
	s.deferred = append(s.deferred, deferredNote{channel: channel, text: text})
	s.armDeferLocked()
}

func (s *Session) armDeferLocked() {
	delay := 40 * time.Millisecond
	if until := s.speechUntil.Load(); until > time.Now().UnixNano() {
		wait := time.Until(time.Unix(0, until)) + 40*time.Millisecond
		if wait > delay {
			delay = wait
		}
	}
	if s.lineOpen.Load() && delay < 200*time.Millisecond {
		delay = 200 * time.Millisecond
	}
	if s.deferTimer != nil {
		s.deferTimer.Stop()
	}
	s.deferTimer = time.AfterFunc(delay, s.flushDeferred)
}

func (s *Session) flushDeferred() {
	if s.Speaking() {
		s.deferMu.Lock()
		if len(s.deferred) > 0 {
			s.armDeferLocked()
		}
		s.deferMu.Unlock()
		return
	}
	s.deferMu.Lock()
	batch := s.deferred
	s.deferred = nil
	if s.deferTimer != nil {
		s.deferTimer.Stop()
		s.deferTimer = nil
	}
	s.deferMu.Unlock()
	for _, item := range batch {
		// Only commentary is queued. A developer note here is quiet
		// context that should have been sent immediately; deliver it
		// without treating the flush as a new spoken turn.
		if item.channel == "developer" {
			if err := s.sendContext(item.channel, item.text); err != nil {
				s.note(fmt.Sprintf("[steer] failed: %v", err))
			}
			continue
		}
		text := item.text
		if err := s.sendContext(item.channel, text); err != nil {
			s.note(fmt.Sprintf("[%s] after line failed: %v", noteTag(item.channel), err))
			continue
		}
		s.note(fmt.Sprintf("[%s] after line", noteTag(item.channel)))
	}
}

func noteTag(channel string) string {
	if channel == "commentary" {
		return "nudge"
	}
	return "steer"
}

func (s *Session) sendContext(channel, text string) error {
	if s.contextSend != nil {
		return s.contextSend(channel, text)
	}
	return s.AppendContext(channel, text)
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
	if s.events == nil {
		return
	}
	// Closed/error must not be dropped: the agent reconnects on them.
	// turn.done must not be dropped either. A burst of partial
	// transcripts used to fill this channel and the turn never reached
	// Jev, so the voice model stayed in a reply.
	if ev.Kind == EventClosed || ev.Kind == EventError || ev.Kind == EventTurnDone {
		select {
		case s.events <- ev:
		case <-time.After(time.Second):
		}
		return
	}
	select {
	case s.events <- ev:
	default:
	}
}

// Close ends the session.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.deferMu.Lock()
		if s.deferTimer != nil {
			s.deferTimer.Stop()
			s.deferTimer = nil
		}
		s.deferred = nil
		s.deferMu.Unlock()
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
