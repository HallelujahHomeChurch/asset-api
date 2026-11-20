package main

import (
	"context"
	"errors"
	"time"
)

// The elected worker admits serially. A slow capture must not block a free
// slot from serving another capture, including one that arrives after the poll.
type liveClaims struct {
	active    int
	completed chan error
}

func newLiveClaims() *liveClaims { return &liveClaims{completed: make(chan error, 2)} }

func (p *liveClaims) poll(ctx context.Context, claim func(context.Context) (func() error, error), interval time.Duration) (bool, error) {
	for p.active < cap(p.completed) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		run, err := claim(ctx)
		if err != nil {
			return false, err
		}
		if run == nil {
			break
		}
		p.active++
		go func() { p.completed <- run() }()
	}
	if p.active == 0 {
		return false, nil
	}
	select {
	case err := <-p.completed:
		p.active--
		return true, err
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(interval):
		return true, nil
	}
}

func (p *liveClaims) join() error {
	var result error
	for p.active > 0 {
		result = errors.Join(result, <-p.completed)
		p.active--
	}
	return result
}
