package syncer

import (
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
)

func TestNextAtAnInterval(t *testing.T) {
	s := config.Sync{Every: "15m"}
	now := time.Date(2026, 3, 4, 10, 7, 0, 0, time.UTC)
	if got := next(s, time.Time{}, now); !got.Equal(now) {
		t.Errorf("the first run is at %v, want at once", got)
	}
	last := now.Add(-time.Minute)
	if got := next(s, last, now); !got.Equal(last.Add(15 * time.Minute)) {
		t.Errorf("the next run is at %v, want 15 minutes after the last began", got)
	}
}

func TestNextAtATimeOfDay(t *testing.T) {
	s := config.Sync{At: "02:30", Days: []string{"mon", "thu"}}
	// Wednesday 4 March 2026
	wednesday := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		now  time.Time
		want time.Time
	}{
		{wednesday, time.Date(2026, 3, 5, 2, 30, 0, 0, time.UTC)},
		// on the day, before and at the time
		{time.Date(2026, 3, 5, 2, 29, 0, 0, time.UTC), time.Date(2026, 3, 5, 2, 30, 0, 0, time.UTC)},
		{time.Date(2026, 3, 5, 2, 30, 0, 0, time.UTC), time.Date(2026, 3, 9, 2, 30, 0, 0, time.UTC)},
		// over the end of the week
		{time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 9, 2, 30, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := next(s, time.Time{}, c.now); !got.Equal(c.want) {
			t.Errorf("after %v the next run is at %v, want %v", c.now, got, c.want)
		}
	}
	// a single day comes round again a week later
	weekly := config.Sync{At: "02:30", Days: []string{"thu"}}
	at := time.Date(2026, 3, 5, 2, 30, 0, 0, time.UTC)
	if got := next(weekly, at, at.Add(time.Second)); !got.Equal(at.AddDate(0, 0, 7)) {
		t.Errorf("a weekly run is next at %v", got)
	}
}

func TestNextOverAChangeToSummerTime(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no time zone database:", err)
	}
	// 02:30 does not happen on Sunday 29 March 2026 in Berlin
	s := config.Sync{At: "02:30", Days: []string{"sun"}}
	now := time.Date(2026, 3, 28, 12, 0, 0, 0, berlin)
	got := next(s, time.Time{}, now)
	if got.Day() != 29 || got.Hour() != 3 || got.Minute() != 30 {
		t.Errorf("the run skipped by summer time is at %v, want 03:30 that day", got)
	}
}
