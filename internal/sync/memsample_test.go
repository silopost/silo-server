package sync_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"runtime"
	"runtime/pprof"
	"sync/atomic"
	"testing"
	"time"
)

// memSampler records the peak memory of the test process while a sync runs:
// the Go runtime (Sys, HeapInuse) and the resident set of the process as the
// operating system sees it. The process also holds the test's own IMAP
// client and Postgres pool, so the figures are an upper bound for silo-server
// (Dovecot and Postgres run in containers and are not counted).
type memSampler struct {
	stop, done   chan struct{}
	pprofPath    string
	samples      int
	maxRSS       uint64
	peakRSS      uint64 // the OS's own peak (peak working set / VmHWM)
	maxSys       uint64
	maxHeapInuse uint64
	rssOK        bool
}

// startMemSampler samples every interval until stop. With pprofPath set, a
// heap profile is written there each time HeapInuse reaches a new maximum
// (so the file ends up holding the profile at the peak).
func startMemSampler(interval time.Duration, pprofPath string) *memSampler {
	s := &memSampler{stop: make(chan struct{}), done: make(chan struct{}), pprofPath: pprofPath}
	go func() {
		defer close(s.done)
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			s.sample()
			select {
			case <-s.stop:
				s.sample()
				return
			case <-tk.C:
			}
		}
	}()
	return s
}

func (s *memSampler) sample() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s.samples++
	s.maxSys = max(s.maxSys, ms.Sys)
	if ms.HeapInuse > s.maxHeapInuse {
		s.maxHeapInuse = ms.HeapInuse
		if s.pprofPath != "" {
			if f, err := os.Create(s.pprofPath); err == nil {
				_ = pprof.WriteHeapProfile(f)
				_ = f.Close()
			}
		}
	}
	if rss, peak, ok := processRSS(); ok {
		s.rssOK = true
		s.maxRSS = max(s.maxRSS, rss)
		s.peakRSS = max(s.peakRSS, peak)
	}
}

// finish stops sampling; the fields are final afterwards.
func (s *memSampler) finish() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
}

// countingHandler counts WARN and ERROR records of the worker's logger.
type countingHandler struct {
	slog.Handler
	warns, errors *atomic.Int64
}

func (h countingHandler) Handle(ctx context.Context, r slog.Record) error {
	switch {
	case r.Level >= slog.LevelError:
		h.errors.Add(1)
	case r.Level >= slog.LevelWarn:
		h.warns.Add(1)
	}
	return h.Handler.Handle(ctx, r)
}

func (h countingHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return countingHandler{h.Handler.WithAttrs(as), h.warns, h.errors}
}

func (h countingHandler) WithGroup(name string) slog.Handler {
	return countingHandler{h.Handler.WithGroup(name), h.warns, h.errors}
}

// syncReport is the JSON written to SILO_SYNC_TEST_REPORT by TestInitialSync.
type syncReport struct {
	Messages          int     `json:"messages"`
	PopulateSeconds   float64 `json:"populateSeconds"`
	LiveSeconds       float64 `json:"liveSeconds"`
	InitialSeconds    float64 `json:"initialSyncSeconds"`
	MaxRSSBytes       uint64  `json:"maxRssBytes"`
	PeakRSSBytes      uint64  `json:"peakRssBytes"`
	MaxSysBytes       uint64  `json:"maxGoSysBytes"`
	MaxHeapInuseBytes uint64  `json:"maxHeapInuseBytes"`
	Samples           int     `json:"samples"`
	LogErrors         int64   `json:"logErrors"`
	LogWarnings       int64   `json:"logWarnings"`
	TestFailed        bool    `json:"testFailed"`
	GOOS              string  `json:"goos"`
	Note              string  `json:"note"`
}

func writeSyncReport(t *testing.T, path string, r syncReport) {
	t.Helper()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Errorf("report: %v", err)
		return
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Errorf("report: %v", err)
	}
}
