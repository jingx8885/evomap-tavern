//go:build windows

package audio

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	waveMapper         = 0xFFFFFFFF
	waveFormatPCM      = 1
	whdrDone           = 0x00000001
	mmsyserrNoErr      = 0
	winmmSrcFrameBytes = DownlinkRate * 2 / 50 // 20ms of 24kHz s16le
	winmmFrameBytes    = PlayRate * 2 / 50     // 20ms of 48kHz s16le
	winmmOutBufs       = 12
	winmmPrimeBytes    = winmmSrcFrameBytes * 5 // ~100ms of source before start
)

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	Size           uint16
}

type waveHdr struct {
	Data          *byte
	BufferLength  uint32
	BytesRecorded uint32
	User          uintptr
	Flags         uint32
	Loops         uint32
	Next          uintptr
	Reserved      uintptr
}

type waveInCapsW struct {
	Mid           uint16
	Pid           uint16
	DriverVersion uint32
	Name          [32]uint16
	Formats       uint32
	Channels      uint16
	Reserved      uint16
}

var (
	modWinmm           = windows.NewLazySystemDLL("winmm.dll")
	procWaveOutOpen    = modWinmm.NewProc("waveOutOpen")
	procWaveOutClose   = modWinmm.NewProc("waveOutClose")
	procWaveOutPrepare = modWinmm.NewProc("waveOutPrepareHeader")
	procWaveOutUnprep  = modWinmm.NewProc("waveOutUnprepareHeader")
	procWaveOutWrite   = modWinmm.NewProc("waveOutWrite")
	procWaveOutReset   = modWinmm.NewProc("waveOutReset")
	procWaveInOpen     = modWinmm.NewProc("waveInOpen")
	procWaveInClose    = modWinmm.NewProc("waveInClose")
	procWaveInPrepare  = modWinmm.NewProc("waveInPrepareHeader")
	procWaveInUnprep   = modWinmm.NewProc("waveInUnprepareHeader")
	procWaveInAddBuf   = modWinmm.NewProc("waveInAddBuffer")
	procWaveInStart    = modWinmm.NewProc("waveInStart")
	procWaveInReset    = modWinmm.NewProc("waveInReset")
	procWaveInGetNum   = modWinmm.NewProc("waveInGetNumDevs")
	procWaveInGetCaps  = modWinmm.NewProc("waveInGetDevCapsW")
)

func openWinmmPlayer() *Player {
	event, err := createAutoEvent()
	hasEvent := err == nil && event != 0
	var cb, flags uintptr
	if hasEvent {
		cb = uintptr(event)
		flags = callbackEvent
	}

	var hwo uintptr
	wfx := waveFormatEx{
		FormatTag:      waveFormatPCM,
		Channels:       1,
		SamplesPerSec:  PlayRate,
		AvgBytesPerSec: uint32(PlayRate * 2),
		BlockAlign:     2,
		BitsPerSample:  16,
	}
	r, _, err := procWaveOutOpen.Call(
		uintptr(unsafe.Pointer(&hwo)),
		uintptr(waveMapper),
		uintptr(unsafe.Pointer(&wfx)),
		cb, 0, flags,
	)
	if r != mmsyserrNoErr || hwo == 0 {
		if hasEvent {
			_ = windows.CloseHandle(event)
		}
		_ = err
		return nil
	}

	in := make(chan []byte, 256)
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if hasEvent {
			defer windows.CloseHandle(event)
		}
		winmmLoop(hwo, in, stopCh, event)
	}()

	p := &Player{kind: "winmm"}
	p.stream = func(pcm []byte) {
		if len(pcm) == 0 {
			return
		}
		cp := make([]byte, len(pcm))
		copy(cp, pcm)
		select {
		case <-stopCh:
		case in <- cp:
		}
	}
	var once atomic.Bool
	p.closeFn = func() {
		if once.Swap(true) {
			return
		}
		close(stopCh)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	return p
}

type winmmOutBuf struct {
	pcm  []byte
	hdr  waveHdr
	busy bool
}

func winmmLoop(hwo uintptr, in <-chan []byte, stop <-chan struct{}, event windows.Handle) {
	defer func() {
		_, _, _ = procWaveOutReset.Call(hwo)
		_, _, _ = procWaveOutClose.Call(hwo)
	}()
	bufs := make([]winmmOutBuf, winmmOutBufs)
	var pin runtime.Pinner
	defer pin.Unpin()
	for i := range bufs {
		bufs[i].pcm = make([]byte, winmmFrameBytes)
		bufs[i].hdr.Data = &bufs[i].pcm[0]
		bufs[i].hdr.BufferLength = uint32(winmmFrameBytes)
		pin.Pin(&bufs[i].pcm[0])
		pin.Pin(&bufs[i].hdr)
	}

	acc := make([]byte, 0, winmmSrcFrameBytes*16)
	primed := false
	for {
		winmmRecycle(hwo, bufs)
		// After an utterance the device drains. Starting the next one
		// from a single 20ms buffer underruns immediately (stutter).
		if primed && len(acc) < winmmSrcFrameBytes && !winmmAnyBusy(bufs) {
			primed = false
		}
		if !primed && len(acc) >= winmmPrimeBytes {
			primed = true
		}
		if primed {
			if err := winmmFill(hwo, bufs, &acc); err != nil {
				return
			}
		}
		select {
		case <-stop:
			return
		case pcm, ok := <-in:
			if !ok {
				return
			}
			acc = append(acc, pcm...)
		default:
			if event != 0 {
				_, _ = windows.WaitForSingleObject(event, 2)
			} else {
				time.Sleep(2 * time.Millisecond)
			}
		}
	}
}

func winmmAnyBusy(bufs []winmmOutBuf) bool {
	for i := range bufs {
		if bufs[i].busy {
			return true
		}
	}
	return false
}

func winmmRecycle(hwo uintptr, bufs []winmmOutBuf) {
	size := uintptr(unsafe.Sizeof(waveHdr{}))
	for i := range bufs {
		if !bufs[i].busy || bufs[i].hdr.Flags&whdrDone == 0 {
			continue
		}
		_, _, _ = procWaveOutUnprep.Call(hwo, uintptr(unsafe.Pointer(&bufs[i].hdr)), size)
		bufs[i].busy = false
	}
}

func winmmFill(hwo uintptr, bufs []winmmOutBuf, acc *[]byte) error {
	size := uintptr(unsafe.Sizeof(waveHdr{}))
	for i := range bufs {
		if bufs[i].busy {
			continue
		}
		if len(*acc) < winmmSrcFrameBytes {
			return nil
		}
		up := UpsampleS16LE2x((*acc)[:winmmSrcFrameBytes])
		*acc = (*acc)[winmmSrcFrameBytes:]
		copy(bufs[i].pcm, up)
		bufs[i].hdr.Flags = 0
		bufs[i].hdr.BytesRecorded = 0
		r, _, err := procWaveOutPrepare.Call(hwo, uintptr(unsafe.Pointer(&bufs[i].hdr)), size)
		if r != mmsyserrNoErr {
			return fmt.Errorf("waveOutPrepareHeader: %v", err)
		}
		r, _, err = procWaveOutWrite.Call(hwo, uintptr(unsafe.Pointer(&bufs[i].hdr)), size)
		if r != mmsyserrNoErr {
			_, _, _ = procWaveOutUnprep.Call(hwo, uintptr(unsafe.Pointer(&bufs[i].hdr)), size)
			return fmt.Errorf("waveOutWrite: %v", err)
		}
		bufs[i].busy = true
	}
	return nil
}
