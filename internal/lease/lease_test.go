package lease_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/emulators/fault"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/store"
)

const name = "leases/test"

type env struct {
	emu   *gcsemu.Emulator
	clock *clockwork.FakeClock
	lease *lease.Lease
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
	return env{emu: emu, clock: clock, lease: lease.New(s, name, 10*time.Minute, clock)}
}

func holderOf(t *testing.T, e env) (string, int64) {
	t.Helper()
	data, _, ok := e.emu.Object("state", name)
	if !ok {
		return "", 0
	}
	var rec struct {
		Holder  string `json:"holder"`
		Renewal int64  `json:"renewal"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec.Holder, rec.Renewal
}

func TestAcquireAndRelease(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	h, err := e.lease.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.lease.Acquire(ctx); !errors.Is(err, lease.ErrHeld) {
		t.Fatalf("second acquire: %v, want ErrHeld", err)
	}
	if err := h.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := e.emu.Object("state", name); ok {
		t.Fatal("lease object remains after release")
	}
	if _, err := e.lease.Acquire(ctx); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func TestTwoContenders(t *testing.T) {
	ctx := context.Background()
	for range 10 {
		e := setup(t)
		var wg sync.WaitGroup
		results := make([]error, 2)
		for i := range results {
			wg.Go(func() { _, results[i] = e.lease.Acquire(ctx) })
		}
		wg.Wait()
		won := 0
		for _, err := range results {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, lease.ErrHeld):
				t.Fatalf("contender failed: %v", err)
			}
		}
		if won != 1 {
			t.Fatalf("%d contenders took the lease, want exactly 1", won)
		}
	}
}

func TestManyContendersForAnExpiredLease(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	if _, err := e.lease.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(10 * time.Minute)
	var wg sync.WaitGroup
	results := make([]error, 5)
	for i := range results {
		wg.Go(func() { _, results[i] = e.lease.Acquire(ctx) })
	}
	wg.Wait()
	won := 0
	for _, err := range results {
		if err == nil {
			won++
		} else if !errors.Is(err, lease.ErrHeld) {
			t.Fatalf("contender failed: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d contenders took the expired lease, want exactly 1", won)
	}
}

func TestHolderThatDies(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	dead, err := e.lease.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The holder neither renews nor releases.
	e.clock.Advance(10*time.Minute - time.Second)
	if _, err := e.lease.Acquire(ctx); !errors.Is(err, lease.ErrHeld) {
		t.Fatalf("acquire before expiry: %v, want ErrHeld", err)
	}
	e.clock.Advance(time.Second)
	next, err := e.lease.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire after expiry: %v", err)
	}
	holder, _ := holderOf(t, e)

	// If the first holder comes back, it finds the lease gone, and its
	// release leaves the new holder's lease alone.
	if err := dead.Renew(ctx); !errors.Is(err, lease.ErrLost) {
		t.Fatalf("renewal by the old holder: %v, want ErrLost", err)
	}
	if err := dead.Release(ctx); !errors.Is(err, lease.ErrLost) {
		t.Fatalf("release by the old holder: %v, want ErrLost", err)
	}
	if h, _ := holderOf(t, e); h != holder {
		t.Fatal("the old holder changed the new holder's lease")
	}
	if err := next.Renew(ctx); err != nil {
		t.Fatalf("new holder's renewal: %v", err)
	}
}

func TestReleaseAfterTakeoverLeavesTheNewLease(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	old, err := e.lease.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(11 * time.Minute)
	if _, err := e.lease.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	holder, _ := holderOf(t, e)
	if err := old.Release(ctx); err != nil {
		t.Fatalf("release of a taken-over lease: %v", err)
	}
	if h, _ := holderOf(t, e); h != holder {
		t.Fatal("releasing an old lease deleted the new one")
	}
}

func TestRenewalExtendsTheLease(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	h, err := e.lease.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := h.Expires()
	e.clock.Advance(5 * time.Minute)
	if err := h.Renew(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.Expires().Sub(start); got != 5*time.Minute {
		t.Fatalf("renewal moved expiry by %v, want 5m", got)
	}
	if _, n := holderOf(t, e); n != 2 {
		t.Fatalf("renewal number = %d, want 2", n)
	}
	e.clock.Advance(6 * time.Minute) // past the first expiry
	if _, err := e.lease.Acquire(ctx); !errors.Is(err, lease.ErrHeld) {
		t.Fatalf("acquire after the original expiry: %v, want ErrHeld", err)
	}
	e.clock.Advance(4 * time.Minute)
	if _, err := e.lease.Acquire(ctx); err != nil {
		t.Fatalf("acquire after the renewed expiry: %v", err)
	}
}

func TestRenewalFailsWhenTakenOver(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	h, err := e.lease.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.emu.Put("state", name, []byte(`{"holder":"someone else","renewal":1}`))
	if err := h.Renew(ctx); !errors.Is(err, lease.ErrLost) {
		t.Fatalf("renewal after takeover: %v, want ErrLost", err)
	}
	if err := h.Release(ctx); !errors.Is(err, lease.ErrLost) {
		t.Fatalf("release after a lost renewal: %v", err)
	}
	if holder, _ := holderOf(t, e); holder != "someone else" {
		t.Fatal("a lost lease's holder changed the object")
	}
}

func TestRenewalFailsOnAnError(t *testing.T) {
	e := setup(t)
	h, err := e.lease.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e.emu.Faults().Add(fault.Rule{Method: http.MethodPost,
		Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "Backend Error")})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.Renew(ctx); !errors.Is(err, lease.ErrLost) {
		t.Fatalf("renewal through an outage: %v, want ErrLost", err)
	}
	e.emu.Faults().Reset()
	// Once lost, it stays lost: the holder can't know what happened.
	if err := h.Renew(context.Background()); !errors.Is(err, lease.ErrLost) {
		t.Fatalf("renewal after a failed one: %v, want ErrLost", err)
	}
	// The failed renewal may or may not have been written; a release checks
	// that the lease is still this holder's before deleting it.
	if err := h.Release(context.Background()); err != nil {
		t.Fatalf("release after a failed renewal: %v", err)
	}
	if _, _, ok := e.emu.Object("state", name); ok {
		t.Fatal("release after a failed renewal left the holder's own lease")
	}
}

func TestReleaseAfterAFailedRenewalLeavesSomeoneElsesLease(t *testing.T) {
	e := setup(t)
	h, err := e.lease.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e.emu.Faults().Add(fault.Rule{Method: http.MethodPost, Respond: fault.Drop})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := h.Renew(ctx); !errors.Is(err, lease.ErrLost) {
		t.Fatalf("renewal with connections dropped: %v, want ErrLost", err)
	}
	e.emu.Faults().Reset()
	e.emu.Put("state", name, []byte(`{"holder":"someone else","renewal":1}`))
	if err := h.Release(context.Background()); !errors.Is(err, lease.ErrLost) {
		t.Fatalf("release of someone else's lease: %v, want ErrLost", err)
	}
	if holder, _ := holderOf(t, e); holder != "someone else" {
		t.Fatal("release deleted someone else's lease")
	}
}

func TestLostResponses(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	lose := func() {
		e.emu.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 1, After: true,
			Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "Backend Error")})
	}
	lose()
	h, err := e.lease.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire whose response was lost: %v", err)
	}
	lose()
	if err := h.Renew(ctx); err != nil {
		t.Fatalf("renewal whose response was lost: %v", err)
	}
	if err := h.Renew(ctx); err != nil {
		t.Fatalf("renewal after one whose response was lost: %v", err)
	}
	if err := h.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := e.emu.Object("state", name); ok {
		t.Fatal("lease remains after release")
	}

	// Taking over an expired lease, with the response lost.
	if _, err := e.lease.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(10 * time.Minute)
	lose()
	if _, err := e.lease.Acquire(ctx); err != nil {
		t.Fatalf("takeover whose response was lost: %v", err)
	}
}

func TestUnreadableLeaseCanBeTaken(t *testing.T) {
	e := setup(t)
	e.emu.Put("state", name, []byte("not json"))
	if _, err := e.lease.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire over an unreadable lease: %v", err)
	}
}

func TestAcquireFailsWhenStorageIsDown(t *testing.T) {
	e := setup(t)
	e.emu.Faults().Add(fault.Rule{
		Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "Backend Error")})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := e.lease.Acquire(ctx)
	if err == nil || errors.Is(err, lease.ErrHeld) {
		t.Fatalf("acquire with storage down: %v, want an error other than ErrHeld", err)
	}
}

// TestAHeldLeaseSaysWhoHoldsItAndUntilWhen: the lease records its owner,
// and someone who finds it held learns who holds it and until when, as
// ErrHeld; an owner that said nothing is named as such.
func TestAHeldLeaseSaysWhoHoldsItAndUntilWhen(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.lease.Owner = "lanescape-runners-00021-z5t"
	held, err := e.lease.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.lease.Acquire(ctx)
	var he *lease.HeldError
	if !errors.Is(err, lease.ErrHeld) || !errors.As(err, &he) {
		t.Fatalf("Acquire while held = %v", err)
	}
	if he.Owner != "lanescape-runners-00021-z5t" || !he.Expires.Equal(held.Expires()) {
		t.Fatalf("held by %q until %s, want the owner until %s", he.Owner, he.Expires, held.Expires())
	}
	// A renewal keeps the owner.
	e.clock.Advance(time.Minute)
	if err := held.Renew(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.lease.Acquire(ctx); !errors.As(err, &he) || he.Owner != "lanescape-runners-00021-z5t" || !he.Expires.Equal(held.Expires()) {
		t.Fatalf("after a renewal: %v", err)
	}
	if err := held.Release(ctx); err != nil {
		t.Fatal(err)
	}
	e.lease.Owner = ""
	if _, err := e.lease.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.lease.Acquire(ctx); !errors.As(err, &he) || he.Owner != "" || !strings.Contains(err.Error(), "didn't say who") {
		t.Fatalf("an owner that said nothing: %v", err)
	}
}

// TestARenewalWhoseRecoveryCantBeReadIsReleasedIfStillOurs: a renewal
// written with its response lost, then refused on retry, can't show the
// lease is someone else's if it can't read it back, as when shutdown ends
// that read; the lease then counts as lost but uncertain, and a release
// reads it again and gives it up, since it's still ours.
func TestARenewalWhoseRecoveryCantBeReadIsReleasedIfStillOurs(t *testing.T) {
	e := setup(t)
	h, err := e.lease.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The renewal is written and its response lost; the client's retry is
	// refused, its precondition gone; and reading the lease back fails.
	e.emu.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 1, After: true,
		Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "Backend Error")})
	e.emu.Faults().Add(fault.Rule{Method: http.MethodGet,
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "no")})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = h.Renew(ctx)
	// Lost, from the failed read back: the renewal itself was written.
	if !errors.Is(err, lease.ErrLost) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("renewal = %v, want ErrLost from the read's 403", err)
	}
	if _, renewal := holderOf(t, e); renewal != 2 {
		t.Fatalf("the renewal wasn't written: renewal %d", renewal)
	}
	e.emu.Faults().Reset()
	if err := h.Release(context.Background()); err != nil {
		t.Fatalf("release = %v", err)
	}
	if _, _, ok := e.emu.Object("state", name); ok {
		t.Fatal("the lease, still ours, was left held")
	}
}

// TestReleaseOutlastsALateRenewal: a renewal cut short may still be written
// after a release read the lease; while the lease names its holder, the
// release deletes the newer generation too.
func TestReleaseOutlastsALateRenewal(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	h, err := e.lease.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	holder, _ := holderOf(t, e)
	// The renewal lands after the holder last wrote: a newer generation
	// naming the same holder.
	data, _, _ := e.emu.Object("state", name)
	var rec map[string]any
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	rec["renewal"] = 2
	late, _ := json.Marshal(rec)
	e.emu.Put("state", name, late)
	if err := h.Release(ctx); err != nil {
		t.Fatalf("release = %v", err)
	}
	if _, _, ok := e.emu.Object("state", name); ok {
		t.Fatalf("the late renewal of %s was left held", holder)
	}
	// Someone else's lease at a newer generation is left alone.
	h2, err := e.lease.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.emu.Put("state", name, []byte(`{"holder":"someone-else","renewal":1,"expires":"2999-01-01T00:00:00Z"}`))
	if err := h2.Release(ctx); err != nil {
		t.Fatalf("release = %v", err)
	}
	if other, _ := holderOf(t, e); other != "someone-else" {
		t.Fatalf("someone else's lease was deleted: %q", other)
	}
}

// TestAReleaseWhoseDeleteFailedCanBeRetried: a delete whose outcome isn't
// known leaves the release retryable; the retry reads the lease and deletes
// it if it still names its holder.
func TestAReleaseWhoseDeleteFailedCanBeRetried(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	h, err := e.lease.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.emu.Faults().Add(fault.Rule{Method: http.MethodDelete, Times: 1,
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "no")})
	if err := h.Release(ctx); err == nil {
		t.Fatal("the failed delete released")
	}
	if err := h.Release(ctx); err != nil {
		t.Fatalf("retry = %v", err)
	}
	if _, _, ok := e.emu.Object("state", name); ok {
		t.Fatal("the lease remains after the retried release")
	}
}
