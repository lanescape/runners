package github

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lanescape/runners/emulators/fault"
)

func (e *Emulator) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app/installations", e.listInstallations)
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", e.createToken)
	mux.HandleFunc("GET /repositories/{id}", e.authed(e.getRepoByID))
	mux.HandleFunc("GET /repos/{owner}/{repo}", e.authed(e.repo(e.getRepo)))
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs/{id}", e.authed(e.repo(e.getRun)))
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", e.authed(e.repo(e.listPulls)))
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", e.authed(e.repo(e.getPull)))
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", e.authed(e.repo(e.listPullCommits)))
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead...}", e.authed(e.repo(e.compare)))
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/ref/{ref...}", e.authed(e.repo(e.getRef)))
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}", e.authed(e.repo(e.getContents)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "Not Found")
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{
		"message":           msg,
		"documentation_url": "https://docs.github.com/rest",
		"status":            strconv.Itoa(code),
	})
}

// authed requires an installation token, and sets the rate limit headers
// GitHub sends with every answer.
func (e *Emulator) authed(h func(http.ResponseWriter, *http.Request, *Installation)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in := e.installation(r)
		if in == nil {
			writeError(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		hd := w.Header()
		hd.Set("X-RateLimit-Limit", "5000")
		hd.Set("X-RateLimit-Remaining", "4999")
		hd.Set("X-RateLimit-Used", "1")
		hd.Set("X-RateLimit-Reset", strconv.FormatInt(e.clock.Now().Add(time.Hour).Unix(), 10))
		hd.Set("X-RateLimit-Resource", "core")
		h(w, r, in)
	}
}

// visible reports whether an installation may read a repository: its own
// account's, or any public one.
func visible(in *Installation, r *Repo) bool {
	return r.Owner == in.Account || !r.Private
}

// repo finds the repository a request names, case-insensitively as GitHub
// does, and holds the emulator's lock while h runs.
func (e *Emulator) repo(h func(http.ResponseWriter, *http.Request, *Repo)) func(http.ResponseWriter, *http.Request, *Installation) {
	return func(w http.ResponseWriter, r *http.Request, in *Installation) {
		owner, name := r.PathValue("owner"), r.PathValue("repo")
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, rp := range e.repos {
			if strings.EqualFold(rp.Owner.Login, owner) && strings.EqualFold(rp.Name, name) && visible(in, rp) {
				h(w, r, rp)
				return
			}
		}
		writeError(w, http.StatusNotFound, "Not Found")
	}
}

func accountJSON(a *Account) map[string]any {
	return map[string]any{"login": a.Login, "id": a.ID, "type": a.Type, "node_id": fmt.Sprintf("A_%d", a.ID)}
}

func (e *Emulator) repoJSON(r *Repo) map[string]any {
	if r == nil {
		return nil
	}
	vis := "public"
	if r.Private {
		vis = "private"
	}
	m := map[string]any{
		"id":             r.ID,
		"node_id":        fmt.Sprintf("R_%d", r.ID),
		"name":           r.Name,
		"full_name":      r.FullName(),
		"owner":          accountJSON(r.Owner),
		"private":        r.Private,
		"visibility":     vis,
		"fork":           r.Parent != nil,
		"default_branch": r.DefaultBranch,
		"url":            e.URL + "/repos/" + r.FullName(),
		"html_url":       "https://github.com/" + r.FullName(),
	}
	return m
}

func (e *Emulator) getRepoByID(w http.ResponseWriter, r *http.Request, in *Installation) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	e.mu.Lock()
	defer e.mu.Unlock()
	rp := e.repos[id]
	if err != nil || rp == nil || !visible(in, rp) {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, e.repoJSON(rp))
}

func (e *Emulator) getRepo(w http.ResponseWriter, _ *http.Request, rp *Repo) {
	writeJSON(w, http.StatusOK, e.repoJSON(rp))
}

func (e *Emulator) runJSON(run *Run) map[string]any {
	var pulls []map[string]any
	for _, p := range run.Pulls {
		pulls = append(pulls, map[string]any{
			"id":     p.Number,
			"number": p.Number,
			"head":   map[string]any{"ref": p.HeadRef, "sha": p.HeadSHA, "repo": map[string]any{"id": p.Head.ID, "name": p.Head.Name}},
			"base":   map[string]any{"ref": p.BaseRef, "sha": p.Base.branches[p.BaseRef], "repo": map[string]any{"id": p.Base.ID, "name": p.Base.Name}},
		})
	}
	var conclusion any
	if run.Conclusion != "" {
		conclusion = run.Conclusion
	}
	return map[string]any{
		"id":              run.ID,
		"name":            run.Path,
		"node_id":         fmt.Sprintf("WFR_%d", run.ID),
		"head_branch":     run.HeadBranch,
		"head_sha":        run.HeadSHA,
		"path":            run.Path,
		"run_number":      run.Number,
		"run_attempt":     run.Attempt,
		"event":           run.Event,
		"status":          run.Status,
		"conclusion":      conclusion,
		"workflow_id":     1,
		"url":             fmt.Sprintf("%s/repos/%s/actions/runs/%d", e.URL, run.Repo.FullName(), run.ID),
		"pull_requests":   pulls,
		"actor":           accountJSON(run.Actor),
		"repository":      e.repoJSON(run.Repo),
		"head_repository": e.repoJSON(run.HeadRepo),
		"created_at":      e.clock.Now().UTC().Format(time.RFC3339),
		"updated_at":      e.clock.Now().UTC().Format(time.RFC3339),
	}
}

func (e *Emulator) getRun(w http.ResponseWriter, r *http.Request, rp *Repo) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run := e.runs[id]
	if err != nil || run == nil || run.Repo != rp {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, e.runJSON(run))
}

func (e *Emulator) pullJSON(p *Pull) map[string]any {
	headLabel := p.HeadRef
	var headRepo any
	if p.Head != nil {
		headLabel = p.Head.Owner.Login + ":" + p.HeadRef
		headRepo = e.repoJSON(p.Head)
	}
	return map[string]any{
		"id":     int64(p.Base.ID)*1000 + int64(p.Number),
		"number": p.Number,
		"state":  p.State,
		"title":  fmt.Sprintf("Pull request %d", p.Number),
		"user":   accountJSON(p.Author),
		"head": map[string]any{
			"label": headLabel, "ref": p.HeadRef, "sha": p.HeadSHA, "repo": headRepo,
		},
		"base": map[string]any{
			"label": p.Base.Owner.Login + ":" + p.BaseRef, "ref": p.BaseRef, "sha": p.Base.branches[p.BaseRef],
			"repo": e.repoJSON(p.Base), "user": accountJSON(p.Base.Owner),
		},
		"draft":      false,
		"merged":     false,
		"commits":    len(e.between(p.Base.branches[p.BaseRef], p.HeadSHA)),
		"created_at": p.Created.UTC().Format(time.RFC3339),
		"html_url":   fmt.Sprintf("https://github.com/%s/pull/%d", p.Base.FullName(), p.Number),
	}
}

func (e *Emulator) getPull(w http.ResponseWriter, r *http.Request, rp *Repo) {
	n, err := strconv.Atoi(r.PathValue("number"))
	p := rp.pulls[n]
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, e.pullJSON(p))
}

// page cuts a list by the request's page and per_page, as GitHub does, and
// sets the Link header that tells the client where the next page is.
func page[T any](w http.ResponseWriter, r *http.Request, items []T, base string) []T {
	q := r.URL.Query()
	per := 30
	if s := q.Get("per_page"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			per = min(n, 100)
		}
	}
	pg := 1
	if s := q.Get("page"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			pg = n
		}
	}
	last := max((len(items)+per-1)/per, 1)
	link := func(n int) string {
		q.Set("page", strconv.Itoa(n))
		q.Set("per_page", strconv.Itoa(per))
		return fmt.Sprintf("<%s%s?%s>", base, r.URL.Path, q.Encode())
	}
	var links []string
	if pg < last {
		links = append(links, link(pg+1)+`; rel="next"`, link(last)+`; rel="last"`)
	}
	if pg > 1 {
		links = append(links, link(1)+`; rel="first"`, link(pg-1)+`; rel="prev"`)
	}
	if len(links) > 0 {
		w.Header().Set("Link", strings.Join(links, ", "))
	}
	start := (pg - 1) * per
	if start >= len(items) {
		return []T{}
	}
	return items[start:min(start+per, len(items))]
}

func (e *Emulator) listPulls(w http.ResponseWriter, r *http.Request, rp *Repo) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	var ps []*Pull
	for _, p := range rp.pulls {
		if state != "all" && p.State != state {
			continue
		}
		if h := q.Get("head"); h != "" && (p.Head == nil || !strings.EqualFold(p.Head.Owner.Login+":"+p.HeadRef, h)) {
			continue
		}
		if b := q.Get("base"); b != "" && p.BaseRef != b {
			continue
		}
		ps = append(ps, p)
	}
	slices.SortFunc(ps, func(a, b *Pull) int { return b.Number - a.Number })
	out := []map[string]any{}
	for _, p := range page(w, r, ps, e.URL) {
		out = append(out, e.pullJSON(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (e *Emulator) commitJSON(sha string) map[string]any {
	c := e.commits[sha]
	var parents []map[string]any
	for _, p := range c.Parents {
		parents = append(parents, map[string]any{"sha": p})
	}
	return map[string]any{
		"sha":     sha,
		"commit":  map[string]any{"message": c.Message, "author": map[string]any{"date": c.Time.UTC().Format(time.RFC3339)}},
		"parents": parents,
	}
}

// maxPullCommits is how many commits GitHub lists for a pull request.
const maxPullCommits = 250

func (e *Emulator) listPullCommits(w http.ResponseWriter, r *http.Request, rp *Repo) {
	n, err := strconv.Atoi(r.PathValue("number"))
	p := rp.pulls[n]
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	shas := e.between(p.Base.branches[p.BaseRef], p.HeadSHA)
	if len(shas) > maxPullCommits {
		shas = shas[:maxPullCommits]
	}
	out := []map[string]any{}
	for _, s := range page(w, r, shas, e.URL) {
		out = append(out, e.commitJSON(s))
	}
	writeJSON(w, http.StatusOK, out)
}

func (e *Emulator) compare(w http.ResponseWriter, r *http.Request, rp *Repo) {
	baseName, headName, ok := strings.Cut(r.PathValue("basehead"), "...")
	if !ok {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	base, ok1 := e.resolve(rp, baseName)
	head, ok2 := e.resolve(rp, headName)
	if !ok1 || !ok2 {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	ahead := e.between(base, head)
	behind := e.between(head, base)
	status := "diverged"
	switch {
	case len(ahead) == 0 && len(behind) == 0:
		status = "identical"
	case len(behind) == 0:
		status = "ahead"
	case len(ahead) == 0:
		status = "behind"
	}
	fromHead := e.reachable(head)
	mergeBase := ""
	best := -1
	for s := range e.reachable(base) {
		if d, ok := fromHead[s]; ok && (best < 0 || d < best) {
			mergeBase, best = s, d
		}
	}
	if mergeBase == "" {
		writeError(w, http.StatusNotFound, fmt.Sprintf("No common ancestor between %s and %s.", baseName, headName))
		return
	}
	commits := []map[string]any{}
	for _, s := range ahead {
		commits = append(commits, e.commitJSON(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":            status,
		"ahead_by":          len(ahead),
		"behind_by":         len(behind),
		"total_commits":     len(ahead),
		"base_commit":       e.commitJSON(base),
		"merge_base_commit": e.commitJSON(mergeBase),
		"commits":           commits,
		"files":             []any{},
	})
}

func (e *Emulator) getRef(w http.ResponseWriter, r *http.Request, rp *Repo) {
	ref := r.PathValue("ref")
	var sha string
	var ok bool
	if b, isBranch := strings.CutPrefix(ref, "heads/"); isBranch {
		sha, ok = rp.branches[b]
	} else if t, isTag := strings.CutPrefix(ref, "tags/"); isTag {
		sha, ok = rp.tags[t]
	}
	if !ok {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref":     "refs/" + ref,
		"node_id": "REF_" + sha[:8],
		"url":     e.URL + "/repos/" + rp.FullName() + "/git/refs/" + ref,
		"object":  map[string]any{"type": "commit", "sha": sha, "url": e.URL + "/repos/" + rp.FullName() + "/git/commits/" + sha},
	})
}

func (e *Emulator) getContents(w http.ResponseWriter, r *http.Request, rp *Repo) {
	path := r.PathValue("path")
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = rp.DefaultBranch
	}
	sha, ok := e.resolve(rp, ref)
	if !ok {
		writeError(w, http.StatusNotFound, "No commit found for the ref "+ref)
		return
	}
	data, ok := e.commits[sha].Files[path]
	if !ok {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	// GitHub wraps base64 content at 60 characters.
	enc := base64.StdEncoding.EncodeToString([]byte(data))
	var lines []string
	for len(enc) > 60 {
		lines = append(lines, enc[:60])
		enc = enc[60:]
	}
	lines = append(lines, enc)
	name := path[strings.LastIndex(path, "/")+1:]
	writeJSON(w, http.StatusOK, map[string]any{
		"type":     "file",
		"encoding": "base64",
		"size":     len(data),
		"name":     name,
		"path":     path,
		"content":  strings.Join(lines, "\n") + "\n",
		"sha":      newSHA(data),
		"url":      e.URL + "/repos/" + rp.FullName() + "/contents/" + url.PathEscape(path),
	})
}

// on is a rule for path: everything under it if it ends with a slash,
// otherwise exactly it.
func on(path string, times int, respond func(http.ResponseWriter, *http.Request)) fault.Rule {
	r := fault.Rule{PathPrefix: path, Times: times, Respond: respond}
	if !strings.HasSuffix(path, "/") {
		r.Match = func(req *http.Request) bool { return req.URL.Path == path }
	}
	return r
}

// RateLimit makes the next times requests to path fail with GitHub's primary
// rate limit answer, resetting after reset. A path ending with a slash means
// every path under it.
func (e *Emulator) RateLimit(path string, times int, reset time.Duration) {
	e.faults.Add(on(path, times, func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-RateLimit-Limit", "5000")
		h.Set("X-RateLimit-Remaining", "0")
		h.Set("X-RateLimit-Used", "5000")
		h.Set("X-RateLimit-Reset", strconv.FormatInt(e.clock.Now().Add(reset).Unix(), 10))
		h.Set("X-RateLimit-Resource", "core")
		writeJSON(w, http.StatusForbidden, map[string]any{
			"message":           "API rate limit exceeded for installation.",
			"documentation_url": "https://docs.github.com/rest/overview/rate-limits-for-the-rest-api",
			"status":            "403",
		})
	}))
}

// SecondaryRateLimit makes the next times requests to path fail with
// GitHub's secondary rate limit answer.
func (e *Emulator) SecondaryRateLimit(path string, times int) {
	e.faults.Add(on(path, times, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusForbidden, map[string]any{
			"message":           "You have exceeded a secondary rate limit. Please wait a few minutes before you try again.",
			"documentation_url": "https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits",
			"status":            "403",
		})
	}))
}

// ServerError makes the next times requests to path fail with code, such as
// 502 or 503.
func (e *Emulator) ServerError(path string, code, times int) {
	e.faults.Add(on(path, times, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, code, http.StatusText(code))
	}))
}
