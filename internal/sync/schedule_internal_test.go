package sync

import (
	"testing"
	"time"
)

// TestIdleWait: the idle select of session.run must wake up for body work
// whose pacing deadline passed after backfillStep read the clock (it then
// reported "no work" because the deadline was still ahead). Without a timer
// the backfill, and the initial-sync-done marker with it, waited for the next
// poll (TestIncremental / TestSendSync timed out after the 1 min PollInterval).
func TestIdleWait(t *testing.T) {
	fires := func(c <-chan time.Time, within time.Duration) bool {
		if c == nil {
			return false
		}
		select {
		case <-c:
			return true
		case <-time.After(within):
			return false
		}
	}

	overdue := &session{bodyWork: true, backfillAt: time.Now().Add(-time.Millisecond)}
	if !fires(overdue.idleWait(), time.Second) {
		t.Fatal("body work with an overdue pacing deadline: no wake-up")
	}
	ahead := &session{bodyWork: true, backfillAt: time.Now().Add(50 * time.Millisecond)}
	c := ahead.idleWait()
	if fires(c, 10*time.Millisecond) || !fires(c, time.Second) {
		t.Fatal("body work paced 50ms ahead: want a wake-up at the deadline")
	}
	if (&session{}).idleWait() != nil {
		t.Fatal("no body work, not restoring: want no timer")
	}
	if !fires((&session{restoring: true}).idleWait(), 2*time.Second) {
		t.Fatal("restoring: want the 1s restore tick")
	}
}
