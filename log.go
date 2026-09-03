package main

import "log"

// Logger is a tiny leveled logger: Infof and Errorf always print, Debugf only
// with -v. Kept trivial on purpose.
type Logger struct {
	Verbose bool
}

func (l *Logger) Infof(format string, a ...any)  { log.Printf("[info]  "+format, a...) }
func (l *Logger) Errorf(format string, a ...any) { log.Printf("[error] "+format, a...) }
func (l *Logger) Debugf(format string, a ...any) {
	if l.Verbose {
		log.Printf("[debug] "+format, a...)
	}
}
