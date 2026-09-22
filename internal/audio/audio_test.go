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
