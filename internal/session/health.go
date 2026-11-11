package session

import "time"

const (
	minBackoff = 2 * time.Second
	maxBackoff = time.Minute
)

// delay is the wait after the given number of consecutive failures: 2s, doubling per
// failure up to 60s.
func delay(failures int) time.Duration {
	d := minBackoff
	for i := 1; i < failures && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

// backoff tracks consecutive failures of one thing and when it may be tried again.
// It has no lock; the Session's mutex guards it.
type backoff struct {
	failures int
	until    time.Time
	err      error
}

func (b *backoff) active(now time.Time) bool { return now.Before(b.until) }

func (b *backoff) fail(now time.Time, err error) {
	b.failures++
	b.until = now.Add(delay(b.failures))
	b.err = err
}

func (b *backoff) reset() { *b = backoff{} }

// blocked returns the error of the last failure while the backoff is running.
func (b *backoff) blocked(now time.Time) error {
	if b.active(now) {
		return b.err
	}
	return nil
}

// member is a node of the cluster together with its health.
type member struct {
	name   string
	url    string
	node   Node
	health backoff
}
