// Package storage emulates Cloud Storage's JSON and XML APIs, as
// cloud.google.com/go/storage calls them over HTTP: buckets of objects with
// generations, preconditions, reads, lists by prefix and deletes.
//
// Versioning isn't emulated: a bucket keeps only each object's live
// generation, as the service's buckets do.
package storage

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
	"google.golang.org/api/option"

	"github.com/lanescape/runners/emulators/fault"
)

// Emulator is a running Cloud Storage emulator.
type Emulator struct {
	// URL is the emulator's base address, with no trailing slash.
	URL string

	srv    *httptest.Server
	faults fault.Injector
	clock  clockwork.Clock

	mu      sync.Mutex
	buckets map[string]*bucket
	lastGen int64
}

type bucket struct {
	name    string
	created time.Time
	objects map[string]*object
}

type object struct {
	name            string
	data            []byte
	generation      int64
	metageneration  int64
	contentType     string
	contentEncoding string
	cacheControl    string
	metadata        map[string]string
	created         time.Time
	updated         time.Time
}

// Option configures an Emulator.
type Option func(*Emulator)

// WithClock sets the clock that stamps objects' times.
func WithClock(c clockwork.Clock) Option { return func(e *Emulator) { e.clock = c } }

// New starts an emulator. Close it when done.
func New(opts ...Option) *Emulator {
	e := &Emulator{clock: clockwork.NewRealClock(), buckets: map[string]*bucket{}}
	for _, o := range opts {
		o(e)
	}
	e.srv = httptest.NewServer(e.faults.Wrap(http.HandlerFunc(e.serve)))
	e.URL = e.srv.URL
	return e
}

// Close stops the emulator.
func (e *Emulator) Close() { e.srv.Close() }

// Faults is where tests make requests fail.
func (e *Emulator) Faults() *fault.Injector { return &e.faults }

// ClientOptions are what cloud.google.com/go/storage needs to talk to the
// emulator instead of Google.
func (e *Emulator) ClientOptions() []option.ClientOption {
	return []option.ClientOption{
		option.WithEndpoint(e.URL + "/storage/v1/"),
		option.WithoutAuthentication(),
	}
}

// CreateBucket makes an empty bucket.
func (e *Emulator) CreateBucket(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.buckets[name]; !ok {
		e.buckets[name] = &bucket{name: name, created: e.clock.Now(), objects: map[string]*object{}}
	}
}

// Object returns an object's contents and generation, for tests to inspect.
func (e *Emulator) Object(bucketName, name string) (data []byte, generation int64, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b := e.buckets[bucketName]
	if b == nil {
		return nil, 0, false
	}
	o := b.objects[name]
	if o == nil {
		return nil, 0, false
	}
	return bytes.Clone(o.data), o.generation, true
}

// Put writes an object directly, as another writer would, and returns its
// generation.
func (e *Emulator) Put(bucketName, name string, data []byte) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	b := e.buckets[bucketName]
	if b == nil {
		panic("storage emulator: no bucket " + bucketName)
	}
	return e.write(b, name, data, "application/octet-stream", objectMeta{}).generation
}

// Names lists a bucket's object names in order, for tests to inspect.
func (e *Emulator) Names(bucketName string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	b := e.buckets[bucketName]
	if b == nil {
		return nil
	}
	var names []string
	for n := range b.objects {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// nextGeneration returns a new generation. Like Cloud Storage's, generations
// are microsecond timestamps, made strictly increasing.
func (e *Emulator) nextGeneration() int64 {
	g := e.clock.Now().UnixMicro()
	if g <= e.lastGen {
		g = e.lastGen + 1
	}
	e.lastGen = g
	return g
}

type objectMeta struct {
	ContentType     string            `json:"contentType"`
	ContentEncoding string            `json:"contentEncoding"`
	CacheControl    string            `json:"cacheControl"`
	Metadata        map[string]string `json:"metadata"`
}

func (e *Emulator) write(b *bucket, name string, data []byte, contentType string, m objectMeta) *object {
	now := e.clock.Now()
	o := &object{
		name:            name,
		data:            data,
		generation:      e.nextGeneration(),
		metageneration:  1,
		contentType:     contentType,
		contentEncoding: m.ContentEncoding,
		cacheControl:    m.CacheControl,
		metadata:        m.Metadata,
		created:         now,
		updated:         now,
	}
	b.objects[name] = o
	return o
}

// apiError is an error in the form the JSON API returns, and that the XML API
// returns as XML.
type apiError struct {
	code    int
	reason  string
	xmlCode string
	message string
}

func (a *apiError) Error() string { return a.message }

func errNoBucket(name string) *apiError {
	return &apiError{http.StatusNotFound, "notFound", "NoSuchBucket", "The specified bucket does not exist."}
}

func errNoObject(b, o string) *apiError {
	return &apiError{http.StatusNotFound, "notFound", "NoSuchKey", fmt.Sprintf("No such object: %s/%s", b, o)}
}

var errPrecondition = &apiError{http.StatusPreconditionFailed, "conditionNotMet", "PreconditionFailed",
	"At least one of the pre-conditions you specified did not hold."}

var errNotModified = &apiError{code: http.StatusNotModified}

func errInvalid(msg string) *apiError {
	return &apiError{http.StatusBadRequest, "invalid", "InvalidArgument", msg}
}

func errNotEmulated(what string) *apiError {
	return &apiError{http.StatusNotImplemented, "notImplemented", "NotImplemented", what + " isn't emulated"}
}

func writeJSONError(w http.ResponseWriter, err error) {
	var a *apiError
	if !errors.As(err, &a) {
		a = &apiError{http.StatusInternalServerError, "backendError", "InternalError", err.Error()}
	}
	if a.code == http.StatusNotModified {
		w.WriteHeader(a.code)
		return
	}
	body := map[string]any{"error": map[string]any{
		"code":    a.code,
		"message": a.message,
		"errors":  []map[string]any{{"message": a.message, "domain": "global", "reason": a.reason}},
	}}
	writeJSON(w, a.code, body)
}

func writeXMLError(w http.ResponseWriter, r *http.Request, err error) {
	var a *apiError
	if !errors.As(err, &a) {
		a = &apiError{http.StatusInternalServerError, "backendError", "InternalError", err.Error()}
	}
	if a.code == http.StatusNotModified {
		w.WriteHeader(a.code)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
	w.WriteHeader(a.code)
	if r.Method != http.MethodHead {
		fmt.Fprintf(w, "<?xml version='1.0' encoding='UTF-8'?><Error><Code>%s</Code><Message>%s</Message></Error>", a.xmlCode, a.message)
	}
}

// JSONError answers as the JSON API does with an error, such as 503
// backendError or 429 rateLimitExceeded, for fault rules.
func JSONError(code int, reason, message string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSONError(w, &apiError{code: code, reason: reason, xmlCode: reason, message: message})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (e *Emulator) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.EscapedPath()
	switch {
	case strings.HasPrefix(p, "/upload/storage/v1/b/"):
		e.serveUpload(w, r, strings.TrimPrefix(p, "/upload/storage/v1/b/"))
	case strings.HasPrefix(p, "/storage/v1/b/"):
		e.serveJSON(w, r, strings.TrimPrefix(p, "/storage/v1/b/"))
	default:
		e.serveXML(w, r, strings.TrimPrefix(p, "/"))
	}
}

// splitObjectPath splits "<bucket>/o/<object>" with the object escaped.
func splitObjectPath(rest string) (bucketName, objectName string, hasObjects, hasObject bool, err error) {
	bucketName, after, found := strings.Cut(rest, "/")
	if bucketName, err = url.PathUnescape(bucketName); err != nil {
		return "", "", false, false, err
	}
	if !found {
		return bucketName, "", false, false, nil
	}
	if after == "o" {
		return bucketName, "", true, false, nil
	}
	escaped, ok := strings.CutPrefix(after, "o/")
	if !ok {
		return "", "", false, false, errors.New("unknown path")
	}
	objectName, err = url.PathUnescape(escaped)
	return bucketName, objectName, true, true, err
}

func (e *Emulator) serveJSON(w http.ResponseWriter, r *http.Request, rest string) {
	bucketName, objectName, hasObjects, hasObject, err := splitObjectPath(rest)
	if err != nil {
		writeJSONError(w, &apiError{http.StatusNotFound, "notFound", "NotFound", "Not Found"})
		return
	}
	switch {
	case !hasObjects && r.Method == http.MethodGet:
		e.getBucket(w, bucketName)
	case hasObjects && !hasObject && r.Method == http.MethodGet:
		e.list(w, r, bucketName)
	case hasObject && r.Method == http.MethodGet && r.URL.Query().Get("alt") == "media":
		e.download(w, r, bucketName, objectName, queryConds(r.URL.Query()), writeJSONError)
	case hasObject && r.Method == http.MethodGet:
		e.getMetadata(w, r, bucketName, objectName)
	case hasObject && r.Method == http.MethodDelete:
		e.delete(w, r, bucketName, objectName)
	default:
		writeJSONError(w, errNotEmulated(r.Method+" "+r.URL.Path))
	}
}

func (e *Emulator) serveXML(w http.ResponseWriter, r *http.Request, rest string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeXMLError(w, r, errNotEmulated("XML "+r.Method))
		return
	}
	bucketEsc, objectEsc, ok := strings.Cut(rest, "/")
	if !ok || objectEsc == "" {
		writeXMLError(w, r, errNotEmulated("XML bucket requests"))
		return
	}
	bucketName, err1 := url.PathUnescape(bucketEsc)
	objectName, err2 := url.PathUnescape(objectEsc)
	if err1 != nil || err2 != nil {
		writeXMLError(w, r, errInvalid("bad path"))
		return
	}
	c, err := headerConds(r.Header)
	if err == nil {
		c.generation, err = optInt(r.URL.Query().Get("generation"))
	}
	if err != nil {
		writeXMLError(w, r, errInvalid(err.Error()))
		return
	}
	e.download(w, r, bucketName, objectName, c, func(w http.ResponseWriter, err error) { writeXMLError(w, r, err) })
}

// conds are an operation's preconditions; nil fields are absent.
type conds struct {
	generation             *int64 // a read of a specific generation
	ifGenerationMatch      *int64
	ifGenerationNotMatch   *int64
	ifMetagenerationMatch  *int64
	ifMetagenerationNotMat *int64
	err                    error
}

func optInt(s string) (*int64, error) {
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid number %q", s)
	}
	return &v, nil
}

func queryConds(q url.Values) conds {
	var c conds
	var errs []error
	get := func(name string) *int64 {
		v, err := optInt(q.Get(name))
		errs = append(errs, err)
		return v
	}
	c.generation = get("generation")
	c.ifGenerationMatch = get("ifGenerationMatch")
	c.ifGenerationNotMatch = get("ifGenerationNotMatch")
	c.ifMetagenerationMatch = get("ifMetagenerationMatch")
	c.ifMetagenerationNotMat = get("ifMetagenerationNotMatch")
	c.err = errors.Join(errs...)
	return c
}

func headerConds(h http.Header) (conds, error) {
	var c conds
	var errs []error
	get := func(name string) *int64 {
		v, err := optInt(h.Get(name))
		errs = append(errs, err)
		return v
	}
	c.ifGenerationMatch = get("x-goog-if-generation-match")
	c.ifMetagenerationMatch = get("x-goog-if-metageneration-match")
	return c, errors.Join(errs...)
}

// checkRead applies preconditions to a read, metadata request or delete of
// an existing object. A failed "not match" condition on a read is 304, as
// Cloud Storage answers.
func (c conds) checkExisting(o *object, read bool) error {
	if c.generation != nil && *c.generation != o.generation {
		return errNoObject("", o.name)
	}
	if c.ifGenerationMatch != nil && *c.ifGenerationMatch != o.generation {
		return errPrecondition
	}
	if c.ifMetagenerationMatch != nil && *c.ifMetagenerationMatch != o.metageneration {
		return errPrecondition
	}
	if c.ifGenerationNotMatch != nil && *c.ifGenerationNotMatch == o.generation {
		if read {
			return errNotModified
		}
		return errPrecondition
	}
	if c.ifMetagenerationNotMat != nil && *c.ifMetagenerationNotMat == o.metageneration {
		if read {
			return errNotModified
		}
		return errPrecondition
	}
	return nil
}

// checkWrite applies preconditions to a write. A missing object has
// generation 0, so ifGenerationMatch=0 means create only.
func (c conds) checkWrite(o *object) error {
	gen, metagen := int64(0), int64(0)
	if o != nil {
		gen, metagen = o.generation, o.metageneration
	}
	if c.ifGenerationMatch != nil && *c.ifGenerationMatch != gen {
		return errPrecondition
	}
	// The "not match" conditions, and metageneration ones, fail when there's
	// no live object, as Cloud Storage's do.
	if c.ifGenerationNotMatch != nil && (o == nil || *c.ifGenerationNotMatch == gen) {
		return errPrecondition
	}
	if c.ifMetagenerationMatch != nil && (o == nil || *c.ifMetagenerationMatch != metagen) {
		return errPrecondition
	}
	if c.ifMetagenerationNotMat != nil && (o == nil || *c.ifMetagenerationNotMat == metagen) {
		return errPrecondition
	}
	return nil
}

func (e *Emulator) lookup(bucketName, objectName string) (*bucket, *object, error) {
	b := e.buckets[bucketName]
	if b == nil {
		return nil, nil, errNoBucket(bucketName)
	}
	o := b.objects[objectName]
	if o == nil {
		return b, nil, errNoObject(bucketName, objectName)
	}
	return b, o, nil
}

func (e *Emulator) getBucket(w http.ResponseWriter, name string) {
	e.mu.Lock()
	b := e.buckets[name]
	e.mu.Unlock()
	if b == nil {
		writeJSONError(w, errNoBucket(name))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":                  "storage#bucket",
		"id":                    b.name,
		"name":                  b.name,
		"timeCreated":           b.created.UTC().Format(time.RFC3339Nano),
		"updated":               b.created.UTC().Format(time.RFC3339Nano),
		"location":              "NORTHAMERICA-NORTHEAST1",
		"storageClass":          "STANDARD",
		"metageneration":        "1",
		"iamConfiguration":      map[string]any{"publicAccessPrevention": "enforced", "uniformBucketLevelAccess": map[string]any{"enabled": true}},
		"locationType":          "region",
		"projectNumber":         "0",
		"softDeletePolicy":      map[string]any{"retentionDurationSeconds": "0"},
		"hierarchicalNamespace": map[string]any{"enabled": false},
	})
}

func (e *Emulator) resource(b *bucket, o *object) map[string]any {
	sum := md5.Sum(o.data)
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32.Checksum(o.data, crc32.MakeTable(crc32.Castagnoli)))
	res := map[string]any{
		"kind":           "storage#object",
		"id":             fmt.Sprintf("%s/%s/%d", b.name, o.name, o.generation),
		"selfLink":       fmt.Sprintf("%s/storage/v1/b/%s/o/%s", e.URL, b.name, url.PathEscape(o.name)),
		"mediaLink":      fmt.Sprintf("%s/download/storage/v1/b/%s/o/%s?generation=%d&alt=media", e.URL, b.name, url.PathEscape(o.name), o.generation),
		"name":           o.name,
		"bucket":         b.name,
		"generation":     strconv.FormatInt(o.generation, 10),
		"metageneration": strconv.FormatInt(o.metageneration, 10),
		"contentType":    o.contentType,
		"storageClass":   "STANDARD",
		"size":           strconv.Itoa(len(o.data)),
		"md5Hash":        base64.StdEncoding.EncodeToString(sum[:]),
		"crc32c":         base64.StdEncoding.EncodeToString(crc[:]),
		"etag":           etag(o),
		"timeCreated":    o.created.UTC().Format(time.RFC3339Nano),
		"updated":        o.updated.UTC().Format(time.RFC3339Nano),
	}
	if o.contentEncoding != "" {
		res["contentEncoding"] = o.contentEncoding
	}
	if o.cacheControl != "" {
		res["cacheControl"] = o.cacheControl
	}
	if len(o.metadata) > 0 {
		res["metadata"] = o.metadata
	}
	return res
}

func etag(o *object) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(o.generation))
	binary.BigEndian.PutUint64(b[8:], uint64(o.metageneration))
	return base64.RawStdEncoding.EncodeToString(b[:])
}

func (e *Emulator) getMetadata(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	c := queryConds(r.URL.Query())
	if c.err != nil {
		writeJSONError(w, errInvalid(c.err.Error()))
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	b, o, err := e.lookup(bucketName, objectName)
	if err == nil {
		err = c.checkExisting(o, true)
	}
	if err != nil {
		writeJSONError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e.resource(b, o))
}

func (e *Emulator) download(w http.ResponseWriter, r *http.Request, bucketName, objectName string, c conds, fail func(http.ResponseWriter, error)) {
	if c.err != nil {
		fail(w, errInvalid(c.err.Error()))
		return
	}
	e.mu.Lock()
	_, o, err := e.lookup(bucketName, objectName)
	if err == nil {
		err = c.checkExisting(o, true)
	}
	var snapshot object
	if err == nil {
		snapshot = *o
	}
	e.mu.Unlock()
	if err != nil {
		fail(w, err)
		return
	}
	o = &snapshot
	data := o.data
	h := w.Header()
	h.Set("Content-Type", o.contentType)
	h.Set("Last-Modified", o.updated.UTC().Format(http.TimeFormat))
	h.Set("ETag", `"`+etag(o)+`"`)
	h.Set("X-Goog-Generation", strconv.FormatInt(o.generation, 10))
	h.Set("X-Goog-Metageneration", strconv.FormatInt(o.metageneration, 10))
	h.Set("X-Goog-Stored-Content-Length", strconv.Itoa(len(data)))
	h.Set("X-Goog-Stored-Content-Encoding", "identity")
	h.Set("X-Goog-Storage-Class", "STANDARD")
	sum := md5.Sum(data)
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)))
	h.Add("X-Goog-Hash", "crc32c="+base64.StdEncoding.EncodeToString(crc[:]))
	h.Add("X-Goog-Hash", "md5="+base64.StdEncoding.EncodeToString(sum[:]))
	for k, v := range o.metadata {
		h.Set("X-Goog-Meta-"+k, v)
	}
	if rng := r.Header.Get("Range"); rng != "" {
		start, end, ok := parseRange(rng, int64(len(data)))
		if !ok {
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			fail(w, &apiError{http.StatusRequestedRangeNotSatisfiable, "requestedRangeNotSatisfiable", "InvalidRange",
				"The requested range cannot be satisfied."})
			return
		}
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		if r.Method != http.MethodHead {
			_, _ = w.Write(data[start : end+1])
		}
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

// parseRange reads a single "bytes=" range: a-b, a- or -n.
func parseRange(h string, size int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 || size == 0 {
			return 0, 0, false
		}
		return max(size-n, 0), size - 1, true
	}
	s, err := strconv.ParseInt(a, 10, 64)
	if err != nil || s < 0 || s >= size {
		return 0, 0, false
	}
	if b == "" {
		return s, size - 1, true
	}
	en, err := strconv.ParseInt(b, 10, 64)
	if err != nil || en < s {
		return 0, 0, false
	}
	return s, min(en, size-1), true
}

func (e *Emulator) delete(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	c := queryConds(r.URL.Query())
	if c.err != nil {
		writeJSONError(w, errInvalid(c.err.Error()))
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	b, o, err := e.lookup(bucketName, objectName)
	if err == nil {
		err = c.checkExisting(o, false)
	}
	if err != nil {
		writeJSONError(w, err)
		return
	}
	delete(b.objects, objectName)
	w.WriteHeader(http.StatusNoContent)
}

func (e *Emulator) list(w http.ResponseWriter, r *http.Request, bucketName string) {
	q := r.URL.Query()
	if q.Get("versions") == "true" {
		writeJSONError(w, errNotEmulated("listing versions"))
		return
	}
	prefix, delim := q.Get("prefix"), q.Get("delimiter")
	startOffset, endOffset := q.Get("startOffset"), q.Get("endOffset")
	maxResults := 1000
	if s := q.Get("maxResults"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			writeJSONError(w, errInvalid("invalid maxResults"))
			return
		}
		maxResults = min(n, 1000)
	}
	after := ""
	if tok := q.Get("pageToken"); tok != "" {
		b, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			writeJSONError(w, errInvalid("invalid pageToken"))
			return
		}
		after = string(b)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	b := e.buckets[bucketName]
	if b == nil {
		writeJSONError(w, errNoBucket(bucketName))
		return
	}
	var names []string
	for n := range b.objects {
		names = append(names, n)
	}
	sort.Strings(names)

	var items []map[string]any
	var prefixes []string
	seen := map[string]bool{}
	next := ""
	count := 0
	for _, n := range names {
		if !strings.HasPrefix(n, prefix) || (startOffset != "" && n < startOffset) || (endOffset != "" && n >= endOffset) {
			continue
		}
		key := n
		isPrefix := false
		if delim != "" {
			if i := strings.Index(n[len(prefix):], delim); i >= 0 {
				key = n[:len(prefix)+i+len(delim)]
				isPrefix = true
			}
		}
		if key <= after || seen[key] {
			continue
		}
		if count == maxResults {
			next = base64.RawURLEncoding.EncodeToString([]byte(after))
			break
		}
		seen[key] = true
		count++
		after = key
		if isPrefix {
			prefixes = append(prefixes, key)
		} else {
			items = append(items, e.resource(b, b.objects[n]))
		}
	}
	res := map[string]any{"kind": "storage#objects"}
	if len(items) > 0 {
		res["items"] = items
	}
	if len(prefixes) > 0 {
		res["prefixes"] = prefixes
	}
	if next != "" {
		res["nextPageToken"] = next
	}
	writeJSON(w, http.StatusOK, res)
}

func (e *Emulator) serveUpload(w http.ResponseWriter, r *http.Request, rest string) {
	bucketName, _, hasObjects, hasObject, err := splitObjectPath(rest)
	if err != nil || !hasObjects || hasObject || r.Method != http.MethodPost {
		writeJSONError(w, errNotEmulated(r.Method+" "+r.URL.Path))
		return
	}
	q := r.URL.Query()
	c := queryConds(q)
	if c.err != nil {
		writeJSONError(w, errInvalid(c.err.Error()))
		return
	}
	var meta struct {
		objectMeta
		Name    string `json:"name"`
		CRC32C  string `json:"crc32c"`
		MD5Hash string `json:"md5Hash"`
	}
	var data []byte
	switch q.Get("uploadType") {
	case "media":
		if data, err = io.ReadAll(r.Body); err != nil {
			writeJSONError(w, errInvalid("reading body"))
			return
		}
		meta.ContentType = r.Header.Get("Content-Type")
	case "multipart":
		meta.objectMeta, meta.Name, meta.CRC32C, meta.MD5Hash, data, err = readMultipart(r)
		if err != nil {
			writeJSONError(w, errInvalid(err.Error()))
			return
		}
	default:
		// The SDK uploads anything that fits in one chunk as multipart;
		// larger, resumable uploads aren't needed yet.
		writeJSONError(w, errNotEmulated("uploadType="+q.Get("uploadType")))
		return
	}
	name := q.Get("name")
	if name == "" {
		name = meta.Name
	}
	if name == "" {
		writeJSONError(w, &apiError{http.StatusBadRequest, "required", "InvalidArgument", "Required"})
		return
	}
	if meta.CRC32C != "" {
		var crc [4]byte
		binary.BigEndian.PutUint32(crc[:], crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)))
		if got := base64.StdEncoding.EncodeToString(crc[:]); got != meta.CRC32C {
			writeJSONError(w, errInvalid(fmt.Sprintf("Provided CRC32C %q doesn't match calculated CRC32C %q.", meta.CRC32C, got)))
			return
		}
	}
	if meta.MD5Hash != "" {
		sum := md5.Sum(data)
		if got := base64.StdEncoding.EncodeToString(sum[:]); got != meta.MD5Hash {
			writeJSONError(w, errInvalid(fmt.Sprintf("Provided MD5 hash %q doesn't match calculated MD5 hash %q.", meta.MD5Hash, got)))
			return
		}
	}
	if meta.ContentType == "" {
		meta.ContentType = "application/octet-stream"
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	b := e.buckets[bucketName]
	if b == nil {
		writeJSONError(w, errNoBucket(bucketName))
		return
	}
	if err := c.checkWrite(b.objects[name]); err != nil {
		writeJSONError(w, err)
		return
	}
	o := e.write(b, name, data, meta.ContentType, meta.objectMeta)
	writeJSON(w, http.StatusOK, e.resource(b, o))
}

func readMultipart(r *http.Request) (m objectMeta, name, crc, md5Hash string, data []byte, err error) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/related" {
		return m, "", "", "", nil, errors.New("multipart upload needs multipart/related")
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	metaPart, err := mr.NextPart()
	if err != nil {
		return m, "", "", "", nil, errors.New("missing metadata part")
	}
	var meta struct {
		objectMeta
		Name    string `json:"name"`
		CRC32C  string `json:"crc32c"`
		MD5Hash string `json:"md5Hash"`
	}
	if err := json.NewDecoder(metaPart).Decode(&meta); err != nil {
		return m, "", "", "", nil, errors.New("metadata isn't JSON")
	}
	mediaPart, err := mr.NextPart()
	if err != nil {
		return m, "", "", "", nil, errors.New("missing media part")
	}
	if data, err = io.ReadAll(mediaPart); err != nil {
		return m, "", "", "", nil, errors.New("reading media")
	}
	if meta.ContentType == "" {
		meta.ContentType = mediaPart.Header.Get("Content-Type")
	}
	return meta.objectMeta, meta.Name, meta.CRC32C, meta.MD5Hash, data, nil
}
