//go:build !windows

package desk

import "fmt"

func listWindows() ([]Window, int64, error) {
	return nil, 0, fmt.Errorf("window snapshot is Windows-only")
}

func activateWindow(Window) error {
	return fmt.Errorf("desktop input is Windows-only")
}

func sendHotkey(string) error {
	return fmt.Errorf("desktop input is Windows-only")
}

func pasteText(string) error {
	return fmt.Errorf("desktop input is Windows-only")
}
