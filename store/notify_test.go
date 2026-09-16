package store

import (
	"context"
	"testing"
)

func TestNotifyTransitionOnlyReportsEdges(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	// A first sighting of a condition that is false has nothing to announce: a
	// daemon that starts with everything working must be silent.
	edge, err := s.NotifyTransition(ctx, NotifyTunnelDown, 0, false, "")
	if err != nil {
		t.Fatalf("NotifyTransition: %v", err)
	}
	if edge {
		t.Fatal("a first sighting of an inactive condition was reported as an edge")
	}

	edge, err = s.NotifyTransition(ctx, NotifyTunnelDown, 0, true, "no handshake")
	if err != nil {
		t.Fatalf("NotifyTransition: %v", err)
	}
	if !edge {
		t.Fatal("the condition becoming true was not an edge")
	}

	for range 3 {
		edge, err = s.NotifyTransition(ctx, NotifyTunnelDown, 0, true, "no handshake")
		if err != nil {
			t.Fatalf("NotifyTransition: %v", err)
		}
		if edge {
			t.Fatal("a condition that stayed true was reported as an edge")
		}
	}

	edge, err = s.NotifyTransition(ctx, NotifyTunnelDown, 0, false, "the tunnel is back")
	if err != nil {
		t.Fatalf("NotifyTransition: %v", err)
	}
	if !edge {
		t.Fatal("the condition clearing was not an edge")
	}

	// What is stored is the state that is true, because the Notifications page
	// reads it back: a healthy tunnel described as down is worse than none.
	states, err := s.NotifyStates(ctx)
	if err != nil {
		t.Fatalf("NotifyStates: %v", err)
	}
	if len(states) != 1 || states[0].Active || states[0].Detail != "the tunnel is back" {
		t.Fatalf("state after clearing = %+v", states)
	}
	if states[0].NotifiedAt == nil {
		t.Error("the clearing edge did not record when it was announced")
	}
}

func TestNotifyTransitionIsPerPair(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.NotifyTransition(ctx, NotifyLockStale, 1, true, "held"); err != nil {
		t.Fatalf("NotifyTransition: %v", err)
	}
	edge, err := s.NotifyTransition(ctx, NotifyLockStale, 2, true, "held")
	if err != nil {
		t.Fatalf("NotifyTransition: %v", err)
	}
	if !edge {
		t.Fatal("a second pair's condition was swallowed by the first pair's state")
	}

	states, err := s.NotifyStates(ctx)
	if err != nil {
		t.Fatalf("NotifyStates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("got %d tracked conditions, want 2", len(states))
	}
}
