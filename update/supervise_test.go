package update

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newSupervisor puts the test binary in both places the supervisor may run one
// from, and scripts the exit statuses its children will answer with.
func newSupervisor(t *testing.T, exits string) Supervisor {
	t.Helper()

	root := t.TempDir()
	body := selfBytes(t)

	fallback := filepath.Join(root, "image", "jcc-mirror")
	if err := os.MkdirAll(filepath.Dir(fallback), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fallback, body, 0o755); err != nil {
		t.Fatal(err)
	}

	m := Manager{Dir: filepath.Join(root, "bin")}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Binary(), body, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv(envExits, exits)
	t.Setenv(envCounter, filepath.Join(root, "runs"))

	return Supervisor{
		Manager: m, Fallback: fallback, Args: []string{"child"},
		Logf: func(format string, a ...any) { t.Logf(format, a...) },
	}
}

// The whole point of the supervisor: an update that has not proved itself and
// exits non-zero is undone, and the binary that did work is started instead.
func TestSupervisorRollsBackAnUnprovenUpdate(t *testing.T) {
	sv := newSupervisor(t, "3,0")
	if err := sv.SaveState(State{State: StateApplied, ToVersion: "v9", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	code, err := sv.Run(t.Context())
	if err != nil || code != 0 {
		t.Fatalf("code = %d, err = %v", code, err)
	}

	// Nothing was kept as the previous binary, so the way back is the image's -
	// which is an empty binary directory.
	if sv.Installed() {
		t.Error("the updated binary is still in place")
	}
	st, ok, err := sv.LoadState()
	if err != nil || !ok || st.State != StateRolledBack {
		t.Fatalf("state = %+v, ok = %v, err = %v", st, ok, err)
	}
	if st.RollbackReason == "" {
		t.Error("the rollback recorded no reason, so nothing can report why")
	}
}

// A confirmed update has already run for long enough to be trusted, so a crash
// then is an ordinary crash: rolling back would hide it, and Docker's restart
// policy is what handles it.
func TestSupervisorLeavesAConfirmedUpdateAlone(t *testing.T) {
	sv := newSupervisor(t, "4")
	if err := sv.SaveState(State{State: StateConfirmed, ToVersion: "v9"}); err != nil {
		t.Fatal(err)
	}

	code, err := sv.Run(t.Context())
	if err != nil || code != 4 {
		t.Fatalf("code = %d, err = %v, want 4", code, err)
	}
	if !sv.Installed() {
		t.Error("a confirmed update was rolled back")
	}
}

// The daemon rolled itself back onto a binary it cannot name, and stepped aside
// so the supervisor could pick the right one up again.
func TestSupervisorRestartsOnTheRestartCode(t *testing.T) {
	sv := newSupervisor(t, "90,0")
	if err := os.Remove(sv.Binary()); err != nil {
		t.Fatal(err)
	}

	code, err := sv.Run(t.Context())
	if err != nil || code != 0 {
		t.Fatalf("code = %d, err = %v", code, err)
	}
	if runs := countRuns(t, os.Getenv(envCounter)); runs != 2 {
		t.Errorf("the child ran %d times, want 2", runs)
	}
}

func TestSupervisorPassesACleanExitOn(t *testing.T) {
	sv := newSupervisor(t, "0")
	code, err := sv.Run(t.Context())
	if err != nil || code != 0 {
		t.Fatalf("code = %d, err = %v", code, err)
	}
	if !sv.Installed() {
		t.Error("a clean exit rolled the update back")
	}
}

func countRuns(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the run counter: %v", err)
	}
	n := 0
	for _, b := range raw {
		if b >= '0' && b <= '9' {
			n = n*10 + int(b-'0')
		}
	}
	return n
}
