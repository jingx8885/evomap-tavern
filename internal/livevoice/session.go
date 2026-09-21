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

	"github.com/jingx8885/evomap-tavern/internal/audio"
	"github.com/jingx8885/evomap-tavern/internal/config"
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
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU},
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
	dc, err := pc.CreateDataChannel("oai-events", nil)
	if err == nil {
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			s.handleEvent(msg.Data)
		})
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

// uplink streams PCMU frames at 20ms cadence: mic frames when available,
// silence otherwise (the gateway needs active RTP to play speakable text).
func (s *Session) uplink(ctx context.Context) {
	micFrames, micStop, err := audio.OpenMic(audio.PCMUUplinkRate)
	if err != nil {
		s.logf("uplink: %v", err)
	}
	s.micStop = micStop
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
			if micFrames != nil {
				select {
				case f, ok := <-micFrames:
					if ok {
						frame = f
					}
				default:
				}
			}
			if frame == nil {
				select {
				case f := <-silence:
					frame = f
				default:
					frame = bytes.Repeat([]byte{audio.SilenceByte}, audio.PCMUFrameBytes)
				}
			}
			_ = s.track.WriteSample(media.Sample{
				Data:     frame,
				Duration: audio.PCMUFrameDur,
			})
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
	if s.Verbose && etype != "session.output_audio.delta" {
		s.logf("event %s", etype)
	}
	switch etype {
	case "session.started":
		s.startedOne.Do(func() { close(s.started) })
		s.emit(Event{Kind: EventStarted})
	case "session.input_audio.append":
		s.emit(Event{Kind: EventInputEcho})
	case "session.output_audio.delta":
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
		if s.player != nil {
			s.player.WritePCM(pcm)
		}
	case "turn.created":
		s.turnUser.Reset()
		s.turnAssistant.Reset()
		s.interim.Reset()
	case "output_transcript.added", "output_transcript.delta", "turn.delta":
		text := transcriptText(ev)
		if text == "" {
			return
		}
		speaker := detectSpeaker(ev)
		if speaker == "user" {
			s.turnUser.WriteString(text)
		} else {
			s.turnAssistant.WriteString(text)
		}
		s.interim.WriteString(text)
		s.emit(Event{Kind: EventTranscript, Speaker: speaker, Text: text})
	case "turn.done", "turn.completed", "response.done":
		s.finishTurn()
	case "session.usage.updated":
		usage, _ := ev["usage"].(map[string]any)
		s.emit(Event{Kind: EventUsage, Usage: usage})
	case "error":
		msg, _ := ev["error"].(map[string]any)
		s.emit(Event{Kind: EventError,
			Err: fmt.Errorf("%v", msg["message"])})
	}
}

func (s *Session) finishTurn() {
	user := s.turnUser.String()
	assistant := s.turnAssistant.String()
	if assistant == "" {
		assistant = s.interim.String()
	}
	s.turnUser.Reset()
	s.turnAssistant.Reset()
	s.interim.Reset()
	s.emit(Event{Kind: EventTurnDone, Text: user,
		Usage: map[string]any{"assistant": assistant}})
}

// transcriptText extracts delta text from the event, tolerating the
// several shapes the gateway has used.
func transcriptText(ev map[string]any) string {
	for _, k := range []string{"delta", "text"} {
		if s, ok := ev[k].(string); ok && s != "" {
			return s
		}
	}
	if item, ok := ev["item"].(map[string]any); ok {
		for _, k := range []string{"text", "transcript"} {
			if s, ok := item[k].(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// detectSpeaker distinguishes user vs assistant transcript events.
func detectSpeaker(ev map[string]any) string {
	if role, _ := ev["role"].(string); role != "" {
		return role
	}
	if item, ok := ev["item"].(map[string]any); ok {
		if role, _ := item["role"].(string); role != "" {
			return role
		}
	}
	return "assistant"
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
	return s.sendJSON(map[string]any{
		"type":    "session.context.append",
		"channel": "speakable",
		"content": []map[string]string{{"type": "input_text", "text": text}},
	})
}

// Steer updates session instructions mid-conversation.
func (s *Session) Steer(instructions string) error {
	return s.sendJSON(map[string]any{
		"type":    "session.update",
		"session": map[string]any{"instructions": instructions},
	})
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
