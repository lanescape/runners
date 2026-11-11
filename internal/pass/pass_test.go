package pass_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/emulators/fault"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/store"
)

type env struct {
	emu    *gcsemu.Emulator
	clock  *clockwork.FakeClock
	store  *store.GCS
	runner *pass.Runner
}

func setup(t *testing.T) env {
	t.Helper()
	clock := clockwork.NewFakeClockAt(time.Now())
	emu := gcsemu.New(gcsemu.WithClock(clock))
	t.Cleanup(emu.Close)
	emu.CreateBucket("state")
	s, err := store.NewGCS(context.Background(), "state", emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return env{emu: emu, clock: clock, store: s, runner: &pass.Runner{
		Lease: lease.New(s, pass.LeaseName, pass.Validity, clock),
		Clock: clock,
	}}
}

func renewal(t *testing.T, e env) int64 {
	t.Helper()
	data, _, ok := e.emu.Object("state", pass.LeaseName)
	if !ok {
		return 0
	}
	var rec struct {
		Renewal int64 `json:"renewal"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec.Renewal
}

// start runs a pass in the background whose body waits for release or for
// its context to end, and returns once the pass is under way.
func start(t *testing.T, e env) (p chan *pass.Pass, release chan struct{}, result chan error) {
	t.Helper()
	p = make(chan *pass.Pass, 1)
	release = make(chan struct{})
	result = make(chan error, 1)
	go func() {
		result <- e.runner.Run(context.Background(), func(ps *pass.Pass) error {
			p <- ps
			select {
			case <-release:
				return nil
			case <-ps.Context().Done():
				return ps.Context().Err()
			}
		})
	}()
	// The deadline's timer and the renewal ticker.
	if err := e.clock.BlockUntilContext(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	return p, release, result
}

// renewals reports each renewal that succeeds, once Renew has returned.
func renewals(e env) <-chan struct{} {
	ch := make(chan struct{}, 16)
	pass.OnRenewal(e.runner, func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	})
	return ch
}

// renewTo moves the clock on by one renewal interval and waits until that
// renewal has returned, so that the next move can't end it mid-flight.
func renewTo(t *testing.T, e env, renewed <-chan struct{}, want int64) {
	t.Helper()
	e.clock.Advance(pass.RenewEvery)
	select {
	case <-renewed:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for renewal %d", want)
	}
	if n := renewal(t, e); n != want {
		t.Fatalf("lease renewal = %d, want %d", n, want)
	}
}

func TestPassRenewsEveryMinuteAndReleases(t *testing.T) {
	e := setup(t)
	renewed := renewals(e)
	_, release, result := start(t, e)
	if n := renewal(t, e); n != 1 {
		t.Fatalf("lease renewal = %d at start, want 1", n)
	}
	for want := int64(2); want <= 6; want++ {
		renewTo(t, e, renewed, want)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, _, ok := e.emu.Object("state", pass.LeaseName); ok {
		t.Fatal("lease not released at the end of the pass")
	}
}

func TestOnePassAtATime(t *testing.T) {
	e := setup(t)
	_, release, result := start(t, e)
	ran := false
	err := e.runner.Run(context.Background(), func(*pass.Pass) error { ran = true; return nil })
	if !errors.Is(err, pass.ErrHeld) || ran {
		t.Fatalf("second pass: err %v, ran %v; want ErrHeld without running", err, ran)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := e.runner.Run(context.Background(), func(*pass.Pass) error { return nil }); err != nil {
		t.Fatalf("pass after the first ended: %v", err)
	}
}

func TestPassDeadline(t *testing.T) {
	e := setup(t)
	renewed := renewals(e)
	ps, _, result := start(t, e)
	p := <-ps
	if got := p.Deadline().Sub(e.clock.Now()); got != pass.Deadline {
		t.Fatalf("deadline is %v away, want %v", got, pass.Deadline)
	}
	// Renewals carry on until the deadline.
	for want := int64(2); want <= 7; want++ {
		renewTo(t, e, renewed, want)
	}
	e.clock.Advance(pass.RenewEvery)
	err := <-result
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pass past its deadline returned %v, want a deadline error", err)
	}
	// No call outlives the deadline, the release included: the lease is
	// left to expire.
	if _, _, ok := e.emu.Object("state", pass.LeaseName); !ok {
		t.Fatal("lease released after the deadline")
	}
}

func TestPassStopsWhenRenewalFails(t *testing.T) {
	e := setup(t)
	ps, _, result := start(t, e)
	p := <-ps
	e.emu.Put("state", pass.LeaseName, []byte(`{"holder":"another pass","renewal":1}`))
	e.clock.Advance(pass.RenewEvery)
	err := <-result
	if !errors.Is(err, pass.ErrLeaseLost) {
		t.Fatalf("pass after a failed renewal returned %v, want ErrLeaseLost", err)
	}
	if !p.Lost() {
		t.Fatal("pass doesn't report its lease lost")
	}
	if !errors.Is(context.Cause(p.Context()), pass.ErrLeaseLost) {
		t.Fatalf("context cause = %v", context.Cause(p.Context()))
	}
	data, _, _ := e.emu.Object("state", pass.LeaseName)
	if string(data) != `{"holder":"another pass","renewal":1}` {
		t.Fatalf("lease changed by the pass that lost it: %s", data)
	}
}

func TestPassStopsWhenRenewalErrs(t *testing.T) {
	e := setup(t)
	_, _, result := start(t, e)
	e.emu.Faults().Add(fault.Rule{
		Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "Backend Error")})
	e.clock.Advance(pass.RenewEvery)
	// The renewal retries until its own minute is up.
	if err := e.clock.BlockUntilContext(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(pass.RenewEvery)
	err := <-result
	e.emu.Faults().Reset()
	if !errors.Is(err, pass.ErrLeaseLost) {
		t.Fatalf("pass after a renewal error returned %v, want ErrLeaseLost", err)
	}
	if _, _, ok := e.emu.Object("state", pass.LeaseName); !ok {
		t.Fatal("a pass that couldn't renew released the lease; it must be left to expire")
	}
}

func TestCallsAreCappedByTheTimeLeft(t *testing.T) {
	e := setup(t)
	result := make(chan error, 1)
	called := make(chan struct{})
	go func() {
		result <- e.runner.Run(context.Background(), func(p *pass.Pass) error {
			e.emu.Faults().Add(fault.Rule{PathPrefix: "/state/anything", Delay: time.Hour})
			close(called)
			_, _, err := e.store.Get(p.Context(), "anything")
			return err
		})
	}()
	<-called
	e.clock.Advance(pass.Deadline)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("a call outlived its pass")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a call outlived its pass's deadline")
	}
	e.emu.Faults().Reset()
}

func TestWithinIsCappedByTheDeadline(t *testing.T) {
	e := setup(t)
	err := e.runner.Run(context.Background(), func(p *pass.Pass) error {
		short, cancel := p.Within(30 * time.Second)
		defer cancel()
		if d, _ := short.Deadline(); d.Sub(e.clock.Now()) != 30*time.Second {
			t.Errorf("short step's deadline is %v away", d.Sub(e.clock.Now()))
		}
		long, cancel2 := p.Within(time.Hour)
		defer cancel2()
		if d, _ := long.Deadline(); !d.Equal(p.Deadline()) {
			t.Errorf("long step's deadline %v isn't the pass's %v", d, p.Deadline())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPassPastItsDeadlineFailsWhateverItsBodySays(t *testing.T) {
	e := setup(t)
	result := make(chan error, 1)
	go func() {
		result <- e.runner.Run(context.Background(), func(p *pass.Pass) error {
			<-p.Context().Done()
			return nil
		})
	}()
	if err := e.clock.BlockUntilContext(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(pass.Deadline)
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pass cut short by its deadline returned %v, want a deadline error", err)
	}
}

func TestASlowReleaseStopsAtTheDeadline(t *testing.T) {
	e := setup(t)
	e.runner.Deadline = 300 * time.Millisecond
	e.runner.Clock = clockwork.NewRealClock()
	e.runner.Lease = lease.New(e.store, pass.LeaseName, pass.Validity, e.runner.Clock)
	start := time.Now()
	err := e.runner.Run(context.Background(), func(p *pass.Pass) error {
		e.emu.Faults().Add(fault.Rule{Method: http.MethodDelete, Delay: time.Minute})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the pass took %v, past its deadline, to release", d)
	}
	e.emu.Faults().Reset()
}

func TestAllRunsEveryPartInOrder(t *testing.T) {
	e := setup(t)
	var ran []string
	part := func(name string, err error) pass.Body {
		return func(*pass.Pass) error { ran = append(ran, name); return err }
	}
	failed := errors.New("the machines' part failed")
	err := e.runner.Run(context.Background(), pass.All(part("machines", failed), part("disks", nil)))
	if !errors.Is(err, failed) || len(ran) != 2 || ran[0] != "machines" || ran[1] != "disks" {
		t.Fatalf("ran %v, err %v; a failed part doesn't stop the next", ran, err)
	}
}
