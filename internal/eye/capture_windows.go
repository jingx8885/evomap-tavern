//go:build windows

package eye

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// CaptureDesktop grabs one JPEG of the desktop. It does not click.
func CaptureDesktop(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := os.CreateTemp("", "eye-screen-*.jpg")
	if err != nil {
		return nil, err
	}
	path := out.Name()
	out.Close()
	defer os.Remove(path)

	script := `
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
function Save-Screen($bounds) {
  $bmp = New-Object System.Drawing.Bitmap ([int]$bounds.Width), ([int]$bounds.Height)
  $g = [System.Drawing.Graphics]::FromImage($bmp)
  $g.CopyFromScreen([int]$bounds.X, [int]$bounds.Y, 0, 0, $bmp.Size)
  $g.Dispose()
  $codec = [System.Drawing.Imaging.ImageCodecInfo]::GetImageEncoders() | Where-Object { $_.MimeType -eq 'image/jpeg' } | Select-Object -First 1
  $ep = New-Object System.Drawing.Imaging.EncoderParameters 1
  $ep.Param[0] = New-Object System.Drawing.Imaging.EncoderParameter ([System.Drawing.Imaging.Encoder]::Quality, [long]55)
  $bmp.Save($env:EYE_JPEG, $codec, $ep)
  $bmp.Dispose()
}
$bounds = [System.Windows.Forms.SystemInformation]::VirtualScreen
if ($bounds.Width -le 0 -or $bounds.Height -le 0 -or ($bounds.Width * $bounds.Height) -gt 16000000) {
  $bounds = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds
}
Save-Screen $bounds
`
	f, err := os.CreateTemp("", "eye-screen-*.ps1")
	if err != nil {
		return nil, err
	}
	scriptPath := f.Name()
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		os.Remove(scriptPath)
		return nil, err
	}
	f.Close()
	defer os.Remove(scriptPath)

	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-STA", "-WindowStyle", "Hidden",
		"-ExecutionPolicy", "Bypass", "-File", scriptPath)
	cmd.Env = append(os.Environ(), "EYE_JPEG="+path)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("screen capture: %v: %s", err, clipCaption(string(raw), 180))
	}
	jpeg, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(jpeg) < 8 {
		return nil, fmt.Errorf("screen capture was empty")
	}
	return jpeg, nil
}
