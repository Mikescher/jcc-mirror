package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func newNotifyTarget(t *testing.T, s *Store, name string, events ...string) NotifyTarget {
	t.Helper()
	target := NotifyTarget{Name: name, UserID: "7", UserKey: "secret", Enabled: true, Events: events}
	if err := s.CreateNotifyTarget(context.Background(), &target); err != nil {
		t.Fatalf("CreateNotifyTarget(%q): %v", name, err)
	}
	return target
}

func TestEveryNotifyKindHasATopic(t *testing.T) {
	kinds := []string{
		NotifySyncFailed, NotifySyncOK, NotifyDeleteBlocked, NotifySpaceLow,
		NotifyLockStale, NotifyTunnelDown, NotifyDBReplaced,
		NotifyUpdateApplied, NotifyUpdateRolledBack,
	}
	for _, kind := range kinds {
		topic, ok := NotifyTopicFor(kind)
		if !ok {
			t.Errorf("notification kind %q has no topic", kind)
			continue
		}
		if !slices.ContainsFunc(NotifyTopics, func(nt NotifyTopic) bool { return nt.Key == topic }) {
			t.Errorf("notification kind %q points at %q, which is not a topic", kind, topic)
		}
	}
	if _, ok := NotifyTopicFor("not.a.kind"); ok {
		t.Error("an unknown kind was given a topic")
	}
}

func TestNotifyTargetRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	created := newNotifyTarget(t, s, " phone ", "update", "sync_failed", "update")
	if created.ID == 0 || created.Name != "phone" || created.Sender != DefaultNotifySender || !created.UserKeySet {
		t.Fatalf("created = %+v", created)
	}
	if want := []string{"sync_failed", "update"}; !slices.Equal(created.Events, want) {
		t.Errorf("events = %v, want %v: deduped and in table order", created.Events, want)
	}

	got, err := s.NotifyTargetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("NotifyTargetByID: %v", err)
	}
	if got.UserKey != "secret" || got.UserID != "7" || !got.Enabled || !slices.Equal(got.Events, created.Events) {
		t.Errorf("read back = %+v", got)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "secret") {
		t.Errorf("the user key was marshalled: %s", raw)
	}
	if !strings.Contains(string(raw), `"userKeySet":true`) {
		t.Errorf("userKeySet missing: %s", raw)
	}

	got.Events, got.Enabled, got.Channel = nil, false, "ro"
	if err := s.UpdateNotifyTarget(ctx, &got); err != nil {
		t.Fatalf("UpdateNotifyTarget: %v", err)
	}
	again, err := s.NotifyTargetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("NotifyTargetByID: %v", err)
	}
	if again.Enabled || again.Channel != "ro" || again.Events == nil || len(again.Events) != 0 {
		t.Errorf("after update = %+v", again)
	}

	if err := s.DeleteNotifyTarget(ctx, created.ID); err != nil {
		t.Fatalf("DeleteNotifyTarget: %v", err)
	}
	if err := s.DeleteNotifyTarget(ctx, created.ID); !errors.Is(err, ErrNoNotifyTarget) {
		t.Errorf("second delete = %v, want ErrNoNotifyTarget", err)
	}
	if _, err := s.NotifyTargetByID(ctx, created.ID); !errors.Is(err, ErrNoNotifyTarget) {
		t.Errorf("read after delete = %v, want ErrNoNotifyTarget", err)
	}
	all, err := s.NotifyTargets(ctx)
	if err != nil || all == nil || len(all) != 0 {
		t.Errorf("NotifyTargets = %#v (%v), want an empty, non-nil slice", all, err)
	}
}

func TestNotifyTargetNormalizeRefuses(t *testing.T) {
	cases := map[string]NotifyTarget{
		"no name":       {UserID: "7", UserKey: "k"},
		"no user id":    {Name: "x", UserKey: "k"},
		"user id text":  {Name: "x", UserID: "u-7", UserKey: "k"},
		"no user key":   {Name: "x", UserID: "7"},
		"unknown topic": {Name: "x", UserID: "7", UserKey: "k", Events: []string{"sync_failed", "coffee"}},
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			if err := target.Normalize(); err == nil {
				t.Errorf("%+v was accepted", target)
			}
		})
	}
}

func TestNotifyTargetNamesAreUnique(t *testing.T) {
	s := newStore(t)
	newNotifyTarget(t, s, "phone")

	dup := NotifyTarget{Name: "phone", UserID: "8", UserKey: "k"}
	err := s.CreateNotifyTarget(context.Background(), &dup)
	if err == nil || !strings.Contains(err.Error(), `a notification target called "phone" already exists`) {
		t.Errorf("duplicate name = %v", err)
	}
}

func TestUpdateUnknownNotifyTarget(t *testing.T) {
	s := newStore(t)
	target := NotifyTarget{ID: 99, Name: "x", UserID: "7", UserKey: "k"}
	if err := s.UpdateNotifyTarget(context.Background(), &target); !errors.Is(err, ErrNoNotifyTarget) {
		t.Errorf("UpdateNotifyTarget = %v, want ErrNoNotifyTarget", err)
	}
}

// replayNotifyTargetsMigration puts back the settings 0009 consumes and runs its
// data statements again: on a fresh file it has already run, over nothing.
func replayNotifyTargetsMigration(t *testing.T, s *Store, settings map[string]string) {
	t.Helper()
	ctx := context.Background()
	for k, v := range settings {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO config (key, value, updated_at) VALUES (?, ?, 0)`, k, v); err != nil {
			t.Fatalf("plant %s: %v", k, err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO config_audit (ts, key, old_value, new_value, actor) VALUES (0, ?, NULL, ?, 'test')`,
			k, v); err != nil {
			t.Fatalf("plant audit of %s: %v", k, err)
		}
	}

	body, err := migrationFS.ReadFile("migrations/0009_notify_targets.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	_, data, ok := strings.Cut(string(body), ") STRICT;")
	if !ok {
		t.Fatal("the migration no longer has the marker this test splits on")
	}
	if _, err := s.db.ExecContext(ctx, data); err != nil {
		t.Fatalf("apply the migration: %v", err)
	}
}

func countNotifySettings(t *testing.T, s *Store) (config, audit int) {
	t.Helper()
	ctx := context.Background()
	const where = ` WHERE key LIKE 'notify.%' AND key <> 'notify.tunnel_grace'`
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM config`+where).Scan(&config); err != nil {
		t.Fatalf("count: %v", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM config_audit`+where).Scan(&audit); err != nil {
		t.Fatalf("count: %v", err)
	}
	return config, audit
}

func TestNotifyTargetsMigrationCarriesTheAccountOver(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	replayNotifyTargetsMigration(t, s, map[string]string{
		"notify.user_id":           " 7 ",
		"notify.user_key":          "secret",
		"notify.channel":           "mirror",
		"notify.sender":            "",
		"notify.on_sync_ok":        "true",
		"notify.on_tunnel_down":    "false",
		"notify.on_delete_blocked": "0",
		"notify.tunnel_grace":      "30m",
	})

	targets, err := s.NotifyTargets(ctx)
	if err != nil {
		t.Fatalf("NotifyTargets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(targets))
	}
	got := targets[0]
	if got.UserID != "7" || got.UserKey != "secret" || got.Channel != "mirror" ||
		got.Sender != DefaultNotifySender || !got.Enabled || got.Name == "" {
		t.Errorf("migrated target = %+v", got)
	}
	want := []string{"sync_failed", "sync_ok", "space_low", "lock_stale", "db_replaced", "update"}
	if !slices.Equal(got.Events, want) {
		t.Errorf("events = %v, want %v", got.Events, want)
	}

	if c, a := countNotifySettings(t, s); c != 0 || a != 0 {
		t.Errorf("%d settings and %d audit rows survived the migration", c, a)
	}
	if grace, err := s.ConfigGet(ctx, KeyNotifyTunnelGrace); err != nil || grace != "30m" {
		t.Errorf("tunnel grace = %q (%v), want it kept", grace, err)
	}
}

func TestNotifyTargetsMigrationSkipsAHalfAccount(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	replayNotifyTargetsMigration(t, s, map[string]string{
		"notify.user_id":    "7",
		"notify.channel":    "mirror",
		"notify.on_sync_ok": "true",
	})

	if targets, _ := s.NotifyTargets(ctx); len(targets) != 0 {
		t.Errorf("an account with no key became a target: %+v", targets)
	}
	if c, a := countNotifySettings(t, s); c != 0 || a != 0 {
		t.Errorf("%d settings and %d audit rows survived the migration", c, a)
	}
}
