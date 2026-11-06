//go:build !windows && !linux

package sync_test

// processRSS is not implemented here; only the Go runtime figures are reported.
func processRSS() (rss, peak uint64, ok bool) { return 0, 0, false }
