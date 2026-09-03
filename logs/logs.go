// Package logs is the leveled logger the whole binary shares. Infof and Errorf
// always print, Debugf only with -v. Kept trivial on purpose: the durable record
// is the events table, not stdout.
package logs

import "log"

type Logger struct {
	Verbose bool
}

func (l *Logger) Infof(format string, a ...any)  { log.Printf("[info]  "+format, a...) }
func (l *Logger) Warnf(format string, a ...any)  { log.Printf("[warn]  "+format, a...) }
func (l *Logger) Errorf(format string, a ...any) { log.Printf("[error] "+format, a...) }
func (l *Logger) Debugf(format string, a ...any) {
	if l.Verbose {
		log.Printf("[debug] "+format, a...)
	}
}
