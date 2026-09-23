package audio

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMuLawRoundTrip(t *testing.T) {
	for _, s := range []int16{0, 1000, -1000, 20000, -20000, 32767, -32768} {
		u := MuLawEncode(s)
		d := MulawDecodeTable[u]
		if abs16(int32(d)-int32(s)) > 2000 {
			t.Fatalf("sample %d -> %d too far", s, d)
		}
	}
}

func abs16(x int32) int32 {
	if x < 0 {
		return -x
	}
	return x
}

func TestResamplePCM(t *testing.T) {
	pcm := make([]byte, 160*2)
	for i := 0; i < 160; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(i*100))
	}
	out := ResamplePCM(pcm, 16000, 8000)
	if len(out) != 80*2 {
		t.Fatalf("want 160 bytes, got %d", len(out))
	}
}

func TestUpsampleS16LE2x(t *testing.T) {
	pcm := make([]byte, 8)
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(1000)))
	}
	out := UpsampleS16LE2x(pcm)
	if len(out) != 16 {
		t.Fatalf("len %d", len(out))
	}
	for i := 0; i < 8; i++ {
		if v := int16(binary.LittleEndian.Uint16(out[i*2:])); v != 1000 {
			t.Fatalf("dc sample %d = %d", i, v)
		}
	}
	ramp := make([]byte, 4)
	binary.LittleEndian.PutUint16(ramp[0:], uint16(int16(0)))
	binary.LittleEndian.PutUint16(ramp[2:], uint16(int16(10)))
	got := UpsampleS16LE2x(ramp)
	want := []int16{0, 5, 10, 10}
	for i, w := range want {
		if v := int16(binary.LittleEndian.Uint16(got[i*2:])); v != w {
			t.Fatalf("ramp[%d]=%d want %d", i, v, w)
		}
	}

	// The following sample belongs to the next block. The midpoint at the
	// boundary has to use it; holding the last sample is a click every block.
	continued := make([]byte, 6)
	binary.LittleEndian.PutUint16(continued[0:], uint16(int16(0)))
	binary.LittleEndian.PutUint16(continued[2:], uint16(int16(10)))
	binary.LittleEndian.PutUint16(continued[4:], uint16(int16(20)))
	dst := make([]byte, 8)
	UpsampleS16LE2xFrame(dst, continued, 2)
	want = []int16{0, 5, 10, 15}
	for i, w := range want {
		if v := int16(binary.LittleEndian.Uint16(dst[i*2:])); v != w {
			t.Fatalf("continued[%d]=%d want %d", i, v, w)
		}
	}
}

func TestPlaybackCueDoesNotReprimeAcrossAShortGap(t *testing.T) {
	const (
		frame = 100
		prime = 400
	)
	var cue playbackCue
	t0 := time.Unix(0, 0)
	if cue.ready(prime-1, false, t0, frame, prime) {
		t.Fatal("started before the first utterance was primed")
	}
	if !cue.ready(prime, false, t0, frame, prime) {
		t.Fatal("primed utterance should play")
	}

	drained := t0.Add(time.Second)
	if !cue.ready(0, true, drained.Add(time.Hour), frame, prime) {
		t.Fatal("a playing device should stay armed")
	}
	if !cue.ready(0, false, drained, frame, prime) {
		t.Fatal("a just-drained device should stay armed")
	}
	if !cue.ready(frame, false, drained.Add(40*time.Millisecond), frame, prime) {
		t.Fatal("audio inside the idle window should play without another prime")
	}

	again := drained.Add(80 * time.Millisecond)
	if !cue.ready(0, false, again, frame, prime) {
		t.Fatal("second drain should stay armed")
	}
	idle := again.Add(playbackIdleReset)
	if cue.ready(0, false, idle, frame, prime) {
		t.Fatal("a long idle should require a new prime")
	}
	if cue.ready(frame, false, idle.Add(time.Millisecond), frame, prime) {
		t.Fatal("one frame after a long idle should wait for a full prime")
	}
	if !cue.ready(prime, false, idle.Add(2*time.Millisecond), frame, prime) {
		t.Fatal("a full prime after idle should play")
	}
}

func TestWriteWAV(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.wav")
	if err := WriteWAV(f, []byte{1, 0, 2, 0}, 24000); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(f)
	if string(raw[:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		t.Fatal("bad wav header")
	}
	pcm, rate, ch, err := ReadWAV(f)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 24000 || ch != 1 || len(pcm) != 4 {
		t.Fatalf("roundtrip rate=%d ch=%d n=%d", rate, ch, len(pcm))
	}
}

func TestMouthOpenSilence(t *testing.T) {
	pcm := make([]byte, 480)
	if v := MouthOpen(pcm); v != 0 {
		t.Fatalf("silence %v", v)
	}
}

func TestMouthOpenLoud(t *testing.T) {
	pcm := make([]byte, 480)
	for i := 0; i < 240; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(22000)))
	}
	v := MouthOpen(pcm)
	if v < 0.5 {
		t.Fatalf("want open mouth, got %v", v)
	}
}

func TestMouthOpenUsesLastWindow(t *testing.T) {
	pcm := make([]byte, (mouthWindow+240)*2)
	for i := mouthWindow; i < mouthWindow+240; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(22000)))
	}
	if v := MouthOpen(pcm); v < 0.5 {
		t.Fatalf("trailing speech should open mouth, got %v", v)
	}
	quiet := make([]byte, (mouthWindow+240)*2)
	for i := 0; i < 240; i++ {
		binary.LittleEndian.PutUint16(quiet[i*2:], uint16(int16(22000)))
	}
	if v := MouthOpen(quiet); v != 0 {
		t.Fatalf("leading speech should be ignored, got %v", v)
	}
}

func TestChunkHasVoiceLeadingSpeech(t *testing.T) {
	pcm := make([]byte, (mouthWindow+240)*2)
	for i := 0; i < 240; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(22000)))
	}
	if !ChunkHasVoice(pcm) {
		t.Fatal("leading speech should count as voice for mic ducking")
	}
	if ChunkHasVoice(make([]byte, mouthWindow*4)) {
		t.Fatal("silence is not voice")
	}
}

func TestDownlinkGateDropsIdleTimelineAndKeepsSpeechContext(t *testing.T) {
	frame := func(sample int16) []byte {
		pcm := make([]byte, mouthFrameBytes)
		for i := 0; i < len(pcm); i += 2 {
			binary.LittleEndian.PutUint16(pcm[i:], uint16(sample))
		}
		return pcm
	}
	quiet := frame(0)
	voice := frame(22000)

	var gate DownlinkGate
	if got := gate.Filter(bytes.Repeat(quiet, 50)); len(got) != 0 {
		t.Fatalf("idle silence leaked into playback: %d bytes", len(got))
	}

	got := gate.Filter(voice)
	wantFrames := downlinkGatePreRollFrames + 1
	if len(got) != wantFrames*mouthFrameBytes {
		t.Fatalf("speech start bytes=%d want=%d", len(got), wantFrames*mouthFrameBytes)
	}
	if !ChunkHasVoice(got[len(got)-mouthFrameBytes:]) {
		t.Fatal("speech frame was not preserved")
	}

	got = gate.Filter(bytes.Repeat(quiet, downlinkGateHangoverFrames+20))
	if len(got) != downlinkGateHangoverFrames*mouthFrameBytes {
		t.Fatalf("hangover bytes=%d want=%d", len(got), downlinkGateHangoverFrames*mouthFrameBytes)
	}

	got = gate.Filter(voice)
	if len(got) != (downlinkGatePreRollFrames+1)*mouthFrameBytes {
		t.Fatalf("resumed speech bytes=%d", len(got))
	}
}

func TestDownlinkGateBuffersPartialFrames(t *testing.T) {
	voice := make([]byte, mouthFrameBytes)
	for i := 0; i < len(voice); i += 2 {
		binary.LittleEndian.PutUint16(voice[i:], uint16(int16(22000)))
	}

	var gate DownlinkGate
	if got := gate.Filter(voice[:100]); len(got) != 0 {
		t.Fatalf("partial frame emitted early: %d", len(got))
	}
	got := gate.Filter(voice[100:])
	if len(got) != mouthFrameBytes {
		t.Fatalf("completed frame bytes=%d want=%d", len(got), mouthFrameBytes)
	}
}

func TestMouthEnvelope(t *testing.T) {
	if MouthEnvelope(1.5) != 0 {
		t.Fatal("expected pause")
	}
	if MouthEnvelope(0.2) <= 0 {
		t.Fatal("expected burst")
	}
}

func TestTonePCM(t *testing.T) {
	pcm := TonePCM(440, 0.01)
	if len(pcm) != DownlinkRate/100*2 {
		t.Fatalf("len %d", len(pcm))
	}
}

func ulawAt(sample int16) []byte {
	pcm := make([]byte, PCMUFrameBytes*2)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], uint16(sample))
	}
	return MuLawEncodeBytes(pcm)
}

func TestNearGateDropsRoomAndKeepsCloseSpeech(t *testing.T) {
	g := &NearGate{floor: nearFloorInit, scale: 1}
	room := ulawAt(700)
	roomRMS := UlawRMS(room)
	if roomRMS < 0.015 || roomRMS > 0.035 {
		t.Fatalf("room rms=%v", roomRMS)
	}
	for i := 0; i < nearFloorWarmup+30; i++ {
		if got := g.Filter(room); len(got) != 0 {
			t.Fatalf("room frame %d leaked (%d frames, rms=%.4f floor=%.4f)", i, len(got), roomRMS, g.floor)
		}
	}
	voice := ulawAt(6000)
	if UlawRMS(voice) < 0.12 {
		t.Fatalf("voice rms=%v", UlawRMS(voice))
	}
	if got := g.Filter(voice); len(got) != 0 {
		t.Fatal("a single loud frame is a click, not a turn")
	}
	got := g.Filter(voice)
	if len(got) < 2 {
		t.Fatalf("close speech should open with preroll, got %d", len(got))
	}
	silence := bytes.Repeat([]byte{SilenceByte}, PCMUFrameBytes)
	passed := 0
	for i := 0; i < nearHangoverFrames+5; i++ {
		if len(g.Filter(silence)) > 0 {
			passed++
		}
	}
	if passed != nearHangoverFrames {
		t.Fatalf("hangover passed %d want %d", passed, nearHangoverFrames)
	}
}

func TestNearGateOpensImmediatelyForCloseVoice(t *testing.T) {
	g := &NearGate{floor: nearFloorInit, scale: 1}
	voice := ulawAt(8000)
	if len(g.Filter(voice)) != 0 {
		t.Fatal("first frame should arm, not open")
	}
	got := g.Filter(voice)
	if len(got) < 2 {
		t.Fatalf("a voice well above the room should open during warmup, got %d", len(got))
	}
}

func TestNearGateHoldsOutAHotRoom(t *testing.T) {
	g := &NearGate{floor: nearFloorInit, scale: 1}
	room := ulawAt(2600)
	roomRMS := UlawRMS(room)
	if roomRMS < 0.06 || roomRMS > 0.12 {
		t.Fatalf("hot room rms=%v", roomRMS)
	}
	for i := 0; i < nearFloorWarmup+40; i++ {
		if got := g.Filter(room); len(got) != 0 {
			t.Fatalf("hot room frame %d leaked (rms=%.4f floor=%.4f)", i, roomRMS, g.floor)
		}
	}
	voice := ulawAt(14000)
	if UlawRMS(voice) < g.threshold() {
		t.Fatalf("voice rms=%v below threshold %v", UlawRMS(voice), g.threshold())
	}
	if len(g.Filter(voice)) != 0 {
		t.Fatal("first close frame should only arm the gate")
	}
	if len(g.Filter(voice)) < 2 {
		t.Fatal("speech louder than the hot room should open")
	}
}

func TestNearGateDisabledPassesRoom(t *testing.T) {
	t.Setenv("TAVERN_MIC_NEAR", "off")
	g := NewNearGate()
	room := ulawAt(700)
	got := g.Filter(room)
	if len(got) != 1 || len(got[0]) != PCMUFrameBytes {
		t.Fatalf("disabled gate dropped audio: %#v", got)
	}
}

func TestNearGateEnv(t *testing.T) {
	cases := []struct {
		env      string
		disabled bool
		scale    float64
	}{
		{"", false, 1},
		{"1", false, 1},
		{"off", true, 1},
		{"0", true, 1},
		{"false", true, 1},
		{"no", true, 1},
		{"0.7", false, 0.7},
		{"1.5", false, 1.5},
		{"nope", false, 1},
		{"9", false, 1},
		{"-1", false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			if tc.env == "" {
				t.Setenv("TAVERN_MIC_NEAR", "")
			} else {
				t.Setenv("TAVERN_MIC_NEAR", tc.env)
			}
			g := NewNearGate()
			if g.disabled != tc.disabled || g.scale != tc.scale {
				t.Fatalf("disabled=%v scale=%v", g.disabled, g.scale)
			}
		})
	}
}

func TestNearGateNilAndEmpty(t *testing.T) {
	var g *NearGate
	room := ulawAt(700)
	got := g.Filter(room)
	if len(got) != 1 || !bytes.Equal(got[0], room) {
		t.Fatal("nil gate should pass the frame")
	}
	g = &NearGate{floor: nearFloorInit, scale: 1}
	if g.Filter(nil) != nil || g.Filter([]byte{}) != nil {
		t.Fatal("empty frame should stay off the uplink")
	}
}

func TestNearGateScaleChangesWhoGetsThrough(t *testing.T) {
	medium := ulawBetween(0.09, 0.11)
	loose := &NearGate{floor: nearFloorInit, scale: 0.7}
	if len(loose.Filter(medium)) != 0 {
		t.Fatal("first frame arms")
	}
	if len(loose.Filter(medium)) < 2 {
		t.Fatal("a looser scale should open for this level during warmup")
	}
	strict := &NearGate{floor: nearFloorInit, scale: 1.5}
	if len(strict.Filter(medium)) != 0 || len(strict.Filter(medium)) != 0 {
		t.Fatal("a stricter scale should hold the same level out")
	}
}

func TestNearGatePrerollClickReopenAndFloor(t *testing.T) {
	silence := bytes.Repeat([]byte{SilenceByte}, PCMUFrameBytes)
	voice := ulawAt(8000)

	preroll := &NearGate{floor: nearFloorInit, scale: 1, seen: nearFloorWarmup + 1}
	for i := 0; i < 10; i++ {
		if len(preroll.Filter(silence)) != 0 {
			t.Fatalf("silence %d leaked", i)
		}
	}
	if len(preroll.Filter(voice)) != 0 {
		t.Fatal("one loud frame should only arm")
	}
	got := preroll.Filter(voice)
	if len(got) != nearPreRollFrames+1 {
		t.Fatalf("preroll %d want %d", len(got), nearPreRollFrames+1)
	}

	click := &NearGate{floor: nearFloorInit, scale: 1, seen: nearFloorWarmup + 1}
	if len(click.Filter(voice)) != 0 || len(click.Filter(silence)) != 0 || len(click.Filter(voice)) != 0 {
		t.Fatal("a click, a gap, and another click should not open")
	}
	if len(click.Filter(voice)) < 2 {
		t.Fatal("two close frames after the gap should open")
	}

	again := &NearGate{floor: nearFloorInit, scale: 1, seen: nearFloorWarmup + 1}
	again.Filter(voice)
	if len(again.Filter(voice)) < 1 {
		t.Fatal("open")
	}
	for i := 0; i < nearHangoverFrames; i++ {
		if len(again.Filter(silence)) != 1 {
			t.Fatalf("hangover %d dropped", i)
		}
	}
	if len(again.Filter(silence)) != 0 || again.open {
		t.Fatal("the gate should close after hangover")
	}
	if len(again.Filter(voice)) != 0 {
		t.Fatal("reopen should arm first")
	}
	if len(again.Filter(voice)) < 2 {
		t.Fatal("close speech after a pause should open again")
	}

	held := &NearGate{floor: nearFloorInit, scale: 1, seen: nearFloorWarmup + 1}
	held.Filter(voice)
	held.Filter(voice)
	floor := held.floor
	for i := 0; i < 20; i++ {
		if len(held.Filter(voice)) != 1 {
			t.Fatalf("open frame %d", i)
		}
	}
	if held.floor != floor {
		t.Fatalf("floor moved from %.4f to %.4f while she was close", floor, held.floor)
	}

	zero := &NearGate{floor: nearFloorInit, scale: 0, seen: nearFloorWarmup + 1}
	zero.Filter(voice)
	if len(zero.Filter(voice)) < 2 {
		t.Fatal("scale 0 should behave as 1")
	}
}

func ulawBetween(lo, hi float64) []byte {
	for s := int16(200); s < 16000; s += 25 {
		frame := ulawAt(s)
		if r := UlawRMS(frame); r >= lo && r < hi {
			return frame
		}
	}
	panic("no ulaw frame in range")
}

func TestUlawRMSSilence(t *testing.T) {
	ulaw := bytes.Repeat([]byte{SilenceByte}, PCMUFrameBytes)
	if r := UlawRMS(ulaw); r != 0 {
		t.Fatalf("silence rms %v", r)
	}
}

func TestDecimateS16LE48kTo8k(t *testing.T) {
	n := 48000 * 20 / 1000
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		s := int16(math.Sin(2*math.Pi*440*float64(i)/48000) * 12000)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(s))
	}
	out := DecimateS16LE(pcm, 6)
	if len(out) != PCMUFrameBytes*2 {
		t.Fatalf("len %d", len(out))
	}
}

func TestPCMToUplinkUlaw16k(t *testing.T) {
	n := 16000 * 20 / 1000
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		s := int16(math.Sin(2*math.Pi*440*float64(i)/16000) * 12000)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(s))
	}
	ulaw := PCMToUplinkUlaw(pcm, 16000, 1)
	if len(ulaw) != PCMUFrameBytes {
		t.Fatalf("len %d", len(ulaw))
	}
	if UlawRMS(ulaw) < 0.1 {
		t.Fatalf("expected energy, rms=%v", UlawRMS(ulaw))
	}
}

func TestMixStereoS16LE(t *testing.T) {
	pcm := make([]byte, 8)
	l0, r0 := int16(1000), int16(3000)
	l1, r1 := int16(-1000), int16(-3000)
	binary.LittleEndian.PutUint16(pcm[0:], uint16(l0))
	binary.LittleEndian.PutUint16(pcm[2:], uint16(r0))
	binary.LittleEndian.PutUint16(pcm[4:], uint16(l1))
	binary.LittleEndian.PutUint16(pcm[6:], uint16(r1))
	out := MixStereoS16LE(pcm)
	if len(out) != 4 {
		t.Fatalf("len %d", len(out))
	}
	a := int16(binary.LittleEndian.Uint16(out[0:]))
	b := int16(binary.LittleEndian.Uint16(out[2:]))
	if a != 2000 || b != -2000 {
		t.Fatalf("got %d %d", a, b)
	}
}

func TestPCMToUplinkUlawStereoMatchesMono(t *testing.T) {
	n := 48000 * 20 / 1000
	mono := make([]byte, n*2)
	stereo := make([]byte, n*4)
	for i := 0; i < n; i++ {
		s := int16(math.Sin(2*math.Pi*440*float64(i)/48000) * 8000)
		binary.LittleEndian.PutUint16(mono[i*2:], uint16(s))
		binary.LittleEndian.PutUint16(stereo[i*4:], uint16(s))
		binary.LittleEndian.PutUint16(stereo[i*4+2:], uint16(s))
	}
	a := PCMToUplinkUlaw(mono, 48000, 1)
	b := PCMToUplinkUlaw(stereo, 48000, 2)
	if len(a) != len(b) || len(a) != PCMUFrameBytes {
		t.Fatalf("len %d %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("byte %d %d vs %d", i, a[i], b[i])
		}
	}
}

func TestMicPickScorePrefersArray(t *testing.T) {
	ext := micPickScore("外部麦克风 (Realtek(R) Audio)", 0.014)
	arr := micPickScore("麦克风阵列 (适用于数字麦克风的英特尔® 智音技术)", 0.0005)
	virt := micPickScore("麦克风阵列 (网易虚拟音频设备)", 0.02)
	if arr <= ext {
		t.Fatalf("array %v should beat jack hiss %v", arr, ext)
	}
	if arr <= virt {
		t.Fatalf("intel array %v should beat virtual %v", arr, virt)
	}
}

func TestOfferFrameDropsOldestWhenFull(t *testing.T) {
	out := make(chan []byte, 2)
	stop := make(chan struct{})
	if !OfferFrame(out, stop, []byte{1}) || !OfferFrame(out, stop, []byte{2}) {
		t.Fatal("queue should accept the first frames")
	}
	if !OfferFrame(out, stop, []byte{3}) {
		t.Fatal("a full queue should keep the newest frame")
	}
	a := <-out
	b := <-out
	if a[0] != 2 || b[0] != 3 {
		t.Fatalf("got %v %v, want the live tail 2 then 3", a, b)
	}
	close(stop)
	if OfferFrame(out, stop, []byte{4}) {
		t.Fatal("closed capture should not enqueue")
	}
}

func TestOpenMicYieldsFrames(t *testing.T) {
	ch, stop, err := OpenMic(PCMUUplinkRate)
	if err != nil {
		t.Skip(err)
	}
	defer stop()
	select {
	case f, ok := <-ch:
		if !ok || len(f) != PCMUFrameBytes {
			t.Fatalf("frame ok=%v len=%d", ok, len(f))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no mic frames")
	}
}
