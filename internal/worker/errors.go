package worker

import (
	"errors"
	"time"
)

// DeferError asks the worker to run the job again at Until without counting
// the attempt (the job is fine, its resource is not available: an account
// that is offline or waiting for credentials).
type DeferError struct {
	Until  time.Time
	Reason string
}

func (e *DeferError) Error() string { return "deferred: " + e.Reason }

// Defer returns a DeferError for a retry after d.
func Defer(d time.Duration, reason string) error {
	return &DeferError{Until: time.Now().Add(d), Reason: reason}
}

// PermanentError buries the job at once (status dead) without retries.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err so the job is not retried.
func Permanent(err error) error { return &PermanentError{Err: err} }

func asDefer(err error) (*DeferError, bool) {
	var d *DeferError
	return d, errors.As(err, &d)
}

func isPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

func isDeferred(err error) bool { _, ok := asDefer(err); return ok }
