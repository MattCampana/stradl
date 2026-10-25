package agent

import "time"

// Clock abstracts time so the Runner's schedule can be tested without
// waiting.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is the part of *time.Timer the Runner uses.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.t.C }
func (t realTimer) Stop() bool          { return t.t.Stop() }

// wallSince measures elapsed wall-clock time. Go's monotonic clock pauses
// while a Mac sleeps, so only the wall clock reveals how long it slept.
func wallSince(now, then time.Time) time.Duration {
	return now.Round(0).Sub(then.Round(0))
}
