package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// ExitRestart is what the daemon exits with when the binary that should run has
// changed but it cannot name the replacement - the one case being a rollback
// back onto the binary in the image, whose path only the entrypoint knows. Every
// other restart is a re-exec inside the same process (DESIGN.md §5).
const ExitRestart = 90

// Supervisor is the safety net of DESIGN.md §5: it runs the daemon as a child
// and, if an update that has not yet proved itself exits non-zero, puts the
// previous binary back and starts that instead.
//
// It exists because the one bug that is genuinely hard to recover from remotely
// is a broken updater. Everything else can be fixed from the dashboard; a binary
// that will not start cannot be.
//
// It is a Go command rather than a shell script because the runtime image is
// distroless and has no shell - and because it is the entrypoint, so it must not
// depend on anything the image might not carry.
type Supervisor struct {
	Manager

	// Fallback is the binary baked into the image, run whenever no update has
	// been installed and after a rollback that removed the managed one.
	Fallback string
	Args     []string
	Logf     func(string, ...any)
}

func (s Supervisor) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
	}
}

// Run starts the daemon and returns its exit code. It returns as soon as the
// child exits for a reason a rollback cannot fix, which is every reason but one:
// Docker's restart policy is what handles the rest.
func (s Supervisor) Run(ctx context.Context) (int, error) {
	if s.Fallback == "" {
		return 1, errors.New("supervisor has no binary to fall back to")
	}

	for {
		target, managed := s.Fallback, false
		if s.Installed() {
			target, managed = s.Binary(), true
		}

		code, err := s.runOnce(ctx, target)
		switch {
		case err != nil && !managed:
			return 1, err
		case err != nil:
			// A managed binary that will not even start is the same failure as one
			// that starts and exits immediately, and it gets the same answer -
			// including when the update was already confirmed, because a binary
			// that cannot be executed will not become executable on the next try.
			s.logf("supervisor: %v", err)
			if !s.rollback(fmt.Sprintf("the updated binary could not be started: %v", err)) {
				return 1, err
			}
			continue
		case code == ExitRestart && ctx.Err() == nil:
			// The daemon rolled itself back onto the image's binary and stepped
			// aside so this loop could pick it up again.
			s.logf("supervisor: the daemon asked to be restarted")
			continue
		case code == 0 || ctx.Err() != nil:
			return code, nil
		}

		st, ok, stErr := s.LoadState()
		if stErr != nil {
			s.logf("supervisor: %v", stErr)
		}
		// Only an update that has not yet confirmed itself is treated as the
		// cause. A confirmed one has already run for long enough to be trusted, so
		// a crash then is an ordinary crash and rolling back would hide it.
		if !ok || st.State != StateApplied || !s.rollback(
			fmt.Sprintf("the updated binary exited with status %d before it had run long enough to be trusted", code)) {
			return code, nil
		}
	}
}

// rollback puts the working binary back and says whether it managed to, so the
// caller can stop rather than loop when the recovery path is itself broken.
func (s Supervisor) rollback(reason string) bool {
	s.logf("supervisor: %s", reason)

	res, err := s.Rollback(reason)
	if err != nil {
		s.logf("supervisor: rollback failed: %v", err)
		return false
	}
	if res.Exec == "" {
		s.logf("supervisor: rolled back to the binary in the image")
	} else {
		s.logf("supervisor: rolled back to %s", res.Exec)
	}
	return true
}

// runOnce runs one child to completion, relaying the signals a container stop
// sends so the daemon still gets to shut down cleanly.
func (s Supervisor) runOnce(ctx context.Context, target string) (int, error) {
	cmd := exec.Command(target, s.Args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("start %s: %w", target, err)
	}
	s.logf("supervisor: running %s %v (pid %d)", target, s.Args, cmd.Process.Pid)

	// Signals are relayed rather than acted on: the supervisor has nothing to
	// clean up, and the daemon's own handler is what writes the shutdown record.
	// The self-update re-execs the child in place, so the PID being signalled
	// stays the right one across an update.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)

	done := make(chan struct{})
	defer close(done)
	go func() {
		// stop is cleared once it has fired: a closed channel is always ready, and
		// left in the select it would spin sending SIGTERM.
		stop := ctx.Done()
		for {
			select {
			case <-done:
				return
			case <-stop:
				stop = nil
				_ = cmd.Process.Signal(syscall.SIGTERM)
			case sig := <-signals:
				_ = cmd.Process.Signal(sig)
			}
		}
	}()

	err := cmd.Wait()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), nil
	default:
		return 1, fmt.Errorf("wait for %s: %w", target, err)
	}
}

// Exec replaces this process with path, keeping the PID, the container and every
// open listener's address. That is the whole trick of DESIGN.md §5: there is no
// Docker restart, and to anything watching the container nothing happened.
//
// It never returns on success, so everything that has to be closed must be
// closed before it is called.
func Exec(path string, args []string) error {
	argv := append([]string{path}, args...)
	if err := syscall.Exec(path, argv, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", path, err)
	}
	return nil
}
