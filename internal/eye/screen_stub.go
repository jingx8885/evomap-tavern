//go:build !windows

package eye

import "fmt"

func captureScreenJPEG(maxEdge int) ([]byte, error) {
	return nil, fmt.Errorf("screen capture is Windows-only in this build")
}
