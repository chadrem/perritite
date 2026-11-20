package config

import "time"

// SetTokenWaitDelay shortens the leftover-child grace period for tests.
func SetTokenWaitDelay(d time.Duration) func() {
	old := tokenWaitDelay
	tokenWaitDelay = d
	return func() { tokenWaitDelay = old }
}
