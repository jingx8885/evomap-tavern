//go:build windows

package audio

import (
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	winmmMicBufs    = 16
	callbackEvent   = 0x00050000
	whdrPrepared    = 0x00000002
	winmmMicTimeout = 20
)

type winmmCapBuf struct {
	pcm []byte
	hdr waveHdr
}

var procCreateEventW = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateEventW")

func waveInDeviceName(id uint32) string {
	var caps waveInCapsW
	r, _, _ := procWaveInGetCaps.Call(uintptr(id), uintptr(unsafe.Pointer(&caps)), unsafe.Sizeof(caps))
	if r != mmsyserrNoErr {
		return fmt.Sprintf("device-%d", id)
	}
	name := windows.UTF16ToString(caps.Name[:])
	if name == "" {
		return fmt.Sprintf("device-%d", id)
	}
	return name
}

func probeUlaw(ch <-chan []byte, d time.Duration) (max float64, n int) {
	deadline := time.After(d)
	for {
		select {
		case <-deadline:
			return
		case f, ok := <-ch:
			if !ok {
				return
			}
			n++
			if r := UlawRMS(f); r > max {
				max = r
			}
		}
	}
}

// OpenMic enumerates winmm capture devices, probes RMS, and uses the
// loudest one. WAVE_MAPPER often picks an empty 3.5mm jack as "default"
// and then the uplink is digital silence.
func OpenMic(_ int) (<-chan []byte, func(), error) {
	n, _, _ := procWaveInGetNum.Call()
	if n == 0 {
		return nil, func() {}, fmt.Errorf("no recording device")
	}
	formats := []struct {
		rate     uint32
		channels uint16
	}{
		{8000, 1}, {16000, 1}, {8000, 2}, {16000, 2}, {48000, 1}, {48000, 2},
	}
	type hit struct {
		id       uint32
		name     string
		rate     uint32
		channels uint16
		rms      float64
		score    float64
	}
	var best hit
	var last error
	for i := uint32(0); i < uint32(n); i++ {
		name := waveInDeviceName(i)
		for _, f := range formats {
			ch, stop, err := openWinmmMic(i, f.rate, f.channels)
			if err != nil {
				last = err
				continue
			}
			rms, frames := probeUlaw(ch, 180*time.Millisecond)
			stop()
			if frames == 0 {
				continue
			}
			score := micPickScore(name, rms)
			if best.rate == 0 || score > best.score {
				best = hit{i, name, f.rate, f.channels, rms, score}
			}
			break
		}
	}
	if best.rate == 0 {
		if last == nil {
			last = fmt.Errorf("waveIn produced no frames")
		}
		return nil, func() {}, last
	}
	ch, stop, err := openWinmmMic(best.id, best.rate, best.channels)
	if err != nil {
		return nil, func() {}, err
	}
	chName := "mono"
	if best.channels == 2 {
		chName = "stereo"
	}
	setMicFormat(fmt.Sprintf("winmm %dHz %s #%d %s rms=%.4f",
		best.rate, chName, best.id, best.name, best.rms))
	return ch, stop, nil
}

func openWinmmMic(devID uint32, rate uint32, channels uint16) (<-chan []byte, func(), error) {
	event, err := createAutoEvent()
	if err != nil {
		return nil, func() {}, err
	}
	block := uint16(channels * 2)
	wfx := waveFormatEx{
		FormatTag:      waveFormatPCM,
		Channels:       channels,
		SamplesPerSec:  rate,
		AvgBytesPerSec: rate * uint32(block),
		BlockAlign:     block,
		BitsPerSample:  16,
	}
	var hwi uintptr
	r, _, callErr := procWaveInOpen.Call(
		uintptr(unsafe.Pointer(&hwi)),
		uintptr(devID),
		uintptr(unsafe.Pointer(&wfx)),
		uintptr(event),
		0,
		callbackEvent,
	)
	if r != mmsyserrNoErr || hwi == 0 {
		_ = windows.CloseHandle(event)
		return nil, func() {}, fmt.Errorf("waveInOpen %s %dHz ch=%d: %v", waveInDeviceName(devID), rate, channels, callErr)
	}

	frameBytes := int(rate) * int(channels) * 2 * int(PCMUFrameDur/time.Millisecond) / 1000
	if frameBytes < 4 {
		_, _, _ = procWaveInClose.Call(hwi)
		_ = windows.CloseHandle(event)
		return nil, func() {}, fmt.Errorf("bad capture frame size")
	}
	bufs := make([]winmmCapBuf, winmmMicBufs)
	var pin runtime.Pinner
	for i := range bufs {
		bufs[i].pcm = make([]byte, frameBytes)
		bufs[i].hdr = waveHdr{
			Data:         &bufs[i].pcm[0],
			BufferLength: uint32(frameBytes),
		}
		pin.Pin(&bufs[i].pcm[0])
		pin.Pin(&bufs[i].hdr)
		if err := waveInPrepAdd(hwi, &bufs[i].hdr); err != nil {
			pin.Unpin()
			_, _, _ = procWaveInReset.Call(hwi)
			_, _, _ = procWaveInClose.Call(hwi)
			_ = windows.CloseHandle(event)
			return nil, func() {}, err
		}
	}
	if r, _, callErr = procWaveInStart.Call(hwi); r != mmsyserrNoErr {
		pin.Unpin()
		_, _, _ = procWaveInReset.Call(hwi)
		_, _, _ = procWaveInClose.Call(hwi)
		_ = windows.CloseHandle(event)
		return nil, func() {}, fmt.Errorf("waveInStart: %v", callErr)
	}

	out := make(chan []byte, 16)
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(out)
		defer pin.Unpin()
		defer windows.CloseHandle(event)
		defer func() {
			_, _, _ = procWaveInReset.Call(hwi)
			size := uintptr(unsafe.Sizeof(waveHdr{}))
			for i := range bufs {
				_, _, _ = procWaveInUnprep.Call(hwi, uintptr(unsafe.Pointer(&bufs[i].hdr)), size)
			}
			_, _, _ = procWaveInClose.Call(hwi)
		}()
		winmmCaptureLoop(hwi, bufs, out, stopCh, event, int(rate), int(channels), frameBytes)
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(stopCh)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		})
	}
	return out, stop, nil
}

func createAutoEvent() (windows.Handle, error) {
	r, _, err := procCreateEventW.Call(0, 0, 0, 0)
	if r == 0 {
		return 0, fmt.Errorf("CreateEventW: %v", err)
	}
	return windows.Handle(r), nil
}

func waveInPrepAdd(hwi uintptr, hdr *waveHdr) error {
	hdr.Flags = 0
	hdr.BytesRecorded = 0
	size := uintptr(unsafe.Sizeof(*hdr))
	r, _, err := procWaveInPrepare.Call(hwi, uintptr(unsafe.Pointer(hdr)), size)
	if r != mmsyserrNoErr {
		return fmt.Errorf("waveInPrepareHeader: %v", err)
	}
	r, _, err = procWaveInAddBuf.Call(hwi, uintptr(unsafe.Pointer(hdr)), size)
	if r != mmsyserrNoErr {
		_, _, _ = procWaveInUnprep.Call(hwi, uintptr(unsafe.Pointer(hdr)), size)
		return fmt.Errorf("waveInAddBuffer: %v", err)
	}
	return nil
}

func waveInRequeue(hwi uintptr, hdr *waveHdr) error {
	hdr.BytesRecorded = 0
	hdr.Flags = whdrPrepared
	size := uintptr(unsafe.Sizeof(*hdr))
	r, _, err := procWaveInAddBuf.Call(hwi, uintptr(unsafe.Pointer(hdr)), size)
	if r != mmsyserrNoErr {
		return fmt.Errorf("waveInAddBuffer: %v", err)
	}
	return nil
}

func winmmCaptureLoop(hwi uintptr, bufs []winmmCapBuf, out chan []byte, stop <-chan struct{}, event windows.Handle, rate, channels, frameBytes int) {
	acc := make([]byte, 0, frameBytes*2)
	ulawAcc := make([]byte, 0, PCMUFrameBytes*2)
	align := channels * 2
	if align < 2 {
		align = 2
	}
	for {
		select {
		case <-stop:
			return
		default:
		}
		_, _ = windows.WaitForSingleObject(event, winmmMicTimeout)
		progress := false
		for i := range bufs {
			hdr := &bufs[i].hdr
			if hdr.Flags&whdrDone == 0 {
				continue
			}
			progress = true
			n := int(hdr.BytesRecorded)
			if n > len(bufs[i].pcm) {
				n = len(bufs[i].pcm)
			}
			n = n / align * align
			if n > 0 {
				acc = append(acc, bufs[i].pcm[:n]...)
			}
			if err := waveInRequeue(hwi, hdr); err != nil {
				return
			}
			for len(acc) >= frameBytes {
				ulawAcc = append(ulawAcc, PCMToUplinkUlaw(acc[:frameBytes], rate, channels)...)
				acc = acc[frameBytes:]
				for len(ulawAcc) >= PCMUFrameBytes {
					frame := make([]byte, PCMUFrameBytes)
					copy(frame, ulawAcc[:PCMUFrameBytes])
					ulawAcc = ulawAcc[PCMUFrameBytes:]
					if !OfferFrame(out, stop, frame) {
						return
					}
				}
			}
		}
		if !progress {
			select {
			case <-stop:
				return
			default:
			}
		}
	}
}
