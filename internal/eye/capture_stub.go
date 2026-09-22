//go:build !windows

package eye

import (
	"context"
	"fmt"
)

// CaptureDesktop is only implemented on Windows.
func CaptureDesktop(ctx context.Context) ([]byte, error) {
	_ = ctx
	return nil, fmt.Errorf("desktop capture is only available on windows")
}
