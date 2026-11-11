package fetch

import (
	"slices"
	"testing"
	"time"
)

func TestStreakJoins(t *testing.T) {
	t.Parallel()
	s := streak{started: true, start: 0, pos: 1000}
	tests := []struct {
		name     string
		s        streak
		off, end int64
		want     bool
	}{
		{"no streak yet", streak{}, 0, 10, false},
		{"continues at pos", s, 1000, 1100, true},
		{"ahead within slack", s, 1300, 1400, true},
		{"ahead beyond slack", s, 1301, 1400, false},
		{"behind within slack", s, 600, 700, true},
		{"behind beyond slack", s, 600, 699, false},
		{"overlaps pos", s, 900, 1100, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.s.joins(tt.off, tt.end, 300); got != tt.want {
				t.Errorf("joins(%d, %d) = %v, want %v", tt.off, tt.end, got, tt.want)
			}
		})
	}
}

func TestWindowAt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                 string
		pos, readAhead, size int64
		wantFirst, wantLast  int64
	}{
		{"inside file", 150, 300, 10000, 1, 4},
		{"no read-ahead", 150, 0, 10000, 1, 1},
		{"clipped at end", 950, 300, 1000, 9, 9},
		{"position at end", 1000, 300, 1000, 10, 9},
		{"empty file", 0, 300, 0, 0, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			first, last := windowAt(tt.pos, tt.readAhead, 100, tt.size)
			if first != tt.wantFirst || last != tt.wantLast {
				t.Errorf("windowAt = [%d, %d], want [%d, %d]", first, last, tt.wantFirst, tt.wantLast)
			}
		})
	}
}

func TestRank(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		views []sourceView
		want  []ranked
	}{
		{
			name:  "fastest first, slow below half",
			views: []sourceView{{rate: 40, known: true, usable: true}, {rate: 100, known: true, usable: true}, {rate: 60, known: true, usable: true}},
			want:  []ranked{{1, true}, {2, true}, {0, false}},
		},
		{
			name:  "unknown equals fastest known",
			views: []sourceView{{rate: 100, known: true, usable: true}, {usable: true}, {rate: 10, known: true, usable: true}},
			want:  []ranked{{0, true}, {1, true}, {2, false}},
		},
		{
			name:  "nothing known keeps order",
			views: []sourceView{{usable: true}, {usable: true}},
			want:  []ranked{{0, true}, {1, true}},
		},
		{
			name:  "unusable left out of rates and order",
			views: []sourceView{{rate: 1000, known: true}, {rate: 10, known: true, usable: true}},
			want:  []ranked{{1, true}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := rank(tt.views); !slices.Equal(got, tt.want) {
				t.Errorf("rank = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAssign(t *testing.T) {
	t.Parallel()
	fast := func(free int) sourceView { return sourceView{rate: 100, known: true, free: free, usable: true} }
	slow := func(free int) sourceView { return sourceView{rate: 10, known: true, free: free, usable: true} }
	tests := []struct {
		name    string
		wanted  []int64
		views   []sourceView
		streams int
		want    []assignment
	}{
		{
			name:    "fast takes lowest, slow skips the first F",
			wanted:  []int64{10, 11, 12, 13, 14, 15, 16},
			views:   []sourceView{slow(2), fast(2)},
			streams: 2,
			want:    []assignment{{1, 10}, {1, 11}, {0, 12}, {0, 13}},
		},
		{
			name:    "F counts busy fast slots",
			wanted:  []int64{20, 21, 22},
			views:   []sourceView{fast(0), slow(2)},
			streams: 2,
			want:    []assignment{{1, 22}},
		},
		{
			name:    "slow idles when the window is shorter than F",
			wanted:  []int64{5, 6},
			views:   []sourceView{fast(1), slow(2)},
			streams: 2,
			want:    []assignment{{0, 5}},
		},
		{
			name:    "fills every free slot",
			wanted:  []int64{1, 2, 3, 4, 5},
			views:   []sourceView{{free: 2, usable: true}, {free: 2, usable: true}},
			streams: 2,
			want:    []assignment{{0, 1}, {0, 2}, {1, 3}, {1, 4}},
		},
		{
			name:    "cooling source gets nothing and does not count as fast",
			wanted:  []int64{1, 2},
			views:   []sourceView{{rate: 100, known: true, free: 2}, slow(2)},
			streams: 2,
			want:    []assignment{{1, 1}, {1, 2}},
		},
		{
			name:    "nothing wanted",
			views:   []sourceView{fast(2)},
			streams: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := assign(tt.wanted, tt.views, tt.streams); !slices.Equal(got, tt.want) {
				t.Errorf("assign = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHedgeDelay(t *testing.T) {
	t.Parallel()
	floor, ceiling := 500*time.Millisecond, time.Minute
	tests := []struct {
		name string
		n    int64
		rate float64
		want time.Duration
	}{
		{"unknown rate", 1000, 0, floor},
		{"twice the expected time", 1000, 1000, 2 * time.Second},
		{"never below floor", 1000, 1e6, floor},
		{"capped at the read timeout", 1000, 1e-300, ceiling},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hedgeDelay(tt.n, tt.rate, floor, ceiling); got != tt.want {
				t.Errorf("hedgeDelay = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCooldown(t *testing.T) {
	t.Parallel()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got := cooldown(i + 1); got != w {
			t.Errorf("cooldown(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := cooldown(1000); got != maxCooldown {
		t.Errorf("cooldown(1000) = %v, want %v", got, maxCooldown)
	}
}

func TestProbeCache(t *testing.T) {
	t.Parallel()
	var c probeCache
	for i := range int64(probeChunks) {
		c.put(i, []byte{byte(i)})
	}
	if _, ok := c.get(0); !ok {
		t.Fatal("chunk 0 missing")
	}
	c.put(probeChunks, nil)
	if _, ok := c.get(1); ok {
		t.Error("least recently used chunk 1 kept")
	}
	if data, ok := c.get(0); !ok || data[0] != 0 {
		t.Error("recently used chunk 0 dropped")
	}
}
