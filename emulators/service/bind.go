package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// errKeys means the issuer's published keys couldn't be read.
var errKeys = errors.New("the issuer's keys couldn't be read")

// claims are what the emulator uses of a job's token, as the service does.
type claims struct {
	repository string
	repoID     int64
	ownerID    int64
	runID      int64
	attempt    int64
	event      string
	ref        string
	refType    string
	sha        string
	workflow   string
	checkRunID int64
}

func numeric(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0 && strconv.FormatInt(n, 10) == s
}

// token verifies a request's bearer token against the issuer: signed by
// its published keys, unexpired, for the emulator, from the organization.
func (e *Emulator) token(r *http.Request) (*claims, error) {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return nil, errors.New("no token")
	}
	t, err := e.verify.Verify(r.Context(), tok)
	if err != nil && strings.Contains(err.Error(), "fetching keys") {
		// The issuer's keys couldn't be read: nothing is confirmed.
		return nil, errKeys
	}
	if err != nil {
		return nil, fmt.Errorf("the token isn't one the service accepts: %v", err)
	}
	var raw map[string]any
	if err := t.Claims(&raw); err != nil {
		return nil, errors.New("the token's claims aren't readable")
	}
	str := func(k string) string { s, _ := raw[k].(string); return s }
	c := &claims{repository: str("repository"), event: str("event_name"), ref: str("ref"), refType: str("ref_type"), sha: str("sha"),
		workflow: str("job_workflow_ref")}
	for k, into := range map[string]*int64{"repository_id": &c.repoID, "repository_owner_id": &c.ownerID, "run_id": &c.runID,
		"run_attempt": &c.attempt} {
		n, ok := numeric(str(k))
		if !ok {
			return nil, fmt.Errorf("the token isn't one the service accepts: %s isn't a numeric ID", k)
		}
		*into = n
	}
	if s := str("check_run_id"); s != "" {
		n, ok := numeric(s)
		if !ok {
			return nil, errors.New("the token isn't one the service accepts: check_run_id isn't a numeric ID")
		}
		c.checkRunID = n
	}
	if c.ownerID != e.org {
		return nil, errors.New("the token's repository isn't the organization's")
	}
	if c.event == "" || c.ref == "" || c.repository == "" {
		return nil, errors.New("the token isn't one the service accepts: event_name, ref and repository are required")
	}
	return c, nil
}

// binding is a run's bound scope, and for a pull request's, its base.
type binding struct {
	scope    string
	base     string
	event    string
	headRepo int64
	at       time.Time
}

// chain is a binding's restore chain (specs/0002-machines.md, Scope): its
// own scope, if it saves, then its base; main's and read's are main's.
func (b *binding) chain() []string {
	switch {
	case b.scope == "main" || b.scope == "read":
		return []string{"main"}
	case strings.HasPrefix(b.scope, "branch-"):
		return []string{b.scope, "main"}
	case b.base != "":
		return []string{b.scope, b.base}
	}
	return []string{b.scope, "main"}
}

type bindRequest struct {
	PullRequest      int    `json:"pull_request"`
	HeadRepositoryID int64  `json:"head_repository_id"`
	HeadSHA          string `json:"head_sha"`
}

// bind is POST /v1/bind.
func (e *Emulator) bind(w http.ResponseWriter, r *http.Request) {
	refuse := func(reason string) { Refuse(reason)(w, r) }
	c, err := e.token(r)
	if errors.Is(err, errKeys) {
		Unavailable(5*time.Second)(w, r)
		return
	}
	if err != nil {
		refuse(err.Error())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10+1))
	if err != nil || len(body) > 4<<10 {
		refuse("the body is unreadable or too large")
		return
	}
	var req bindRequest
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if trimmed[0] != '{' || dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF {
			refuse("the body isn't a binding request")
			return
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.unavailable {
		Unavailable(5*time.Second)(w, r)
		return
	}
	rp := e.repos[c.repoID]
	if rp == nil {
		refuse("run binding refused: no such repository")
		return
	}
	rn := e.runs[c.runID]
	if rn == nil || rn.repo != rp {
		refuse("run binding refused: no such run of the repository")
		return
	}
	if !e.runInProgress(rn) {
		refuse("run binding refused: the run isn't in progress")
		return
	}
	b := e.bindings[rn.id]
	if b == nil {
		b = e.decide(c, req, rp, rn)
		b.at = e.now()
		e.bindings[rn.id] = b
	}
	writeJSON(w, http.StatusOK, map[string]any{"scope": b.scope})
}

// runInProgress reports whether any of a run's jobs is still going, or it
// has none finished yet.
func (e *Emulator) runInProgress(rn *run) bool {
	any, done := false, true
	for _, j := range e.jobs {
		if j.run == rn && j.attempt == rn.attempt {
			any = true
			if j.status != "completed" {
				done = false
			}
		}
	}
	return !any || !done
}

// decide is the binder's rules (specs/0002-machines.md, Scope), from what
// the emulator holds of GitHub's records.
func (e *Emulator) decide(c *claims, req bindRequest, rp *repo, rn *run) *binding {
	read := &binding{scope: "read", event: rn.event, headRepo: rn.headRepo}
	if rn.event == "pull_request_target" {
		read.headRepo = 0
	}
	if c.event != rn.event {
		return read
	}
	f := parseRunners(rp.runners)
	var b *binding
	switch rn.event {
	case "push":
		branch, ok := strings.CutPrefix(c.ref, "refs/heads/")
		if !ok || c.refType != "branch" || branch == "" || c.sha != rn.sha {
			return read
		}
		// The run's commit must be on the branch now, as GitHub's
		// comparison of the two shows.
		if branch == "main" {
			if !rp.onBranch("main", rn.sha) {
				return read
			}
			return &binding{scope: "main", event: rn.event, headRepo: rp.id}
		}
		name, listed := f.branches[branch]
		if !listed || rn.ref != c.ref || !rp.onBranch(branch, rn.sha) {
			return read
		}
		return &binding{scope: "branch-" + name, event: rn.event, headRepo: rp.id}
	case "pull_request":
		m := pullRef.FindStringSubmatch(c.ref)
		if m == nil || rn.headRepo != rp.id {
			return read
		}
		n, _ := strconv.Atoi(m[1])
		p := rp.pulls[n]
		if p == nil || p.headRepo != rn.headRepo || rn.pull != p {
			return read
		}
		b = &binding{scope: fmt.Sprintf("pr%d", n), base: baseOf(f, p.base), event: rn.event, headRepo: p.headRepo}
	case "pull_request_target":
		prefix := strings.ToLower(c.repository + "/.github/workflows/")
		if f.bootstrap == "" || !strings.HasPrefix(strings.ToLower(f.bootstrap), prefix) || !strings.HasSuffix(f.bootstrap, "@refs/heads/main") ||
			c.workflow != f.bootstrap || req.PullRequest <= 0 || req.HeadRepositoryID <= 0 || !commitSHA.MatchString(req.HeadSHA) {
			return read
		}
		p := rp.pulls[req.PullRequest]
		if p == nil || p.headRepo != req.HeadRepositoryID || p.headSHA != req.HeadSHA {
			return read
		}
		b = &binding{scope: fmt.Sprintf("pr%d", p.number), base: baseOf(f, p.base), event: rn.event, headRepo: p.headRepo}
	default:
		return read
	}
	// A head that's more than one open pull request's can't say which
	// pull request's run this is.
	heads := 0
	for _, p := range rp.pulls {
		if p.open && p.headRepo == b.headRepo && p.headSHA == e.pullHead(rp, b) {
			heads++
		}
	}
	if heads > 1 {
		return &binding{scope: "read", event: rn.event, headRepo: b.headRepo}
	}
	return b
}

func (e *Emulator) pullHead(rp *repo, b *binding) string {
	n, _ := strconv.Atoi(strings.TrimPrefix(b.scope, "pr"))
	return rp.pulls[n].headSHA
}

var (
	pullRef   = regexp.MustCompile(`^refs/pull/([1-9][0-9]{0,8})/merge$`)
	commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
	scopeName = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)
)

// runners is what the service reads of the default branch's
// .github/runners.json.
type runners struct {
	bootstrap string
	branches  map[string]string
}

// parseRunners reads runners.json as the service does: it ignores a
// branch's entry for the default branch, one whose name isn't valid, and
// every entry whose name another shares.
func parseRunners(file string) runners {
	out := runners{branches: map[string]string{}}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(file), &m) != nil {
		return out
	}
	_ = json.Unmarshal(m["bootstrap"], &out.bootstrap)
	var listed map[string]json.RawMessage
	if json.Unmarshal(m["branches"], &listed) != nil {
		return out
	}
	byName := map[string][]string{}
	for branch, raw := range listed {
		var name string
		if json.Unmarshal(raw, &name) == nil && branch != "main" && branch != "" && scopeName.MatchString(name) {
			byName[name] = append(byName[name], branch)
		}
	}
	for name, branches := range byName {
		if len(branches) == 1 {
			out.branches[branches[0]] = name
		}
	}
	return out
}

// baseOf is a pull request's base: its branch's scope if listed, or else
// main's.
func baseOf(f runners, branch string) string {
	if name, ok := f.branches[branch]; ok {
		return "branch-" + name
	}
	return "main"
}
