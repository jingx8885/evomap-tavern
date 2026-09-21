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
	waveMapper      = 0xFFFFFFFF
	waveFormatPCM   = 1
	whdrDone        = 0x00000001
	mmsyserrNoErr   = 0
	winmmFrameBytes = DownlinkRate * 2 / 50 // 20ms of s16le mono
	winmmOutBufs    = 8
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
	var hwo uintptr
	wfx := waveFormatEx{
		FormatTag:      waveFormatPCM,
		Channels:       1,
		SamplesPerSec:  DownlinkRate,
		AvgBytesPerSec: uint32(DownlinkRate * 2),
		BlockAlign:     2,
		BitsPerSample:  16,
	}
	r, _, err := procWaveOutOpen.Call(
		uintptr(unsafe.Pointer(&hwo)),
		uintptr(waveMapper),
		uintptr(unsafe.Pointer(&wfx)),
		0, 0, 0,
	)
	if r != mmsyserrNoErr || hwo == 0 {
		_ = err
		return nil
	}

	in := make(chan []byte, 256)
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		winmmLoop(hwo, in, stopCh)
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

func winmmLoop(hwo uintptr, in <-chan []byte, stop <-chan struct{}) {
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

	acc := make([]byte, 0, winmmFrameBytes*16)
	primed := false
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		winmmRecycle(hwo, bufs)
		if !primed && len(acc) >= winmmFrameBytes*3 {
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
		case <-tick.C:
		}
	}
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
		if len(*acc) < winmmFrameBytes {
			return nil
		}
		copy(bufs[i].pcm, (*acc)[:winmmFrameBytes])
		*acc = (*acc)[winmmFrameBytes:]
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
