// Package backoff is how the actions wait between attempts at a call that
// may succeed later.
package backoff

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Policy is how many attempts to make and how long to wait between them.
type Policy struct {
	// Attempts is how many calls to make in all.
	Attempts int
	// First is the wait after the first failure; each wait after doubles,
	// up to Max.
	First, Max time.Duration
	// Sleep waits, or returns the context's error; nil means a timer.
	Sleep func(context.Context, time.Duration) error
}

// Wait is the wait before attempt n+1, after n failures, at least what the
// server asked for, but never more than Max.
func (p Policy) Wait(n int, asked time.Duration) time.Duration {
	d := p.First
	for i := 1; i < n && d < p.Max; i++ {
		d *= 2
	}
	d = max(d, asked)
	return min(d, p.Max)
}

// Pause sleeps for d, or until ctx ends.
func (p Policy) Pause(ctx context.Context, d time.Duration) error {
	if p.Sleep != nil {
		return p.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RetryAfter is what an answer's Retry-After header asks for, in seconds or
// as a date; zero if it asks nothing.
func RetryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
