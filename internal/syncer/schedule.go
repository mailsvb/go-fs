package syncer

import (
	"slices"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
)

// next is when a sync runs next, after a run that started at last, the zero
// time before the first one. One at an interval runs at once and then that
// long after the start of each run; one at a time of day runs at the next
// such time on one of its days, in the time zone of now.
func next(s config.Sync, last, now time.Time) time.Time {
	if every := s.Interval(); every > 0 {
		if last.IsZero() {
			return now
		}
		return last.Add(every)
	}
	hour, minute, ok := s.TimeOfDay()
	days := s.Weekdays()
	if !ok || len(days) == 0 {
		return time.Time{}
	}
	year, month, day := now.Date()
	for d := range 8 {
		// time.Date moves a time that a change to summer time skips past
		// the gap, so a day never goes without its run
		at := time.Date(year, month, day+d, hour, minute, 0, 0, now.Location())
		if at.After(now) && slices.Contains(days, at.Weekday()) {
			return at
		}
	}
	return time.Time{}
}
