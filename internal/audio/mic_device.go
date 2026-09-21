//go:build tavern_mic

// Real microphone capture via miniaudio (malgo): 16kHz s16 mono,
// resampled to 8kHz and mu-law encoded. Build with -tags tavern_mic.
package audio

import (
	"fmt"
	"sync"

	"github.com/gen2brain/malgo"
)

const micCaptureRate = 16000

// OpenMic opens the default input device and yields 160-byte mu-law frames.
func OpenMic(_ int) (<-chan []byte, func(), error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, func() {}, fmt.Errorf("malgo init: %w", err)
	}
	out := make(chan []byte, 64)
	var once sync.Once
	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = 1
	cfg.SampleRate = micCaptureRate
	cfg.PeriodSizeInMilliseconds = 20
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
		ctx.Uninit()
		ctx.Free()
		return nil, func() {}, fmt.Errorf("malgo device: %w", err)
	}
	if err := device.Start(); err != nil {
		device.Uninit()
		ctx.Uninit()
		ctx.Free()
		return nil, func() {}, fmt.Errorf("malgo start: %w", err)
	}
	setMicFormat("malgo 16kHz mono")
	stop := func() {
		once.Do(func() {
			device.Stop()
			device.Uninit()
			ctx.Uninit()
			ctx.Free()
			close(out)
		})
	}
	return out, stop, nil
}
