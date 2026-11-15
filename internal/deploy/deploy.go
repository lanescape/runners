// Package deploy orders the service's deploys, so that a revision a later
// deploy replaced makes no more machines or base images' builders
// (specs/0004-deployment.md, Which revision makes machines).
//
// Each deploy's ID is CD's run ID and attempt, <run ID>-<attempt>, which
// CD gives the revision as RUNNERS_DEPLOY_ID. A later run has a higher run
// ID, and a run's later attempt a higher attempt, so IDs order deploys: CD
// deploys only main's head, a rerun of it is a later attempt, and going
// back is a revert, a later run. The bucket holds the latest a serving
// revision has seen:
//
//	deploys/latest.json  the latest deploy's ID, written by its revision's passes
package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/store"
)

// Key is the latest deploy's record in the bucket.
const Key = "deploys/latest.json"

// ID is a deploy's ID: CD's run ID and attempt. The zero ID is no deploy's,
// as a revision run by hand or in tests has, and comes before every other.
type ID struct {
	Run     int64
	Attempt int64
}

// Parse reads an ID, <run ID>-<attempt>; "" is the zero ID.
func Parse(s string) (ID, error) {
	if s == "" {
		return ID{}, nil
	}
	run, attempt, ok := strings.Cut(s, "-")
	r, rerr := strconv.ParseInt(run, 10, 64)
	a, aerr := strconv.ParseInt(attempt, 10, 64)
	if !ok || rerr != nil || aerr != nil || r <= 0 || a <= 0 {
		return ID{}, fmt.Errorf("deploy ID %q isn't <run ID>-<attempt>", s)
	}
	return ID{Run: r, Attempt: a}, nil
}

// IsZero reports whether it's no deploy's ID.
func (i ID) IsZero() bool { return i == ID{} }

// Before reports whether i is an earlier deploy's than j.
func (i ID) Before(j ID) bool {
	return i.Run < j.Run || i.Run == j.Run && i.Attempt < j.Attempt
}

func (i ID) String() string {
	if i.IsZero() {
		return "none"
	}
	return fmt.Sprintf("%d-%d", i.Run, i.Attempt)
}

// Record is the latest deploy, as Key holds it.
type Record struct {
	RunID   int64     `json:"run_id"`
	Attempt int64     `json:"attempt"`
	At      time.Time `json:"at"`
}

// Fence is a revision's view of the deploys: its own ID, and the latest
// recorded.
type Fence struct {
	Own     ID
	Objects store.Objects
	Clock   clockwork.Clock
	Logger  *slog.Logger
}

func (f *Fence) log() *slog.Logger {
	if f.Logger == nil {
		return slog.Default()
	}
	return f.Logger
}

// latest reads the recorded latest deploy, the zero ID if there's none,
// with its generation, 0 if there's none.
func (f *Fence) latest(ctx context.Context) (ID, int64, error) {
	data, gen, err := f.Objects.Get(ctx, Key)
	if errors.Is(err, store.ErrNotFound) {
		return ID{}, 0, nil
	}
	if err != nil {
		return ID{}, 0, err
	}
	var r Record
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&r); err != nil {
		return ID{}, 0, fmt.Errorf("%s: %w", Key, err)
	}
	return ID{Run: r.RunID, Attempt: r.Attempt}, gen, nil
}

// Serving records that the revision of Own is serving: Own becomes the
// latest deploy if it's later than the one recorded, written at the
// generation it was read at, so that an earlier deploy's revision never
// writes over a later one's; a write that finds the record changed reads it
// again. A revision with no ID writes nothing.
func (f *Fence) Serving(ctx context.Context) error {
	if f.Own.IsZero() {
		return nil
	}
	for range 5 {
		cur, gen, err := f.latest(ctx)
		if err != nil {
			return err
		}
		if !cur.Before(f.Own) {
			return nil
		}
		data, err := json.Marshal(Record{RunID: f.Own.Run, Attempt: f.Own.Attempt, At: f.Clock.Now().UTC()})
		if err != nil {
			return err
		}
		if gen == 0 {
			_, err = f.Objects.Create(ctx, Key, data)
		} else {
			_, err = f.Objects.Replace(ctx, Key, data, gen)
		}
		if errors.Is(err, store.ErrPrecondition) {
			continue
		}
		if err != nil {
			return err
		}
		f.log().Info("latest deploy recorded", "deploy", f.Own.String(), "replaced", cur.String())
		return nil
	}
	return errors.New("the latest deploy's record kept changing as it was written")
}

// Newer is the latest deploy recorded, and whether it's later than Own: if
// it is, this revision was replaced, and makes no machines or builders. A
// revision with no ID comes before any recorded.
func (f *Fence) Newer(ctx context.Context) (ID, bool, error) {
	cur, _, err := f.latest(ctx)
	if err != nil {
		return ID{}, false, err
	}
	return cur, f.Own.Before(cur), nil
}

// Pass records, as a pass's first step, that this revision is serving: a
// pass runs only in a revision that took a request, so a revision that
// never served, such as one Cloud Run never moved traffic to, records
// nothing and stops no other. A failure is logged, and doesn't stop the
// pass; the next pass tries again.
func (f *Fence) Pass(p *pass.Pass) error {
	if err := f.Serving(p.Context()); err != nil {
		f.log().Warn("recording the latest deploy", "deploy", f.Own.String(), "err", err.Error())
	}
	return nil
}
