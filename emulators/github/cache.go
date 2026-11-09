package github

// The Actions cache service, as the current cache service (v2) serves it:
// the Twirp service github.actions.results.api.v1.CacheService at
// ACTIONS_RESULTS_URL, authenticated with a job's ACTIONS_RUNTIME_TOKEN, and
// the signed blob URLs it hands out, which take uploads and serve downloads
// as the blob store behind GitHub's URLs does, by its REST API's Put Blob,
// Put Block, Put Block List, Get Blob and Get Blob Properties.
//
// The protocol follows the source of @actions/cache 6.1.0, which
// actions/cache v6.1.0 (55cc8345863c7cc4c66a329aec7e433d2d1c52a9) pins in
// its package-lock.json: lib/generated/results/api/v1/cache.js for the
// messages, lib/internal/shared/cacheTwirpClient.js for the requests,
// lib/cache.js for restoreCacheV2 and saveCacheV2, and
// lib/internal/uploadUtils.js and downloadUtils.js for the blob transfers.
// What the service decides follows GitHub's documentation of dependency
// caching: its key matching, its restrictions for accessing a cache, its
// cache access for low-trust workflow triggers, and its usage limits and
// eviction policy.

import (
	"bytes"
	"cmp"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// The cache service's limits and the emulator's own choices of what GitHub
// doesn't document.
const (
	// CacheLimit is a repository's cache storage limit unless
	// SetCacheLimit changes it: 10 GB, GitHub's default.
	CacheLimit = 10 << 30
	// CacheUnused is how long an entry not accessed lasts: GitHub removes
	// entries not accessed in over 7 days.
	CacheUnused = 7 * 24 * time.Hour
	// CacheURLFor is how long a signed blob URL lasts, and how long an
	// entry created but not finalized holds its key: the emulator's
	// choice.
	CacheURLFor = time.Hour
	// CacheWriteDenied begins the message of a reservation refused because
	// the job's token is read-only, as @actions/cache's
	// CACHE_WRITE_DENIED_PREFIX matches it.
	CacheWriteDenied = "cache write denied:"

	cacheService  = "github.actions.results.api.v1.CacheService"
	cacheBlobPath = "/_cache/blobs/"
	maxCacheKey   = 512
)

// trustedCacheEvents are the triggers that may create or overwrite caches in
// the default branch's scope; a run of any other event on the default
// branch only reads them.
var trustedCacheEvents = map[string]bool{
	"push": true, "workflow_dispatch": true, "repository_dispatch": true, "delete": true,
	"registry_package": true, "page_build": true, "schedule": true,
}

// CacheEntry is an entry of a repository's Actions cache.
type CacheEntry struct {
	ID int64
	// Scope is the ref of the run that created it, such as
	// refs/heads/main or refs/pull/1/merge.
	Scope        string
	Key          string
	Version      string
	Size         int64
	Created      time.Time
	LastAccessed time.Time
	// Finalized is whether its upload was finalized: only a finalized
	// entry is ever matched.
	Finalized bool

	repo    int64
	data    []byte
	written bool
	// blocks are the uncommitted blocks Put Block staged, and committed
	// the blocks of the list Put Block List last committed.
	blocks    map[string][]byte
	committed map[string][]byte
}

// cacheScope is a scope a runtime token may read, and whether it may write
// it.
type cacheScope struct {
	ref   string
	write bool
}

// runtimeGrant is what a job's ACTIONS_RUNTIME_TOKEN may do in the cache,
// decided when it's minted, as GitHub decides it when it issues the token:
// its scopes, its own first, in the order a lookup searches them.
type runtimeGrant struct {
	job    *WorkflowJob
	repo   int64
	scopes []cacheScope
}

func (e *Emulator) initCache() {
	e.caches = map[int64]*CacheEntry{}
	e.runtimeTokens = map[string]*runtimeGrant{}
	e.cacheLimits = map[int64]int64{}
	e.cacheKey = make([]byte, 32)
	_, _ = rand.Read(e.cacheKey)
}

func (e *Emulator) cacheRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /twirp/"+cacheService+"/{method}", e.cacheTwirp)
	mux.HandleFunc(cacheBlobPath+"{id}", e.cacheBlob)
}

// ResultsURL is ACTIONS_RESULTS_URL for every job: where the cache service
// is, with a trailing slash, as the runner gives it.
func (e *Emulator) ResultsURL() string { return e.URL + "/" }

// RuntimeToken is a job's ACTIONS_RUNTIME_TOKEN, good for the cache while
// its job runs. What it may read and write is decided now, from the job's
// run, as GitHub decides it when it issues the token: its run's ref, which
// it writes unless that's the default branch and the run's event isn't one
// that may write there; a pull request's base branch; and the default
// branch, which every run reads.
func (e *Emulator) RuntimeToken(wj *WorkflowJob) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runtimeToken(wj)
}

// runtimeToken is RuntimeToken; the caller holds e.mu.
func (e *Emulator) runtimeToken(wj *WorkflowJob) string {
	run := wj.Run
	def := "refs/heads/" + run.Repo.DefaultBranch
	own := cacheScope{ref: run.Ref, write: run.Ref != def || trustedCacheEvents[run.Event]}
	g := &runtimeGrant{job: wj, repo: run.Repo.ID, scopes: []cacheScope{own}}
	// A pull request's run is its merge ref's, which is its own branch for
	// the cache, as GitHub's documentation says: it reads its base, never
	// its head branch's pushes.
	if run.Event == "pull_request" && run.BaseRef != "" {
		g.scopes = append(g.scopes, cacheScope{ref: "refs/heads/" + run.BaseRef})
	}
	g.scopes = append(g.scopes, cacheScope{ref: def})
	g.scopes = slices.CompactFunc(g.scopes, func(a, b cacheScope) bool { return a.ref == b.ref })
	var b [20]byte
	_, _ = rand.Read(b[:])
	tok := "rt_" + hex.EncodeToString(b[:])
	e.runtimeTokens[tok] = g
	return tok
}

// SetCacheLimit sets a repository's cache storage limit, as its
// administrators can.
func (e *Emulator) SetCacheLimit(r *Repo, bytes int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cacheLimits[r.ID] = bytes
	e.evictCache(r.ID)
}

// CacheEntries are a repository's cache entries, finalized or not, oldest
// first, for tests to inspect.
func (e *Emulator) CacheEntries(r *Repo) []CacheEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evictCache(r.ID)
	var out []CacheEntry
	for _, c := range e.caches {
		if c.repo == r.ID {
			cp := *c
			cp.data, cp.blocks = nil, nil
			out = append(out, cp)
		}
	}
	slices.SortFunc(out, func(a, b CacheEntry) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// twirpError answers as a Twirp server does: the error's code, its HTTP
// status, and its message as msg.
func twirpError(w http.ResponseWriter, code string, msg string) {
	status := map[string]int{
		"invalid_argument": http.StatusBadRequest, "malformed": http.StatusBadRequest, "bad_route": http.StatusNotFound,
		"unauthenticated": http.StatusUnauthorized, "permission_denied": http.StatusForbidden, "not_found": http.StatusNotFound,
		"already_exists": http.StatusConflict, "internal": http.StatusInternalServerError,
	}[code]
	writeJSON(w, status, map[string]any{"code": code, "msg": msg})
}

// cacheMetadata is a request's metadata, which @actions/cache doesn't send,
// and which the service doesn't trust: the token decides.
type cacheMetadata struct {
	RepositoryID     json.Number `json:"repository_id,omitempty"`
	RepositoryIDJSON json.Number `json:"repositoryId,omitempty"`
	Scope            []struct {
		Scope      string      `json:"scope"`
		Permission json.Number `json:"permission"`
	} `json:"scope,omitempty"`
}

type createCacheEntryRequest struct {
	Metadata *cacheMetadata `json:"metadata,omitempty"`
	Key      string         `json:"key"`
	Version  string         `json:"version"`
}

// Protobuf's JSON takes a field by its proto name or its JSON name; a
// message has each field once.
type finalizeCacheEntryUploadRequest struct {
	Metadata      *cacheMetadata `json:"metadata,omitempty"`
	Key           string         `json:"key"`
	SizeBytes     json.Number    `json:"size_bytes"`
	SizeBytesJSON json.Number    `json:"sizeBytes"`
	Version       string         `json:"version"`
}

type getCacheEntryDownloadURLRequest struct {
	Metadata        *cacheMetadata `json:"metadata,omitempty"`
	Key             string         `json:"key"`
	RestoreKeys     []string       `json:"restore_keys,omitempty"`
	RestoreKeysJSON []string       `json:"restoreKeys,omitempty"`
	Version         string         `json:"version"`
}

// validKey is a key @actions/cache accepts: at most 512 characters, as
// JavaScript counts them, in UTF-16 code units, with no comma.
func validKey(k string) bool {
	return k != "" && len(utf16.Encode([]rune(k))) <= maxCacheKey && !strings.Contains(k, ",")
}

// cacheTwirp serves the cache service's three methods, in JSON, as
// @actions/cache calls them.
func (e *Emulator) cacheTwirp(w http.ResponseWriter, r *http.Request) {
	if ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(ct) != "application/json" {
		twirpError(w, "bad_route", "the emulator serves the cache service's JSON only")
		return
	}
	scheme, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		twirpError(w, "malformed", "the body couldn't be read")
		return
	}
	decode := func(into any) bool {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		dec.UseNumber()
		if dec.Decode(into) != nil || dec.Decode(&struct{}{}) != io.EOF {
			twirpError(w, "malformed", "the body isn't the method's request")
			return false
		}
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tick()
	g := e.runtimeTokens[tok]
	if !strings.EqualFold(scheme, "Bearer") || g == nil || g.job.Status == "completed" {
		twirpError(w, "unauthenticated", "the runtime token isn't valid")
		return
	}
	e.evictCache(g.repo)
	switch r.PathValue("method") {
	case "CreateCacheEntry":
		var q createCacheEntryRequest
		if decode(&q) {
			e.createCacheEntry(w, g, q)
		}
	case "FinalizeCacheEntryUpload":
		var q finalizeCacheEntryUploadRequest
		if decode(&q) {
			if q.SizeBytes == "" {
				q.SizeBytes = q.SizeBytesJSON
			} else if q.SizeBytesJSON != "" {
				twirpError(w, "malformed", "size_bytes is given twice")
				return
			}
			e.finalizeCacheEntry(w, g, q)
		}
	case "GetCacheEntryDownloadURL":
		var q getCacheEntryDownloadURLRequest
		if decode(&q) {
			if len(q.RestoreKeys) > 0 && len(q.RestoreKeysJSON) > 0 {
				twirpError(w, "malformed", "restore_keys is given twice")
				return
			}
			q.RestoreKeys = append(q.RestoreKeys, q.RestoreKeysJSON...)
			e.cacheDownloadURL(w, g, q)
		}
	default:
		twirpError(w, "bad_route", "no such method")
	}
}

// findCache is an entry of a repository's scope with a key and version,
// finalized or not.
func (e *Emulator) findCache(repo int64, scope, key, version string) *CacheEntry {
	for _, c := range e.caches {
		if c.repo == repo && c.Scope == scope && c.Key == key && c.Version == version {
			return c
		}
	}
	return nil
}

func (e *Emulator) createCacheEntry(w http.ResponseWriter, g *runtimeGrant, q createCacheEntryRequest) {
	if !validKey(q.Key) || q.Version == "" {
		twirpError(w, "invalid_argument", "the key or version isn't valid")
		return
	}
	own := g.scopes[0]
	if !own.write {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "signed_upload_url": "",
			"message": CacheWriteDenied + " the run's cache token is read-only for " + own.ref})
		return
	}
	if e.findCache(g.repo, own.ref, q.Key, q.Version) != nil {
		twirpError(w, "already_exists", "cache entry with the same key, version, and scope already exists")
		return
	}
	now := e.clock.Now()
	c := &CacheEntry{ID: e.id(), Scope: own.ref, Key: q.Key, Version: q.Version, Created: now, LastAccessed: now, repo: g.repo,
		blocks: map[string][]byte{}}
	e.caches[c.ID] = c
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "signed_upload_url": e.signBlob(c, "w", now.Add(CacheURLFor)), "message": ""})
}

func (e *Emulator) finalizeCacheEntry(w http.ResponseWriter, g *runtimeGrant, q finalizeCacheEntryUploadRequest) {
	size, err := strconv.ParseInt(q.SizeBytes.String(), 10, 64)
	if !validKey(q.Key) || q.Version == "" || err != nil || size < 0 {
		twirpError(w, "invalid_argument", "the key, version or size isn't valid")
		return
	}
	own := g.scopes[0]
	if !own.write {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "entry_id": "0",
			"message": CacheWriteDenied + " the run's cache token is read-only for " + own.ref})
		return
	}
	c := e.findCache(g.repo, own.ref, q.Key, q.Version)
	switch {
	case c == nil || c.Finalized:
		twirpError(w, "not_found", "no cache entry is being created with that key and version")
		return
	case !c.written:
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "entry_id": "0", "message": "nothing has been uploaded"})
		return
	case int64(len(c.data)) != size:
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "entry_id": "0",
			"message": fmt.Sprintf("the upload's size, %d, isn't %d", len(c.data), size)})
		return
	}
	now := e.clock.Now()
	c.Finalized, c.Size, c.Created, c.LastAccessed, c.blocks, c.committed = true, size, now, now, nil, nil
	e.evictCache(g.repo)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "entry_id": strconv.FormatInt(c.ID, 10), "message": ""})
}

// cacheDownloadURL looks up an entry, in each scope the token reads, in
// order: the key, then entries whose keys it prefixes, then each restore
// key, exact and then as a prefix; of several prefixed, the most recently
// created.
func (e *Emulator) cacheDownloadURL(w http.ResponseWriter, g *runtimeGrant, q getCacheEntryDownloadURLRequest) {
	if !validKey(q.Key) || q.Version == "" || len(q.RestoreKeys) > 9 || slices.ContainsFunc(q.RestoreKeys, func(k string) bool { return !validKey(k) }) {
		twirpError(w, "invalid_argument", "a key or the version isn't valid")
		return
	}
	var found *CacheEntry
	for _, sc := range g.scopes {
		var pool []*CacheEntry
		for _, c := range e.caches {
			if c.repo == g.repo && c.Finalized && c.Scope == sc.ref && c.Version == q.Version {
				pool = append(pool, c)
			}
		}
		slices.SortFunc(pool, func(a, b *CacheEntry) int { return cmp.Or(b.Created.Compare(a.Created), cmp.Compare(b.ID, a.ID)) })
		for _, k := range append([]string{q.Key}, q.RestoreKeys...) {
			if i := slices.IndexFunc(pool, func(c *CacheEntry) bool { return c.Key == k }); i >= 0 {
				found = pool[i]
			} else if i := slices.IndexFunc(pool, func(c *CacheEntry) bool { return strings.HasPrefix(c.Key, k) }); i >= 0 {
				found = pool[i]
			}
			if found != nil {
				break
			}
		}
		if found != nil {
			break
		}
	}
	if found == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "signed_download_url": "", "matched_key": ""})
		return
	}
	found.LastAccessed = e.clock.Now()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "signed_download_url": e.signBlob(found, "r", e.clock.Now().Add(CacheURLFor)),
		"matched_key": found.Key})
}

// evictCache removes a repository's entries not accessed in over 7 days,
// entries created but not finalized within CacheURLFor, and, while its
// finalized entries are over its limit, the least recently accessed until
// they're within it. The caller holds e.mu.
func (e *Emulator) evictCache(repo int64) {
	now := e.clock.Now()
	var live []*CacheEntry
	var total int64
	for id, c := range e.caches {
		if c.repo != repo {
			continue
		}
		if (c.Finalized && now.Sub(c.LastAccessed) > CacheUnused) || (!c.Finalized && now.Sub(c.Created) >= CacheURLFor) {
			delete(e.caches, id)
			continue
		}
		if c.Finalized {
			live = append(live, c)
			total += c.Size
		}
	}
	limit := int64(CacheLimit)
	if l, ok := e.cacheLimits[repo]; ok {
		limit = l
	}
	if total <= limit {
		return
	}
	slices.SortFunc(live, func(a, b *CacheEntry) int {
		return cmp.Or(a.LastAccessed.Compare(b.LastAccessed), cmp.Compare(a.ID, b.ID))
	})
	for _, c := range live {
		if total <= limit {
			break
		}
		delete(e.caches, c.ID)
		total -= c.Size
	}
}

// signBlob is a signed URL for an entry's blob: with permission r, for
// reading it, or w, for writing it, until expires.
func (e *Emulator) signBlob(c *CacheEntry, perm string, expires time.Time) string {
	se := strconv.FormatInt(expires.Unix(), 10)
	return fmt.Sprintf("%s%s%d?sp=%s&se=%s&sig=%s", e.URL, cacheBlobPath, c.ID, perm, se, e.blobSignature(c.ID, perm, se))
}

func (e *Emulator) blobSignature(id int64, perm, se string) string {
	m := hmac.New(sha256.New, e.cacheKey)
	fmt.Fprintf(m, "%d\n%s\n%s", id, perm, se)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// blobError answers as the blob store does: an XML error with its
// code.
func blobError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-ms-error-code", code)
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, msg)
}

// cacheBlob serves a signed blob URL as the blob store does for what
// @actions/cache's transfers send: Put Blob, Put Block and Put Block List
// to write it, and Get Blob, whole or by range, and Get Blob Properties to
// read it.
func (e *Emulator) cacheBlob(w http.ResponseWriter, r *http.Request) {
	// The bytes move outside the emulator's lock, so a slow transfer holds
	// up nothing else: an upload's are read first, and the answer is made
	// in memory and sent after.
	var body []byte
	if r.Method == http.MethodPut {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			blobError(w, http.StatusBadRequest, "InvalidInput", "The body couldn't be read.")
			return
		}
		body = b
	}
	rec := httptest.NewRecorder()
	e.blobLocked(rec, r, body)
	for k, vs := range rec.Header() {
		w.Header()[k] = vs
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

// blobLocked answers a blob request, with its body read, under the
// emulator's lock.
func (e *Emulator) blobLocked(w http.ResponseWriter, r *http.Request, body []byte) {
	q := r.URL.Query()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	perm, se := q.Get("sp"), q.Get("se")
	e.mu.Lock()
	defer e.mu.Unlock()
	exp, perr := strconv.ParseInt(se, 10, 64)
	if err != nil || perr != nil || !hmac.Equal([]byte(q.Get("sig")), []byte(e.blobSignature(id, perm, se))) {
		blobError(w, http.StatusForbidden, "AuthenticationFailed", "The signature isn't valid.")
		return
	}
	if !e.clock.Now().Before(time.Unix(exp, 0)) {
		blobError(w, http.StatusForbidden, "AuthenticationFailed", "Signed expiry time has passed.")
		return
	}
	c := e.caches[id]
	write := r.Method == http.MethodPut
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case !write && !read:
		blobError(w, http.StatusMethodNotAllowed, "UnsupportedHttpVerb", "The resource doesn't support the verb.")
		return
	case write && perm != "w", read && perm != "r":
		blobError(w, http.StatusForbidden, "AuthorizationPermissionMismatch", "This request is not authorized to perform this operation using this permission.")
		return
	case c == nil || (read && !c.Finalized):
		blobError(w, http.StatusNotFound, "BlobNotFound", "The specified blob does not exist.")
		return
	case write && c.Finalized:
		blobError(w, http.StatusForbidden, "AuthorizationFailure", "The entry is finalized, and immutable.")
		return
	}
	if read {
		e.readBlob(w, r, c)
		return
	}
	switch q.Get("comp") {
	case "":
		if r.Header.Get("x-ms-blob-type") != "BlockBlob" {
			blobError(w, http.StatusBadRequest, "MissingRequiredHeader", "An HTTP header that's mandatory for this request is not specified: x-ms-blob-type.")
			return
		}
		c.data, c.written, c.blocks, c.committed = body, true, map[string][]byte{}, map[string][]byte{}
	case "block":
		bid := q.Get("blockid")
		if raw, err := base64.StdEncoding.DecodeString(bid); err != nil || len(raw) == 0 || len(raw) > 64 {
			blobError(w, http.StatusBadRequest, "InvalidQueryParameterValue", "The block ID isn't valid.")
			return
		}
		c.blocks[bid] = body
	case "blocklist":
		var list struct {
			Blocks []struct {
				XMLName xml.Name
				ID      string `xml:",chardata"`
			} `xml:",any"`
		}
		if xml.Unmarshal(body, &list) != nil {
			blobError(w, http.StatusBadRequest, "InvalidXmlDocument", "The block list isn't valid XML.")
			return
		}
		// Latest is a block's uncommitted version if it has one, or else its
		// committed one, so a list sent again, as a retry does, commits the
		// same blob.
		var data []byte
		kept := map[string][]byte{}
		for _, b := range list.Blocks {
			var part []byte
			ok := false
			switch b.XMLName.Local {
			case "Uncommitted":
				part, ok = c.blocks[b.ID]
			case "Committed":
				part, ok = c.committed[b.ID]
			case "Latest":
				if part, ok = c.blocks[b.ID]; !ok {
					part, ok = c.committed[b.ID]
				}
			}
			if !ok {
				blobError(w, http.StatusBadRequest, "InvalidBlockList", "The specified block list is invalid.")
				return
			}
			kept[b.ID] = part
			data = append(data, part...)
		}
		c.data, c.written, c.blocks, c.committed = data, true, map[string][]byte{}, kept
	default:
		blobError(w, http.StatusBadRequest, "InvalidQueryParameterValue", "The comp parameter isn't one the emulator serves.")
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"0x%X"`, e.clock.Now().UnixNano()))
	w.WriteHeader(http.StatusCreated)
}

// readBlob serves a finalized blob, whole, or the one range that Range or
// x-ms-range asks for.
func (e *Emulator) readBlob(w http.ResponseWriter, r *http.Request, c *CacheEntry) {
	data := c.data
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("x-ms-blob-type", "BlockBlob")
	h.Set("Accept-Ranges", "bytes")
	h.Set("ETag", fmt.Sprintf(`"0x%X"`, c.Created.UnixNano()))
	h.Set("Last-Modified", c.Created.UTC().Format(http.TimeFormat))
	rng := r.Header.Get("x-ms-range")
	if rng == "" {
		rng = r.Header.Get("Range")
	}
	status, start, end := http.StatusOK, int64(0), int64(len(data))
	if rng != "" {
		spec, ok := strings.CutPrefix(rng, "bytes=")
		a, b, dash := strings.Cut(spec, "-")
		s, err1 := strconv.ParseInt(a, 10, 64)
		var err2 error
		last := int64(len(data)) - 1
		if b != "" {
			last, err2 = strconv.ParseInt(b, 10, 64)
		}
		last = min(last, int64(len(data))-1)
		if !ok || !dash || err1 != nil || err2 != nil || s < 0 || s >= int64(len(data)) || last < s {
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			blobError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The range specified is invalid for the current size of the resource.")
			return
		}
		status, start, end = http.StatusPartialContent, s, min(last+1, int64(len(data)))
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(data)))
	}
	h.Set("Content-Length", strconv.FormatInt(end-start, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data[start:end])
	}
}
