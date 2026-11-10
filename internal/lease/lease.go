// Package lease keeps a lease in the bucket: an object taken with a
// generation precondition, valid for a fixed time unless renewed, and
// released by deleting exactly the generation its holder last wrote
// (specs/0001-architecture.md, Passes).
package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/internal/store"
)

// ErrHeld means someone else holds the lease and it hasn't expired.
var ErrHeld = errors.New("lease is held")

// ErrLost means the lease is no longer this holder's: it was taken over, or
// its holder can't tell whether it still has it.
var ErrLost = errors.New("lease lost")

// record is what the lease object holds.
type record struct {
	Holder  string    `json:"holder"`
	Renewal int64     `json:"renewal"`
	Expires time.Time `json:"expires"`
}

// Lease is a named lease in a bucket.
type Lease struct {
	objects  store.Objects
	name     string
	validity time.Duration
	clock    clockwork.Clock
}

// New returns the lease kept in object name, valid for validity after each
// write.
func New(objects store.Objects, name string, validity time.Duration, clock clockwork.Clock) *Lease {
	return &Lease{objects: objects, name: name, validity: validity, clock: clock}
}

// Held is a lease its holder has taken.
type Held struct {
	lease  *Lease
	holder string

	mu         sync.Mutex
	generation int64
	renewal    int64
	expires    time.Time
	lost       bool
	// uncertain means a renewal failed without showing that anyone else
	// took the lease, so it may still be ours, at a generation we don't know.
	uncertain bool
}

func newHolder() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Acquire takes the lease if no one holds it, or its holder let it expire.
// It returns ErrHeld if someone else holds it.
func (l *Lease) Acquire(ctx context.Context) (*Held, error) {
	h := &Held{lease: l, holder: newHolder()}
	rec := h.next()
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	gen, err := l.objects.Create(ctx, l.name, data)
	if err == nil {
		h.took(gen, rec)
		return h, nil
	}
	if !errors.Is(err, store.ErrPrecondition) {
		return nil, fmt.Errorf("taking lease %s: %w", l.name, err)
	}
	// It exists: perhaps our own write, whose response was lost and whose
	// retry then failed its precondition; perhaps an expired lease.
	cur, curGen, err := l.read(ctx)
	if errors.Is(err, store.ErrNotFound) {
		// Released between our write and our read: someone else's turn may
		// have come and gone, so try again next time rather than loop.
		return nil, ErrHeld
	}
	if err != nil {
		return nil, fmt.Errorf("reading lease %s: %w", l.name, err)
	}
	if cur.Holder == h.holder && cur.Renewal == rec.Renewal {
		h.took(curGen, cur)
		return h, nil
	}
	if l.clock.Now().Before(cur.Expires) {
		return nil, ErrHeld
	}
	gen, err = l.objects.Replace(ctx, l.name, data, curGen)
	if errors.Is(err, store.ErrPrecondition) {
		// Someone else replaced it first, or our replacement's response
		// was lost; look once more.
		again, againGen, rerr := l.read(ctx)
		if rerr == nil && again.Holder == h.holder && again.Renewal == rec.Renewal {
			h.took(againGen, again)
			return h, nil
		}
		return nil, ErrHeld
	}
	if err != nil {
		return nil, fmt.Errorf("taking expired lease %s: %w", l.name, err)
	}
	h.took(gen, rec)
	return h, nil
}

func (l *Lease) read(ctx context.Context) (record, int64, error) {
	data, gen, err := l.objects.Get(ctx, l.name)
	if err != nil {
		return record{}, 0, err
	}
	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		// Unreadable, so no one can be shown to hold it: treat it as
		// expired, and let the generation precondition settle any race.
		return record{}, gen, nil
	}
	return rec, gen, nil
}

// next is the record of the holder's next write.
func (h *Held) next() record {
	return record{Holder: h.holder, Renewal: h.renewal + 1, Expires: h.lease.clock.Now().Add(h.lease.validity)}
}

func (h *Held) took(gen int64, rec record) {
	h.generation, h.renewal, h.expires = gen, rec.Renewal, rec.Expires
}

// Expires is when the lease ends unless renewed.
func (h *Held) Expires() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.expires
}

// Renew extends the lease by its validity, with the same precondition. Any
// failure loses the lease: once it can't be shown to be ours, its holder must
// act as if it isn't. The error then wraps ErrLost.
func (h *Held) Renew(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lost {
		return ErrLost
	}
	rec := h.next()
	data, err := json.Marshal(rec)
	if err != nil {
		h.lost = true
		return fmt.Errorf("%w: %w", ErrLost, err)
	}
	gen, err := h.lease.objects.Replace(ctx, h.lease.name, data, h.generation)
	if errors.Is(err, store.ErrPrecondition) {
		// Our renewal may have been written and its response lost.
		cur, curGen, rerr := h.lease.read(ctx)
		if rerr == nil && cur.Holder == h.holder && cur.Renewal == rec.Renewal {
			h.took(curGen, cur)
			return nil
		}
	}
	if err != nil {
		h.lost = true
		h.uncertain = !errors.Is(err, store.ErrPrecondition)
		return fmt.Errorf("%w: renewing %s: %w", ErrLost, h.lease.name, err)
	}
	h.took(gen, rec)
	return nil
}

// Release ends the lease, deleting only the generation this holder last
// wrote, so a lease someone else has taken since is left alone. A lease shown
// to be someone else's isn't touched. After a renewal whose outcome is
// unknown, it deletes the lease only if it still names this holder.
func (h *Held) Release(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	gen := h.generation
	if h.lost {
		if !h.uncertain {
			return ErrLost
		}
		cur, curGen, err := h.lease.read(ctx)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("releasing %s: %w", h.lease.name, err)
		}
		if cur.Holder != h.holder {
			return ErrLost
		}
		gen = curGen
	}
	h.lost, h.uncertain = true, false
	err := h.lease.objects.Delete(ctx, h.lease.name, gen)
	if err == nil || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrPrecondition) {
		return nil
	}
	return fmt.Errorf("releasing %s: %w", h.lease.name, err)
}
