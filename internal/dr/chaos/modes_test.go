package chaos

import "runtime"

// permissionBitsMatter reports whether file modes can be asserted. Windows has no Unix permission
// bits: Go reports 0666 for files and 0777 for directories whatever mode was requested, and the
// files are protected by NTFS ACLs, which this code does not set. Tests skip only those checks.
func permissionBitsMatter() bool { return runtime.GOOS != "windows" }
