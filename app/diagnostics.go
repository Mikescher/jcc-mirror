package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/remote"
	"blackforestbytes.com/jcc-mirror/store"
)

// probeTimeout bounds every diagnostic that talks to the publisher. They are
// questions someone is waiting on with a page open, so a hung one has to come
// back as an answer rather than as a spinner.
const probeTimeout = 30 * time.Second

// throughputCap is the most a throughput probe will pull. It is a measurement,
// not a transfer: the schedule's cap applies to it like everything else, so a
// larger sample would only take longer to say the same thing.
const throughputCap = 256 << 20

// DiagnosticsView is what the settings pages' diagnostics panels are drawn
// from. The tunnel, the peers and the remotes are already in Status; what is
// added here is the state nothing else reports (DESIGN.md §4).
type DiagnosticsView struct {
	Status  Status              `json:"status"`
	Pairs   []PairDiagnostics   `json:"pairs"`
	Notify  []store.NotifyState `json:"notify"`
	NonNFC  int64               `json:"nonNfcNames"`
	DataDir string              `json:"dataDir"`

	// Log is the container's own log, which on a Synology is exactly the place an
	// operator cannot easily get to (DESIGN.md §4).
	Log []logs.Line `json:"log"`
}

// PairDiagnostics is the per-pair half: where it lands and how much room is left
// there, which is the check a plan is refused on (DESIGN.md §2.6).
type PairDiagnostics struct {
	ID        int64        `json:"id"`
	Name      string       `json:"name"`
	Type      string       `json:"type"`
	LocalPath string       `json:"localPath"`
	Space     engine.Space `json:"space"`
	SpaceErr  string       `json:"spaceError,omitempty"`
}

func (a *App) handleGetDiagnostics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	view := DiagnosticsView{Status: a.Status(ctx), DataDir: a.opts.DataDir}

	pairs, err := a.store.Pairs(ctx)
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	for _, p := range pairs {
		d := PairDiagnostics{ID: p.ID, Name: p.Name, Type: p.Type, LocalPath: p.LocalPath}
		if space, err := engine.FreeSpace(p.LocalPath); err != nil {
			d.SpaceErr = err.Error()
		} else {
			d.Space = space
		}
		view.Pairs = append(view.Pairs, d)
	}

	if view.Notify, err = a.store.NotifyStates(ctx); err != nil {
		a.log.Errorf("dashboard: %v", err)
	}

	// The non-NFC counter is the walk's own warning sign: a tree that came past
	// macOS decomposes its umlauts, and every title with one then looks new on
	// every scan (DESIGN.md §2.3).
	view.NonNFC = a.nonNFCNames()

	view.Log, _ = a.log.Tail(intParam64(r, "afterLog", 0))
	writeJSON(w, http.StatusOK, view)
}

// RemoteListing is one directory of one remote's tree, as the explorer shows it.
// Millis is on it because how long a single listing takes is what the scan
// schedule is set from.
type RemoteListing struct {
	Remote  int64          `json:"remote"`
	Name    string         `json:"name"`
	Path    string         `json:"path"`
	Target  string         `json:"target"`
	Millis  int64          `json:"millis"`
	Entries []remote.Entry `json:"entries"`
}

// handleRemoteList is the raw directory explorer. It changes nothing, but it does
// cost the publisher a round trip, which is why it lists one directory rather
// than walking.
func (a *App) handleRemoteList(w http.ResponseWriter, r *http.Request) {
	rec, client, err := a.remoteFromRequest(r.Context(), r.URL.Query().Get("remote"))
	if err != nil {
		a.fail(w, r, remoteErrorCode(err), err)
		return
	}

	dir := strings.Trim(r.URL.Query().Get("path"), "/")
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	start := time.Now()
	entries, err := client.List(ctx, dir)
	took := time.Since(start)
	if err != nil {
		a.fail(w, r, http.StatusBadGateway, err)
		return
	}

	writeJSON(w, http.StatusOK, RemoteListing{
		Remote: rec.ID, Name: rec.Name,
		Path: dir, Target: client.TargetFor(dir), Millis: took.Milliseconds(), Entries: entries,
	})
}

// PingResult is one ICMP round trip through the tunnel - the M0 check that the
// rootserver forwards between spokes, kept because it is the first thing to try
// when nothing else works (DESIGN.md §2.2).
type PingResult struct {
	Target string `json:"target"`
	Millis int64  `json:"millis"`
}

func (a *App) handlePing(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	target, err := a.pingTarget(r.Context(), fields["target"], fields["remote"])
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNoRemote) {
			code = http.StatusNotFound
		}
		a.fail(w, r, code, err)
		return
	}

	a.mu.Lock()
	tun := a.tunnel
	a.mu.Unlock()
	if tun == nil {
		a.fail(w, r, http.StatusConflict, errors.New("the tunnel is not up, so there is nothing to ping through"))
		return
	}

	rtt, err := tun.Ping(r.Context(), target, 5*time.Second)
	if err != nil {
		a.fail(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, PingResult{Target: target.String(), Millis: rtt.Milliseconds()})
}

// pingTarget resolves what to ping: whatever was asked for, or - since the one
// address worth checking is the publisher's - the host of the remote named, or
// of the first remote when none is.
func (a *App) pingTarget(ctx context.Context, want, remoteRef string) (netip.Addr, error) {
	if want = strings.TrimSpace(want); want != "" {
		addr, err := netip.ParseAddr(want)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("%q is not an IP address", want)
		}
		return addr, nil
	}

	rec, err := a.resolveRemote(ctx, remoteRef)
	if err != nil {
		return netip.Addr{}, err
	}
	host := rec.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// A hostname cannot be pinged from here: the netstack resolver is wired up for
	// the connections the mirror opens, not for ICMP, and the address that matters
	// is the publisher's.
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, errors.New("no address to ping: send one as `target`")
	}
	return addr, nil
}

// ThroughputResult is a bounded read of one remote file, timed end to end. It is
// the number that says
// whether netstack or the rootserver's uplink is the ceiling, which is the thing
// the bandwidth schedule has to respect (DESIGN.md §2.2).
type ThroughputResult struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Millis int64  `json:"millis"`
}

func (a *App) handleThroughput(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	path := strings.Trim(fields["path"], "/")
	if path == "" {
		a.fail(w, r, http.StatusBadRequest, errors.New("which file? send a remote path as `path`"))
		return
	}

	want := int64(16 << 20)
	if raw := strings.TrimSpace(fields["bytes"]); raw != "" {
		if want, err = strconv.ParseInt(raw, 10, 64); err != nil || want <= 0 {
			a.fail(w, r, http.StatusBadRequest, fmt.Errorf("bytes %q is not a size", raw))
			return
		}
	}
	if want > throughputCap {
		want = throughputCap
	}

	rec, client, err := a.remoteFromRequest(r.Context(), fields["remote"])
	if err != nil {
		a.fail(w, r, remoteErrorCode(err), err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	start := time.Now()
	body, _, err := client.OpenRange(ctx, path, 0, want)
	if err != nil {
		a.fail(w, r, http.StatusBadGateway, err)
		return
	}
	defer body.Close()

	read, err := io.Copy(io.Discard, io.LimitReader(body, want))
	took := time.Since(start)
	if err != nil {
		a.fail(w, r, http.StatusBadGateway, err)
		return
	}

	res := ThroughputResult{Path: path, Bytes: read, Millis: took.Milliseconds()}
	a.Event(r.Context(), store.LevelInfo, store.KindRemoteProbe,
		fmt.Sprintf("throughput probe: %d bytes of %s in %s", read, path, took.Round(time.Millisecond)),
		map[string]any{"remote": rec.ID, "name": rec.Name, "path": path, "bytes": read, "millis": res.Millis})
	writeJSON(w, http.StatusOK, res)
}
