package store_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/store"
)

func newStore(t *testing.T) (*gcsemu.Emulator, *store.GCS) {
	t.Helper()
	emu := gcsemu.New()
	t.Cleanup(emu.Close)
	emu.CreateBucket("state")
	s, err := store.NewGCS(context.Background(), "state", emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return emu, s
}

func TestCreateIsCreateOnly(t *testing.T) {
	ctx := context.Background()
	_, s := newStore(t)
	g1, err := s.Create(ctx, "a/b", []byte(`{"n":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "a/b", []byte(`{"n":2}`)); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("second create: %v, want ErrPrecondition", err)
	}
	data, gen, err := s.Get(ctx, "a/b")
	if err != nil || string(data) != `{"n":1}` || gen != g1 {
		t.Fatalf("get = %q, %d, %v; want first write at %d", data, gen, err, g1)
	}
}

func TestReplaceAndDeleteNeedTheGeneration(t *testing.T) {
	ctx := context.Background()
	_, s := newStore(t)
	g1, err := s.Create(ctx, "x", []byte("1"))
	if err != nil {
		t.Fatal(err)
	}
	g2, err := s.Replace(ctx, "x", []byte("2"), g1)
	if err != nil || g2 <= g1 {
		t.Fatalf("replace = %d, %v", g2, err)
	}
	if _, err := s.Replace(ctx, "x", []byte("3"), g1); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("stale replace: %v", err)
	}
	if _, err := s.Replace(ctx, "missing", []byte("3"), g1); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("replace of missing object: %v", err)
	}
	if err := s.Delete(ctx, "x", g1); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("stale delete: %v", err)
	}
	if err := s.Delete(ctx, "x", g2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(ctx, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := s.Delete(ctx, "x", g2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete of missing object: %v", err)
	}
}

func TestListByPrefix(t *testing.T) {
	ctx := context.Background()
	_, s := newStore(t)
	for _, n := range []string{"bindings/1/10", "bindings/1/11", "bindings/2/10", "leases/pass", "bindings/10/1"} {
		if _, err := s.Create(ctx, n, []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(ctx, "bindings/1/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, o := range got {
		if o.Generation == 0 {
			t.Errorf("%s has no generation", o.Name)
		}
		names = append(names, o.Name)
	}
	if want := []string{"bindings/1/10", "bindings/1/11"}; !slices.Equal(names, want) {
		t.Fatalf("list = %v, want %v", names, want)
	}
}

func TestListPages(t *testing.T) {
	ctx := context.Background()
	emu, s := newStore(t)
	var want []string
	for i := range 2500 {
		n := "p/" + time.Duration(i).String()
		emu.Put("state", n, []byte("x"))
		want = append(want, n)
	}
	slices.Sort(want)
	got, err := s.List(ctx, "p/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d objects, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Name != want[i] {
			t.Fatalf("object %d is %s, want %s", i, got[i].Name, want[i])
		}
	}
}

func TestTransientErrorsAreRetried(t *testing.T) {
	ctx := context.Background()
	emu, s := newStore(t)
	emu.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 2,
		Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "Backend Error")})
	if _, err := s.Create(ctx, "r", []byte("1")); err != nil {
		t.Fatalf("create through two 503s: %v", err)
	}
	emu.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 1,
		Respond: gcsemu.JSONError(http.StatusTooManyRequests, "rateLimitExceeded", "Rate limit")})
	if data, _, err := s.Get(ctx, "r"); err != nil || string(data) != "1" {
		t.Fatalf("get through a 429: %q, %v", data, err)
	}
}

// A create whose response is lost is retried with the same precondition,
// which then fails: the caller must look at what's there.
func TestLostCreateResponse(t *testing.T) {
	ctx := context.Background()
	emu, s := newStore(t)
	emu.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 1, After: true,
		Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "Backend Error")})
	if _, err := s.Create(ctx, "r", []byte("mine")); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("create with lost response: %v, want ErrPrecondition", err)
	}
	if data, _, ok := emu.Object("state", "r"); !ok || string(data) != "mine" {
		t.Fatalf("object = %q, %v", data, ok)
	}
}

func TestCallsStopAtTheContextsDeadline(t *testing.T) {
	emu, s := newStore(t)
	emu.Faults().Add(fault.Rule{Delay: time.Minute})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := s.Get(ctx, "r"); err == nil {
		t.Fatal("get succeeded past its deadline")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("get took %v despite its deadline", d)
	}
}
