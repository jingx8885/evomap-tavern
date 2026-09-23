// Package logq writes console lines off the caller's goroutine.
//
// A Windows console can stall a write (slow redraw, or QuickEdit
// selection pauses output). The voice event pump and the mic pacer log
// from hot loops, so a blocking write there stalls downlink audio.
package logq

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const depth = 1024

var (
	once    sync.Once
	lines   chan string
	dropped atomic.Int64
	out     io.Writer = os.Stdout
)

func start() {
	lines = make(chan string, depth)
	go func() {
		for line := range lines {
			if n := dropped.Swap(0); n > 0 {
				fmt.Fprintf(out, "[log] dropped %d lines while the console was slow\n", n)
			}
			io.WriteString(out, line)
		}
	}()
}

// Flush waits up to d for queued lines to reach the console.
func Flush(d time.Duration) {
	once.Do(start)
	deadline := time.Now().Add(d)
	for len(lines) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

// Printf queues one line. It never blocks; a full queue drops the line.
func Printf(format string, args ...any) {
	Println(fmt.Sprintf(format, args...))
}

// Println queues text as one line.
func Println(text string) {
	once.Do(start)
	select {
	case lines <- text + "\n":
	default:
		dropped.Add(1)
	}
}
