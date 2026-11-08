// Package pass runs the reconciler's passes one at a time, under the lease
// (specs/0001-architecture.md, Passes): taken at the start, renewed every
// minute, released at the end, with an absolute deadline that caps every call
// a pass makes, and a stop to everything once a renewal fails.
package pass

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/internal/lease"
)

// The spec's timings. The lease outlives the deadline by enough that a pass
// whose renewals all fail silently has still stopped before anyone else can
// take the lease.
const (
	LeaseName    = "leases/pass"
	Validity     = 10 * time.Minute
	RenewEvery   = time.Minute
	Deadline     = 7 * time.Minute
	releaseLimit = 10 * time.Second
)

// ErrHeld means another pass is running.
var ErrHeld = lease.ErrHeld

// ErrLeaseLost is the cause of a pass's context ending when a renewal fails.
var ErrLeaseLost = errors.New("pass stopped: its lease could not be renewed")

// Body is the work of one pass.
type Body func(p *Pass) error

// Runner runs passes.
type Runner struct {
	Lease  *lease.Lease
	Clock  clockwork.Clock
	Logger *slog.Logger

	// Deadline and RenewEvery default to the spec's.
	Deadline   time.Duration
	RenewEvery time.Duration
}

// Pass is one running pass.
type Pass struct {
	ctx      context.Context
	clock    clockwork.Clock
	deadline time.Time
}

// Context is the pass's context. Every call the pass makes uses it, or one
// derived from it, so that no call outlives the pass's deadline, and every
// call stops once the lease is lost.
func (p *Pass) Context() context.Context { return p.ctx }

// Deadline is when the pass must have finished.
func (p *Pass) Deadline() time.Time { return p.deadline }

// Within derives a context for one step with a shorter deadline of its own,
// such as reading the queue, still capped by the pass's.
func (p *Pass) Within(d time.Duration) (context.Context, context.CancelFunc) {
	return withDeadline(p.ctx, p.clock, minTime(p.clock.Now().Add(d), p.deadline))
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Lost reports whether the pass has stopped because its lease was lost.
func (p *Pass) Lost() bool { return errors.Is(context.Cause(p.ctx), ErrLeaseLost) }

// Run takes the lease and runs body under it. It returns ErrHeld, without
// running body, if another pass holds the lease.
func (r *Runner) Run(ctx context.Context, body Body) error {
	deadline := r.Deadline
	if deadline == 0 {
		deadline = Deadline
	}
	every := r.RenewEvery
	if every == 0 {
		every = RenewEvery
	}
	log := r.Logger
	if log == nil {
		log = slog.Default()
	}

	end := r.Clock.Now().Add(deadline)
	dctx, cancelDeadline := withDeadline(ctx, r.Clock, end)
	defer cancelDeadline()
	held, err := r.Lease.Acquire(dctx)
	if err != nil {
		return err
	}
	pctx, stop := context.WithCancelCause(dctx)
	defer stop(nil)

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		t := r.Clock.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-pctx.Done():
				return
			case <-t.Chan():
			}
			// A renewal that hasn't succeeded by the next one is due has
			// failed. It's capped by the pass's deadline too, and the
			// lease's expiry is later still.
			rctx, cancel := withDeadline(pctx, r.Clock, r.Clock.Now().Add(every))
			err := held.Renew(rctx)
			cancel()
			if err != nil && dctx.Err() != nil {
				// The pass reached its deadline mid-renewal: it's over,
				// but its lease wasn't shown lost.
				return
			}
			if err != nil {
				log.Error("pass lease renewal failed; stopping the pass", "err", err)
				stop(fmt.Errorf("%w: %w", ErrLeaseLost, err))
				return
			}
		}
	})

	berr := body(&Pass{ctx: pctx, clock: r.Clock, deadline: end})
	close(done)
	wg.Wait()

	lost := errors.Is(context.Cause(pctx), ErrLeaseLost)
	// Whether the body was cut short is decided when it returns; the
	// release after it doesn't change that.
	cut := dctx.Err()
	// Release in what's left of the pass, even if the caller has gone: the
	// release is capped by the deadline like every other call. A pass that
	// reached its deadline, or whose renewal failed, leaves the lease to
	// expire.
	if !lost && r.Clock.Now().Before(end) {
		rctx, cancel := withDeadline(context.WithoutCancel(ctx), r.Clock, minTime(r.Clock.Now().Add(releaseLimit), end))
		defer cancel()
		if err := held.Release(rctx); err != nil {
			log.Warn("pass lease release failed; it will expire", "err", err)
		}
	}
	if lost {
		return errors.Join(context.Cause(pctx), berr)
	}
	if err := cut; err != nil {
		// The pass was cut short, by its deadline or its caller, whatever
		// the body made of it: it didn't finish.
		return errors.Join(fmt.Errorf("pass stopped: %w", err), berr)
	}
	return berr
}

// withDeadline is context.WithDeadline on the given clock, so that tests can
// move time on.
func withDeadline(parent context.Context, clock clockwork.Clock, t time.Time) (context.Context, context.CancelFunc) {
	if _, fake := clock.(*clockwork.FakeClock); !fake {
		return context.WithDeadline(parent, t)
	}
	c := &deadlineContext{parent: parent, deadline: t, done: make(chan struct{})}
	timer := clock.AfterFunc(t.Sub(clock.Now()), func() { c.end(context.DeadlineExceeded) })
	stop := context.AfterFunc(parent, func() { c.end(parent.Err()) })
	return c, func() {
		timer.Stop()
		stop()
		c.end(context.Canceled)
	}
}

// deadlineContext ends at a deadline on a fake clock. It has its own done
// channel, so contexts derived from it see its own error.
type deadlineContext struct {
	parent   context.Context
	deadline time.Time
	done     chan struct{}

	mu  sync.Mutex
	err error
}

func (c *deadlineContext) end(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
		close(c.done)
	}
}

func (c *deadlineContext) Deadline() (time.Time, bool) {
	if d, ok := c.parent.Deadline(); ok && d.Before(c.deadline) {
		return d, true
	}
	return c.deadline, true
}

func (c *deadlineContext) Done() <-chan struct{} { return c.done }

func (c *deadlineContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *deadlineContext) Value(key any) any { return c.parent.Value(key) }
