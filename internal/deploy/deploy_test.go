package deploy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/emulators/fault"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/deploy"
	"github.com/lanescape/runners/internal/store"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]deploy.ID{
		"":            {},
		"19283746-1":  {Run: 19283746, Attempt: 1},
		"19283746-12": {Run: 19283746, Attempt: 12},
	} {
		got, err := deploy.Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"19283746", "a-1", "1-b", "0-1", "1-0", "-1-1", "1-1-1", " 1-1"} {
		if _, err := deploy.Parse(in); err == nil {
			t.Errorf("Parse(%q) didn't fail", in)
		}
	}
}

func TestOrder(t *testing.T) {
	ordered := []deploy.ID{{}, {Run: 5, Attempt: 1}, {Run: 5, Attempt: 2}, {Run: 6, Attempt: 1}, {Run: 60, Attempt: 1}}
	for i, a := range ordered {
		for j, b := range ordered {
			if got := a.Before(b); got != (i < j) {
				t.Errorf("%v.Before(%v) = %v", a, b, got)
			}
		}
	}
	if !(deploy.ID{}).IsZero() || (deploy.ID{Run: 1, Attempt: 1}).IsZero() {
		t.Error("IsZero")
	}
	if s := (deploy.ID{Run: 5, Attempt: 2}).String(); s != "5-2" {
		t.Errorf("String = %s", s)
	}
}

type env struct {
	gcs     *gcsemu.Emulator
	objects store.Objects
	clock   *clockwork.FakeClock
}

func setup(t *testing.T) *env {
	t.Helper()
	gcs := gcsemu.New()
	t.Cleanup(gcs.Close)
	gcs.CreateBucket("state")
	objects, err := store.NewGCS(context.Background(), "state", gcs.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { objects.Close() })
	return &env{gcs: gcs, objects: objects, clock: clockwork.NewFakeClock()}
}

func (e *env) fence(id deploy.ID) *deploy.Fence {
	return &deploy.Fence{Own: id, Objects: e.objects, Clock: e.clock}
}

func (e *env) latest(t *testing.T) (deploy.Record, bool) {
	t.Helper()
	data, _, ok := e.gcs.Object("state", deploy.Key)
	if !ok {
		return deploy.Record{}, false
	}
	var r deploy.Record
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r, true
}

// TestALaterDeployReplacesAnEarlier: a revision records its deploy only if
// it's later than the one recorded, so an earlier deploy's revision never
// writes over a later one's, and finds itself replaced.
func TestALaterDeployReplacesAnEarlier(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	old, cur := e.fence(deploy.ID{Run: 100, Attempt: 1}), e.fence(deploy.ID{Run: 100, Attempt: 2})
	if _, replaced, err := old.Newer(ctx); replaced || err != nil {
		t.Fatalf("with nothing recorded, replaced = %v, %v", replaced, err)
	}
	if err := old.Serving(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cur.Serving(ctx); err != nil {
		t.Fatal(err)
	}
	// The earlier revision, serving again, writes nothing.
	_, gen, _ := e.gcs.Object("state", deploy.Key)
	if err := old.Serving(ctx); err != nil {
		t.Fatal(err)
	}
	if _, again, _ := e.gcs.Object("state", deploy.Key); again != gen {
		t.Fatal("an earlier deploy's revision wrote the record")
	}
	if r, _ := e.latest(t); r.RunID != 100 || r.Attempt != 2 {
		t.Fatalf("latest = %+v", r)
	}
	if latest, replaced, err := old.Newer(ctx); !replaced || err != nil || latest != cur.Own {
		t.Fatalf("the earlier revision: latest %v, replaced %v, %v", latest, replaced, err)
	}
	if _, replaced, err := cur.Newer(ctx); replaced || err != nil {
		t.Fatalf("the latest revision: replaced %v, %v", replaced, err)
	}
	// A revert is a later run.
	revert := e.fence(deploy.ID{Run: 101, Attempt: 1})
	if err := revert.Serving(ctx); err != nil {
		t.Fatal(err)
	}
	if _, replaced, _ := cur.Newer(ctx); !replaced {
		t.Fatal("a revert's later run didn't replace the revision")
	}
}

// TestARevisionWithNoDeployIDRecordsNothing: a revision run outside a
// deploy never records one, so it never stops a deploy's revision, and a
// recorded deploy replaces it.
func TestARevisionWithNoDeployIDRecordsNothing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	none := e.fence(deploy.ID{})
	if err := none.Serving(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.latest(t); ok {
		t.Fatal("a revision with no deploy ID recorded one")
	}
	if _, replaced, err := none.Newer(ctx); replaced || err != nil {
		t.Fatalf("with nothing recorded, replaced = %v, %v", replaced, err)
	}
	if err := e.fence(deploy.ID{Run: 7, Attempt: 1}).Serving(ctx); err != nil {
		t.Fatal(err)
	}
	if _, replaced, _ := none.Newer(ctx); !replaced {
		t.Fatal("a recorded deploy didn't replace a revision with no deploy ID")
	}
}

// TestRacingRevisionsKeepTheLatest: two revisions recording at once, each
// at the generation it read, leave the later deploy recorded.
func TestRacingRevisionsKeepTheLatest(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := e.fence(deploy.ID{Run: 1, Attempt: 1}).Serving(ctx); err != nil {
		t.Fatal(err)
	}
	// The later one's first write finds the record changed by the earlier,
	// which wrote after it read: it reads again and writes over it.
	later := e.fence(deploy.ID{Run: 3, Attempt: 1})
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 1,
		Respond: gcsemu.JSONError(http.StatusPreconditionFailed, "conditionNotMet", "Precondition Failed")})
	if err := later.Serving(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.latest(t); r.RunID != 3 {
		t.Fatalf("latest = %+v", r)
	}
	// One that can't read the record says so, and isn't told it's the
	// latest.
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 0,
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "Forbidden")})
	if _, _, err := later.Newer(ctx); err == nil {
		t.Fatal("a record that can't be read wasn't an error")
	}
}
