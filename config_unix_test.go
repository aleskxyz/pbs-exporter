//go:build unix

package main

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO as token file is rejected instead of blocking the start.
func TestTokenFileFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	done := make(chan error, 1)
	go func() { _, err := readTokenFile(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected an error for a FIFO")
		}
	case <-time.After(2 * time.Second):
		t.Error("reading a FIFO blocks")
	}
}
