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
