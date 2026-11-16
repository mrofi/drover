package main

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// logEntry is one line an instance printed (slot >= 0) or one event drover logged about the app (slot < 0).
type logEntry struct {
	seq  uint64
	at   time.Time
	slot int
	text string
}

// logBuffer keeps the most recent lines of one app in memory, for the logs endpoint. It is not a log store:
// everything is also written to drover's stdout, which is where long-term collection should read from.
type logBuffer struct {
	mu      sync.Mutex
	max     int
	entries []logEntry
	seq     uint64
	changed chan struct{} // closed and replaced on every add, to wake followers
}

func newLogBuffer(max int) *logBuffer {
	return &logBuffer{max: max, changed: make(chan struct{})}
}

func (b *logBuffer) add(slot int, text string) {
	b.mu.Lock()
	b.seq++
	b.entries = append(b.entries, logEntry{seq: b.seq, at: time.Now().UTC(), slot: slot, text: text})
	if len(b.entries) > b.max {
		b.entries = append(b.entries[:0], b.entries[len(b.entries)-b.max:]...)
	}
	close(b.changed)
	b.changed = make(chan struct{})
	b.mu.Unlock()
}

// read returns the entries after seq (for one slot when slot >= 0, else all), the seq to continue from, and
// a channel that is closed when more arrive.
func (b *logBuffer) read(after uint64, slot int) ([]logEntry, uint64, <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []logEntry
	for _, e := range b.entries {
		if e.seq > after && (slot < 0 || e.slot == slot) {
			out = append(out, e)
		}
	}
	return out, b.seq, b.changed
}

func writeLogs(w io.Writer, app string, entries []logEntry) {
	for _, e := range entries {
		if e.slot < 0 {
			fmt.Fprintf(w, "%s [%s] %s\n", e.at.Format(time.RFC3339), app, e.text)
		} else {
			fmt.Fprintf(w, "%s [%s#%d] %s\n", e.at.Format(time.RFC3339), app, e.slot, e.text)
		}
	}
}
