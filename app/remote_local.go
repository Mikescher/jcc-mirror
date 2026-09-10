package app

import (
	"path"
	"strings"

	"blackforestbytes.com/jcc-mirror/remote/localfs"
)

// localRemote is a local directory standing in for the publisher's share. It is
// what Options.RemoteDir installs, and it is the same fake the engine's own tests
// run on: SMB has no in-process server the way HTTP does, so without it the
// daemon could only be exercised end to end against a real NAS (DESIGN.md §2.1).
type localRemote struct{ *localfs.FS }

// Close is a no-op: there is no session to end, only a directory.
func (localRemote) Close() error { return nil }

// NonNFCNames is always zero. The counter is a warning about the publisher's
// tree, and a directory created by a test is nobody's source.
func (localRemote) NonNFCNames() int64 { return 0 }

func (r localRemote) Target() string { return r.Root() }

func (r localRemote) TargetFor(rel string) string {
	if rel = strings.Trim(rel, "/"); rel == "" {
		return r.Root()
	}
	return path.Join(r.Root(), rel)
}
