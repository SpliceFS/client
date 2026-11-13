package fetch

import (
	"sync"
	"testing"
	"time"
)

func TestRateMeter(t *testing.T) {
	t.Parallel()
	meter := NewMeter()
	if _, ok := meter.Rate("a"); ok {
		t.Fatal("new meter knows a rate")
	}
	meter.Failed("a")
	if _, ok := meter.Rate("a"); ok {
		t.Fatal("a failure created a rate")
	}
	meter.Observe("a", 1000, 0)
	if _, ok := meter.Rate("a"); ok {
		t.Fatal("a sample without duration created a rate")
	}
	meter.Observe("a", 1000, time.Second)
	if r, _ := meter.Rate("a"); r != 1000 {
		t.Fatalf("first sample: rate %v, want 1000", r)
	}
	meter.Observe("a", 2000, time.Second)
	if r, _ := meter.Rate("a"); r != 1300 {
		t.Fatalf("second sample: rate %v, want 1300", r)
	}
	meter.Failed("a")
	if r, _ := meter.Rate("a"); r != 650 {
		t.Fatalf("after failure: rate %v, want 650", r)
	}
}

func TestRateMeterConcurrent(t *testing.T) {
	t.Parallel()
	meter := NewMeter()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				meter.Observe("a", 100, time.Millisecond)
				meter.Failed("a")
				meter.Rate("a")
			}
		})
	}
	wg.Wait()
	if _, ok := meter.Rate("a"); !ok {
		t.Error("rate lost")
	}
}

func TestSystemClock(t *testing.T) {
	t.Parallel()
	var c SystemClock
	if c.Now().IsZero() {
		t.Error("Now is zero")
	}
	if !c.AfterFunc(time.Hour, func() { t.Error("timer fired") }).Stop() {
		t.Error("Stop of a pending timer returned false")
	}
}
