package audio

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
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
}
