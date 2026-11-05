package ops

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSnoozePresets(t *testing.T) {
	warsaw, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, warsaw)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		name string
		now  time.Time
		loc  *time.Location
		want string
	}{
		{"thursday morning", at("2026-11-05 10:00"), warsaw,
			"later_today=2026-11-05T18:00:00+01:00 tomorrow=2026-11-06T08:00:00+01:00 this_weekend=2026-11-07T08:00:00+01:00 next_week=2026-11-09T08:00:00+01:00"},
		{"less than an hour before 18:00", at("2026-11-05 17:01"), warsaw,
			"tomorrow=2026-11-06T08:00:00+01:00 this_weekend=2026-11-07T08:00:00+01:00 next_week=2026-11-09T08:00:00+01:00"},
		{"friday: the weekend is tomorrow", at("2026-11-06 09:00"), warsaw,
			"later_today=2026-11-06T18:00:00+01:00 tomorrow=2026-11-07T08:00:00+01:00 next_week=2026-11-09T08:00:00+01:00"},
		{"saturday", at("2026-11-07 12:00"), warsaw,
			"later_today=2026-11-07T18:00:00+01:00 tomorrow=2026-11-08T08:00:00+01:00 next_week=2026-11-09T08:00:00+01:00"},
		{"sunday: next week is tomorrow", at("2026-11-08 20:00"), warsaw,
			"tomorrow=2026-11-09T08:00:00+01:00"},
		{"monday: next monday", at("2026-11-09 07:00"), warsaw,
			"later_today=2026-11-09T18:00:00+01:00 tomorrow=2026-11-10T08:00:00+01:00 this_weekend=2026-11-14T08:00:00+01:00 next_week=2026-11-16T08:00:00+01:00"},
		{"the day is the user's, not UTC's", time.Date(2026, 11, 5, 23, 30, 0, 0, time.UTC), warsaw,
			"later_today=2026-11-06T18:00:00+01:00 tomorrow=2026-11-07T08:00:00+01:00 next_week=2026-11-09T08:00:00+01:00"},
		{"daylight saving starts overnight", at("2026-03-28 10:00"), warsaw,
			"later_today=2026-03-28T18:00:00+01:00 tomorrow=2026-03-29T08:00:00+02:00 next_week=2026-03-30T08:00:00+02:00"},
		{"utc without a time zone", time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC), nil,
			"later_today=2026-11-05T18:00:00Z tomorrow=2026-11-06T08:00:00Z this_weekend=2026-11-07T08:00:00Z next_week=2026-11-09T08:00:00Z"},
	}
	for _, c := range cases {
		var got []string
		for _, p := range SnoozePresets(c.now, c.loc) {
			if p.Label == "" {
				t.Errorf("%s: %s has no label", c.name, p.Key)
			}
			got = append(got, fmt.Sprintf("%s=%s", p.Key, p.Until.Format(time.RFC3339)))
		}
		if g := strings.Join(got, " "); g != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, g, c.want)
		}
	}
}
