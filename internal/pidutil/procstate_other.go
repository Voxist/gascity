//go:build !darwin

package pidutil

// procStateDead and procStartTime have no syscall-based implementation outside
// darwin. Linux answers both from /proc before these are consulted; every other
// platform keeps the portable ps fallbacks. See procstate_darwin.go for why
// darwin needs its own.
func procStateDead(int) (dead, known bool) { return false, false }

func procStartTime(int) (string, bool) { return "", false }
