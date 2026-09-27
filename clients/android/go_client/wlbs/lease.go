package wlbs

import (
	"context"
	"sync"
	"time"
)

// LeaseGuard watches every worker including idle workers. Refresh updates must
// come ONLY from an authenticated full catalog/OK. It cannot enforce server
// revoke before notification; the server remains authoritative for access.
type LeaseGuard struct {
	mu              sync.Mutex
	generation, seq string
	expires         time.Time
	revoked         bool
	changed         chan struct{}
}

func NewLeaseGuard(generation, seq string, expires time.Time) (*LeaseGuard, error) {
	if _, e := CompareDecimal(generation, "0"); e != nil {
		return nil, e
	}
	if _, e := CompareDecimal(seq, "0"); e != nil {
		return nil, e
	}
	return &LeaseGuard{generation: generation, seq: seq, expires: expires, changed: make(chan struct{})}, nil
}
func (l *LeaseGuard) Update(generation, seq string, expires time.Time, revoked bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	g, e := CompareDecimal(generation, l.generation)
	if e != nil {
		return e
	}
	s, e := CompareDecimal(seq, l.seq)
	if e != nil {
		return e
	}
	if g < 0 || (g == 0 && s < 0) {
		return failure("LEASE_CONFLICT")
	}
	if l.revoked && !revoked {
		return failure("GRANT_REVOKED")
	}
	if g == 0 && s == 0 && !expires.Equal(l.expires) && !revoked {
		return failure("LEASE_CONFLICT")
	}
	if g > 0 && !revoked {
		return failure("GRANT_REVOKED")
	}
	l.generation = generation
	l.seq = seq
	l.expires = expires
	l.revoked = revoked
	close(l.changed)
	l.changed = make(chan struct{})
	return nil
}
func (l *LeaseGuard) Check(generation string, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.check(generation, now)
}
func (l *LeaseGuard) check(generation string, now time.Time) error {
	if l.revoked || generation != l.generation {
		return failure("GRANT_REVOKED")
	}
	if !now.Before(l.expires) {
		return failure("LEASE_EXPIRED")
	}
	return nil
}

// Watch blocks until cancel/revoke/expiry then calls closeWorker exactly once.
// Checking at most every 250ms also observes wall-clock jumps after suspend.
func (l *LeaseGuard) Watch(ctx context.Context, generation string, closeWorker func()) error {
	defer closeWorker()
	for {
		l.mu.Lock()
		err := l.check(generation, time.Now())
		changed := l.changed
		left := time.Until(l.expires)
		l.mu.Unlock()
		if err != nil {
			return err
		}
		if left > 250*time.Millisecond {
			left = 250 * time.Millisecond
		}
		timer := time.NewTimer(left)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
