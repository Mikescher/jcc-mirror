//go:build !linux && !darwin

package engine

// The deployment is a Linux container and development happens on Linux or macOS;
// anywhere else the preflight is skipped rather than the build broken.
func statfs(string) (free, total int64, err error) { return 0, 0, ErrNoStatfs }
