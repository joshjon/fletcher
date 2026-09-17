// Package host provides bounded inspection and maintenance of the local daemon.
package host

import (
	"strings"
	"sync"
)

// Logs retains a bounded process-local tail of the daemon's structured logs.
type Logs struct {
	mu   sync.Mutex
	data []byte
}

// Write records at most 256 KiB, discarding oldest complete or partial lines.
func (l *Logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	const capacity = 256 * 1024
	if len(p) >= capacity {
		l.data = append(l.data[:0], p[len(p)-capacity:]...)
	} else {
		overflow := len(l.data) + len(p) - capacity
		if overflow > 0 {
			l.data = l.data[overflow:]
		}
		l.data = append(l.data, p...)
	}
	return len(p), nil
}

// Tail returns at most lines recent records from this process.
func (l *Logs) Tail(lines int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	parts := strings.Split(strings.TrimSpace(string(l.data)), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}
