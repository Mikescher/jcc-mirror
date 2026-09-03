package engine

import (
	"testing"

	"blackforestbytes.com/jcc-mirror/store"
)

func TestMatchPath(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{"an exact path", "Filme/A/a.mkv", "Filme/A/a.mkv", true},
		{"a different path", "Filme/A/a.mkv", "Filme/A/b.mkv", false},

		{"a star covers a whole segment", "Filme/*", "Filme/a.mkv", true},
		{"a star covers part of a segment", "*.mkv", "a.mkv", true},
		{"a star does not cross a slash", "Filme/*", "Filme/A/a.mkv", false},
		{"a bare star is one segment", "*", "Filme/a.mkv", false},
		{"a star matches an empty rest of segment", "a*", "a", true},

		{"a doublestar crosses many segments", "Filme/**", "Filme/A/B/a.mkv", true},
		{"a doublestar crosses one segment", "Filme/**", "Filme/a.mkv", true},
		{"a doublestar stands for no segment at all", "Filme/**/a.mkv", "Filme/a.mkv", true},
		{"a doublestar in the middle", "Filme/**/a.mkv", "Filme/A/B/a.mkv", true},
		{"a leading doublestar over no segment", "**/*.mkv", "a.mkv", true},
		{"a leading doublestar over several", "**/*.mkv", "Filme/A/a.mkv", true},
		{"a bare doublestar takes everything", "**", "Filme/A/a.mkv", true},
		{"a doublestar still has to reach its tail", "Filme/**/a.mkv", "Filme/A/b.mkv", false},
		{"a doublestar does not escape its prefix", "Filme/**", "Serien/a.mkv", false},

		{"a character class", "Serien/S0[0-9]", "Serien/S01", true},
		{"a character class that does not hold", "Serien/S0[0-9]", "Serien/S0x", false},
		{"a negated character class", "[^.]*.mkv", ".hidden.mkv", false},
		{"a question mark is one character", "e0?.mkv", "e01.mkv", true},
		{"a question mark is not two", "e0?.mkv", "e011.mkv", false},

		// A bad glob must select nothing rather than everything: the pattern is
		// operator input, and the alternative is a pair that mirrors the whole share.
		{"a malformed pattern matches nothing", "Filme/[a-", "Filme/[a-", false},
		{"a malformed pattern does not open the tree", "[", "Filme", false},

		{"a pattern longer than the path", "Filme/A/a.mkv", "Filme/A", false},
		{"a path longer than the pattern", "Filme/A", "Filme/A/a.mkv", false},
		{"an empty pattern", "", "Filme", false},
		{"an empty path", "Filme", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchPath(tt.pattern, tt.path); got != tt.want {
				t.Errorf("matchPath(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

func TestMatchesAny(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		path     string
		want     bool
	}{
		{"no patterns", nil, "Filme/a.mkv", false},
		{"the second pattern hits", []string{"Serien", "Filme"}, "Filme/a.mkv", true},

		// A pattern naming a directory takes its whole subtree: that is what makes
		// an exclude of "Serien" stop the walk from descending at all.
		{"a directory takes its subtree", []string{"Serien"}, "Serien/S01/e01.mkv", true},
		{"a directory deeper in takes its subtree", []string{"Serien/S01"}, "Serien/S01/e01.mkv", true},
		{"a glob on a directory takes its subtree", []string{"Serien/S0[0-9]"}, "Serien/S01/e01.mkv", true},
		{"a directory does not take a sibling", []string{"Serien/S01"}, "Serien/S02/e01.mkv", false},
		{"a prefix of a name is not a directory", []string{"Ser"}, "Serien/e01.mkv", false},

		{"a file pattern", []string{"**/*.nfo"}, "Filme/A/a.nfo", true},
		{"a file pattern that does not hold", []string{"**/*.nfo"}, "Filme/A/a.mkv", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesAny(tt.patterns, tt.path); got != tt.want {
				t.Errorf("matchesAny(%q, %q) = %v, want %v", tt.patterns, tt.path, got, tt.want)
			}
		})
	}
}

func TestFilterAllows(t *testing.T) {
	tests := []struct {
		name     string
		includes []string
		excludes []string
		path     string
		want     bool
	}{
		{"no patterns at all means everything", nil, nil, "Filme/A/a.mkv", true},
		{"an empty include list means everything", []string{}, nil, "Filme/A/a.mkv", true},

		{"an include selects its subtree", []string{"Filme"}, nil, "Filme/A/a.mkv", true},
		{"a path outside every include is out", []string{"Filme"}, nil, "Serien/e01.mkv", false},
		{"one of several includes is enough", []string{"Filme", "Serien"}, nil, "Serien/e01.mkv", true},

		// An exclude is checked first and cannot be overruled: the narrow rule is
		// the one an operator writes to keep something off a full disk.
		{"an exclude beats an include", []string{"Filme/**"}, []string{"Filme/Trash"}, "Filme/Trash/a.mkv", false},
		{"an exclude beats the include of the same path", []string{"Filme/a.mkv"}, []string{"Filme/a.mkv"}, "Filme/a.mkv", false},
		{"an exclude of a parent takes the subtree", nil, []string{"Serien"}, "Serien/S01/e01.mkv", false},
		{"an exclude that misses lets the file through", nil, []string{"Serien"}, "Filme/a.mkv", true},
		{"an extension exclude", nil, []string{"**/*.nfo"}, "Filme/A/a.nfo", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFilter(store.Pair{Includes: tt.includes, Excludes: tt.excludes})
			if got := f.allows(tt.path); got != tt.want {
				t.Errorf("allows(%q) = %v, want %v (includes %q, excludes %q)", tt.path, got, tt.want, tt.includes, tt.excludes)
			}
		})
	}
}

func TestFilterAllowsDir(t *testing.T) {
	tests := []struct {
		name     string
		includes []string
		excludes []string
		dir      string
		want     bool
	}{
		{"the root is always descended", []string{"Filme"}, []string{"**"}, "", true},

		// Only the excludes apply to a directory: one that matches no include may
		// still hold a file that does, and guessing which prefixes of a pattern
		// could match later is how a walk loses half the tree.
		{"a directory matching no include is still descended", []string{"**/*.mkv"}, nil, "Filme", true},
		{"a directory outside every include is still descended", []string{"Filme/**"}, nil, "Serien", true},

		{"an excluded directory is not descended", nil, []string{"Serien"}, "Serien", false},
		{"a directory below an excluded one is not descended", nil, []string{"Serien"}, "Serien/S01", false},
		{"a directory beside an excluded one is descended", nil, []string{"Serien"}, "Filme", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFilter(store.Pair{Includes: tt.includes, Excludes: tt.excludes})
			if got := f.allowsDir(tt.dir); got != tt.want {
				t.Errorf("allowsDir(%q) = %v, want %v (includes %q, excludes %q)", tt.dir, got, tt.want, tt.includes, tt.excludes)
			}
		})
	}
}
