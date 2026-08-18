//go:build !unix && !windows

package session

import "os"

// Platforms without a kernel-backed file locking implementation get no
// advisory lock. Unix uses flock and Windows uses LockFileEx in their
// platform-specific files.
func acquireLock(*os.File) error { return nil }

func releaseLock(*os.File) error { return nil }
