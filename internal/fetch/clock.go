package fetch

import "time"

// SystemClock is the Clock backed by the time package.
type SystemClock struct{}

var _ Clock = SystemClock{}

// Now returns time.Now().
func (SystemClock) Now() time.Time { return time.Now() }

// AfterFunc calls time.AfterFunc.
func (SystemClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
