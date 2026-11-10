package state

import (
	"sync"
)

// updateHealthMu guards State.UpdateHealth, which the automatic updater, the API and the console share.
var updateHealthMu sync.Mutex

// LockUpdateHealth locks the update record. Hold it briefly and never while waiting on the update mutex.
func (*State) LockUpdateHealth() {
	updateHealthMu.Lock()
}

// UnlockUpdateHealth unlocks the update record.
func (*State) UnlockUpdateHealth() {
	updateHealthMu.Unlock()
}
