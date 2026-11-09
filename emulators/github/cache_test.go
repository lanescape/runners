package github_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	ghemu "github.com/lanescape/runners/emulators/github"
)

// These tests call the Actions cache service as @actions/cache 6.1.0 does:
// Twirp's JSON, with proto field names and int64s as strings, at
// ACTIONS_RESULTS_URL with ACTIONS_RUNTIME_TOKEN, and the blob store's
// Put Blob, Put Block, Put Block List and Get Blob at the signed URLs.

const version = "1d3b2a7c0e9f8a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b"

type cacheWorld struct {
	t      *testing.T
	gh     *ghemu.Emulator
	clock  *clockwork.FakeClock
	repo   *ghemu.Repo
	member *ghemu.Account
}

func newCacheWorld(t *testing.T) *cacheWorld {
	t.Helper()
	clock := clockwork.NewFakeClockAt(time.Now())
	gh := ghemu.New(ghemu.WithClock(clock))
	t.Cleanup(gh.Close)
	org := gh.Org(1, "org")
	return &cacheWorld{t: t, gh: gh, clock: clock, repo: gh.Repo(org, 10, "app"), member: gh.User(20, "member")}
}

// job starts a job of run and returns its client.
func (w *cacheWorld) job(run *ghemu.Run) *cacheClient {
	wj := w.gh.Queue(run, ghemu.JobSpec{Labels: []string{"ubuntu-24.04"}})
	w.gh.RunJob(wj)
	return &cacheClient{t: w.t, base: w.gh.ResultsURL(), token: w.gh.RuntimeToken(wj), wj: wj, gh: w.gh}
}

func (w *cacheWorld) push(branch string) *cacheClient { return w.job(w.gh.PushRun(w.repo, branch)) }

// pull opens a pull request from a new branch head into base.
func (w *cacheWorld) pull(base, head string) *ghemu.Pull {
	w.repo.Commit(head, map[string]string{head: "1"})
	return w.gh.PullRequest(w.repo, base, w.repo, head, w.member)
}

type cacheClient struct {
	t     *testing.T
	base  string
	token string
	wj    *ghemu.WorkflowJob
	gh    *ghemu.Emulator
}

// safe is an answer without its signed URLs, which are credentials, for a
// failure's message.
func safe(out map[string]any) map[string]any {
	m := map[string]any{}
	for k, v := range out {
		if strings.HasSuffix(k, "_url") {
			v = "(redacted)"
		}
		m[k] = v
	}
	return m
}

// call is a Twirp request, as cacheTwirpClient.js sends it.
func (c *cacheClient) call(method string, req map[string]any) (int, map[string]any) {
	c.t.Helper()
	body, _ := json.Marshal(req)
	r, _ := http.NewRequest(http.MethodPost, c.base+"twirp/github.actions.results.api.v1.CacheService/"+method, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	return resp.StatusCode, out
}

func (c *cacheClient) create(key string) (int, map[string]any) {
	return c.call("CreateCacheEntry", map[string]any{"key": key, "version": version})
}

func (c *cacheClient) finalize(key string, size int) (int, map[string]any) {
	return c.call("FinalizeCacheEntryUpload", map[string]any{"key": key, "version": version, "size_bytes": fmt.Sprint(size)})
}

// lookup asks for a download URL, and returns the matched key, or "".
func (c *cacheClient) lookup(key string, restore ...string) (string, string) {
	c.t.Helper()
	req := map[string]any{"key": key, "version": version}
	if len(restore) > 0 {
		req["restore_keys"] = restore
	}
	code, out := c.call("GetCacheEntryDownloadURL", req)
	if code != http.StatusOK {
		c.t.Fatalf("GetCacheEntryDownloadURL answered %d: %v", code, safe(out))
	}
	if out["ok"] != true {
		return "", ""
	}
	return out["matched_key"].(string), out["signed_download_url"].(string)
}

func do(t *testing.T, method, target string, body []byte, header map[string]string) (int, []byte, http.Header) {
	t.Helper()
	r, _ := http.NewRequest(method, target, bytes.NewReader(body))
	for k, v := range header {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		// The URL is a credential: only the failure is logged.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		t.Fatalf("%s failed: %v", method, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

// save creates, uploads with Put Blob, and finalizes an entry.
func (c *cacheClient) save(key string, data []byte) {
	c.t.Helper()
	code, out := c.create(key)
	if code != http.StatusOK || out["ok"] != true {
		c.t.Fatalf("CreateCacheEntry answered %d: %v", code, safe(out))
	}
	if code, b, _ := do(c.t, http.MethodPut, out["signed_upload_url"].(string), data, map[string]string{"x-ms-blob-type": "BlockBlob"}); code != http.StatusCreated {
		c.t.Fatalf("Put Blob answered %d: %s", code, b)
	}
	if code, out := c.finalize(key, len(data)); code != http.StatusOK || out["ok"] != true || out["entry_id"] == "0" {
		c.t.Fatalf("FinalizeCacheEntryUpload answered %d: %v", code, safe(out))
	}
}

// restored is what a lookup's URL serves, or nil for no match.
func (c *cacheClient) restored(key string, restore ...string) []byte {
	c.t.Helper()
	matched, url := c.lookup(key, restore...)
	if matched == "" {
		return nil
	}
	code, b, _ := do(c.t, http.MethodGet, url, nil, nil)
	if code != http.StatusOK {
		c.t.Fatalf("Get Blob answered %d: %s", code, b)
	}
	return b
}

func TestTheCacheSavesAndRestores(t *testing.T) {
	w := newCacheWorld(t)
	c := w.push("main")
	data := bytes.Repeat([]byte("cache"), 1000)
	c.save("npm-linux-abc", data)
	got := c.restored("npm-linux-abc")
	if !bytes.Equal(got, data) {
		t.Fatal("restored other bytes")
	}
	// Ranges, as Range or x-ms-range ask.
	_, url := c.lookup("npm-linux-abc")
	for _, h := range []string{"Range", "x-ms-range"} {
		code, b, hd := do(t, http.MethodGet, url, nil, map[string]string{h: "bytes=10-19"})
		if code != http.StatusPartialContent || string(b) != string(data[10:20]) || hd.Get("Content-Range") != fmt.Sprintf("bytes 10-19/%d", len(data)) {
			t.Fatalf("%s: %d %q %s", h, code, b, hd.Get("Content-Range"))
		}
	}
	if code, _, hd := do(t, http.MethodHead, url, nil, nil); code != http.StatusOK || hd.Get("Content-Length") != fmt.Sprint(len(data)) {
		t.Fatalf("HEAD answered %d, length %s", code, hd.Get("Content-Length"))
	}
	// A download carries the blob's ETag, the same each time, as the blob
	// store's SDK checks.
	_, _, h1 := do(t, http.MethodGet, url, nil, nil)
	_, _, h2 := do(t, http.MethodHead, url, nil, nil)
	if h1.Get("ETag") == "" || h1.Get("ETag") != h2.Get("ETag") {
		t.Fatalf("ETags %q and %q", h1.Get("ETag"), h2.Get("ETag"))
	}
	if code, _, _ := do(t, http.MethodGet, url, nil, map[string]string{"Range": fmt.Sprintf("bytes=%d-", len(data))}); code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("a range past the end answered %d", code)
	}
	// A range that ends past the end is served to the end.
	if code, b, _ := do(t, http.MethodGet, url, nil, map[string]string{"Range": "bytes=4990-9223372036854775807"}); code != http.StatusPartialContent || string(b) != string(data[4990:]) {
		t.Fatalf("a range to the largest end answered %d %q", code, b)
	}
	// Another version is another entry.
	if code, out := c.call("GetCacheEntryDownloadURL", map[string]any{"key": "npm-linux-abc", "version": strings.Repeat("0", 64)}); code != http.StatusOK || out["ok"] != false {
		t.Fatalf("another version: %d %v", code, safe(out))
	}
	if es := w.gh.CacheEntries(w.repo); len(es) != 1 || es[0].Scope != "refs/heads/main" || es[0].Size != int64(len(data)) || !es[0].Finalized {
		t.Fatalf("entries %+v", es)
	}
}

func TestTheCacheTakesBlocks(t *testing.T) {
	w := newCacheWorld(t)
	c := w.push("main")
	_, out := c.create("blocks")
	url := out["signed_upload_url"].(string)
	var list strings.Builder
	list.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
	var want []byte
	for i, part := range []string{"first ", "second ", "third"} {
		id := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("block-%06d", i)))
		if code, b, _ := do(t, http.MethodPut, url+"&comp=block&blockid="+id, []byte(part), nil); code != http.StatusCreated {
			t.Fatalf("Put Block answered %d: %s", code, b)
		}
		fmt.Fprintf(&list, "<Latest>%s</Latest>", id)
		want = append(want, part...)
	}
	list.WriteString("</BlockList>")
	// Finalizing before the block list is committed finds nothing uploaded.
	if code, out := c.finalize("blocks", len(want)); code != http.StatusOK || out["ok"] != false {
		t.Fatalf("finalizing before the upload answered %d %v", code, safe(out))
	}
	// A list sent again, as a retry after a lost answer does, commits the
	// same blob from the blocks the first committed.
	for range 2 {
		if code, b, _ := do(t, http.MethodPut, url+"&comp=blocklist", []byte(list.String()), nil); code != http.StatusCreated {
			t.Fatalf("Put Block List answered %d: %s", code, b)
		}
	}
	if code, _, _ := do(t, http.MethodPut, url+"&comp=blocklist", []byte(`<BlockList><Uncommitted>`+base64.StdEncoding.EncodeToString([]byte("block-000000"))+`</Uncommitted></BlockList>`), nil); code != http.StatusBadRequest {
		t.Fatalf("a committed block named as uncommitted answered %d", code)
	}
	if code, out := c.finalize("blocks", len(want)+1); out["ok"] != false {
		t.Fatalf("a wrong size finalized: %d %v", code, safe(out))
	}
	if code, out := c.finalize("blocks", len(want)); code != http.StatusOK || out["ok"] != true {
		t.Fatalf("finalize answered %d %v", code, safe(out))
	}
	if got := c.restored("blocks"); string(got) != string(want) {
		t.Fatalf("restored %q", got)
	}
	// A finalized entry is immutable.
	if code, _, _ := do(t, http.MethodPut, url, []byte("other"), map[string]string{"x-ms-blob-type": "BlockBlob"}); code != http.StatusForbidden {
		t.Fatalf("a write after finalizing answered %d", code)
	}
}

func TestTheCacheRefusesDuplicateKeys(t *testing.T) {
	w := newCacheWorld(t)
	c := w.push("main")
	if code, out := c.create("dup"); code != http.StatusOK || out["ok"] != true {
		t.Fatalf("create answered %d %v", code, safe(out))
	}
	// While it's being created, and once it's finalized.
	if code, out := c.create("dup"); code != http.StatusConflict || out["code"] != "already_exists" {
		t.Fatalf("a second create answered %d %v", code, safe(out))
	}
	c.save("other", []byte("x"))
	if code, out := w.push("main").create("other"); code != http.StatusConflict || out["code"] != "already_exists" {
		t.Fatalf("a create of a finalized key answered %d %v", code, safe(out))
	}
	// Another version, or another scope, may have the key.
	if code, out := c.call("CreateCacheEntry", map[string]any{"key": "other", "version": strings.Repeat("1", 64)}); code != http.StatusOK || out["ok"] != true {
		t.Fatalf("another version answered %d %v", code, safe(out))
	}
	w.repo.Commit("feature", map[string]string{"a": "1"})
	if code, out := w.push("feature").create("other"); code != http.StatusOK || out["ok"] != true {
		t.Fatalf("another scope answered %d %v", code, safe(out))
	}
	// A creation never finalized lets its key go after its URL expires.
	w.clock.Advance(ghemu.CacheURLFor)
	if code, out := c.create("dup"); code != http.StatusOK || out["ok"] != true {
		t.Fatalf("a create after an abandoned one answered %d %v", code, safe(out))
	}
}

func TestTheCacheMatchesPrefixes(t *testing.T) {
	w := newCacheWorld(t)
	c := w.push("main")
	c.save("npm-feature-a9b253ff", []byte("older"))
	w.clock.Advance(time.Second)
	c.save("npm-feature-fd3052de", []byte("newer"))
	w.clock.Advance(time.Second)
	c.save("npm-other", []byte("other"))
	for _, tc := range []struct {
		key     string
		restore []string
		want    string
	}{
		{"npm-feature-a9b253ff", nil, "npm-feature-a9b253ff"},
		// A key that prefixes others matches the newest of them.
		{"npm-feature-", nil, "npm-feature-fd3052de"},
		{"npm-feature-d5ea0750", []string{"npm-feature-", "npm-"}, "npm-feature-fd3052de"},
		{"npm-feature-d5ea0750", []string{"npm-feature-a9b253ff", "npm-"}, "npm-feature-a9b253ff"},
		// An exact restore key beats a newer entry it prefixes.
		{"x", []string{"npm-feature-a9b253ff"}, "npm-feature-a9b253ff"},
		{"npm-feature-d5ea0750", []string{"npm-"}, "npm-other"},
		{"pip-", []string{"cargo-"}, ""},
	} {
		if got, _ := c.lookup(tc.key, tc.restore...); got != tc.want {
			t.Errorf("%s %v matched %q, want %q", tc.key, tc.restore, got, tc.want)
		}
	}
	// Keys follow @actions/cache's rules.
	if code, out := c.create("a,b"); code != http.StatusBadRequest || out["code"] != "invalid_argument" {
		t.Fatalf("a key with a comma answered %d %v", code, safe(out))
	}
	if code, out := c.create(strings.Repeat("k", 513)); code != http.StatusBadRequest {
		t.Fatalf("a long key answered %d %v", code, safe(out))
	}
	// Characters count as JavaScript counts them, in UTF-16 code units.
	if code, out := c.create(strings.Repeat("é", 512)); code != http.StatusOK || out["ok"] != true {
		t.Fatalf("512 two-byte characters answered %d %v", code, safe(out))
	}
	if code, out := c.create(strings.Repeat("😀", 257)); code != http.StatusBadRequest {
		t.Fatalf("514 code units answered %d %v", code, safe(out))
	}
}

func TestTheCachesScopes(t *testing.T) {
	w := newCacheWorld(t)
	w.push("main").save("k-main", []byte("main"))
	w.repo.Commit("feature-a", map[string]string{"a": "1"})
	w.push("feature-a").save("k-feature-a", []byte("feature-a"))
	// A pull request into main, and one into feature-a, each saving in its
	// merge ref's scope.
	intoMain := w.pull("main", "fix")
	pr := w.job(w.gh.PullRequestRun(intoMain))
	pr.save("k-pr", []byte("pr"))
	intoA := w.pull("feature-a", "feature-b")
	prA := w.job(w.gh.PullRequestRun(intoA))
	prA.save("k-pr-a", []byte("pr-a"))
	w.repo.Commit("feature-c", map[string]string{"c": "1"})
	w.repo.Tag("v1", w.repo.Head("main"))
	readers := map[string]*cacheClient{
		"main push":            w.push("main"),
		"feature-a push":       w.push("feature-a"),
		"feature-c push":       w.push("feature-c"),
		"tag":                  w.job(w.gh.TagRun(w.repo, "v1")),
		"pull request to main": w.job(w.gh.PullRequestRun(intoMain)),
		"pull request to a":    w.job(w.gh.PullRequestRun(intoA)),
		"another pull request": w.job(w.gh.PullRequestRun(w.pull("main", "another"))),
	}
	want := map[string][]string{
		"main push":            {"k-main"},
		"feature-a push":       {"k-main", "k-feature-a"},
		"feature-c push":       {"k-main"},
		"tag":                  {"k-main"},
		"pull request to main": {"k-main", "k-pr"},
		"pull request to a":    {"k-main", "k-feature-a", "k-pr-a"},
		"another pull request": {"k-main"},
	}
	for who, c := range readers {
		var got []string
		for _, k := range []string{"k-main", "k-feature-a", "k-pr", "k-pr-a"} {
			if m, _ := c.lookup(k); m == k {
				got = append(got, k)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want[who]) {
			t.Errorf("%s reads %v, want %v", who, got, want[who])
		}
	}
	// A lookup searches the run's own scope, then its base's, then the
	// default branch's.
	w.push("main").save("shared-1", []byte("main"))
	w.clock.Advance(time.Second)
	w.push("feature-a").save("shared-2", []byte("feature-a"))
	if got := readers["pull request to a"].restored("shared-", "shared-"); string(got) != "feature-a" {
		t.Fatalf("a pull request into feature-a restored %q", got)
	}
	prA.save("shared-0", []byte("own"))
	if got := readers["pull request to a"].restored("shared-"); string(got) != "own" {
		t.Fatalf("a pull request's own entry wasn't first: %q", got)
	}
	if got := readers["feature-a push"].restored("shared-"); string(got) != "feature-a" {
		t.Fatalf("feature-a restored %q", got)
	}
}

func TestUntrustedTriggersOnlyReadTheDefaultBranch(t *testing.T) {
	w := newCacheWorld(t)
	w.push("main").save("k", []byte("main"))
	pr := w.pull("main", "fix")
	for _, c := range []*cacheClient{
		w.job(w.gh.PullRequestTargetRun(pr)),
		w.job(w.gh.EventRun(w.repo, "issue_comment", "main")),
		w.job(w.gh.EventRun(w.repo, "workflow_run", "main")),
	} {
		if m, _ := c.lookup("k"); m != "k" {
			t.Fatalf("%s didn't read the default branch", c.wj.Run.Event)
		}
		code, out := c.create("poison")
		if code != http.StatusOK || out["ok"] != false || !strings.HasPrefix(out["message"].(string), ghemu.CacheWriteDenied) {
			t.Fatalf("%s's create answered %d %v", c.wj.Run.Event, code, safe(out))
		}
	}
	// Trusted triggers write it; a pull_request run writes its merge ref.
	for _, event := range []string{"workflow_dispatch", "schedule", "repository_dispatch"} {
		w.job(w.gh.EventRun(w.repo, event, "main")).save("k-"+event, []byte(event))
	}
	w.job(w.gh.PullRequestRun(pr)).save("k-pr", []byte("pr"))
	scopes := map[string]string{}
	for _, e := range w.gh.CacheEntries(w.repo) {
		scopes[e.Key] = e.Scope
	}
	if scopes["k-schedule"] != "refs/heads/main" || scopes["k-pr"] != fmt.Sprintf("refs/pull/%d/merge", pr.Number) || scopes["poison"] != "" {
		t.Fatalf("scopes %v", scopes)
	}
	// An untrusted trigger on another branch writes that branch's scope.
	w.repo.Commit("other", map[string]string{"o": "1"})
	w.job(w.gh.EventRun(w.repo, "issue_comment", "other")).save("k-other", []byte("other"))
}

func TestTheCacheEvicts(t *testing.T) {
	w := newCacheWorld(t)
	c := w.push("main")
	w.gh.SetCacheLimit(w.repo, 25)
	c.save("a", bytes.Repeat([]byte("a"), 10))
	w.clock.Advance(time.Second)
	c.save("b", bytes.Repeat([]byte("b"), 10))
	w.clock.Advance(time.Second)
	// Reading a makes b the least recently used.
	if m, _ := c.lookup("a"); m != "a" {
		t.Fatal("no a")
	}
	w.clock.Advance(time.Second)
	c.save("c", bytes.Repeat([]byte("c"), 10))
	var keys []string
	for _, e := range w.gh.CacheEntries(w.repo) {
		keys = append(keys, e.Key)
	}
	if fmt.Sprint(keys) != "[a c]" {
		t.Fatalf("over the limit, kept %v", keys)
	}
	// Eviction stops once what's left is within the limit, at it
	// exactly.
	w.gh.SetCacheLimit(w.repo, 20)
	w.clock.Advance(time.Second)
	c.save("d", bytes.Repeat([]byte("d"), 10))
	keys = nil
	for _, e := range w.gh.CacheEntries(w.repo) {
		keys = append(keys, e.Key)
	}
	if fmt.Sprint(keys) != "[c d]" {
		t.Fatalf("at the limit, kept %v", keys)
	}
	// An entry not accessed in over 7 days goes.
	w.gh.SetCacheLimit(w.repo, ghemu.CacheLimit)
	w.clock.Advance(ghemu.CacheUnused - time.Second)
	if m, _ := c.lookup("c"); m != "c" {
		t.Fatal("c went early")
	}
	w.clock.Advance(2 * time.Second)
	keys = nil
	for _, e := range w.gh.CacheEntries(w.repo) {
		keys = append(keys, e.Key)
	}
	if fmt.Sprint(keys) != "[c]" {
		t.Fatalf("after 7 days unused, kept %v", keys)
	}
}

func TestTheCachesTokensAndURLs(t *testing.T) {
	w := newCacheWorld(t)
	c := w.push("main")
	c.save("k", []byte("data"))
	_, url := c.lookup("k")
	_, out := c.create("upload")
	upload := out["signed_upload_url"].(string)
	// The download URL only reads, the upload URL only writes, and neither
	// with a changed signature.
	if code, _, _ := do(t, http.MethodPut, url, []byte("x"), map[string]string{"x-ms-blob-type": "BlockBlob"}); code != http.StatusForbidden {
		t.Fatalf("a write with the download URL answered %d", code)
	}
	if code, _, _ := do(t, http.MethodGet, upload, nil, nil); code != http.StatusForbidden {
		t.Fatalf("a read with the upload URL answered %d", code)
	}
	if code, _, _ := do(t, http.MethodGet, strings.Replace(url, "sp=r", "sp=w", 1), nil, nil); code != http.StatusForbidden {
		t.Fatalf("a changed URL answered %d", code)
	}
	if code, _, _ := do(t, http.MethodPut, upload, []byte("x"), nil); code != http.StatusBadRequest {
		t.Fatalf("a Put Blob without its blob type answered %d", code)
	}
	w.clock.Advance(ghemu.CacheURLFor)
	if code, _, _ := do(t, http.MethodGet, url, nil, nil); code != http.StatusForbidden {
		t.Fatalf("an expired URL answered %d", code)
	}
	// A token is good while its job runs, and only for its repository.
	other := w.gh.Repo(w.gh.Account(1), 11, "other")
	oc := w.job(w.gh.PushRun(other, "main"))
	if m, _ := oc.lookup("k"); m != "" {
		t.Fatal("another repository's job read the entry")
	}
	w.gh.CompleteJob(c.wj, "success")
	if code, out := c.create("late"); code != http.StatusUnauthorized || out["code"] != "unauthenticated" {
		t.Fatalf("a finished job's token answered %d %v", code, safe(out))
	}
	bad := &cacheClient{t: t, base: c.base, token: "guess", gh: w.gh}
	if code, _ := bad.create("x"); code != http.StatusUnauthorized {
		t.Fatalf("a guessed token answered %d", code)
	}
	// Unknown fields are malformed, as protobuf's JSON says.
	if code, out := oc.call("CreateCacheEntry", map[string]any{"key": "k", "version": version, "extra": 1}); code != http.StatusBadRequest || out["code"] != "malformed" {
		t.Fatalf("an unknown field answered %d %v", code, safe(out))
	}
}

// A stalled upload holds up nothing else: its bytes are read outside the
// emulator's lock.
func TestAStalledUploadHoldsUpNothing(t *testing.T) {
	w := newCacheWorld(t)
	c := w.push("main")
	_, out := c.create("slow")
	pr, pw := io.Pipe()
	req, _ := http.NewRequest(http.MethodPut, out["signed_upload_url"].(string), pr)
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	done := make(chan int)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	_, _ = pw.Write([]byte("part"))
	looked := make(chan struct{})
	go func() {
		c.lookup("other")
		w.gh.Read(func(*ghemu.State) {})
		close(looked)
	}()
	select {
	case <-looked:
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled upload held up a lookup")
	}
	_, _ = pw.Write([]byte(" and the rest"))
	pw.Close()
	if code := <-done; code != http.StatusCreated {
		t.Fatalf("the upload answered %d", code)
	}
	if code, out := c.finalize("slow", len("part and the rest")); code != http.StatusOK || out["ok"] != true {
		t.Fatalf("finalize answered %d %v", code, safe(out))
	}
}
