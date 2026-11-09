package service

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The interface's timings (specs/0006-cache.md).
const (
	RestoreFor = 15 * time.Minute
	ReserveFor = time.Hour
	// SessionFor is how long an upload's session URI takes bytes, as Cloud
	// Storage keeps a resumable upload open.
	SessionFor = 7 * 24 * time.Hour
)

var (
	familyPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	keyPattern      = regexp.MustCompile(`^[0-9a-f]{20}$`)
	platformPattern = regexp.MustCompile(`^[a-z0-9]{1,16}-[a-z0-9]{1,16}$`)
	sha256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	idPattern       = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type entry struct {
	id, scope, platform, family, key string
	repo                             int64
	state                            string
	object                           string
	size                             int64
	sha256                           string
	job                              int64
	boundAt, committedAt             time.Time
}

type reservation struct {
	id, scope, platform, family, key string
	repo, run, job                   int64
	boundAt, expires                 time.Time
	entry                            string
	size                             int64
	sha256                           string
}

type choice struct {
	run                   int64
	platform, family, key string
	step, entry           string
	attempt               int
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// refusal is a 403 with its reason; anything else the handlers return is a
// 503.
type refusal struct{ reason string }

func (r *refusal) Error() string { return r.reason }

func refuse(format string, args ...any) error { return &refusal{fmt.Sprintf(format, args...)} }

var errUnavailable = errors.New("GitHub is unavailable")

// caller is a cache request's job and its run's binding.
type caller struct {
	c       *claims
	repo    *repo
	run     *run
	binding *binding
}

// serveCache authenticates a cache request's caller, as the service does
// (specs/0006-cache.md, Who may use it), and answers with what h returns:
// 200, 403 with a reason, or 503, retryable, with a Retry-After.
func (e *Emulator) serveCache(h func(c *caller, body []byte) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10+1))
		var out any
		if err != nil || len(body) > 4<<10 {
			err = refuse("the body is unreadable or too large")
		} else {
			var c *claims
			if c, err = e.token(r); err != nil {
				err = refuse("%s", err.Error())
			} else {
				e.mu.Lock()
				var cl *caller
				if cl, err = e.caller(c); err == nil {
					out, err = h(cl, body)
				}
				e.mu.Unlock()
			}
		}
		var rf *refusal
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, out)
		case errors.As(err, &rf):
			Refuse(rf.reason)(w, r)
		default:
			Unavailable(5*time.Second)(w, r)
		}
	}
}

func (e *Emulator) caller(c *claims) (*caller, error) {
	if e.unavailable {
		return nil, errUnavailable
	}
	if c.checkRunID == 0 {
		return nil, refuse("the token names no job: it has no check_run_id")
	}
	rp := e.repos[c.repoID]
	if rp == nil {
		return nil, refuse("GitHub shows no repository %d", c.repoID)
	}
	jb := e.jobs[c.checkRunID]
	if jb == nil || jb.run.repo != rp {
		return nil, refuse("GitHub shows no job %d in the token's repository", c.checkRunID)
	}
	if jb.run.id != c.runID || int64(jb.attempt) != c.attempt {
		return nil, refuse("the token's job isn't of the token's run and attempt")
	}
	if jb.status != "in_progress" {
		return nil, refuse("the token's job isn't running")
	}
	b := e.bindings[c.runID]
	if b == nil {
		return nil, refuse("the run isn't bound: its runners job binds it first")
	}
	if b.headRepo != rp.id {
		if b.event != "pull_request_target" {
			return nil, refuse("the run is a fork's pull request's")
		}
		if !jb.run.approved {
			return nil, refuse("the contribution's run has no approval of the contribution environment")
		}
	}
	return &caller{c: c, repo: rp, run: jb.run, binding: b}, nil
}

// decode reads a body as exactly one JSON object with no other fields.
func decode(body []byte, into any, what string) error {
	trimmed := bytes.TrimSpace(body)
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if len(trimmed) == 0 || trimmed[0] != '{' || dec.Decode(into) != nil || dec.Decode(&struct{}{}) != io.EOF {
		return refuse("the body isn't a %s request", what)
	}
	return nil
}

// steps names a scope's steps of the restore order, exact and newest.
func steps(sc string) (string, string) {
	switch {
	case sc == "main":
		return "main_exact", "main_newest"
	case strings.HasPrefix(sc, "branch-"):
		return "branch_exact", "branch_newest"
	}
	return "pull_request_exact", "pull_request_newest"
}

// restore is POST /v1/cache/restore.
func (e *Emulator) restore(cl *caller, body []byte) (any, error) {
	var q struct {
		Family   string `json:"family"`
		Key      string `json:"key"`
		Platform string `json:"platform"`
		Lookup   bool   `json:"lookup"`
	}
	if err := decode(body, &q, "restore"); err != nil {
		return nil, err
	}
	if !familyPattern.MatchString(q.Family) || !keyPattern.MatchString(q.Key) || !platformPattern.MatchString(q.Platform) {
		return nil, refuse("the body isn't a restore request: its family, key or platform isn't the interface's")
	}
	ck := fmt.Sprintf("%d\x00%s\x00%s\x00%s", cl.run.id, q.Platform, q.Family, q.Key)
	ch := e.choices[ck]
	if ch != nil && cl.c.attempt > int64(ch.attempt) {
		// Only the service's own automatic reruns keep a run's choices.
		for a := ch.attempt + 1; a <= int(cl.c.attempt); a++ {
			if !cl.run.automatic[a] {
				ch = nil
				break
			}
		}
	}
	if ch != nil && ch.entry != "" {
		if en := e.entries[ch.entry]; en == nil || en.state != "ready" {
			ch = nil
		}
	}
	if ch == nil {
		ch = &choice{run: cl.run.id, platform: q.Platform, family: q.Family, key: q.Key, step: "none", attempt: int(cl.c.attempt)}
		if en, step := e.pick(cl, q.Platform, q.Family, q.Key); en != nil {
			ch.entry, ch.step = en.id, step
		}
		e.choices[ck] = ch
	}
	if ch.entry == "" {
		return map[string]any{"entry": nil}, nil
	}
	en := e.entries[ch.entry]
	out := map[string]any{"scope": en.scope, "key": en.key, "step": ch.step, "size": en.size, "sha256": en.sha256}
	if !q.Lookup {
		expires := e.now().Add(RestoreFor)
		out["url"], out["expires"] = e.grant(http.MethodGet, en.object, expires), expires.Format(time.RFC3339)
	}
	return map[string]any{"entry": out}, nil
}

// pick is the first entry, in the restore order, of the run's restore
// chain: each scope's ready entry for the key, then its newest of the
// platform and family. A pull request's entry committed by a run bound
// before it was last closed is never chosen.
func (e *Emulator) pick(cl *caller, platform, family, key string) (*entry, string) {
	var pool []*entry
	for _, en := range e.entries {
		if en.repo != cl.repo.id || en.state != "ready" || en.platform != platform || en.family != family {
			continue
		}
		if closed, ok := e.closures[closureKey(en.repo, en.scope)]; ok && !en.boundAt.After(closed) {
			continue
		}
		pool = append(pool, en)
	}
	slices.SortFunc(pool, func(x, y *entry) int {
		return cmp.Or(y.committedAt.Compare(x.committedAt), strings.Compare(y.id, x.id))
	})
	for _, sc := range cl.binding.chain() {
		exact, newest := steps(sc)
		for _, en := range pool {
			if en.scope == sc && en.key == key {
				return en, exact
			}
		}
		for _, en := range pool {
			if en.scope == sc {
				return en, newest
			}
		}
	}
	return nil, "none"
}

// save is POST /v1/cache/save.
func (e *Emulator) save(cl *caller, body []byte) (any, error) {
	var q struct {
		Family   string `json:"family"`
		Key      string `json:"key"`
		Platform string `json:"platform"`
	}
	if err := decode(body, &q, "save"); err != nil {
		return nil, err
	}
	if !familyPattern.MatchString(q.Family) || !keyPattern.MatchString(q.Key) || !platformPattern.MatchString(q.Platform) {
		return nil, refuse("the body isn't a save request: its family, key or platform isn't the interface's")
	}
	if cl.binding.scope == "read" {
		return nil, refuse("the run's scope, read, saves nothing")
	}
	r := &reservation{id: randomID(), scope: cl.binding.scope, platform: q.Platform, family: q.Family, key: q.Key,
		repo: cl.repo.id, run: cl.run.id, job: cl.c.checkRunID, boundAt: cl.binding.at, expires: e.now().Add(ReserveFor)}
	e.reserved[r.id] = r
	headers := map[string]string{"x-goog-resumable": "start", "x-goog-if-generation-match": "0"}
	return map[string]any{"id": r.id, "url": e.grant(http.MethodPost, uploadObject(r), r.expires), "headers": headers,
		"expires": r.expires.Format(time.RFC3339)}, nil
}

func uploadObject(r *reservation) string { return fmt.Sprintf("uploads/%d/%s", r.repo, r.id) }

// commit is POST /v1/cache/commit.
func (e *Emulator) commit(cl *caller, body []byte) (any, error) {
	var q struct {
		ID     string `json:"id"`
		Size   *int64 `json:"size"`
		SHA256 string `json:"sha256"`
	}
	if err := decode(body, &q, "commit"); err != nil {
		return nil, err
	}
	if !idPattern.MatchString(q.ID) || q.Size == nil || *q.Size < 0 || !sha256Pattern.MatchString(q.SHA256) {
		return nil, refuse("the body isn't a commit request: its id, size or sha256 isn't the interface's")
	}
	r := e.reserved[q.ID]
	if r == nil || r.repo != cl.repo.id {
		return nil, refuse("there's no such reservation")
	}
	if r.job != cl.c.checkRunID || r.run != cl.run.id {
		return nil, refuse("the reservation isn't this job's")
	}
	if r.entry != "" {
		if r.size != *q.Size || r.sha256 != q.SHA256 {
			return nil, refuse("the reservation was committed with another size or SHA-256")
		}
		if en := e.entries[r.entry]; en == nil || en.state != "pending" {
			return nil, refuse("the entry this reservation made isn't pending")
		}
		return map[string]any{"state": "pending"}, nil
	}
	if !e.now().Before(r.expires) {
		delete(e.objects, uploadObject(r))
		return nil, refuse("the reservation has expired")
	}
	data, ok := e.objects[uploadObject(r)]
	if !ok {
		return nil, refuse("nothing has been uploaded for the reservation")
	}
	if int64(len(data)) != *q.Size {
		return nil, refuse("the upload's size, %d, isn't the commit's", len(data))
	}
	en := &entry{id: randomID(), scope: r.scope, platform: r.platform, family: r.family, key: r.key, repo: r.repo, state: "pending",
		size: *q.Size, sha256: q.SHA256, job: r.job, boundAt: r.boundAt, committedAt: e.now()}
	en.object = fmt.Sprintf("entries/%d/%s", en.repo, en.id)
	e.objects[en.object] = data
	delete(e.objects, uploadObject(r))
	r.entry, r.size, r.sha256 = en.id, en.size, en.sha256
	e.entries[en.id] = en
	return map[string]any{"state": "pending"}, nil
}

// settle decides a finished job's pending entries: ready if it succeeded,
// gone otherwise. A pull request's committed by a run bound before it was
// last closed goes too.
func (e *Emulator) settle(jb *job) {
	for id, en := range e.entries {
		if en.job != jb.id || en.state != "pending" {
			continue
		}
		closed, isClosed := e.closures[closureKey(en.repo, en.scope)]
		if jb.conclusion != "success" || (isClosed && !en.boundAt.After(closed)) {
			delete(e.objects, en.object)
			delete(e.entries, id)
			continue
		}
		en.state = "ready"
	}
}
