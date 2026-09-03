package logs

import (
	"io"
	"log"
	"testing"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	m.Run()
}

func TestTailReturnsWhatIsNew(t *testing.T) {
	l := &Logger{}

	if lines, seq := l.Tail(0); len(lines) != 0 || seq != 0 {
		t.Fatalf("an empty logger tailed %d lines at seq %d", len(lines), seq)
	}

	l.Infof("one")
	l.Warnf("two %d", 2)

	lines, seq := l.Tail(0)
	if len(lines) != 2 || seq != 2 {
		t.Fatalf("tailed %d lines at seq %d, want 2 at 2", len(lines), seq)
	}
	if lines[0].Text != "one" || lines[1].Text != "two 2" {
		t.Errorf("lines came back as %q and %q", lines[0].Text, lines[1].Text)
	}
	if lines[1].Level != "warn" {
		t.Errorf("level = %q, want warn", lines[1].Level)
	}

	l.Errorf("three")
	lines, _ = l.Tail(seq)
	if len(lines) != 1 || lines[0].Text != "three" {
		t.Fatalf("tailing after seq %d gave %+v", seq, lines)
	}
}

func TestDebugIsOnlyRememberedWhenVerbose(t *testing.T) {
	quiet := &Logger{}
	quiet.Debugf("hidden")
	if lines, _ := quiet.Tail(0); len(lines) != 0 {
		t.Errorf("a quiet logger remembered %d debug lines", len(lines))
	}

	loud := &Logger{Verbose: true}
	loud.Debugf("shown")
	if lines, _ := loud.Tail(0); len(lines) != 1 {
		t.Errorf("a verbose logger remembered %d debug lines, want 1", len(lines))
	}
}

func TestRingDropsTheOldest(t *testing.T) {
	l := &Logger{}
	for i := range tailSize + 10 {
		l.Infof("line %d", i)
	}

	lines, seq := l.Tail(0)
	if len(lines) != tailSize {
		t.Fatalf("ring holds %d lines, want %d", len(lines), tailSize)
	}
	if seq != int64(tailSize+10) {
		t.Errorf("seq = %d, want %d", seq, tailSize+10)
	}
	if lines[0].Seq != 11 {
		t.Errorf("oldest surviving line is seq %d, want 11", lines[0].Seq)
	}
	if lines[len(lines)-1].Text != "line 509" {
		t.Errorf("newest line is %q", lines[len(lines)-1].Text)
	}
}
