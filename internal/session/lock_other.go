//go:build !unix

package session

import "os"

// Platforms without flock get no advisory lock yet. Returning nil is the honest
// option: a sidecar lock file would go stale after a kill and lock the user out
// of the session they most need to resume, which trades a rare corruption for a
// common failure. Windows containment is an open question (§21.7) and a proper
// LockFileEx implementation belongs with that work.
func acquireLock(*os.File) error { return nil }

func releaseLock(*os.File) error { return nil }
