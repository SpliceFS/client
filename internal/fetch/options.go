package fetch

import (
	"cmp"
	"fmt"
	"time"
)

const (
	defaultProbeSize    = 256 << 10
	defaultSwitchAfter  = 1 << 20
	defaultSlack        = 2 << 20
	defaultBlockTimeout = 60 * time.Second
	defaultHedgeFloor   = 500 * time.Millisecond
)

// Options configures a Reader. Size, BlockSize and Streams are required; the
// other fields take their defaults when zero.
type Options struct {
	// Size is the file size in bytes.
	Size int64
	// BlockSize is the unit of block mode fetches and of the Store.
	BlockSize int64
	// Streams is the number of concurrent fetches per source.
	Streams int
	// ReadAhead is how many bytes are kept fetched ahead of the read position.
	ReadAhead int64
	// ProbeSize is the granularity of isolated reads. Default 256 KiB.
	ProbeSize int64
	// SwitchAfter is how far reads must advance sequentially before block
	// mode starts. A file of at most this size is read in block mode from
	// the start. Default 1 MiB.
	SwitchAfter int64
	// Slack is how far out of order a read may be and still count as
	// sequential. Default 2 MiB.
	Slack int64
	// BlockTimeout is the longest a read waits without progress before it
	// fails with ErrUnavailable. Default 60s.
	BlockTimeout time.Duration
	// HedgeFloor is the shortest time a fetch runs before it may be hedged.
	// Default 500ms.
	HedgeFloor time.Duration
	// Clock is the time source. Default SystemClock.
	Clock Clock
}

// validate rejects values that have no sensible meaning.
func (o Options) validate() error {
	checks := []struct {
		ok   bool
		what string
	}{
		{o.Size >= 0, "size is negative"},
		{o.BlockSize > 0, "block size is not positive"},
		{o.Streams > 0, "streams is not positive"},
		{o.ReadAhead >= 0, "read-ahead is negative"},
		{o.ProbeSize >= 0, "probe size is negative"},
		{o.SwitchAfter >= 0, "switch-after is negative"},
		{o.Slack >= 0, "slack is negative"},
		{o.BlockTimeout >= 0, "block timeout is negative"},
		{o.HedgeFloor >= 0, "hedge floor is negative"},
	}
	for _, c := range checks {
		if !c.ok {
			return fmt.Errorf("%w: %s", ErrInvalidOptions, c.what)
		}
	}
	return nil
}

// withDefaults returns o with every zero optional field set to its default.
func (o Options) withDefaults() Options {
	o.ProbeSize = cmp.Or(o.ProbeSize, defaultProbeSize)
	o.SwitchAfter = cmp.Or(o.SwitchAfter, defaultSwitchAfter)
	o.Slack = cmp.Or(o.Slack, defaultSlack)
	o.BlockTimeout = cmp.Or(o.BlockTimeout, defaultBlockTimeout)
	o.HedgeFloor = cmp.Or(o.HedgeFloor, defaultHedgeFloor)
	if o.Clock == nil {
		o.Clock = SystemClock{}
	}
	return o
}
