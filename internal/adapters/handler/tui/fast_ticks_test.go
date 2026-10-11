package tui

import "time"

// Tests execute batched commands synchronously, so the background activity
// refresh tick, the order's settle deadline and the blinking of a cursor must
// not block them at their production intervals.
func init() {
	activityRefreshInterval = time.Millisecond
	orderSettleDeadline = time.Millisecond
	cursorBlinkSpeed = time.Millisecond
}
