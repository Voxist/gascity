//go:build windows

package doctor

import "io/fs"

// fileIdentityOf is the Windows fallback. Windows surfaces no device/inode
// pair through fs.FileInfo, and runningImageResolverFor returns no resolver
// for windows anyway, so the check reports that it did not run rather than
// comparing anything. Returning false here keeps that honest: an unavailable
// identity is reported as unverified, never approximated into an agreement.
func fileIdentityOf(_ fs.FileInfo) (fileIdentity, bool) {
	return fileIdentity{}, false
}
