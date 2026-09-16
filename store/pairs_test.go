package store

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// newPair creates the minimal valid pair the manifest, file and job tests hang
// their rows off.
func newPair(t *testing.T, s *Store, name string) Pair {
	t.Helper()
	p := Pair{Name: name, LocalPath: "/data/" + name}
	if err := s.CreatePair(context.Background(), &p); err != nil {
		t.Fatalf("CreatePair(%q): %v", name, err)
	}
	return p
}

func countRows(t *testing.T, s *Store, table string, pairID int64) int64 {
	t.Helper()
	var n int64
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT count(*) FROM `+table+` WHERE pair_id = ?`, pairID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func countAll(t *testing.T, s *Store, table string) int64 {
	t.Helper()
	var n int64
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestPairNormalizeFillsDefaults(t *testing.T) {
	p := Pair{
		Name:       "  media  ",
		RemotePath: " /Movies/2024/ ",
		LocalPath:  "/data/media/",
		Includes:   []string{" *.mkv ", "", "   "},
		Excludes:   []string{"/sample/"},
	}
	if err := p.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	if p.Name != "media" {
		t.Errorf("name = %q, want %q", p.Name, "media")
	}
	if p.Type != PairRaw {
		t.Errorf("type = %q, want %q", p.Type, PairRaw)
	}
	if p.Mode != ModeAdditive {
		t.Errorf("mode = %q, want %q", p.Mode, ModeAdditive)
	}
	if p.Priority != 100 {
		t.Errorf("priority = %d, want 100", p.Priority)
	}
	if p.RemotePath != "Movies/2024" {
		t.Errorf("remote path = %q, want %q", p.RemotePath, "Movies/2024")
	}
	if p.LocalPath != "/data/media" {
		t.Errorf("local path = %q, want %q", p.LocalPath, "/data/media")
	}
	if want := []string{"*.mkv"}; !slices.Equal(p.Includes, want) {
		t.Errorf("includes = %q, want %q", p.Includes, want)
	}
	if want := []string{"sample"}; !slices.Equal(p.Excludes, want) {
		t.Errorf("excludes = %q, want %q", p.Excludes, want)
	}
}

func TestPairNormalizeKeepsExplicitValues(t *testing.T) {
	p := Pair{
		Name:        "jcc",
		Type:        PairJCC,
		Mode:        ModeMirror,
		LocalPath:   "/data/jcc",
		Priority:    1,
		DeleteGuard: 25,
	}
	if err := p.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Type != PairJCC || p.Mode != ModeMirror {
		t.Errorf("type/mode = %q/%q, want %q/%q", p.Type, p.Mode, PairJCC, ModeMirror)
	}
	if p.Priority != 1 || p.DeleteGuard != 25 {
		t.Errorf("priority/guard = %d/%d, want 1/25", p.Priority, p.DeleteGuard)
	}
	if p.RemotePath != "" {
		t.Errorf("empty remote path became %q, want the remote root", p.RemotePath)
	}
}

func TestPairNormalizeRefuses(t *testing.T) {
	cases := map[string]Pair{
		"no name":             {LocalPath: "/data/x"},
		"blank name":          {Name: "   ", LocalPath: "/data/x"},
		"no local path":       {Name: "x"},
		"dot local path":      {Name: "x", LocalPath: "."},
		"relative local path": {Name: "x", LocalPath: "data/x"},
		"bad type":            {Name: "x", LocalPath: "/data/x", Type: "raws"},
		"bad mode":            {Name: "x", LocalPath: "/data/x", Mode: "read-only"},
		"negative guard":      {Name: "x", LocalPath: "/data/x", DeleteGuard: -1},
		"owner name":          {Name: "x", LocalPath: "/data/x", Owner: "admin:users"},
		"owner without gid":   {Name: "x", LocalPath: "/data/x", Owner: "1026"},
		"negative owner":      {Name: "x", LocalPath: "/data/x", Owner: "-1:100"},
		"mode not octal":      {Name: "x", LocalPath: "/data/x", FileMode: "0888"},
		"mode with setuid":    {Name: "x", LocalPath: "/data/x", DirMode: "4755"},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if err := p.Normalize(); err == nil {
				t.Fatalf("Normalize(%+v) succeeded, want an error", p)
			}
		})
	}
}

func TestPairRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	p := Pair{
		Name:        "media",
		Type:        PairJCC,
		RemotePath:  "Movies",
		LocalPath:   "/data/media",
		Mode:        ModeMirror,
		Includes:    []string{"*.mkv", "*.mp4"},
		Excludes:    []string{"sample", "@eaDir"},
		DeleteGuard: 7,
		Priority:    3,
		Enabled:     true,
	}
	if err := s.CreatePair(ctx, &p); err != nil {
		t.Fatalf("CreatePair: %v", err)
	}
	if p.ID == 0 || p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Fatalf("CreatePair left id=%d created=%v", p.ID, p.CreatedAt)
	}

	got, err := s.PairByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("PairByID: %v", err)
	}
	if got.Name != p.Name || got.Type != p.Type || got.RemotePath != p.RemotePath ||
		got.LocalPath != p.LocalPath || got.Mode != p.Mode {
		t.Errorf("read back %+v, want %+v", got, p)
	}
	if !slices.Equal(got.Includes, p.Includes) {
		t.Errorf("includes = %q, want %q", got.Includes, p.Includes)
	}
	if !slices.Equal(got.Excludes, p.Excludes) {
		t.Errorf("excludes = %q, want %q", got.Excludes, p.Excludes)
	}
	if got.DeleteGuard != 7 || got.Priority != 3 || !got.Enabled {
		t.Errorf("guard/priority/enabled = %d/%d/%v, want 7/3/true", got.DeleteGuard, got.Priority, got.Enabled)
	}
	if got.CreatedAt.UnixMilli() != p.CreatedAt.UnixMilli() {
		t.Errorf("created at %v, want %v", got.CreatedAt, p.CreatedAt)
	}

	all, err := s.Pairs(ctx)
	if err != nil {
		t.Fatalf("Pairs: %v", err)
	}
	if len(all) != 1 || all[0].ID != p.ID {
		t.Fatalf("Pairs returned %d rows", len(all))
	}
}

// A pair without patterns stores '[]', never NULL: a stored "null" would decode
// back to a nil slice everywhere the pair is read.
func TestPairWithoutPatterns(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	got, err := s.PairByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("PairByID: %v", err)
	}
	if got.Includes == nil || len(got.Includes) != 0 {
		t.Errorf("includes = %#v, want an empty non-nil slice", got.Includes)
	}
	if got.Excludes == nil || len(got.Excludes) != 0 {
		t.Errorf("excludes = %#v, want an empty non-nil slice", got.Excludes)
	}
}

func TestPairsRunInPriorityOrder(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	for _, c := range []struct {
		name     string
		priority int
	}{{"media", 50}, {"jcc", 1}, {"programs", 10}} {
		p := Pair{Name: c.name, LocalPath: "/data/" + c.name, Priority: c.priority}
		if err := s.CreatePair(ctx, &p); err != nil {
			t.Fatalf("CreatePair(%q): %v", c.name, err)
		}
	}

	all, err := s.Pairs(ctx)
	if err != nil {
		t.Fatalf("Pairs: %v", err)
	}
	var names []string
	for _, p := range all {
		names = append(names, p.Name)
	}
	if want := []string{"jcc", "programs", "media"}; !slices.Equal(names, want) {
		t.Errorf("order = %q, want %q", names, want)
	}
}

func TestFindPair(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	media := newPair(t, s, "media")
	// A pair whose name is a number: the name has to win, or it becomes
	// unreachable by the only handle its operator knows.
	numeric := newPair(t, s, "1")

	cases := map[string]int64{
		"media":  media.ID,
		" media": media.ID,
		"1":      numeric.ID,
	}
	for ref, want := range cases {
		got, err := s.FindPair(ctx, ref)
		if err != nil {
			t.Errorf("FindPair(%q): %v", ref, err)
			continue
		}
		if got.ID != want {
			t.Errorf("FindPair(%q) = pair %d (%q), want %d", ref, got.ID, got.Name, want)
		}
	}

	byID, err := s.FindPair(ctx, "2")
	if err != nil {
		t.Fatalf("FindPair by id: %v", err)
	}
	if byID.ID != numeric.ID {
		t.Errorf("FindPair(\"2\") = pair %d, want %d", byID.ID, numeric.ID)
	}

	for _, ref := range []string{"", "   ", "nope", "999"} {
		if _, err := s.FindPair(ctx, ref); !errors.Is(err, ErrNoPair) {
			t.Errorf("FindPair(%q) error = %v, want ErrNoPair", ref, err)
		}
	}
}

func TestUpdatePair(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	p.Name = "movies"
	p.Mode = ModeMirror
	p.Excludes = []string{"@eaDir"}
	p.Priority = 5
	p.Enabled = true
	if err := s.UpdatePair(ctx, &p); err != nil {
		t.Fatalf("UpdatePair: %v", err)
	}

	got, err := s.PairByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("PairByID: %v", err)
	}
	if got.Name != "movies" || got.Mode != ModeMirror || got.Priority != 5 || !got.Enabled {
		t.Errorf("read back %+v after the update", got)
	}
	if want := []string{"@eaDir"}; !slices.Equal(got.Excludes, want) {
		t.Errorf("excludes = %q, want %q", got.Excludes, want)
	}
	if got.UpdatedAt.Before(got.CreatedAt) {
		t.Errorf("updated_at %v is before created_at %v", got.UpdatedAt, got.CreatedAt)
	}
}

func TestUnknownPairIsAnError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.PairByID(ctx, 42); !errors.Is(err, ErrNoPair) {
		t.Errorf("PairByID error = %v, want ErrNoPair", err)
	}
	ghost := Pair{ID: 42, Name: "ghost", LocalPath: "/data/ghost"}
	if err := s.UpdatePair(ctx, &ghost); !errors.Is(err, ErrNoPair) {
		t.Errorf("UpdatePair error = %v, want ErrNoPair", err)
	}
	if err := s.DeletePair(ctx, 42); !errors.Is(err, ErrNoPair) {
		t.Errorf("DeletePair error = %v, want ErrNoPair", err)
	}
}

func TestDeletePairCascades(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	sc, _, err := s.BeginScan(ctx, p.ID, false)
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}
	if err := s.PutListing(ctx, p.ID, sc.ID, "", []ManifestEntry{{Path: "a.mkv", Size: 10}}); err != nil {
		t.Fatalf("PutListing: %v", err)
	}
	if err := s.PutFile(ctx, File{PairID: p.ID, Path: "a.mkv", Size: 10}); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if _, err := s.EnqueueJob(ctx, Job{PairID: p.ID, Path: "a.mkv", Op: OpAdd, BytesTotal: 10}); err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	change := Change{PairID: p.ID, Path: "a.mkv", Op: ChangeAdd}
	if err := s.AppendChange(ctx, &change); err != nil {
		t.Fatalf("AppendChange: %v", err)
	}

	if err := s.DeletePair(ctx, p.ID); err != nil {
		t.Fatalf("DeletePair: %v", err)
	}

	for _, table := range []string{"manifest", "files", "jobs", "scans"} {
		if n := countRows(t, s, table, p.ID); n != 0 {
			t.Errorf("%s still holds %d rows of the deleted pair", table, n)
		}
	}
	// changes is ON DELETE SET NULL rather than CASCADE: the history of what was
	// transferred outlives the pair it was transferred for.
	if n := countAll(t, s, "changes"); n != 1 {
		t.Errorf("changes holds %d rows, want the history to survive", n)
	}
}

func TestPairNormalizeOwnership(t *testing.T) {
	p := Pair{Name: "x", LocalPath: "/data/x", Owner: " 1026 : 100 ", FileMode: "0o0", DirMode: "770"}
	if err := p.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Owner != "1026:100" || p.FileMode != "0000" || p.DirMode != "0770" {
		t.Errorf("got owner %q, file mode %q, dir mode %q; want 1026:100, 0000, 0770", p.Owner, p.FileMode, p.DirMode)
	}
	if uid, gid, ok := p.OwnerIDs(); !ok || uid != 1026 || gid != 100 {
		t.Errorf("OwnerIDs = %d, %d, %v", uid, gid, ok)
	}
	if m, ok := p.FileModeBits(); !ok || m != 0 {
		t.Errorf("FileModeBits = %o, %v; want 0, true", m, ok)
	}

	var unset Pair
	if _, _, ok := unset.OwnerIDs(); ok {
		t.Error("an empty owner reports one")
	}
	if _, ok := unset.DirModeBits(); ok {
		t.Error("an empty dir mode reports one")
	}
}
