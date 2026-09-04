package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The names inside the binary directory. They are fixed rather than random
// because the supervisor has to find them again in a process that shares nothing
// with the one that wrote them.
const (
	binaryName   = "jcc-mirror"
	previousName = "jcc-mirror.prev"
	incomingName = "jcc-mirror.new"
	stateName    = "update.json"
)

// Where an update stands. The distinction that matters is applied vs confirmed:
// an applied update has been installed but not yet proven, and that is precisely
// the state the supervisor rolls back out of (DESIGN.md §5).
const (
	StateApplied    = "applied"
	StateConfirmed  = "confirmed"
	StateRolledBack = "rolledback"
)

// State is what one process leaves for the next one to read. It lives in the
// data volume as JSON rather than in sqlite: the supervisor reads it while the
// daemon is dead, and a three-line supervisor should not have to open a
// database.
type State struct {
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updatedAt"`
	Source    string    `json:"source,omitempty"`

	// RemoteModTime is the Last-Modified of the binary that was installed. It is
	// the baseline every later check compares against, and it has to be: the
	// compiled-in build stamp is when the binary was built, the share's timestamp
	// is when it was uploaded, and the second is always later than the first - so
	// comparing against the build stamp alone would find the running binary
	// newer than itself, forever.
	RemoteModTime time.Time `json:"remoteModTime"`
	RemoteSize    int64     `json:"remoteSize,omitempty"`

	FromVersion string `json:"fromVersion,omitempty"`
	ToVersion   string `json:"toVersion,omitempty"`
	ToBuild     string `json:"toBuild,omitempty"`

	RolledBackAt   *time.Time `json:"rolledBackAt,omitempty"`
	RollbackReason string     `json:"rollbackReason,omitempty"`

	// Reported is set once the daemon has told someone about a rollback. The
	// supervisor cannot send a notification - it has no configuration and no
	// database - so it writes the fact down and the restarted daemon reports it.
	Reported bool `json:"reported,omitempty"`
}

// Manager owns the binary directory: the binary in use, the one it replaced, a
// download in progress, and the state file that ties them together.
//
// An absent managed binary is not an error state. It is the normal one on a
// fresh install: the container runs the binary baked into the image, and the
// directory only comes into existence the first time an update is applied.
type Manager struct{ Dir string }

// NewManager places the binary directory inside the data volume, which is the
// only writable thing an unprivileged container has - and the image's own
// /usr/local/bin is read-only for exactly the reason that makes updating in
// place impossible without it.
func NewManager(dataDir string) Manager { return Manager{Dir: filepath.Join(dataDir, "bin")} }

func (m Manager) Binary() string    { return filepath.Join(m.Dir, binaryName) }
func (m Manager) Previous() string  { return filepath.Join(m.Dir, previousName) }
func (m Manager) Incoming() string  { return filepath.Join(m.Dir, incomingName) }
func (m Manager) StatePath() string { return filepath.Join(m.Dir, stateName) }

// Installed reports whether an updated binary is in place, which is what decides
// whether the supervisor runs it or the image's.
func (m Manager) Installed() bool { return executable(m.Binary()) }

// HasPrevious reports whether there is something to roll back to. False does not
// mean a rollback is impossible: removing the managed binary falls back to the
// image's, which is the older one by definition on a first update.
func (m Manager) HasPrevious() bool { return executable(m.Previous()) }

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// LoadState reads the state file. A missing one is the fresh-install case and
// answers false rather than an error; a corrupt one is an error, because
// silently treating it as absent would lose a pending rollback.
func (m Manager) LoadState() (State, bool, error) {
	raw, err := os.ReadFile(m.StatePath())
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("read %q: %w", m.StatePath(), err)
	}

	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, false, fmt.Errorf("read %q: %w", m.StatePath(), err)
	}
	return st, true, nil
}

// SaveState writes the state file through a temporary one: the supervisor may
// read it at any moment, and half a JSON object is a state it cannot act on.
func (m Manager) SaveState(st State) error {
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return fmt.Errorf("create %q: %w", m.Dir, err)
	}

	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.StatePath() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, m.StatePath()); err != nil {
		return fmt.Errorf("write %q: %w", m.StatePath(), err)
	}
	return nil
}

// Baseline is the timestamp a candidate has to beat. It is the later of the
// running build's own stamp and the Last-Modified of whatever was installed, so
// a binary uploaded five minutes after it was built does not read as an update
// to itself on every check.
func (m Manager) Baseline(build time.Time) time.Time {
	st, ok, err := m.LoadState()
	if err != nil || !ok {
		return build
	}
	// After a rollback the running binary is the previous one again, so the only
	// meaningful baseline is its own build stamp.
	if st.State == StateRolledBack {
		return build
	}
	if st.RemoteModTime.After(build) {
		return st.RemoteModTime
	}
	return build
}

// Blocked names an update that was installed and rolled back, so it is not tried
// again unattended. Without it a binary that crashes on startup would be
// downloaded, installed, rolled back and downloaded again on every check - the
// exact loop a self-updater must not be able to get into.
func (m Manager) Blocked() (State, bool) {
	st, ok, err := m.LoadState()
	if err != nil || !ok || st.State != StateRolledBack {
		return State{}, false
	}
	return st, true
}

// Result is what an applied update leaves for the caller to report and act on.
type Result struct {
	State State
	// Exec is the binary to re-exec into. It is always the managed path, which
	// is what the supervisor runs too - so the running process and a restarted
	// container agree on which binary is current.
	Exec string
}

// Apply performs DESIGN.md §5 steps 2, 3 and 5: download, prove the file is
// plausible, keep the current binary as the previous one and rename the new one
// into place. It deliberately does not re-exec - quiescing and stopping belong
// to the daemon, which is the only thing that knows what is in flight.
func (m Manager) Apply(ctx context.Context, src Source, rel Release, from string) (Result, error) {
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create %q: %w", m.Dir, err)
	}

	// A leftover from an interrupted attempt is removed rather than resumed: the
	// binary is small enough that a fresh download costs nothing, and half of one
	// is exactly what the checks below exist to reject.
	_ = os.Remove(m.Incoming())

	size, err := src.Download(ctx, m.Incoming())
	if err != nil {
		_ = os.Remove(m.Incoming())
		return Result{}, err
	}
	if err := Verify(m.Incoming(), rel.Size); err != nil {
		_ = os.Remove(m.Incoming())
		return Result{}, err
	}

	version, build, err := SmokeTest(ctx, m.Incoming())
	if err != nil {
		_ = os.Remove(m.Incoming())
		return Result{}, err
	}

	// The rotation, in the order that survives being interrupted between the two
	// renames: the current binary is moved aside first, so the worst intermediate
	// state is "no managed binary", which falls back to the image's rather than
	// to nothing.
	switch {
	case m.Installed():
		if err := os.Rename(m.Binary(), m.Previous()); err != nil {
			return Result{}, fmt.Errorf("keep the current binary: %w", err)
		}
	default:
		// Nothing managed is in place, so the binary being replaced is the image's
		// and the way back to it is an empty directory. A stale .prev from an
		// earlier update would otherwise be restored over it.
		_ = os.Remove(m.Previous())
	}

	if err := os.Rename(m.Incoming(), m.Binary()); err != nil {
		return Result{}, fmt.Errorf("install the new binary: %w", err)
	}

	st := State{
		State:         StateApplied,
		UpdatedAt:     time.Now().UTC(),
		Source:        rel.URL,
		RemoteModTime: rel.ModTime,
		RemoteSize:    size,
		FromVersion:   from,
		ToVersion:     version,
		ToBuild:       build,
	}
	if err := m.SaveState(st); err != nil {
		return Result{}, err
	}
	return Result{State: st, Exec: m.Binary()}, nil
}

// Rollback puts the previous binary back, or - when the update replaced the
// image's binary rather than an earlier update - removes the managed one so the
// image's is what runs again. It is what the supervisor calls, and what the
// dashboard's rollback button calls.
func (m Manager) Rollback(reason string) (Result, error) {
	st, _, err := m.LoadState()
	if err != nil {
		// A state file that will not parse must not stop a rollback: the rollback
		// is the recovery path, and it is needed most when things are broken.
		st = State{}
	}

	switch {
	case m.HasPrevious():
		if err := os.Rename(m.Previous(), m.Binary()); err != nil {
			return Result{}, fmt.Errorf("restore the previous binary: %w", err)
		}
	case m.Installed():
		if err := os.Remove(m.Binary()); err != nil {
			return Result{}, fmt.Errorf("remove the updated binary: %w", err)
		}
	default:
		return Result{}, errors.New("there is no updated binary to roll back")
	}

	now := time.Now().UTC()
	st.State, st.RolledBackAt, st.RollbackReason, st.Reported = StateRolledBack, &now, reason, false
	if err := m.SaveState(st); err != nil {
		return Result{}, err
	}

	exec := m.Binary()
	if !m.Installed() {
		exec = ""
	}
	return Result{State: st, Exec: exec}, nil
}

// Confirm marks an applied update as one that ran. Until it is called the
// supervisor treats a non-zero exit as the update's fault and puts the previous
// binary back; afterwards a crash is an ordinary crash and Docker's restart
// policy handles it (DESIGN.md §5).
//
// It confirms the update it was given and no other: a process that installs a
// second update just before its own minute is up must not sign off on the one it
// has never run.
func (m Manager) Confirm(applied State) (bool, error) {
	st, ok, err := m.LoadState()
	if err != nil || !ok || st.State != StateApplied || !st.UpdatedAt.Equal(applied.UpdatedAt) {
		return false, err
	}
	st.State = StateConfirmed
	return true, m.SaveState(st)
}

// MarkReported records that the rollback has been announced, so the daemon says
// it once rather than on every restart.
func (m Manager) MarkReported() error {
	st, ok, err := m.LoadState()
	if err != nil || !ok || st.Reported {
		return err
	}
	st.Reported = true
	return m.SaveState(st)
}
