//go:build windows

package desk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type psWin struct {
	ID      int    `json:"id"`
	Process string `json:"process"`
	Title   string `json:"title"`
	HWND    int64  `json:"hwnd"`
}

type psSnap struct {
	ForegroundHWND int64           `json:"foreground_hwnd"`
	Windows        json.RawMessage `json:"windows"`
}

func listWindows() ([]Window, int64, error) {
	script := `
$ErrorActionPreference = 'SilentlyContinue'
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public static class DeskNative {
  [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow();
}
"@
$fg = [int64][DeskNative]::GetForegroundWindow()
$wins = @(Get-Process | Where-Object { $_.MainWindowHandle -ne 0 -and $_.MainWindowTitle } | ForEach-Object {
  [pscustomobject]@{
    id = $_.Id
    process = [string]$_.ProcessName
    title = [string]$_.MainWindowTitle
    hwnd = [int64]$_.MainWindowHandle
  }
} | Select-Object -First 24)
if ($wins.Count -eq 0) {
  [pscustomobject]@{ foreground_hwnd = $fg; windows = @() } | ConvertTo-Json -Compress -Depth 4
} else {
  [pscustomobject]@{ foreground_hwnd = $fg; windows = $wins } | ConvertTo-Json -Compress -Depth 4
}
`
	raw, err := runPS(script, nil)
	if err != nil {
		return nil, 0, err
	}
	var out psSnap
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, 0, fmt.Errorf("window snapshot json: %w", err)
	}
	listed, err := decodePSWindows(out.Windows)
	if err != nil {
		return nil, 0, err
	}
	wins := make([]Window, 0, len(listed))
	for i, w := range listed {
		title := strings.TrimSpace(w.Title)
		if title == "" {
			continue
		}
		wins = append(wins, Window{
			ID:      fmt.Sprintf("w%d", i+1),
			PID:     w.ID,
			Process: strings.TrimSpace(w.Process),
			Title:   title,
			HWND:    w.HWND,
		})
	}
	return wins, out.ForegroundHWND, nil
}

func decodePSWindows(raw json.RawMessage) ([]psWin, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '{' {
		var one psWin
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, fmt.Errorf("window snapshot json: %w", err)
		}
		return []psWin{one}, nil
	}
	var many []psWin
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("window snapshot json: %w", err)
	}
	return many, nil
}

func activateWindow(win Window) error {
	script := `
$ErrorActionPreference = 'Stop'
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public static class DeskNative {
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr hWnd);
  [DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr hWnd, int nCmdShow);
}
"@
$hwnd = [int64]$env:DESK_HWND
if ($hwnd -ne 0) {
  $p = [IntPtr]$hwnd
  [DeskNative]::ShowWindow($p, 9) | Out-Null
  if (-not [DeskNative]::SetForegroundWindow($p)) { throw 'SetForegroundWindow failed' }
  return
}
$sh = New-Object -ComObject WScript.Shell
if (-not $sh.AppActivate($env:DESK_TITLE)) { throw 'AppActivate failed' }
`
	env := map[string]string{
		"DESK_HWND":  fmt.Sprintf("%d", win.HWND),
		"DESK_TITLE": win.Title,
	}
	_, err := runPS(script, env)
	return err
}

func sendHotkey(name string) error {
	seq, ok := hotkeySeq(name)
	if !ok {
		return fmt.Errorf("unknown hotkey %q", name)
	}
	script := `
$ErrorActionPreference = 'Stop'
$sh = New-Object -ComObject WScript.Shell
Start-Sleep -Milliseconds 120
$sh.SendKeys($env:DESK_KEYS)
`
	_, err := runPS(script, map[string]string{"DESK_KEYS": seq})
	return err
}

func pasteText(text string) error {
	f, err := os.CreateTemp("", "desk-paste-*.txt")
	if err != nil {
		return err
	}
	path := f.Name()
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	f.Close()
	defer os.Remove(path)
	script := `
$ErrorActionPreference = 'Stop'
Get-Content -Raw -Encoding UTF8 $env:DESK_PASTE_FILE | Set-Clipboard
$sh = New-Object -ComObject WScript.Shell
Start-Sleep -Milliseconds 80
$sh.SendKeys('^v')
`
	_, err = runPS(script, map[string]string{"DESK_PASTE_FILE": path})
	return err
}

func hotkeySeq(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "ctrl_l":
		return "^l", true
	case "ctrl_i":
		return "^i", true
	case "ctrl_s":
		return "^s", true
	case "ctrl_enter":
		return "^{ENTER}", true
	case "alt_tab":
		return "%{TAB}", true
	case "enter":
		return "{ENTER}", true
	case "escape":
		return "{ESC}", true
	default:
		return "", false
	}
}

func runPS(script string, extra map[string]string) ([]byte, error) {
	f, err := os.CreateTemp("", "desk-*.ps1")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	// The console code page (GBK on Chinese Windows) would garble titles;
	// Go reads stdout as UTF-8.
	script = "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8\n" + script
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	f.Close()
	defer os.Remove(path)

	cmd := exec.Command("powershell.exe", "-NoProfile", "-STA",
		"-ExecutionPolicy", "Bypass", "-File", path)
	cmd.Env = os.Environ()
	for k, v := range extra {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("powershell: %v: %s", err, clip(string(raw), 400))
	}
	return []byte(strings.TrimSpace(string(raw))), nil
}
