//go:build !darwin

package pidutil

// The syscall-based process probes exist only on darwin. Linux answers all of
// these from /proc before they are consulted; every other platform keeps the
// portable ps fallbacks. See procstate_darwin.go for why darwin needs its own.
func procStateDead(int) (dead, known bool) { return false, false }

func procStartTime(int) (string, bool) { return "", false }

func sysctlChildPIDs(int) ([]int, bool) { return nil, false }

func procCmdline(int) ([]string, bool) { return nil, false }
