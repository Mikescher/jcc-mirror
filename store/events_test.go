package store

import (
	"context"
	"testing"
	"time"
)

func TestAppendAndQueryEvents(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	for _, e := range []Event{
		{Kind: KindStartup, Message: "started"},
		{Kind: KindTunnelDown, Level: LevelWarn, Message: "no handshake"},
		{Kind: KindRemoteProbe, Message: "12 files", Data: map[string]any{"files": 12}},
	} {
		e := e
		if err := s.AppendEvent(ctx, &e); err != nil {
			t.Fatalf("AppendEvent %s: %v", e.Kind, err)
		}
		if e.ID == 0 || e.TS.IsZero() {
			t.Fatalf("AppendEvent %s left id=%d ts=%v", e.Kind, e.ID, e.TS)
		}
	}

	all, err := s.Events(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d events, want 3", len(all))
	}
	if all[0].Kind != KindRemoteProbe {
		t.Errorf("newest event = %q, want %q", all[0].Kind, KindRemoteProbe)
	}
	if got := all[0].Data["files"]; got != float64(12) {
		t.Errorf("event data round-tripped as %#v", got)
	}
	if all[1].Level != LevelWarn {
		t.Errorf("level = %q, want %q", all[1].Level, LevelWarn)
	}
	if all[2].Level != LevelInfo {
		t.Errorf("default level = %q, want %q", all[2].Level, LevelInfo)
	}

	warn, err := s.Events(ctx, EventFilter{Levels: []string{LevelWarn}})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(warn) != 1 || warn[0].Kind != KindTunnelDown {
		t.Errorf("level filter returned %d events", len(warn))
	}

	kinds, err := s.Events(ctx, EventFilter{Kinds: []string{KindStartup, KindRemoteProbe}})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(kinds) != 2 {
		t.Errorf("kind filter returned %d events, want 2", len(kinds))
	}
}

func TestEventLimitAndSince(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	old := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 5; i++ {
		e := Event{Kind: KindError, Message: "old", TS: old}
		if err := s.AppendEvent(ctx, &e); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	recent := Event{Kind: KindError, Message: "recent"}
	if err := s.AppendEvent(ctx, &recent); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	limited, err := s.Events(ctx, EventFilter{Limit: 2})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("limit 2 returned %d events", len(limited))
	}

	since, err := s.Events(ctx, EventFilter{Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(since) != 1 || since[0].Message != "recent" {
		t.Errorf("since filter returned %d events", len(since))
	}

	n, err := s.PruneEvents(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
	if n != 5 {
		t.Errorf("pruned %d events, want 5", n)
	}
}
