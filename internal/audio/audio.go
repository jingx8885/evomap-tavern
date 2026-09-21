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
	"strings"
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

// ReadWAV reads a PCM s16le WAVE file.
func ReadWAV(path string) (pcm []byte, sampleRate, channels int, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, 0, err
	}
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, 0, 0, fmt.Errorf("not a WAVE file")
	}
	off := 12
	var bits int
	for off+8 <= len(raw) {
		id := string(raw[off : off+4])
		n := int(binary.LittleEndian.Uint32(raw[off+4:]))
		off += 8
		if off+n > len(raw) {
			return nil, 0, 0, fmt.Errorf("truncated %s chunk", id)
		}
		switch id {
		case "fmt ":
			if n < 16 {
				return nil, 0, 0, fmt.Errorf("bad fmt chunk")
			}
			format := binary.LittleEndian.Uint16(raw[off:])
			if format != 1 {
				return nil, 0, 0, fmt.Errorf("need PCM wav, format=%d", format)
			}
			channels = int(binary.LittleEndian.Uint16(raw[off+2:]))
			sampleRate = int(binary.LittleEndian.Uint32(raw[off+4:]))
			bits = int(binary.LittleEndian.Uint16(raw[off+14:]))
		case "data":
			if bits != 16 {
				return nil, 0, 0, fmt.Errorf("need 16-bit pcm, got %d", bits)
			}
			pcm = append([]byte(nil), raw[off:off+n]...)
			return pcm, sampleRate, channels, nil
		}
		off += n
		if n%2 == 1 {
			off++
		}
	}
	return nil, 0, 0, fmt.Errorf("no data chunk")
}

// UlawFrames splits mu-law bytes into 20ms uplink frames.
func UlawFrames(ulaw []byte) [][]byte {
	var out [][]byte
	for len(ulaw) >= PCMUFrameBytes {
		f := make([]byte, PCMUFrameBytes)
		copy(f, ulaw[:PCMUFrameBytes])
		ulaw = ulaw[PCMUFrameBytes:]
		out = append(out, f)
	}
	if len(ulaw) > 0 {
		f := bytes.Repeat([]byte{SilenceByte}, PCMUFrameBytes)
		copy(f, ulaw)
		out = append(out, f)
	}
	return out
}

// Player streams downlink PCM to speakers.
// Backends: winmm (Windows), afplay (macOS), aplay/ffplay (Linux).
// Chunked file playback is a fallback; winmm streams s16le 24kHz directly.
type Player struct {
	kind       string
	playerPath string
	tmpDir     string
	chunk      bytes.Buffer
	chunkBytes int
	counter    atomic.Int64
	procs      chan *exec.Cmd
	closed     atomic.Bool
	stream     func([]byte)
	closeFn    func()
}

// Kind is the playback backend name, or "none".
func (p *Player) Kind() string {
	if p == nil {
		return "none"
	}
	return p.kind
}

// NewPlayer probes for a usable player; nil means none found.
func NewPlayer() *Player {
	if p := openWinmmPlayer(); p != nil {
		return p
	}
	type cand struct{ name, kind string }
	var names []cand
	switch runtime.GOOS {
	case "darwin":
		names = []cand{{"afplay", "afplay"}, {"ffplay", "ffplay"}}
	case "windows":
		names = []cand{{"ffplay", "ffplay"}}
	default:
		names = []cand{{"aplay", "aplay"}, {"ffplay", "ffplay"}}
	}
	for _, n := range names {
		path, err := exec.LookPath(n.name)
		if err != nil {
			continue
		}
		return &Player{
			kind:       n.kind,
			playerPath: path,
			tmpDir:     filepath.Join(os.TempDir(), "tavernbot-audio"),
			chunkBytes: DownlinkRate * 2 / 5, // ~200ms
			procs:      make(chan *exec.Cmd, 64),
		}
	}
	return nil
}

// WritePCM accumulates downlink PCM; full chunks play asynchronously.
func (p *Player) WritePCM(pcm []byte) {
	if p == nil || len(pcm) == 0 || p.closed.Load() {
		return
	}
	if p.stream != nil {
		p.stream(pcm)
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
	if p == nil || p.stream != nil {
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
		switch p.kind {
		case "afplay":
			cmd = exec.Command(p.playerPath, name)
		case "ffplay":
			cmd = exec.Command(p.playerPath, "-nodisp", "-autoexit", "-loglevel", "quiet", name)
		default:
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
	if p.closeFn != nil {
		p.closeFn()
		return
	}
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

var micFormat atomic.Value

func setMicFormat(s string) { micFormat.Store(s) }

// MicFormat is the last successful OpenMic backend description.
func MicFormat() string {
	s, _ := micFormat.Load().(string)
	if s == "" {
		return "none"
	}
	return s
}

// micPickScore prefers built-in mic arrays over empty analog jacks
// that hiss, and over virtual/loopback devices.
func micPickScore(name string, rms float64) float64 {
	s := rms
	nl := strings.ToLower(name)
	if strings.Contains(name, "阵列") || strings.Contains(nl, "array") {
		s += 1
	}
	if strings.Contains(name, "智音") || strings.Contains(nl, "intel") {
		s += 0.3
	}
	if strings.Contains(name, "虚拟") || strings.Contains(nl, "virtual") {
		s -= 1
	}
	if strings.Contains(name, "外部") || strings.Contains(nl, "external") || strings.Contains(nl, "line in") {
		s -= 0.5
	}
	if strings.Contains(name, "立体声混音") || strings.Contains(nl, "stereo mix") {
		s -= 1
	}
	return s
}

// MixStereoS16LE averages interleaved s16le stereo into mono.
func MixStereoS16LE(pcm []byte) []byte {
	n := len(pcm) / 4
	if n == 0 {
		return nil
	}
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		l := int32(int16(binary.LittleEndian.Uint16(pcm[i*4:])))
		r := int32(int16(binary.LittleEndian.Uint16(pcm[i*4+2:])))
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16((l+r)/2)))
	}
	return out
}

// PCMToUplinkUlaw converts s16le PCM at sampleRate/channels into 8kHz mu-law.
func PCMToUplinkUlaw(pcm []byte, sampleRate, channels int) []byte {
	if channels == 2 {
		pcm = MixStereoS16LE(pcm)
	}
	if sampleRate != PCMUUplinkRate {
		if sampleRate%PCMUUplinkRate == 0 {
			pcm = DecimateS16LE(pcm, sampleRate/PCMUUplinkRate)
		} else {
			pcm = ResamplePCM(pcm, sampleRate, PCMUUplinkRate)
		}
	}
	return MuLawEncodeBytes(pcm)
}

// DecimateS16LE downsamples by an integer factor with a boxcar average
// so 16/48kHz capture does not alias into the 8kHz speech band.
func DecimateS16LE(pcm []byte, factor int) []byte {
	if factor <= 1 || len(pcm) < 2 {
		return pcm
	}
	n := len(pcm) / 2
	outN := n / factor
	if outN < 1 {
		return pcm
	}
	out := make([]byte, outN*2)
	for i := 0; i < outN; i++ {
		var sum int32
		base := i * factor
		for j := 0; j < factor; j++ {
			sum += int32(int16(binary.LittleEndian.Uint16(pcm[(base+j)*2:])))
		}
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(sum/int32(factor))))
	}
	return out
}

// UlawRMS is the 0..1 RMS of a mu-law frame (0xFF silence decodes to 0).
func UlawRMS(ulaw []byte) float64 {
	n := len(ulaw)
	if n == 0 {
		return 0
	}
	var sum float64
	for _, b := range ulaw {
		s := float64(MulawDecodeTable[b])
		sum += s * s
	}
	return math.Sqrt(sum/float64(n)) / 32768.0
}

const mouthWindow = 480 // 20ms at 24kHz; long deltas otherwise smear syllables.

// MouthOpen maps s16le PCM to 0..1 mouth openness from the latest 20ms.
// Downlink silence (near-zero energy) returns 0 so the avatar closes its mouth.
func MouthOpen(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	if n > mouthWindow {
		pcm = pcm[(n-mouthWindow)*2:]
		n = mouthWindow
	}
	var sum, peak float64
	for i := 0; i < n; i++ {
		s := math.Abs(float64(int16(binary.LittleEndian.Uint16(pcm[i*2:]))))
		sum += s * s
		if s > peak {
			peak = s
		}
	}
	rms := math.Sqrt(sum/float64(n)) / 32768.0
	level := rms*0.65 + (peak/32768.0)*0.35
	const noise = 0.01
	if level < noise {
		return 0
	}
	v := (level - noise) / 0.14
	if v > 1 {
		v = 1
	}
	return math.Sqrt(v)
}

// MouthEnvelope is a synthetic speak/pause curve for --lipsync demos.
// Period is 2.4s: ~0.9s of syllables then rest.
func MouthEnvelope(t float64) float64 {
	if t < 0 {
		return 0
	}
	cycle := math.Mod(t, 2.4)
	if cycle >= 0.9 {
		return 0
	}
	return math.Abs(math.Sin(cycle*18)) * (0.4 + 0.6*math.Abs(math.Sin(cycle*7)))
}

// TonePCM renders a sine wave as s16le 24kHz mono.
func TonePCM(freq, seconds float64) []byte {
	if seconds <= 0 {
		return nil
	}
	n := int(float64(DownlinkRate) * seconds)
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		s := int16(math.Sin(2*math.Pi*freq*float64(i)/float64(DownlinkRate)) * 9000)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(s))
	}
	return pcm
}
