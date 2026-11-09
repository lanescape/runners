// Package cache is the service's cache (specs/0006-cache.md): jobs on any
// runner save and restore entries, scoped as saved disks are, by their
// run's binding. An entry is in a store the consumer names. The service's
// own store, service, holds bytes, in the cache's bucket, moved through
// short-lived signed URLs for one object at a time, and only for jobs on
// the service's own machines; every other store is an index, whose entries
// hold only a size and SHA-256 that the job checks the bytes it keeps in
// its runner's own cache against. The service never holds an entry's
// bytes, and no bytes leave the region.
//
// Its records are in the service's own bucket, and the bytes in the
// cache's:
//
//	cache/reservations/<repository>/<reservation>.json  an upload, until it's committed or expires
//	cache/entries/<repository>/<entry>.json             an entry: pending, ready or marked
//	cache/choices/<repository>/<run>/<hash>.json         a run's restore choice for a platform, family and key
//	cache/recent/<repository>/<scope>/<hash>.json        a pull request's most recent choice for a platform and family
//	cache/families/<repository>/<hash>.json              when a family and platform of a lasting scope was last used
//	closures/<repository>/<scope>.json                   the disks' closures, shared
//	leases/disks/<repository>                            the disks' short lease, shared
//
//	uploads/<repository>/<reservation>                   an upload's object, in the cache's bucket
//	entries/<repository>/<series>/<entry>                an entry's object, in the cache's bucket
package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/disks"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/selector"
	"github.com/lanescape/runners/internal/store"
)

// The spec's timings.
const (
	// RestoreFor is how long a restore's signed URL is good for.
	RestoreFor = 15 * time.Minute
	// ReserveFor is a reservation's lifetime, and its upload's signed URL's.
	ReserveFor = time.Hour
	// PendingFor is how long an entry waits for GitHub to show its job
	// finished: GitHub ends every job within 5 days.
	PendingFor = 5 * 24 * time.Hour
	// PullRequestUnused, LastingUnused and LastingKeys are the disks'.
	PullRequestUnused = disks.PullRequestUnused
	LastingUnused     = disks.MainUnused
	LastingKeys       = disks.MainKeys
)

// The states an entry is recorded in.
const (
	Pending = "pending"
	Ready   = "ready"
	// Marked is an entry marked for deletion: it's never chosen, no URL is
	// signed for it, and it's deleted next.
	Marked = "marked"
)

// Entry is a cache entry, as the service records it.
type Entry struct {
	ID           string `json:"id"`
	RepositoryID int64  `json:"repository_id"`
	Scope        string `json:"scope"`
	// Store is the store it's in: service, whose entries' bytes are in the
	// cache's bucket, or one of the consumer's, whose entries are an index.
	Store    string `json:"store"`
	Platform string `json:"platform"`
	Family   string `json:"family"`
	Key      string `json:"key"`
	Series   string `json:"series"`
	State    string `json:"state"`
	// Object is its object in the cache's bucket, and Generation the
	// copy's, 0 until the commit has made it; an index entry has neither.
	Object     string `json:"object"`
	Generation int64  `json:"generation,omitempty"`
	// Size and SHA256 are the job's word, which every restore checks.
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// The job that committed it, whose conclusion decides whether it's
	// kept, and its reservation.
	JobID       int64  `json:"job_id"`
	RunID       int64  `json:"run_id"`
	RunAttempt  int64  `json:"run_attempt"`
	Reservation string `json:"reservation"`
	// BoundAt is when its run was bound: one of a pull request's committed
	// by a run bound before the pull request was last seen closed is never
	// chosen again.
	BoundAt     time.Time `json:"bound_at"`
	CommittedAt time.Time `json:"committed_at"`
	ReadyAt     time.Time `json:"ready_at,omitzero"`
	LastUsed    time.Time `json:"last_used,omitzero"`
	// TouchedAt is when a run last chose or read it as its choice: writing
	// it changes the record's generation, so a mark made from an earlier
	// reading fails.
	TouchedAt time.Time `json:"touched_at,omitzero"`
	MarkedAt  time.Time `json:"marked_at,omitzero"`
}

// Reservation is an upload reserved for a job, until it's committed or
// expires.
type Reservation struct {
	ID           string    `json:"id"`
	RepositoryID int64     `json:"repository_id"`
	RunID        int64     `json:"run_id"`
	RunAttempt   int64     `json:"run_attempt"`
	JobID        int64     `json:"job_id"`
	Scope        string    `json:"scope"`
	Store        string    `json:"store"`
	Platform     string    `json:"platform"`
	Family       string    `json:"family"`
	Key          string    `json:"key"`
	BoundAt      time.Time `json:"bound_at"`
	Created      time.Time `json:"created"`
	Expires      time.Time `json:"expires"`
	// Entry is the entry its commit makes, named before the entry's record
	// is written, so a commit tried again carries on with it; Size, SHA256
	// and UploadGeneration are that commit's. An index entry's Size and
	// SHA256 are its save's, which its commit must repeat.
	Entry            string `json:"entry,omitempty"`
	Size             int64  `json:"size,omitempty"`
	SHA256           string `json:"sha256,omitempty"`
	UploadGeneration int64  `json:"upload_generation,omitempty"`
}

// Choice is a run's restore choice for a platform, family and key: the
// entry every job of the run that asks gets, or none, and the step of the
// restore order it came from.
type Choice struct {
	RepositoryID int64  `json:"repository_id"`
	RunID        int64  `json:"run_id"`
	Scope        string `json:"scope"`
	Store        string `json:"store"`
	Platform     string `json:"platform"`
	Family       string `json:"family"`
	Key          string `json:"key"`
	Step         string `json:"step"`
	Entry        string `json:"entry,omitempty"`
	// Attempt is the run attempt it was picked in; it holds for the
	// attempts the service's automatic reruns started from it.
	Attempt int64     `json:"attempt"`
	At      time.Time `json:"at"`
}

// Recent is a pull request's most recent restore choice for a platform and
// family, kept until the pull request closes: a lasting scope keeps an
// older key while one names it.
type Recent struct {
	RepositoryID int64     `json:"repository_id"`
	Scope        string    `json:"scope"`
	Store        string    `json:"store"`
	Platform     string    `json:"platform"`
	Family       string    `json:"family"`
	Step         string    `json:"step"`
	Entry        string    `json:"entry,omitempty"`
	EntryScope   string    `json:"entry_scope,omitempty"`
	Key          string    `json:"key,omitempty"`
	RunID        int64     `json:"run_id"`
	At           time.Time `json:"at"`
}

// Family is when a family and platform of a lasting scope, main's or a
// long-lived branch's, was last used, kept apart from its entries so that
// deleting one never makes it look unused for longer than it has been.
type Family struct {
	RepositoryID int64     `json:"repository_id"`
	Scope        string    `json:"scope"`
	Store        string    `json:"store"`
	Platform     string    `json:"platform"`
	Family       string    `json:"family"`
	LastUsed     time.Time `json:"last_used"`
}

// Signer signs V4 URLs for one object of the cache's bucket and one method.
type Signer interface {
	// SignedURL signs a URL for method on object, with query and the
	// headers, as name:value, that a request must carry, good until
	// expires. The URL is a credential.
	SignedURL(ctx context.Context, object, method string, query url.Values, headers []string, expires time.Time) (string, error)
}

// Service is the cache.
type Service struct {
	OrganizationID int64
	GitHub         github.API
	// Objects is the service's own bucket, for the records; Bucket the
	// cache's, for the bytes.
	Objects store.Objects
	Bucket  store.Blobs
	Signer  Signer
	Actions *auth.Actions
	Binder  *binding.Binder
	// Disks share their lease, closures and runs' state.
	Disks  *disks.Service
	Clock  clockwork.Clock
	Logger *slog.Logger
}

func (s *Service) log() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

func (s *Service) now() time.Time { return s.Clock.Now().UTC() }

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// What the interface accepts (specs/0005-interface.md).
var (
	familyPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	keyPattern      = regexp.MustCompile(`^[0-9a-f]{20}$`)
	platformPattern = regexp.MustCompile(`^[a-z0-9]{1,16}-[a-z0-9]{1,16}$`)
	sha256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	idPattern       = regexp.MustCompile(`^[0-9a-f]{32}$`)
	storePattern    = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
)

// ServiceStore is the store whose entries' bytes are in the cache's bucket,
// open only to jobs on the service's own machines.
const ServiceStore = "service"

// Series names an entry's versions: the hash of its scope, store, platform,
// family and key.
func Series(sc, store, platform, family, key string) string {
	return selector.Hash(sc + "\x00" + store + "\x00" + platform + "\x00" + family + "\x00" + key)
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func reservationPrefix(repo int64) string { return fmt.Sprintf("cache/reservations/%d/", repo) }
func reservationKey(repo int64, id string) string {
	return reservationPrefix(repo) + id + ".json"
}
func entryPrefix(repo int64) string         { return fmt.Sprintf("cache/entries/%d/", repo) }
func entryKey(repo int64, id string) string { return entryPrefix(repo) + id + ".json" }
func choicePrefix(repo, run int64) string   { return fmt.Sprintf("cache/choices/%d/%d/", repo, run) }
func recentKey(repo int64, sc, store, platform, family string) string {
	return fmt.Sprintf("cache/recent/%d/%s/%s.json", repo, sc, selector.Hash(store+"\x00"+platform+"\x00"+family))
}
func familyKey(repo int64, sc, store, platform, family string) string {
	return fmt.Sprintf("cache/families/%d/%s.json", repo, selector.Hash(sc+"\x00"+store+"\x00"+platform+"\x00"+family))
}
func choiceKey(repo, run int64, store, platform, family, key string) string {
	return choicePrefix(repo, run) + selector.Hash(store+"\x00"+platform+"\x00"+family+"\x00"+key) + ".json"
}

// UploadObject is a reservation's upload's object in the cache's bucket.
func UploadObject(repo int64, id string) string { return fmt.Sprintf("uploads/%d/%s", repo, id) }

// EntryObject is an entry's object in the cache's bucket.
func EntryObject(repo int64, series, id string) string {
	return fmt.Sprintf("entries/%d/%s/%s", repo, series, id)
}

func getJSON[T any](ctx context.Context, o store.Objects, key string) (T, int64, bool, error) {
	var v T
	data, gen, err := o.Get(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return v, 0, false, nil
	}
	if err != nil {
		return v, 0, false, err
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return v, 0, false, fmt.Errorf("record %s: %w", key, err)
	}
	return v, gen, true, nil
}

func replaceJSON(ctx context.Context, o store.Objects, key string, v any, gen int64) (int64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return o.Replace(ctx, key, data, gen)
}

// createOnce writes a record only if it's absent, and returns what's there
// either way.
func createOnce[T any](ctx context.Context, o store.Objects, key string, v T) (T, int64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return v, 0, err
	}
	gen, err := o.Create(ctx, key, data)
	if err == nil {
		return v, gen, nil
	}
	if !errors.Is(err, store.ErrPrecondition) {
		return v, 0, err
	}
	got, gen, ok, err := getJSON[T](ctx, o, key)
	if err != nil {
		return v, 0, err
	}
	if !ok {
		return v, 0, fmt.Errorf("record %s vanished as it was written", key)
	}
	return got, gen, nil
}

// putJSON writes a record, replacing whatever is there.
func putJSON(ctx context.Context, o store.Objects, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	for range 3 {
		_, gen, err := o.Get(ctx, key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil {
			_, err = o.Replace(ctx, key, data, gen)
		} else {
			_, err = o.Create(ctx, key, data)
		}
		if !errors.Is(err, store.ErrPrecondition) {
			return err
		}
	}
	return fmt.Errorf("record %s kept changing", key)
}

type record[T any] struct {
	v   T
	gen int64
}

// listJSON reads every record under a prefix, with its generation.
func listJSON[T any](ctx context.Context, o store.Objects, prefix string) (map[string]record[T], error) {
	objs, err := o.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := map[string]record[T]{}
	for _, ob := range objs {
		v, gen, ok, err := getJSON[T](ctx, o, ob.Name)
		if err != nil {
			return nil, err
		}
		if ok {
			out[ob.Name] = record[T]{v, gen}
		}
	}
	return out, nil
}

// lasting reports whether a scope is main's or a long-lived branch's.
func lasting(sc string) bool {
	p, err := scope.Parse(sc)
	return err == nil && p.Lasting()
}

// isPullRequest reports whether a scope is a pull request's.
func isPullRequest(sc string) bool {
	p, err := scope.Parse(sc)
	if err != nil {
		return false
	}
	_, ok := p.PullRequestNumber()
	return ok
}

// olderThan orders entries by when they were committed, then by ID.
func olderThan(x, y Entry) bool {
	return x.CommittedAt.Before(y.CommittedAt) || (x.CommittedAt.Equal(y.CommittedAt) && x.ID < y.ID)
}

// ScopesHeld are the scopes a consumer's entries hold, so that a shared
// closure stays while they do. It's disks.Service.ScopesHeld.
func (s *Service) ScopesHeld(ctx context.Context, repo int64) (map[string]bool, error) {
	recs, err := listJSON[Entry](ctx, s.Objects, entryPrefix(repo))
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range recs {
		out[r.v.Scope] = true
	}
	prefix := fmt.Sprintf("cache/recent/%d/", repo)
	recents, err := s.Objects.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	for _, o := range recents {
		if sc, _, ok := strings.Cut(strings.TrimPrefix(o.Name, prefix), "/"); ok {
			out[sc] = true
		}
	}
	return out, nil
}
