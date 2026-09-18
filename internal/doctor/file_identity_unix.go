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
	dev, devOK := statFieldToUint64(stat.Dev)
	ino, inoOK := statFieldToUint64(stat.Ino)
	if !devOK || !inoOK {
		return fileIdentity{}, false
	}
	return fileIdentity{dev: dev, ino: ino}, true
}

// statFieldToUint64 normalizes a syscall.Stat_t number to the unsigned width
// lsof prints, reporting false for a field type it does not know.
//
// The concrete types differ per platform (Dev is int32 on darwin and uint64 on
// linux), so widening a negative int32 directly would sign-extend into a value
// no device number ever takes; the 32-bit signed case is converted through its
// unsigned counterpart first. An unrecognized type must not degrade to zero:
// a zero device on the disk side against a real one from lsof reads as a
// divergence on a perfectly healthy host, which is the opposite of this
// package's contract that what cannot be established is reported as unverified.
func statFieldToUint64(v any) (uint64, bool) {
	switch n := v.(type) {
	case int32:
		return uint64(uint32(n)), true
	case uint32:
		return uint64(n), true
	case int64:
		return uint64(n), true
	case uint64:
		return n, true
	default:
		return 0, false
	}
}
