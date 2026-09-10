// Package logs is the leveled logger the whole binary shares. Infof and Errorf
// always print, Debugf only with -v. Kept trivial on purpose: the durable record
// is the events table, not stdout.
//
// It also keeps the last few hundred lines in memory, which is the only thing
// the dashboard's log tail can read - a Synology's container log is exactly the
// place an operator cannot easily get to (DESIGN.md §4).
package logs

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// tailSize is how many lines the ring holds. Enough to cover what just went
// wrong, small enough that it is never worth thinking about the memory.
const tailSize = 500

type Logger struct {
	Verbose bool

	mu    sync.Mutex
	ring  []Line
	first int   // ring index of the oldest line
	seq   int64 // sequence of the newest line; also the total ever written
}

// Line is one logged line as the dashboard reads it. Seq is monotonic and never
// reused, so a client that reconnects asks for everything after the last one it
// saw rather than re-reading the whole ring.
type Line struct {
	Seq   int64     `json:"seq"`
	TS    time.Time `json:"ts"`
	Level string    `json:"level"`
	Text  string    `json:"text"`
}

func (l *Logger) Infof(format string, a ...any)  { l.emit("info", format, a...) }
func (l *Logger) Warnf(format string, a ...any)  { l.emit("warn", format, a...) }
func (l *Logger) Errorf(format string, a ...any) { l.emit("error", format, a...) }
func (l *Logger) Debugf(format string, a ...any) {
	if l.Verbose {
		l.emit("debug", format, a...)
	}
}

func (l *Logger) emit(level, format string, a ...any) {
	text := fmt.Sprintf(format, a...)
	log.Printf("[%-5s] %s", level, text)
	l.remember(Line{TS: time.Now(), Level: level, Text: text})
}

func (l *Logger) remember(line Line) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.seq++
	line.Seq = l.seq

	if len(l.ring) < tailSize {
		l.ring = append(l.ring, line)
		return
	}
	l.ring[l.first] = line
	l.first = (l.first + 1) % tailSize
}

// Tail returns the remembered lines with a sequence above after, oldest first,
// along with the newest sequence there is. A zero after means "everything still
// held", which is what a freshly opened dashboard asks for.
func (l *Logger) Tail(after int64) ([]Line, int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.ring) == 0 {
		return nil, 0
	}

	out := make([]Line, 0, len(l.ring))
	for i := range l.ring {
		line := l.ring[(l.first+i)%len(l.ring)]
		if line.Seq > after {
			out = append(out, line)
		}
	}
	return out, l.seq
}
