package storage_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/emulators/fault"
	gcsemu "github.com/lanescape/runners/emulators/storage"
)

const signer = "signer@project.iam.gserviceaccount.com"

type signing struct {
	t     *testing.T
	emu   *gcsemu.Emulator
	clock *clockwork.FakeClock
	key   *rsa.PrivateKey
	b     *storage.BucketHandle
}

// signedSetup is an emulator with bucket b that trusts signer's key. Its
// clock starts at the real time, since the SDK signs at the real time.
func signedSetup(t *testing.T) *signing {
	t.Helper()
	clock := clockwork.NewFakeClockAt(time.Now())
	emu := gcsemu.New(gcsemu.WithClock(clock))
	t.Cleanup(emu.Close)
	emu.CreateBucket("b")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	emu.TrustSigner(signer, &key.PublicKey)
	c, err := storage.NewClient(context.Background(), emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &signing{t: t, emu: emu, clock: clock, key: key, b: c.Bucket("b")}
}

func (s *signing) signWith(key *rsa.PrivateKey, account, method, object string, headers []string, q url.Values, valid time.Duration) string {
	s.t.Helper()
	u, err := storage.SignedURL("b", object, &storage.SignedURLOptions{
		GoogleAccessID: account,
		SignBytes: func(b []byte) ([]byte, error) {
			sum := sha256.Sum256(b)
			return rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		},
		Method:          method,
		Expires:         time.Now().Add(valid),
		Scheme:          storage.SigningSchemeV4,
		Headers:         headers,
		QueryParameters: q,
		Hostname:        strings.TrimPrefix(s.emu.URL, "http://"),
		Insecure:        true,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return u
}

func (s *signing) sign(method, object string, headers []string, q url.Values, valid time.Duration) string {
	return s.signWith(s.key, signer, method, object, headers, q, valid)
}

func do(t *testing.T, method, u string, header http.Header, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, u, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func xmlCode(body []byte) string {
	s := string(body)
	i, j := strings.Index(s, "<Code>"), strings.Index(s, "</Code>")
	if i < 0 || j < i {
		return ""
	}
	return s[i+len("<Code>") : j]
}

func TestSignedGet(t *testing.T) {
	s := signedSetup(t)
	a := put(t, s.b.Object("dir/entry 1"), "hello, world")

	resp, data := do(t, http.MethodGet, s.sign(http.MethodGet, "dir/entry 1", nil, nil, time.Minute), nil, nil)
	if resp.StatusCode != http.StatusOK || string(data) != "hello, world" {
		t.Fatalf("signed GET = %d %q", resp.StatusCode, data)
	}
	resp, data = do(t, http.MethodGet, s.sign(http.MethodGet, "dir/entry 1", nil, nil, time.Minute), http.Header{"Range": {"bytes=7-"}}, nil)
	if resp.StatusCode != http.StatusPartialContent || string(data) != "world" {
		t.Fatalf("ranged signed GET = %d %q", resp.StatusCode, data)
	}

	// The generation is part of what's signed.
	gen := url.Values{"generation": {strconv.FormatInt(a.Generation, 10)}}
	at := s.sign(http.MethodGet, "dir/entry 1", nil, gen, time.Minute)
	if resp, data = do(t, http.MethodGet, at, nil, nil); resp.StatusCode != http.StatusOK || string(data) != "hello, world" {
		t.Fatalf("GET at the generation = %d %q", resp.StatusCode, data)
	}
	put(t, s.b.Object("dir/entry 1"), "replaced")
	if resp, _ = do(t, http.MethodGet, at, nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET of a replaced generation = %d, want 404", resp.StatusCode)
	}
	tampered := strings.Replace(at, "generation="+strconv.FormatInt(a.Generation, 10), "generation=1", 1)
	if resp, data = do(t, http.MethodGet, tampered, nil, nil); resp.StatusCode != http.StatusForbidden || xmlCode(data) != "SignatureDoesNotMatch" {
		t.Fatalf("GET with a changed generation = %d %s", resp.StatusCode, data)
	}

	// Another object, method, or signer, and an expired URL.
	other := strings.Replace(s.sign(http.MethodGet, "dir/entry 1", nil, nil, time.Minute), "entry%201", "entry%202", 1)
	if resp, data = do(t, http.MethodGet, other, nil, nil); resp.StatusCode != http.StatusForbidden || xmlCode(data) != "SignatureDoesNotMatch" {
		t.Fatalf("GET of another object = %d %s", resp.StatusCode, data)
	}
	if resp, _ = do(t, http.MethodHead, s.sign(http.MethodGet, "dir/entry 1", nil, nil, time.Minute), nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("HEAD with a GET's URL = %d", resp.StatusCode)
	}
	stranger, _ := rsa.GenerateKey(rand.Reader, 2048)
	if resp, data = do(t, http.MethodGet, s.signWith(stranger, "stranger@project.iam.gserviceaccount.com", http.MethodGet, "dir/entry 1", nil, nil, time.Minute), nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET signed by an untrusted account = %d %s", resp.StatusCode, data)
	}
	if resp, _ = do(t, http.MethodGet, s.signWith(stranger, signer, http.MethodGet, "dir/entry 1", nil, nil, time.Minute), nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET signed with the wrong key = %d", resp.StatusCode)
	}
	short := s.sign(http.MethodGet, "dir/entry 1", nil, nil, time.Minute)
	if resp, _ = do(t, http.MethodGet, short, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET before expiry = %d", resp.StatusCode)
	}
	// The emulator's clock started before the URL was signed.
	s.clock.Advance(2 * time.Minute)
	if resp, data = do(t, http.MethodGet, short, nil, nil); resp.StatusCode != http.StatusBadRequest || xmlCode(data) != "ExpiredToken" {
		t.Fatalf("expired GET = %d %s", resp.StatusCode, data)
	}

	// Unsigned reads are the SDK's own, and still served.
	if resp, data = do(t, http.MethodGet, s.emu.URL+"/b/dir/entry%201", nil, nil); resp.StatusCode != http.StatusOK || string(data) != "replaced" {
		t.Fatalf("unsigned GET = %d %q", resp.StatusCode, data)
	}
}

// startUpload starts a resumable upload of object that may only create it,
// as the cache's reservations do, and returns its session URI.
func (s *signing) startUpload(object string) string {
	s.t.Helper()
	u := s.sign(http.MethodPost, object, []string{"x-goog-resumable:start", "x-goog-if-generation-match:0"}, nil, time.Hour)
	resp, data := do(s.t, http.MethodPost, u, http.Header{"X-Goog-Resumable": {"start"}, "X-Goog-If-Generation-Match": {"0"}}, nil)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Location") == "" {
		s.t.Fatalf("starting an upload = %d %s", resp.StatusCode, data)
	}
	return resp.Header.Get("Location")
}

func chunk(t *testing.T, session string, first int, data []byte, total string) *http.Response {
	t.Helper()
	cr := fmt.Sprintf("bytes %d-%d/%s", first, first+len(data)-1, total)
	resp, _ := do(t, http.MethodPut, session, http.Header{"Content-Range": {cr}}, data)
	return resp
}

func status(t *testing.T, session string) *http.Response {
	t.Helper()
	resp, _ := do(t, http.MethodPut, session, http.Header{"Content-Range": {"bytes */*"}}, nil)
	return resp
}

func TestResumableUpload(t *testing.T) {
	s := signedSetup(t)
	session := s.startUpload("uploads/1")
	if resp := status(t, session); resp.StatusCode != 308 || resp.Header.Get("Range") != "" {
		t.Fatalf("status of a new upload = %d, Range %q", resp.StatusCode, resp.Header.Get("Range"))
	}
	body := []byte("0123456789abcdefghij")
	if resp := chunk(t, session, 0, body[:5], "*"); resp.StatusCode != 308 || resp.Header.Get("Range") != "bytes=0-4" {
		t.Fatalf("first chunk = %d, Range %q", resp.StatusCode, resp.Header.Get("Range"))
	}

	// A chunk lost on the way is never persisted; one whose answer is lost
	// is. Either way, asking says where to go on from.
	s.emu.Faults().Add(fault.Rule{Method: http.MethodPut, Times: 1, Respond: fault.Drop})
	req, _ := http.NewRequest(http.MethodPut, session, bytes.NewReader(body[5:10]))
	req.Header.Set("Content-Range", "bytes 5-9/*")
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("a dropped chunk was answered")
	}
	if resp := status(t, session); resp.StatusCode != 308 || resp.Header.Get("Range") != "bytes=0-4" {
		t.Fatalf("status after a dropped chunk = %d, Range %q", resp.StatusCode, resp.Header.Get("Range"))
	}
	s.emu.Faults().Add(fault.Rule{Method: http.MethodPut, Times: 1, After: true, Respond: fault.Drop})
	req, _ = http.NewRequest(http.MethodPut, session, bytes.NewReader(body[5:10]))
	req.Header.Set("Content-Range", "bytes 5-9/*")
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("a chunk whose answer was lost was answered")
	}
	if resp := status(t, session); resp.StatusCode != 308 || resp.Header.Get("Range") != "bytes=0-9" {
		t.Fatalf("status after a lost answer = %d, Range %q", resp.StatusCode, resp.Header.Get("Range"))
	}

	// A chunk past what's been received is refused; one that overlaps it
	// has its overlap ignored.
	if resp := chunk(t, session, 12, body[12:15], "*"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a chunk with a gap = %d", resp.StatusCode)
	}
	if resp := chunk(t, session, 8, body[8:15], "*"); resp.StatusCode != 308 || resp.Header.Get("Range") != "bytes=0-14" {
		t.Fatalf("an overlapping chunk = %d, Range %q", resp.StatusCode, resp.Header.Get("Range"))
	}
	resp := chunk(t, session, 15, body[15:], strconv.Itoa(len(body)))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Goog-Generation") == "" {
		t.Fatalf("last chunk = %d", resp.StatusCode)
	}
	data, gen, ok := s.emu.Object("b", "uploads/1")
	if !ok || !bytes.Equal(data, body) || strconv.FormatInt(gen, 10) != resp.Header.Get("X-Goog-Generation") {
		t.Fatalf("object = %q gen %d, answered %s", data, gen, resp.Header.Get("X-Goog-Generation"))
	}
	if resp := status(t, session); resp.StatusCode != http.StatusOK {
		t.Fatalf("status of a finished upload = %d", resp.StatusCode)
	}

	// The upload may only create the object: a second finishes with 412.
	again := s.startUpload("uploads/1")
	if resp := chunk(t, again, 0, []byte("x"), "1"); resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("second create-only upload = %d, want 412", resp.StatusCode)
	}
	if data, _, _ := s.emu.Object("b", "uploads/1"); !bytes.Equal(data, body) {
		t.Fatalf("object changed to %q", data)
	}

	// A whole object in one request, with its total known only at the end.
	whole := s.startUpload("uploads/2")
	if resp := chunk(t, whole, 0, []byte("abc"), "*"); resp.StatusCode != 308 {
		t.Fatalf("chunk = %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodPut, whole, http.Header{"Content-Range": {"bytes */3"}}, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("finishing with the total = %d", resp.StatusCode)
	}
	if data, _, _ := s.emu.Object("b", "uploads/2"); string(data) != "abc" {
		t.Fatalf("uploads/2 = %q", data)
	}
}

func TestUploadSessionsEnd(t *testing.T) {
	s := signedSetup(t)
	cancelled := s.startUpload("uploads/c")
	if resp, _ := do(t, http.MethodDelete, cancelled, nil, nil); resp.StatusCode != 499 {
		t.Fatalf("cancelling = %d, want 499", resp.StatusCode)
	}
	if resp := status(t, cancelled); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status of a cancelled upload = %d, want 404", resp.StatusCode)
	}
	old := s.startUpload("uploads/o")
	if resp := chunk(t, old, 0, []byte("ab"), "*"); resp.StatusCode != 308 {
		t.Fatalf("chunk = %d", resp.StatusCode)
	}
	// A session URI outlives its signed URL, for a week.
	s.clock.Advance(gcsemu.SessionLifetime - time.Second)
	if resp := status(t, old); resp.StatusCode != 308 {
		t.Fatalf("status of a 6-day-old upload = %d", resp.StatusCode)
	}
	s.clock.Advance(time.Second)
	if resp := chunk(t, old, 2, []byte("c"), "3"); resp.StatusCode != http.StatusGone {
		t.Fatalf("a week-old upload = %d, want 410", resp.StatusCode)
	}
	if _, _, ok := s.emu.Object("b", "uploads/o"); ok {
		t.Fatal("an expired upload made its object")
	}
	if resp := status(t, s.emu.URL+"/b/uploads/o?upload_id=nope"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown upload = %d", resp.StatusCode)
	}
}

func TestSignedWritesNeedTheirHeaders(t *testing.T) {
	s := signedSetup(t)
	u := s.sign(http.MethodPost, "uploads/h", []string{"x-goog-resumable:start", "x-goog-if-generation-match:0"}, nil, time.Hour)
	// Without the precondition it was signed with.
	if resp, data := do(t, http.MethodPost, u, http.Header{"X-Goog-Resumable": {"start"}}, nil); resp.StatusCode != http.StatusForbidden || xmlCode(data) != "SignatureDoesNotMatch" {
		t.Fatalf("POST without a signed header = %d %s", resp.StatusCode, data)
	}
	// With another value for it.
	if resp, _ := do(t, http.MethodPost, u, http.Header{"X-Goog-Resumable": {"start"}, "X-Goog-If-Generation-Match": {"5"}}, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST with a changed signed header = %d", resp.StatusCode)
	}
	// A signed GET's URL doesn't start an upload.
	get := s.sign(http.MethodGet, "uploads/h", nil, nil, time.Hour)
	if resp, _ := do(t, http.MethodPost, get, http.Header{"X-Goog-Resumable": {"start"}}, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST with a GET's URL = %d", resp.StatusCode)
	}
	// Nor does anything unsigned.
	if resp, data := do(t, http.MethodPost, s.emu.URL+"/b/uploads/h", http.Header{"X-Goog-Resumable": {"start"}}, nil); resp.StatusCode != http.StatusForbidden || xmlCode(data) != "AccessDenied" {
		t.Fatalf("unsigned POST = %d %s", resp.StatusCode, data)
	}
	if resp, _ := do(t, http.MethodPut, s.emu.URL+"/b/uploads/h", nil, []byte("x")); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned PUT = %d", resp.StatusCode)
	}
	if _, _, ok := s.emu.Object("b", "uploads/h"); ok {
		t.Fatal("an object was made")
	}
}

func TestCopy(t *testing.T) {
	s := signedSetup(t)
	ctx := context.Background()
	src := put(t, s.b.Object("uploads/1"), "payload")
	dst := s.b.Object("entries/s/1").If(storage.Conditions{DoesNotExist: true})
	c := dst.CopierFrom(s.b.Object("uploads/1").Generation(src.Generation))
	got, err := c.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 7 || got.Generation == 0 || got.Metadata["k"] != "v" {
		t.Fatalf("copy = %+v", got)
	}
	data, gen, _ := s.emu.Object("b", "entries/s/1")
	if string(data) != "payload" || gen != got.Generation {
		t.Fatalf("copied object = %q gen %d", data, gen)
	}
	// Only if absent.
	if _, err := dst.CopierFrom(s.b.Object("uploads/1")).Run(ctx); code(err) != http.StatusPreconditionFailed {
		t.Fatalf("copy over an existing destination = %v, want 412", err)
	}
	// From a generation that's gone, or with the source's precondition
	// failing.
	if _, err := s.b.Object("entries/s/2").CopierFrom(s.b.Object("uploads/1").Generation(src.Generation + 1)).Run(ctx); code(err) != http.StatusNotFound {
		t.Fatalf("copy from a missing generation = %v, want 404", err)
	}
	if _, err := s.b.Object("entries/s/2").CopierFrom(s.b.Object("uploads/1").If(storage.Conditions{GenerationMatch: src.Generation + 1})).Run(ctx); code(err) != http.StatusPreconditionFailed {
		t.Fatalf("copy with a failing source precondition = %v, want 412", err)
	}
	if _, err := s.b.Object("entries/s/2").CopierFrom(s.b.Object("missing")).Run(ctx); code(err) != http.StatusNotFound {
		t.Fatalf("copy of a missing object = %v, want 404", err)
	}
}

func TestLifecycle(t *testing.T) {
	s := signedSetup(t)
	s.emu.AddLifecycleRule("b", "uploads/", 1)
	put(t, s.b.Object("uploads/old"), "x")
	put(t, s.b.Object("entries/old"), "x")
	s.clock.Advance(23 * time.Hour)
	put(t, s.b.Object("uploads/new"), "x")
	if gone := s.emu.RunLifecycle(); len(gone) != 0 {
		t.Fatalf("deleted %v before a day", gone)
	}
	s.clock.Advance(time.Hour)
	if gone := s.emu.RunLifecycle(); len(gone) != 1 || gone[0] != "b/uploads/old" {
		t.Fatalf("deleted %v, want b/uploads/old", gone)
	}
	if size, _, _, ok := s.emu.Attrs("b", "entries/old"); !ok || size != 1 {
		t.Fatal("an object outside the rule's prefix went")
	}
	attrs, err := s.b.Attrs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(attrs.Lifecycle.Rules) != 1 || attrs.Lifecycle.Rules[0].Condition.AgeInDays != 1 ||
		attrs.Lifecycle.Rules[0].Condition.MatchesPrefix[0] != "uploads/" || attrs.Lifecycle.Rules[0].Action.Type != "Delete" {
		t.Fatalf("lifecycle = %+v", attrs.Lifecycle)
	}
}
