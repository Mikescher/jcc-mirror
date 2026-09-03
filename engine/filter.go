package engine

import (
	"path"
	"strings"

	"blackforestbytes.com/jcc-mirror/store"
)

// filter decides what a pair covers. Path globs are the only selection mechanism
// there is: choosing by ClipCorn metadata would mean reading the database, which
// jcc-mirror never does (DESIGN.md §0).
//
// The rules, in the order they are applied to a pair-relative path:
//
//  1. An exclude that matches the path, or any directory above it, drops it.
//     Excluding a directory therefore excludes its whole subtree, and the walk
//     skips it rather than listing it.
//  2. With no includes, everything else is in. With includes, a path is in only
//     if one of them matches it or a directory above it.
//
// A pattern is matched segment by segment: "*" and "?" and "[a-z]" stay inside
// one segment, "**" spans any number of them. "Serien/**" and "Serien" therefore
// both mean the whole subtree.
type filter struct {
	includes []string
	excludes []string
}

func newFilter(p store.Pair) filter {
	return filter{includes: p.Includes, excludes: p.Excludes}
}

// allows reports whether a file belongs to the pair.
func (f filter) allows(rel string) bool {
	if f.excluded(rel) {
		return false
	}
	if len(f.includes) == 0 {
		return true
	}
	return matchesAny(f.includes, rel)
}

// allowsDir reports whether the walk should descend into a directory. Only the
// excludes apply: a directory that matches no include may still hold one, and
// the alternative is guessing which prefixes of a pattern could match later.
func (f filter) allowsDir(rel string) bool {
	return rel == "" || !f.excluded(rel)
}

func (f filter) excluded(rel string) bool {
	return len(f.excludes) > 0 && matchesAny(f.excludes, rel)
}

// matchesAny reports whether any pattern matches the path or a directory above
// it.
func matchesAny(patterns []string, rel string) bool {
	for _, pattern := range patterns {
		if matchPath(pattern, rel) {
			return true
		}
		for dir := path.Dir(rel); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if matchPath(pattern, dir) {
				return true
			}
		}
	}
	return false
}

// matchPath matches a glob against a whole slash-separated path.
func matchPath(pattern, name string) bool {
	if pattern == "" || name == "" {
		return false
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			if len(pattern) == 1 {
				return true
			}
			// Try every split point: "**" may stand for no segments at all.
			for i := 0; i <= len(name); i++ {
				if matchSegments(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		// path.Match only fails on a malformed pattern, which then matches nothing
		// - a bad glob must not quietly select the whole tree.
		if ok, err := path.Match(pattern[0], name[0]); err != nil || !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}
