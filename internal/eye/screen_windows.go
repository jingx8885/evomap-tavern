//go:build windows

package eye

import (
	"fmt"
	"image"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")

	procGetDC                   = user32.NewProc("GetDC")
	procReleaseDC               = user32.NewProc("ReleaseDC")
	procGetSystemMetrics        = user32.NewProc("GetSystemMetrics")
	procSetProcessDPIAware      = user32.NewProc("SetProcessDPIAware")
	procCreateCompatibleDC      = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap  = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject            = gdi32.NewProc("SelectObject")
	procBitBlt                  = gdi32.NewProc("BitBlt")
	procDeleteDC                = gdi32.NewProc("DeleteDC")
	procDeleteObject            = gdi32.NewProc("DeleteObject")
	procGetDIBits               = gdi32.NewProc("GetDIBits")
	dpiOnce                     sync.Once
)

const (
	smCXScreen     = 0
	smCYScreen     = 1
	srcCopy        = 0x00CC0020
	biRGB          = 0
	dibRGBColors   = 0
)

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

func captureScreenJPEG(maxEdge int) ([]byte, error) {
	img, err := capturePrimary()
	if err != nil {
		return nil, err
	}
	raw, _, _, err := EncodeJPEG(img, maxEdge, defaultQuality)
	return raw, err
}

func capturePrimary() (*image.RGBA, error) {
	dpiOnce.Do(func() {
		_, _, _ = procSetProcessDPIAware.Call()
	})
	w, _, _ := procGetSystemMetrics.Call(smCXScreen)
	h, _, _ := procGetSystemMetrics.Call(smCYScreen)
	if w == 0 || h == 0 {
		return nil, fmt.Errorf("screen metrics 0")
	}
	hdc, _, err := procGetDC.Call(0)
	if hdc == 0 {
		return nil, fmt.Errorf("GetDC: %v", err)
	}
	defer procReleaseDC.Call(0, hdc)

	mem, _, err := procCreateCompatibleDC.Call(hdc)
	if mem == 0 {
		return nil, fmt.Errorf("CreateCompatibleDC: %v", err)
	}
	defer procDeleteDC.Call(mem)

	bmp, _, err := procCreateCompatibleBitmap.Call(hdc, w, h)
	if bmp == 0 {
		return nil, fmt.Errorf("CreateCompatibleBitmap: %v", err)
	}
	defer procDeleteObject.Call(bmp)

	old, _, _ := procSelectObject.Call(mem, bmp)
	defer procSelectObject.Call(mem, old)

	ret, _, err := procBitBlt.Call(mem, 0, 0, w, h, hdc, 0, 0, srcCopy)
	if ret == 0 {
		return nil, fmt.Errorf("BitBlt: %v", err)
	}

	width, height := int(w), int(h)
	buf := make([]byte, width*height*4)
	hdr := bitmapInfoHeader{
		Size:      40,
		Width:     int32(width),
		Height:    -int32(height),
		Planes:    1,
		BitCount:  32,
		Compression: biRGB,
	}
	got, _, err := procGetDIBits.Call(
		mem, bmp, 0, uintptr(height),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&hdr)),
		dibRGBColors,
	)
	if got == 0 {
		hdr.Height = int32(height)
		got, _, err = procGetDIBits.Call(
			mem, bmp, 0, uintptr(height),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&hdr)),
			dibRGBColors,
		)
		if got == 0 {
			return nil, fmt.Errorf("GetDIBits: %v", err)
		}
		flipBGRA(buf, width, height)
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := 0; i+3 < len(buf) && i+3 < len(img.Pix); i += 4 {
		img.Pix[i+0] = buf[i+2]
		img.Pix[i+1] = buf[i+1]
		img.Pix[i+2] = buf[i+0]
		img.Pix[i+3] = 255
	}
	return img, nil
}

func flipBGRA(buf []byte, w, h int) {
	stride := w * 4
	tmp := make([]byte, stride)
	for y := 0; y < h/2; y++ {
		top := y * stride
		bot := (h - 1 - y) * stride
		copy(tmp, buf[top:top+stride])
		copy(buf[top:top+stride], buf[bot:bot+stride])
		copy(buf[bot:bot+stride], tmp)
	}
}
