package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SpliceFS/client/internal/remote"
	"github.com/SpliceFS/common/wire"
)

// spaceTTL is how long the capacity of a view is reused. Tools such as df and file
// managers ask often, and the answer moves slowly.
const spaceTTL = 60 * time.Second

// spaceMemo is the last capacity of a view and when it was measured.
type spaceMemo struct {
	mu    sync.Mutex
	value Space
	at    time.Time
	valid bool
}

func (m *spaceMemo) get(now time.Time) (Space, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.value, m.valid && now.Sub(m.at) < spaceTTL
}

func (m *spaceMemo) put(value Space, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.value, m.at, m.valid = value, at, true
}

// StatFS reports the capacity of the view. Every node is expected to end up with every
// file, so each share is as large and as free as its smallest node; a view of the
// cluster root adds up its shares.
func (v *view) StatFS(ctx context.Context) (Space, error) {
	now := v.client.opts.Now()
	if s, ok := v.space.get(now); ok {
		return s, nil
	}
	s, err := v.measure(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return Space{}, ctx.Err()
		}
		return Space{}, fmt.Errorf("statfs: %w: %w", ErrUnavailable, err)
	}
	v.space.put(s, now)
	return s, nil
}

func (v *view) measure(ctx context.Context) (Space, error) {
	shares, err := v.shares(ctx)
	if err != nil {
		return Space{}, err
	}
	_, usable, err := v.c.nodes(ctx)
	if err != nil {
		return Space{}, err
	}
	var asks []spaceAsk
	for _, share := range shares {
		for _, n := range usable {
			asks = append(asks, spaceAsk{share: share, node: n})
		}
	}
	answers := fanOut(ctx, asks, func(ctx context.Context, a spaceAsk) spaceAnswer {
		var space wire.Space
		err := v.c.session.Do(ctx, a.node, func(ctx context.Context, token string) (err error) {
			space, err = a.node.Space(ctx, token, a.share)
			return err
		})
		return spaceAnswer{share: a.share, space: space, err: err}
	})
	return sumShares(shares, answers)
}

// shares are the shares the view lies on: all of them for the cluster root, else the
// one its path starts with.
func (v *view) shares(ctx context.Context) ([]string, error) {
	if v.prefix != "" {
		share, _, _ := strings.Cut(v.prefix, "/")
		return []string{share}, nil
	}
	entries, err := v.c.tree.List(ctx, "")
	if err != nil {
		return nil, err
	}
	shares := make([]string, len(entries))
	for i, e := range entries {
		shares[i] = e.Name
	}
	return shares, nil
}

type spaceAsk struct {
	share string
	node  *remote.Node
}

type spaceAnswer struct {
	share string
	space wire.Space
	err   error
}

// sumShares takes the smallest total and the smallest free space reported for each
// share and adds up the shares. A share that no node answered for makes the sum
// meaningless, so it fails the whole.
func sumShares(shares []string, answers []spaceAnswer) (Space, error) {
	smallest := make(map[string]wire.Space, len(shares))
	failures := make(map[string][]error)
	for _, a := range answers {
		if a.err != nil {
			failures[a.share] = append(failures[a.share], a.err)
			continue
		}
		s, ok := smallest[a.share]
		if !ok {
			s = a.space
		}
		smallest[a.share] = wire.Space{Total: min(s.Total, a.space.Total), Free: min(s.Free, a.space.Free)}
	}
	var sum Space
	for _, share := range shares {
		s, ok := smallest[share]
		if !ok {
			return Space{}, fmt.Errorf("share %q: no node answered: %w", share, errors.Join(failures[share]...))
		}
		sum.Total += s.Total
		sum.Free += s.Free
	}
	return sum, nil
}
