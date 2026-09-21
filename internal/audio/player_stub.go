//go:build !windows

package audio

func openWinmmPlayer() *Player { return nil }
