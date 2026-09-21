//go:build tavern_mic && !windows

// Real microphone capture via miniaudio (malgo): 16kHz s16 mono,
// resampled to 8kHz and mu-law encoded. Build with -tags tavern_mic.
//
// Device choice: TAVERN_MIC (case-insensitive name substring) wins;
// otherwise every capture device is probed briefly and the highest
// micPickScore wins. The system "default input" can be a virtual
// driver that only emits digital silence (remote-desktop audio pipes,
// Macs with no built-in mic), which strands the uplink on silence.
package audio

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/gen2brain/malgo"
)

const micCaptureRate = 16000

// micProbeWindow is how long each capture device is sampled for RMS
// before picking. Short enough that OpenMic stays near-instant.
const micProbeWindow = 250 * time.Millisecond

// OpenMic picks a capture device (TAVERN_MIC override or loudest probe)
// and yields 160-byte mu-law frames.
func OpenMic(_ int) (<-chan []byte, func(), error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, func() {}, fmt.Errorf("malgo init: %w", err)
	}
	devID, name, err := pickCaptureDevice(ctx)
	if err != nil {
		ctx.Uninit()
		ctx.Free()
		return nil, func() {}, err
	}
	out, stopDev, err := openCapture(ctx, devID)
	if err != nil {
		ctx.Uninit()
		ctx.Free()
		return nil, func() {}, err
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			stopDev()
			ctx.Uninit()
			ctx.Free()
		})
	}
	setMicFormat("malgo 16kHz mono [" + name + "]")
	return out, stop, nil
}

// pickCaptureDevice returns the DeviceID to open. An explicit
// TAVERN_MIC substring wins; with multiple devices and no override,
// each is probed for input RMS and micPickScore picks the best.
func pickCaptureDevice(ctx *malgo.AllocatedContext) (*malgo.DeviceID, string, error) {
	devs, err := ctx.Context.Devices(malgo.Capture)
	if err != nil {
		return nil, "", fmt.Errorf("malgo devices: %w", err)
	}
	if len(devs) == 0 {
		return nil, "", fmt.Errorf("no capture devices")
	}
	if want := strings.ToLower(strings.TrimSpace(os.Getenv("TAVERN_MIC"))); want != "" {
		for i := range devs {
			if strings.Contains(strings.ToLower(devs[i].Name()), want) {
				id := devs[i].ID
				return &id, devs[i].Name(), nil
			}
		}
		// Unknown name: fall through to probing rather than fail.
	}
	if len(devs) == 1 {
		id := devs[0].ID
		return &id, devs[0].Name(), nil
	}
	best := -1
	var bestScore float64
	var lastErr error
	for i := range devs {
		id := devs[i].ID
		rms, err := probeCaptureRMS(ctx, &id)
		if err != nil {
			lastErr = err
			continue
		}
		if score := micPickScore(devs[i].Name(), rms); best < 0 || score > bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("no usable capture device")
		}
		return nil, "", lastErr
	}
	id := devs[best].ID
	return &id, devs[best].Name(), nil
}

// probeCaptureRMS opens a device for micProbeWindow and returns input
// RMS. A virtual or muted source reports ~0; real mics show a noise
// floor even in a quiet room.
func probeCaptureRMS(ctx *malgo.AllocatedContext, devID *malgo.DeviceID) (float64, error) {
	var sumSq, count atomic.Uint64
	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = 1
	cfg.SampleRate = micCaptureRate
	cfg.PeriodSizeInMilliseconds = 20
	// ma_device_init keeps pDeviceID until the config is consumed; a
	// Go-allocated ma_device_id must be pinned or cgo panics.
	var pinner runtime.Pinner
	pinner.Pin(devID)
	cfg.Capture.DeviceID = unsafe.Pointer(devID)
	dev, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{
		Data: func(_, input []byte, _ uint32) {
			var s uint64
			for i := 0; i+1 < len(input); i += 2 {
				v := int64(int16(binary.LittleEndian.Uint16(input[i:])))
				s += uint64(v * v)
			}
			sumSq.Add(s)
			count.Add(uint64(len(input) / 2))
		},
	})
	if err != nil {
		pinner.Unpin()
		return 0, err
	}
	defer func() { dev.Uninit(); pinner.Unpin() }()
	if err := dev.Start(); err != nil {
		return 0, err
	}
	time.Sleep(micProbeWindow)
	_ = dev.Stop()
	if n := count.Load(); n > 0 {
		return math.Sqrt(float64(sumSq.Load())/float64(n)) / 32768.0, nil
	}
	return 0, nil
}

// openCapture starts the device and converts 16kHz s16 mono into
// 8kHz mu-law frames. Caller owns the malgo context lifetime.
func openCapture(ctx *malgo.AllocatedContext, devID *malgo.DeviceID) (<-chan []byte, func(), error) {
	out := make(chan []byte, 64)
	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = 1
	cfg.SampleRate = micCaptureRate
	cfg.PeriodSizeInMilliseconds = 20
	var pinner runtime.Pinner
	if devID != nil {
		pinner.Pin(devID)
		cfg.Capture.DeviceID = unsafe.Pointer(devID)
	}
	device, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{
		Data: func(_, inputSamples []byte, _ uint32) {
			pcm8 := ResamplePCM(inputSamples, micCaptureRate, PCMUUplinkRate)
			ulaw := MuLawEncodeBytes(pcm8)
			for len(ulaw) >= PCMUFrameBytes {
				frame := make([]byte, PCMUFrameBytes)
				copy(frame, ulaw[:PCMUFrameBytes])
				ulaw = ulaw[PCMUFrameBytes:]
				select {
				case out <- frame:
				default: // drop frames if the consumer stalls
				}
			}
		},
	})
	if err != nil {
		pinner.Unpin()
		return nil, func() {}, fmt.Errorf("malgo device: %w", err)
	}
	if err := device.Start(); err != nil {
		device.Uninit()
		pinner.Unpin()
		return nil, func() {}, fmt.Errorf("malgo start: %w", err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			device.Stop()
			device.Uninit()
			pinner.Unpin()
			close(out)
		})
	}
	return out, stop, nil
}
