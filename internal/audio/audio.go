// Package audio provides PCMU (G.711 mu-law) codec, resampling, WAV
// writing, and downlink playback. No cgo required.
package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"
)

// Protocol constants: uplink PCMU 8000Hz / 20ms frames;
// downlink PCM s16le 24kHz mono.
const (
	PCMUFrameBytes = 160
	PCMUFrameDur   = 20 * time.Millisecond
	DownlinkRate   = 24000
	PCMUUplinkRate = 8000
	SilenceByte    = 0xFF
)

const (
	mulawBias = 0x84
	mulawClip = 32635
)

// MulawDecodeTable expands a mu-law byte to linear PCM.
var MulawDecodeTable = func() [256]int16 {
	var t [256]int16
	for i := 0; i < 256; i++ {
		u := byte(i)
		u = ^u
		sign := u & 0x80
		exponent := (u >> 4) & 0x07
		mantissa := u & 0x0F
		sample := int32(mantissa)<<3 + mulawBias
		sample <<= exponent
		sample -= mulawBias
		if sign != 0 {
			t[i] = int16(-sample)
		} else {
			t[i] = int16(sample)
		}
	}
	return t
}()

// MuLawEncode encodes one s16 sample to a mu-law byte.
func MuLawEncode(s int16) byte {
	sample := int32(s)
	var sign byte
	if sample < 0 {
		sample = -sample
		sign = 0x80
	}
	if sample > mulawClip {
		sample = mulawClip
	}
	sample += mulawBias
	var exponent byte = 7
	for mask := int32(0x4000); exponent > 0; exponent-- {
		if sample&mask != 0 {
			break
		}
		mask >>= 1
	}
	mantissa := byte((sample >> (exponent + 3)) & 0x0F)
	return ^(sign | (exponent << 4) | mantissa)
}

// MuLawEncodeBytes converts an s16le byte stream to mu-law bytes.
func MuLawEncodeBytes(pcm []byte) []byte {
	out := make([]byte, len(pcm)/2)
	for i := range out {
		s := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		out[i] = MuLawEncode(s)
	}
	return out
}

// ResamplePCM resamples an s16le byte stream with linear interpolation.
func ResamplePCM(pcm []byte, fromRate, toRate int) []byte {
	if fromRate == toRate || len(pcm) < 2 {
		return pcm
	}
	n := len(pcm) / 2
	outN := int(math.Round(float64(n) * float64(toRate) / float64(fromRate)))
	if outN < 1 {
		outN = 1
	}
	out := make([]byte, outN*2)
	for i := 0; i < outN; i++ {
		src := float64(i) * float64(fromRate) / float64(toRate)
		i0 := int(src)
		i1 := i0 + 1
		if i1 >= n {
			i1 = n - 1
		}
		frac := src - float64(i0)
		a := int16(binary.LittleEndian.Uint16(pcm[i0*2:]))
		b := int16(binary.LittleEndian.Uint16(pcm[i1*2:]))
		v := float64(a) + frac*(float64(b)-float64(a))
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(math.Round(v))))
	}
	return out
}

// WriteWAV writes PCM s16le mono to a WAV file.
func WriteWAV(path string, pcm []byte, sampleRate int) error {
	var buf bytes.Buffer
	dataLen := uint32(len(pcm))
	byteRate := uint32(sampleRate * 2)
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36)+dataLen)
	buf.WriteString("WAVEfmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // mono
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&buf, binary.LittleEndian, byteRate)
	binary.Write(&buf, binary.LittleEndian, uint16(2))
	binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, dataLen)
	buf.Write(pcm)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// Player streams downlink PCM chunks to a system player
// (macOS afplay / Linux aplay). Chunked playback avoids buffering the
// whole session. With no player available it returns nil and callers
// should fall back to writing a WAV.
type Player struct {
	playerPath string
	tmpDir     string
	chunk      bytes.Buffer
	chunkBytes int
	counter    atomic.Int64
	procs      chan *exec.Cmd
	closed     atomic.Bool
}

// NewPlayer probes for a usable player; nil means none found.
func NewPlayer() *Player {
	name := "aplay"
	if runtime.GOOS == "darwin" {
		name = "afplay"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return nil
	}
	return &Player{
		playerPath: path,
		tmpDir:     filepath.Join(os.TempDir(), "tavernbot-audio"),
		chunkBytes: DownlinkRate * 2, // ~1s PCM
		procs:      make(chan *exec.Cmd, 64),
	}
}

// WritePCM accumulates downlink PCM; full chunks play asynchronously.
func (p *Player) WritePCM(pcm []byte) {
	if p == nil || len(pcm) == 0 {
		return
	}
	p.chunk.Write(pcm)
	for p.chunk.Len() >= p.chunkBytes {
		block := make([]byte, p.chunkBytes)
		copy(block, p.chunk.Next(p.chunkBytes))
		p.playBlock(block)
	}
}

// Flush plays the remaining partial chunk.
func (p *Player) Flush() {
	if p == nil {
		return
	}
	if p.chunk.Len() > 0 {
		block := make([]byte, p.chunk.Len())
		copy(block, p.chunk.Next(p.chunk.Len()))
		p.playBlock(block)
	}
}

func (p *Player) playBlock(pcm []byte) {
	if p.closed.Load() {
		return
	}
	if err := os.MkdirAll(p.tmpDir, 0o755); err != nil {
		return
	}
	name := filepath.Join(p.tmpDir, fmt.Sprintf("chunk-%d.wav", p.counter.Add(1)))
	if err := WriteWAV(name, pcm, DownlinkRate); err != nil {
		return
	}
	go func() {
		var cmd *exec.Cmd
		if filepath.Base(p.playerPath) == "afplay" {
			cmd = exec.Command(p.playerPath, name)
		} else {
			cmd = exec.Command(p.playerPath, "-q", name)
		}
		select {
		case p.procs <- cmd:
		default:
		}
		_ = cmd.Run()
		_ = os.Remove(name)
	}()
}

// Close stops future playback and briefly waits for in-flight chunks.
func (p *Player) Close() {
	if p == nil {
		return
	}
	p.closed.Store(true)
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case cmd := <-p.procs:
			if cmd.ProcessState == nil {
				if time.Now().After(deadline) {
					_ = cmd.Process.Kill()
					continue
				}
				time.Sleep(100 * time.Millisecond)
				select {
				case p.procs <- cmd:
				default:
				}
			}
		default:
			return
		}
	}
}

// SilenceFrames emits silent PCMU frames at a 20ms cadence.
func SilenceFrames(stop <-chan struct{}) <-chan []byte {
	out := make(chan []byte, 4)
	go func() {
		defer close(out)
		ticker := time.NewTicker(PCMUFrameDur)
		defer ticker.Stop()
		frame := bytes.Repeat([]byte{SilenceByte}, PCMUFrameBytes)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				select {
				case out <- frame:
				case <-stop:
					return
				}
			}
		}
	}()
	return out
}

// ErrNoMicDevice means no capture device is available in this build.
var ErrNoMicDevice = errors.New("microphone capture requires build tag tavern_mic; using silence uplink")
