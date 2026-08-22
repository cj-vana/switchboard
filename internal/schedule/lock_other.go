//go:build !unix && !windows

package schedule

import "os"

// Platforms without a kernel-backed file locking implementation get no
// advisory lock; the ledger's single-owner promise is unenforced there.
// Unix uses flock and Windows uses LockFileEx in their platform files.
func acquireLock(*os.File) error { return nil }

func releaseLock(*os.File) error { return nil }
