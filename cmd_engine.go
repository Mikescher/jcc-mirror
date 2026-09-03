package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/schedule"
	"blackforestbytes.com/jcc-mirror/store"
)

// The plumbing the six mirror commands share. They all work on the same state the
// daemon does - the pairs, the manifest, the job queue - so they all need -data,
// and with -remote-dir they all run against a local directory instead of the
// publisher's share (DESIGN.md §2.3, §2.4).

// openStore opens the state the daemon runs on. sqlite in WAL mode takes a second
// connection from another process in its stride, so this is safe to run against a
// live daemon.
func (cfg *config) openStore(ctx context.Context) (*store.Store, error) {
	if cfg.dataDir == "" {
		return nil, fmt.Errorf("missing -data: the pairs, the manifest and the job queue all live in the store, e.g. -data %s", staticDataDir)
	}
	return store.Open(ctx, cfg.dataDir)
}

// openEngine builds the engine on the stored settings. The close function closes
// the store and the remote, tunnel included, and is safe to defer.
//
// Every mirror command opens the remote, including the two that never send a
// request over it: engine.New needs one to exist. -remote-dir is the cheap way to
// run those without a tunnel.
func (cfg *config) openEngine(ctx context.Context, logger *logs.Logger) (*engine.Engine, *store.Store, func(), error) {
	st, err := cfg.openStore(ctx)
	if err != nil {
		return nil, nil, func() {}, err
	}

	values, err := st.Config(ctx)
	if err != nil {
		st.Close()
		return nil, nil, func() {}, err
	}

	rem, closeRemote, err := cfg.openEngineRemote(ctx, logger)
	if err != nil {
		st.Close()
		return nil, nil, func() {}, err
	}
	closeFn := func() {
		closeRemote()
		st.Close()
	}

	opts := engine.OptionsFrom(values)
	opts.DataDir = cfg.dataDir
	if opts.Limiter, err = cfg.limiter(values, logger); err != nil {
		closeFn()
		return nil, nil, func() {}, err
	}

	eng, err := engine.New(st, rem, logger, opts)
	if err != nil {
		closeFn()
		return nil, nil, func() {}, err
	}
	return eng, st, closeFn, nil
}

// openLocalEngine builds an engine with no publisher behind it, for the
// operations that touch nothing over there. The tunnel being down is one of the
// reasons to want a database rollback, so needing the remote to build one would
// be the wrong dependency (DESIGN.md S5).
func (cfg *config) openLocalEngine(ctx context.Context, logger *logs.Logger) (*engine.Engine, *store.Store, func(), error) {
	st, err := cfg.openStore(ctx)
	if err != nil {
		return nil, nil, func() {}, err
	}

	values, err := st.Config(ctx)
	if err != nil {
		st.Close()
		return nil, nil, func() {}, err
	}

	opts := engine.OptionsFrom(values)
	opts.DataDir = cfg.dataDir

	eng, err := engine.NewOffline(st, logger, opts)
	if err != nil {
		st.Close()
		return nil, nil, func() {}, err
	}
	return eng, st, func() { st.Close() }, nil
}

// limiter is the bandwidth cap a hand-run command gets. The grid's cap applies
// whoever started the transfer - it is there to protect the link, and the link
// does not care who pressed the button - but the window itself does not: a
// command typed at a console is an override by definition, and there is no
// meaningful number to apply to an hour that was meant to move nothing.
func (cfg *config) limiter(values store.Values, logger *logs.Logger) (*engine.Limiter, error) {
	if cfg.limit != "" {
		switch strings.ToLower(strings.TrimSpace(cfg.limit)) {
		case "0", "off", "none", "full":
			logger.Infof("transfer: no bandwidth cap")
			return engine.NewLimiter(0), nil
		}
		n, err := format.ParseSize(cfg.limit)
		if err != nil {
			return nil, fmt.Errorf("-limit %q: %w", cfg.limit, err)
		}
		logger.Infof("transfer: capped at %s/s (%s)", format.Size(n), format.Rate(n, time.Second))
		return engine.NewLimiter(n), nil
	}

	sched, err := schedule.Parse(values.Get(store.KeySchedule))
	if err != nil {
		return nil, fmt.Errorf("the stored transfer window will not parse: %w", err)
	}
	loc, err := time.LoadLocation(values.Get(store.KeyTimezone))
	if err != nil {
		return nil, fmt.Errorf("timezone %q: %w", values.Get(store.KeyTimezone), err)
	}

	w := sched.At(time.Now().In(loc))
	if !w.Open {
		logger.Warnf("transfer: the window is closed right now - running anyway, with no cap")
		return engine.NewLimiter(0), nil
	}
	if w.Limit > 0 {
		logger.Infof("transfer: capped at %s/s (%s) until %s, from the schedule",
			format.Size(w.Limit), format.Rate(w.Limit, time.Second), w.Until.Format("Mon 15:04"))
	}
	return engine.NewLimiter(w.Limit), nil
}

// openPair resolves the pair an operator named, by name or by id.
func openPair(ctx context.Context, st *store.Store, ref string) (store.Pair, error) {
	if strings.TrimSpace(ref) == "" {
		return store.Pair{}, fmt.Errorf("missing -pair: the name or id of the pair to work on, as `jcc-mirror pairs` lists them")
	}
	return st.FindPair(ctx, ref)
}

// pickRef takes the first pair reference that was actually given. A pair can be
// named as a bare argument or as -pair, because both read naturally: `pairs rm
// media` and `scan -pair media`.
func pickRef(refs ...string) string {
	for _, r := range refs {
		if r = strings.TrimSpace(r); r != "" {
			return r
		}
	}
	return ""
}

// takeWord pulls a leading bare argument off the front of the command line. It
// has to happen before the flag set sees the arguments: Go's flag package stops
// parsing at the first non-flag argument, so `pairs set media -name x` would
// otherwise leave -name unparsed.
func takeWord(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// listFlag collects a flag that may be given more than once, and splits each
// value on commas so several can also be written in one. The cost is that a
// value can never contain a comma - which rules out a glob for a directory whose
// name has one in it.
type listFlag struct {
	dst *[]string
}

// String is called on a zero value by the flag package, before dst is anything.
func (l *listFlag) String() string {
	if l == nil || l.dst == nil {
		return ""
	}
	return strings.Join(*l.dst, ",")
}

func (l *listFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*l.dst = append(*l.dst, part)
		}
	}
	return nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// orDash renders an empty string as something visible, so a column that is empty
// on purpose - a pair rooted at the remote root - does not look like a bug.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
