package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func newRemote(t *testing.T, s *Store, name string) Remote {
	t.Helper()
	r := Remote{Name: name, Host: "10.13.13.2", Share: name, User: "ro", Password: "hunter2"}
	if err := s.CreateRemote(context.Background(), &r); err != nil {
		t.Fatalf("CreateRemote(%q): %v", name, err)
	}
	return r
}

func TestRemoteRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	r := Remote{
		Name: " nas ", Host: " 10.13.13.2:4455 ", Share: "media", Path: "/Filme/2024/",
		User: "ro", Password: "hunter2", Domain: "WORKGROUP",
	}
	if err := s.CreateRemote(ctx, &r); err != nil {
		t.Fatalf("CreateRemote: %v", err)
	}
	if r.ID == 0 || r.CreatedAt.IsZero() {
		t.Fatalf("CreateRemote left id=%d created=%v", r.ID, r.CreatedAt)
	}

	got, err := s.RemoteByID(ctx, r.ID)
	if err != nil {
		t.Fatalf("RemoteByID: %v", err)
	}
	if got.Name != "nas" || got.Host != "10.13.13.2:4455" || got.Path != "Filme/2024" ||
		got.Password != "hunter2" || !got.PasswordSet || got.Domain != "WORKGROUP" {
		t.Errorf("round trip = %+v", got)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Errorf("the password is in the JSON: %s", raw)
	}
}

func TestRemoteNormalizeRefuses(t *testing.T) {
	valid := Remote{Name: "nas", Host: "10.13.13.2", Share: "media", User: "ro"}
	cases := map[string]func(*Remote){
		"no name":         func(r *Remote) { r.Name = " " },
		"no host":         func(r *Remote) { r.Host = "" },
		"host is a path":  func(r *Remote) { r.Host = "10.13.13.2/media" },
		"host bad port":   func(r *Remote) { r.Host = "10.13.13.2:0" },
		"no share":        func(r *Remote) { r.Share = "" },
		"share is a path": func(r *Remote) { r.Share = "media/Filme" },
		"path climbs":     func(r *Remote) { r.Path = "../elsewhere" },
		"no user":         func(r *Remote) { r.User = "" },
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			r := valid
			spoil(&r)
			if err := r.Normalize(); err == nil {
				t.Errorf("Normalize accepted %+v", r)
			}
		})
	}
}

func TestRemoteNamesAreUnique(t *testing.T) {
	s := newStore(t)
	newRemote(t, s, "nas")

	dup := Remote{Name: "nas", Host: "10.13.13.9", Share: "other", User: "ro"}
	err := s.CreateRemote(context.Background(), &dup)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second remote called nas: %v", err)
	}
}

func TestUpdateRemote(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	r := newRemote(t, s, "nas")

	r.Host, r.Password = "10.13.13.5", ""
	if err := s.UpdateRemote(ctx, &r); err != nil {
		t.Fatalf("UpdateRemote: %v", err)
	}
	got, err := s.RemoteByID(ctx, r.ID)
	if err != nil {
		t.Fatalf("RemoteByID: %v", err)
	}
	if got.Host != "10.13.13.5" || got.PasswordSet {
		t.Errorf("after update = %+v", got)
	}

	ghost := Remote{ID: 999, Name: "ghost", Host: "h", Share: "s", User: "u"}
	if err := s.UpdateRemote(ctx, &ghost); !errors.Is(err, ErrNoRemote) {
		t.Errorf("UpdateRemote of a ghost = %v, want ErrNoRemote", err)
	}
}

func TestFindAndResolveRemote(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.ResolveRemote(ctx, ""); !errors.Is(err, ErrNoRemote) {
		t.Errorf("ResolveRemote with none = %v, want ErrNoRemote", err)
	}

	first := newRemote(t, s, "nas")
	second := newRemote(t, s, "archive")

	if r, err := s.ResolveRemote(ctx, ""); err != nil || r.ID != first.ID {
		t.Errorf("ResolveRemote(\"\") = %+v, %v; want the first remote", r, err)
	}
	if r, err := s.FindRemote(ctx, "archive"); err != nil || r.ID != second.ID {
		t.Errorf("FindRemote by name = %+v, %v", r, err)
	}
	if r, err := s.FindRemote(ctx, "1"); err != nil || r.ID != first.ID {
		t.Errorf("FindRemote by id = %+v, %v", r, err)
	}
	if _, err := s.FindRemote(ctx, "nowhere"); !errors.Is(err, ErrNoRemote) {
		t.Errorf("FindRemote of a ghost = %v, want ErrNoRemote", err)
	}

	all, err := s.Remotes(ctx)
	if err != nil || len(all) != 2 || all[0].ID != first.ID {
		t.Errorf("Remotes = %+v, %v", all, err)
	}
}

func TestPairReadsFromARemote(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	r := newRemote(t, s, "nas")

	p := Pair{Name: "media", RemoteID: r.ID, LocalPath: "/data/media"}
	if err := s.CreatePair(ctx, &p); err != nil {
		t.Fatalf("CreatePair: %v", err)
	}
	got, err := s.PairByID(ctx, p.ID)
	if err != nil || got.RemoteID != r.ID {
		t.Fatalf("PairByID = %+v, %v; want remote %d", got, err, r.ID)
	}

	got.RemoteID = 0
	if err := s.UpdatePair(ctx, &got); err != nil {
		t.Fatalf("UpdatePair to no remote: %v", err)
	}
	if again, _ := s.PairByID(ctx, p.ID); again.RemoteID != 0 {
		t.Errorf("remote after clearing = %d, want 0", again.RemoteID)
	}

	ghost := Pair{Name: "ghost", RemoteID: 999, LocalPath: "/data/ghost"}
	if err := s.CreatePair(ctx, &ghost); !errors.Is(err, ErrNoRemote) {
		t.Errorf("CreatePair on a missing remote = %v, want ErrNoRemote", err)
	}
}

func TestDeleteRemoteInUseIsRefused(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	r := newRemote(t, s, "nas")

	p := Pair{Name: "media", RemoteID: r.ID, LocalPath: "/data/media"}
	if err := s.CreatePair(ctx, &p); err != nil {
		t.Fatalf("CreatePair: %v", err)
	}

	err := s.DeleteRemote(ctx, r.ID)
	if !errors.Is(err, ErrRemoteInUse) || !strings.Contains(err.Error(), `"media"`) {
		t.Fatalf("DeleteRemote in use = %v, want ErrRemoteInUse naming the pair", err)
	}

	if err := s.DeletePair(ctx, p.ID); err != nil {
		t.Fatalf("DeletePair: %v", err)
	}
	if err := s.DeleteRemote(ctx, r.ID); err != nil {
		t.Fatalf("DeleteRemote once unused: %v", err)
	}
	if err := s.DeleteRemote(ctx, r.ID); !errors.Is(err, ErrNoRemote) {
		t.Errorf("second DeleteRemote = %v, want ErrNoRemote", err)
	}
}

// replayRemotesMigration puts back the settings 0008 consumes and runs its
// statements again: on a fresh file it has already run, over nothing.
func replayRemotesMigration(t *testing.T, s *Store, settings map[string]string) {
	t.Helper()
	ctx := context.Background()
	for k, v := range settings {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO config (key, value, updated_at) VALUES (?, ?, 0)`, k, v); err != nil {
			t.Fatalf("plant %s: %v", k, err)
		}
	}

	body, err := migrationFS.ReadFile("migrations/0008_remotes.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	// Only the data half: the table and the column already exist.
	_, data, ok := strings.Cut(string(body), "CREATE INDEX pairs_remote ON pairs (remote_id);")
	if !ok {
		t.Fatal("the migration no longer has the marker this test splits on")
	}
	if _, err := s.db.ExecContext(ctx, data); err != nil {
		t.Fatalf("apply the migration: %v", err)
	}
}

func TestRemotesMigrationCarriesTheSettingsOver(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	replayRemotesMigration(t, s, map[string]string{
		"remote.host":     "10.13.13.2",
		"remote.share":    "media",
		"remote.path":     "Filme",
		"remote.user":     "ro",
		"remote.password": "hunter2",
		"remote.domain":   "",
	})

	remotes, err := s.Remotes(ctx)
	if err != nil {
		t.Fatalf("Remotes: %v", err)
	}
	if len(remotes) != 1 {
		t.Fatalf("got %d remotes, want 1", len(remotes))
	}
	r := remotes[0]
	if r.Name != "media" || r.Host != "10.13.13.2" || r.Share != "media" || r.Path != "Filme" ||
		r.User != "ro" || r.Password != "hunter2" {
		t.Errorf("migrated remote = %+v", r)
	}

	got, err := s.PairByID(ctx, p.ID)
	if err != nil || got.RemoteID != r.ID {
		t.Errorf("existing pair reads from %d, want %d (%v)", got.RemoteID, r.ID, err)
	}

	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM config WHERE key LIKE 'remote.%'`).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 0 {
		t.Errorf("%d remote.* settings survived the migration", left)
	}
}

func TestRemotesMigrationLeavesATrailForAHalfRemote(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	replayRemotesMigration(t, s, map[string]string{
		"remote.host":     "10.13.13.2",
		"remote.password": "hunter2",
	})

	if remotes, _ := s.Remotes(ctx); len(remotes) != 0 {
		t.Errorf("a remote with no share became a row: %+v", remotes)
	}
	events, err := s.Events(ctx, EventFilter{Kinds: []string{KindConfigChanged}})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 || !strings.Contains(events[0].Message, "10.13.13.2") {
		t.Fatalf("the half remote was dropped without a trail: %+v", events)
	}
	if strings.Contains(events[0].Message, "hunter2") {
		t.Errorf("the trail carries the password: %s", events[0].Message)
	}
}
