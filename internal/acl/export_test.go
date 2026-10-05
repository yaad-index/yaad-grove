package acl

import "time"

// SetClock replaces the gate's clock, for tests of time windows.
func SetClock(g *Gate, now func() time.Time) { g.now = now }

// Nudged is how many users the gate holds a nudge window for.
func Nudged(g *Gate) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.nudged)
}
