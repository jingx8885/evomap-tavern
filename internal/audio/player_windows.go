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
	waveMapper            = 0xFFFFFFFF
	waveFormatPCM         = 1
	whdrDone              = 0x00000001
	mmsyserrNoErr         = 0
	wavErrUnprepared      = 34
	winmmFrameMs          = 40
	winmmSrcFrameBytes    = DownlinkRate * 2 * winmmFrameMs / 1000 // 40ms of 24kHz s16le
	winmmFrameBytes       = PlayRate * 2 * winmmFrameMs / 1000     // 40ms of 48kHz s16le
	winmmOutBufs          = 8                                      // 320ms queued in the device
	winmmPrimeBytes       = winmmSrcFrameBytes * 4                 // ~160ms before the first utterance
	winmmMaxAccBytes      = winmmSrcFrameBytes * 250               // ~10s safety valve; never punch holes in a live reply
	threadPriorityHighest = 2
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
	modWinmm              = windows.NewLazySystemDLL("winmm.dll")
	procWaveOutOpen       = modWinmm.NewProc("waveOutOpen")
	procWaveOutClose      = modWinmm.NewProc("waveOutClose")
	procWaveOutPrepare    = modWinmm.NewProc("waveOutPrepareHeader")
	procWaveOutUnprep     = modWinmm.NewProc("waveOutUnprepareHeader")
	procWaveOutWrite      = modWinmm.NewProc("waveOutWrite")
	procWaveOutReset      = modWinmm.NewProc("waveOutReset")
	procTimeBeginPeriod   = modWinmm.NewProc("timeBeginPeriod")
	procTimeEndPeriod     = modWinmm.NewProc("timeEndPeriod")
	modKernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procGetCurrentThread  = modKernel32.NewProc("GetCurrentThread")
	procSetThreadPriority = modKernel32.NewProc("SetThreadPriority")
	procWaveInOpen        = modWinmm.NewProc("waveInOpen")
	procWaveInClose       = modWinmm.NewProc("waveInClose")
	procWaveInPrepare     = modWinmm.NewProc("waveInPrepareHeader")
	procWaveInUnprep      = modWinmm.NewProc("waveInUnprepareHeader")
	procWaveInAddBuf      = modWinmm.NewProc("waveInAddBuffer")
	procWaveInStart       = modWinmm.NewProc("waveInStart")
	procWaveInReset       = modWinmm.NewProc("waveInReset")
	procWaveInGetNum      = modWinmm.NewProc("waveInGetNumDevs")
	procWaveInGetCaps     = modWinmm.NewProc("waveInGetDevCapsW")
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

	// Idle silence is stripped before WritePCM. What remains is speech,
	// often delivered faster than real time: buffer it and play at 1x.
	// Dropping the oldest chunk here punched holes in the current reply.
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
		// Never block the websocket reader. A full queue means playback
		// is already behind; waiting here held the reply open until
		// the speaker drained it, so the turn never reached Jev.
		select {
		case <-stopCh:
			return
		default:
		}
		select {
		case in <- cp:
			return
		default:
		}
		select {
		case <-in:
		default:
		}
		select {
		case in <- cp:
		case <-stopCh:
		default:
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
	pcm      []byte
	hdr      waveHdr
	busy     bool
	prepared bool
}

func winmmLoop(hwo uintptr, in <-chan []byte, stop <-chan struct{}, event windows.Handle) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	thread, _, _ := procGetCurrentThread.Call()
	_, _, _ = procSetThreadPriority.Call(thread, threadPriorityHighest)
	_, _, _ = procTimeBeginPeriod.Call(1)
	defer procTimeEndPeriod.Call(1)

	bufs := make([]winmmOutBuf, winmmOutBufs)
	var pin runtime.Pinner
	defer pin.Unpin()
	hdrSize := uintptr(unsafe.Sizeof(waveHdr{}))
	defer func() {
		_, _, _ = procWaveOutReset.Call(hwo)
		for i := range bufs {
			if !bufs[i].prepared {
				continue
			}
			_, _, _ = procWaveOutUnprep.Call(hwo, uintptr(unsafe.Pointer(&bufs[i].hdr)), hdrSize)
		}
		_, _, _ = procWaveOutClose.Call(hwo)
	}()
	for i := range bufs {
		bufs[i].pcm = make([]byte, winmmFrameBytes)
		bufs[i].hdr.Data = &bufs[i].pcm[0]
		bufs[i].hdr.BufferLength = uint32(winmmFrameBytes)
		pin.Pin(&bufs[i].pcm[0])
		pin.Pin(&bufs[i].hdr)
		r, _, _ := procWaveOutPrepare.Call(hwo, uintptr(unsafe.Pointer(&bufs[i].hdr)), hdrSize)
		if r != mmsyserrNoErr {
			return
		}
		bufs[i].prepared = true
	}

	acc := make([]byte, 0, winmmSrcFrameBytes*16)
	var cue playbackCue
	for {
		winmmRecycle(bufs)
		// Prime once per utterance. A short gap keeps the cue armed so the
		// next phrase does not sit through another prebuffer (that restart
		// is the chop between words).
		if cue.ready(len(acc), winmmAnyBusy(bufs), time.Now(), winmmSrcFrameBytes, winmmPrimeBytes) {
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
			acc = winmmDrain(in, acc)
			if len(acc) > winmmMaxAccBytes {
				// Pathological backlog only. A normal TTS burst must play
				// through; trimming mid-utterance is what sounded choppy.
				copy(acc, acc[len(acc)-winmmMaxAccBytes:])
				acc = acc[:winmmMaxAccBytes]
			}
		default:
			if event != 0 {
				_, _ = windows.WaitForSingleObject(event, 2)
			} else {
				time.Sleep(2 * time.Millisecond)
			}
		}
	}
}

func winmmDrain(in <-chan []byte, acc []byte) []byte {
	for {
		select {
		case pcm, ok := <-in:
			if !ok {
				return acc
			}
			acc = append(acc, pcm...)
		default:
			return acc
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

func winmmRecycle(bufs []winmmOutBuf) {
	for i := range bufs {
		if !bufs[i].busy || bufs[i].hdr.Flags&whdrDone == 0 {
			continue
		}
		// Header stays prepared. Unprepare/prepare on every 20ms block
		// left a gap the device played as a click.
		bufs[i].hdr.Flags &^= whdrDone
		bufs[i].busy = false
	}
}

func winmmFill(hwo uintptr, bufs []winmmOutBuf, acc *[]byte) error {
	size := uintptr(unsafe.Sizeof(waveHdr{}))
	frameSamples := winmmSrcFrameBytes / 2
	for i := range bufs {
		if bufs[i].busy {
			continue
		}
		if len(*acc) < winmmSrcFrameBytes {
			return nil
		}
		UpsampleS16LE2xFrame(bufs[i].pcm, *acc, frameSamples)
		*acc = (*acc)[winmmSrcFrameBytes:]
		bufs[i].hdr.Flags &^= whdrDone
		bufs[i].hdr.BytesRecorded = 0
		if bufs[i].hdr.Flags&whdrPrepared == 0 {
			bufs[i].hdr.Flags |= whdrPrepared
		}
		r, _, err := procWaveOutWrite.Call(hwo, uintptr(unsafe.Pointer(&bufs[i].hdr)), size)
		if r == wavErrUnprepared {
			bufs[i].hdr.Flags = 0
			r, _, err = procWaveOutPrepare.Call(hwo, uintptr(unsafe.Pointer(&bufs[i].hdr)), size)
			if r != mmsyserrNoErr {
				return fmt.Errorf("waveOutPrepareHeader: %v", err)
			}
			r, _, err = procWaveOutWrite.Call(hwo, uintptr(unsafe.Pointer(&bufs[i].hdr)), size)
		}
		if r != mmsyserrNoErr {
			return fmt.Errorf("waveOutWrite: %v", err)
		}
		bufs[i].busy = true
	}
	return nil
}
