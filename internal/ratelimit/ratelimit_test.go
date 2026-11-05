package ratelimit

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]Limit{
		"600/m":  {Burst: 600, Every: 100 * time.Millisecond},
		"20/m":   {Burst: 20, Every: 3 * time.Second},
		"10/s":   {Burst: 10, Every: 100 * time.Millisecond},
		"3600/h": {Burst: 3600, Every: time.Second},
		"20/1m":  {Burst: 20, Every: 3 * time.Second},
		"off":    {},
		"0":      {},
	} {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "x/m", "-1/m", "10", "10/d", "10/-1s", "5/1ns"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q): no error", bad)
		}
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(Limit{Burst: 2, Every: 10 * time.Second}, func() time.Time { return now })
	for i := range 2 {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("attempt %d denied", i)
		}
	}
	ok, wait := l.Allow("k")
	if ok || wait != 10*time.Second {
		t.Fatalf("third attempt: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Allow("other"); !ok {
		t.Fatal("other key throttled")
	}
	now = now.Add(10 * time.Second)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("not refilled")
	}
	// Idle buckets are dropped once full again.
	now = now.Add(time.Hour)
	l.Allow("trigger")
	if n := l.Len(); n != 1 {
		t.Fatalf("buckets after sweep = %d", n)
	}
	if ok, _ := New(Limit{}, nil).Allow("x"); !ok {
		t.Fatal("disabled limit denied")
	}
}
