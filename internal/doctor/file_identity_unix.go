//go:build !windows

package doctor

import (
	"io/fs"
	"syscall"
)

// fileIdentityOf returns the device/inode pair identifying a file's bytes, and
// whether the host supplied one. A false return means the comparison that
// needs it must be reported as unverified — never assumed to agree.
func fileIdentityOf(info fs.FileInfo) (fileIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, false
	}
	return fileIdentity{
		dev: statFieldToUint64(stat.Dev),
		ino: statFieldToUint64(stat.Ino),
	}, true
}

// statFieldToUint64 normalizes a syscall.Stat_t number to the unsigned width
// lsof prints. The concrete types differ per platform (Dev is int32 on darwin
// and uint64 on linux), so widening a negative int32 directly would
// sign-extend into a value no device number ever takes; the 32-bit signed
// cases are converted through their unsigned counterpart first. Going through
// one helper for both fields also keeps this compiling on platforms where
// either field is narrower.
func statFieldToUint64(v any) uint64 {
	switch n := v.(type) {
	case int32:
		return uint64(uint32(n))
	case uint32:
		return uint64(n)
	case int64:
		return uint64(n)
	case uint64:
		return n
	default:
		return 0
	}
}
