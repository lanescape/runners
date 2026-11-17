package gcs

// These tests run against this repository's Cloud Storage emulator, with
// emulators/fault making its requests fail.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	gcsemu "github.com/lanescape/runners/emulators/storage"

	"github.com/lanescape/runners/actions/internal/backoff"
)

type world struct {
	emu   *gcsemu.Emulator
	c     *Client
	waits *[]time.Duration
}

func setup(t *testing.T) *world {
	t.Helper()
	emu := gcsemu.New()
	t.Cleanup(emu.Close)
	emu.CreateBucket("b-1")
	var waits []time.Duration
	var mu sync.Mutex
	c := &Client{URL: emu.URL, ChunkSize: chunkUnit, Retry: backoff.Policy{Attempts: 4, First: time.Second, Max: 30 * time.Second,
		Sleep: func(_ context.Context, d time.Duration) error {
			mu.Lock()
			defer mu.Unlock()
			waits = append(waits, d)
			return nil
		}}}
	return &world{emu: emu, c: c, waits: &waits}
}

// tokens are fixed tokens, counting each new one asked for.
type tokens struct {
	mu   sync.Mutex
	list []string
	n    int
}

func (k *tokens) Token(context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.list[min(k.n, len(k.list)-1)], nil
}

func (k *tokens) Expired(tok string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if tok == k.list[min(k.n, len(k.list)-1)] {
		k.n++
	}
}

func object(w *world, name string, data []byte) Object {
	gen := w.emu.Put("b-1", name, data)
	return Object{Name: name, Size: int64(len(data)), CRC32C: crc32.Checksum(data, castagnoli), Generation: gen}
}

func random(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func TestDownload(t *testing.T) {
	w := setup(t)
	data := random(1<<20 + 7)
	o := object(w, "123/v1/shard-000.tar.zst", data)
	var got []string
	w.emu.Faults().Add(fault.Rule{Match: func(r *http.Request) bool {
		got = append(got, r.Method+" "+r.URL.EscapedPath()+"?"+r.URL.RawQuery+" "+r.Header.Get("Authorization"))
		return false
	}})
	b, err := io.ReadAll(w.c.Open(context.Background(), &tokens{list: []string{"tok"}}, "b-1", o))
	if err != nil || !bytes.Equal(b, data) {
		t.Fatalf("read %d bytes, %v", len(b), err)
	}
	q := url.Values{"alt": {"media"}, "generation": {strconv.FormatInt(o.Generation, 10)}}.Encode()
	if want := "GET /storage/v1/b/b-1/o/123%2Fv1%2Fshard-000.tar.zst?" + q + " Bearer tok"; len(got) != 1 || got[0] != want {
		t.Fatalf("requests %q, want %q", got, want)
	}
}

func itoa(n int64) string {
	return strings.TrimSpace(strings.Replace(string(rune('0')), "0", "", 1)) + formatInt(n)
}

func formatInt(n int64) string {
	var b []byte
	for {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
		if n == 0 {
			return string(b)
		}
	}
}

// A download is of the object's generation, and nothing else: a newer
// one isn't read in its place.
func TestDownloadIsOfItsGeneration(t *testing.T) {
	w := setup(t)
	o := object(w, "p/shard-000.tar.zst", []byte("first"))
	w.emu.Put("b-1", o.Name, []byte("second"))
	_, err := io.ReadAll(w.c.Open(context.Background(), &tokens{list: []string{"t"}}, "b-1", o))
	if err == nil || !strings.Contains(err.Error(), "isn't there at its generation") {
		t.Fatalf("err = %v", err)
	}
}

// Every byte is checked against the object's CRC32C and size as it
// streams; a mismatch fails the read at its end.
func TestDownloadChecks(t *testing.T) {
	w := setup(t)
	data := random(100_000)
	o := object(w, "p/shard-000.tar.zst", data)
	for name, c := range map[string]struct {
		change func(*Object)
		says   string
	}{
		"a CRC32C":         {func(o *Object) { o.CRC32C ^= 1 }, "its CRC32C is"},
		"a larger size":    {func(o *Object) { o.Size++ }, "it's 100000 bytes, not 100001"},
		"a smaller size":   {func(o *Object) { o.Size-- }, "longer than its 99999 bytes"},
		"nothing the same": {func(o *Object) { o.CRC32C, o.Size = 0, 1 }, "longer than its 1 bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			bad := o
			c.change(&bad)
			r := w.c.Open(context.Background(), &tokens{list: []string{"t"}}, "b-1", bad)
			_, err := io.ReadAll(r)
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("err = %v", err)
			}
			// Every read after says so too, for whoever reads the rest.
			if _, again := r.Read(make([]byte, 10)); again != err {
				t.Fatalf("read again: %v", again)
			}
		})
	}
}

func TestDownloadRetries(t *testing.T) {
	w := setup(t)
	o := object(w, "p/shard-000.tar.zst", []byte("data"))
	w.emu.Faults().Add(fault.Rule{PathPrefix: "/storage/", Times: 2, Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "busy")})
	w.emu.Faults().Add(fault.Rule{PathPrefix: "/storage/", Times: 1, Respond: gcsemu.JSONError(http.StatusTooManyRequests, "rateLimitExceeded", "slow down")})
	w.emu.Faults().Add(fault.Rule{PathPrefix: "/storage/", Times: 1, Respond: fault.Drop})
	b, err := io.ReadAll(w.c.Open(context.Background(), &tokens{list: []string{"t"}}, "b-1", o))
	if err != nil || string(b) != "data" {
		t.Fatalf("read %q, %v", b, err)
	}
	// Go's transport may retry the dropped connection itself.
	if got := *w.waits; len(got) < 3 || got[0] != time.Second || got[1] != 2*time.Second || got[2] != 4*time.Second {
		t.Fatalf("waits = %v", got)
	}

	w = setup(t)
	o = object(w, "p/shard-000.tar.zst", []byte("data"))
	w.emu.Faults().Add(fault.Rule{PathPrefix: "/storage/", Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "busy")})
	if _, err := io.ReadAll(w.c.Open(context.Background(), &tokens{list: []string{"t"}}, "b-1", o)); err == nil || len(*w.waits) != 3 {
		t.Fatalf("err = %v after waits %v", err, *w.waits)
	}

	w = setup(t)
	o = object(w, "p/shard-000.tar.zst", []byte("data"))
	w.emu.Faults().Add(fault.Rule{PathPrefix: "/storage/", Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "no")})
	if _, err := io.ReadAll(w.c.Open(context.Background(), &tokens{list: []string{"t"}}, "b-1", o)); err == nil || len(*w.waits) != 0 {
		t.Fatalf("a 403: err = %v after waits %v", err, *w.waits)
	}
}

// A token refused as expired is replaced at once, once.
func TestDownloadGetsANewToken(t *testing.T) {
	w := setup(t)
	o := object(w, "p/shard-000.tar.zst", []byte("data"))
	expired := func(tok string) fault.Rule {
		return fault.Rule{Match: func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+tok },
			Respond: gcsemu.JSONError(http.StatusUnauthorized, "authError", "Invalid Credentials")}
	}
	w.emu.Faults().Add(expired("old"))
	k := &tokens{list: []string{"old", "new"}}
	if b, err := io.ReadAll(w.c.Open(context.Background(), k, "b-1", o)); err != nil || string(b) != "data" || k.n != 1 || len(*w.waits) != 0 {
		t.Fatalf("read %q, %v, with %d new tokens and waits %v", b, err, k.n, *w.waits)
	}
	w.emu.Faults().Add(expired("new"))
	k = &tokens{list: []string{"old", "new"}}
	if _, err := io.ReadAll(w.c.Open(context.Background(), k, "b-1", o)); err == nil || k.n != 1 {
		t.Fatalf("a new token refused too: %v, with %d new tokens", err, k.n)
	}
}

// Tokens are got again shortly before they expire, and once one is
// refused, one at a time.
func TestRefreshing(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	n := 0
	r := NewRefreshing("first", now.Add(10*time.Minute), func(context.Context) (string, time.Time, error) {
		n++
		return "next", now.Add(time.Hour), nil
	})
	r.Now = func() time.Time { return now }
	if tok, _ := r.Token(context.Background()); tok != "first" || n != 0 {
		t.Fatalf("%s after %d", tok, n)
	}
	now = now.Add(9*time.Minute + time.Second)
	if tok, _ := r.Token(context.Background()); tok != "next" || n != 1 {
		t.Fatalf("%s after %d", tok, n)
	}
	r.Expired("first")
	if tok, _ := r.Token(context.Background()); tok != "next" || n != 1 {
		t.Fatalf("an old token's refusal: %s after %d", tok, n)
	}
	r.Expired("next")
	if tok, _ := r.Token(context.Background()); tok != "next" || n != 2 {
		t.Fatalf("%s after %d", tok, n)
	}
	r = NewRefreshing("x", now.Add(time.Hour), func(context.Context) (string, time.Time, error) {
		return "", time.Time{}, errors.New("refused")
	})
	r.Expired("x")
	if _, err := r.Token(context.Background()); err == nil {
		t.Fatal("no error")
	}
}

func TestTheTokenIsNeverInAnError(t *testing.T) {
	w := setup(t)
	o := object(w, "p/shard-000.tar.zst", []byte("data"))
	const secret = "ya29.secret-token"
	w.emu.Faults().Add(fault.Rule{Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "the token "+secret+" may not")})
	_, err := io.ReadAll(w.c.Open(context.Background(), &tokens{list: []string{secret}}, "b-1", o))
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "the token *** may not") {
		t.Fatalf("download: %v", err)
	}
	_, err = w.c.Upload(context.Background(), &tokens{list: []string{secret}}, "b-1", "p/x", strings.NewReader("x"))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("upload: %v", err)
	}
}

func TestUpload(t *testing.T) {
	for name, size := range map[string]int{"empty": 0, "less than a chunk": 1000, "a chunk": chunkUnit, "chunks and a part": 3*chunkUnit + 17} {
		t.Run(name, func(t *testing.T) {
			w := setup(t)
			data := random(size)
			var auth []string
			w.emu.Faults().Add(fault.Rule{Match: func(r *http.Request) bool {
				auth = append(auth, r.Header.Get("Authorization"))
				return false
			}})
			o, err := w.c.Upload(context.Background(), &tokens{list: []string{"tok"}}, "b-1", "p/shard-000.tar.zst", bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			got, gen, ok := w.emu.Object("b-1", "p/shard-000.tar.zst")
			if !ok || !bytes.Equal(got, data) || o.Generation != gen || o.Size != int64(size) || o.CRC32C != crc32.Checksum(data, castagnoli) {
				t.Fatalf("uploaded %+v; the emulator has %d bytes at %d", o, len(got), gen)
			}
			// Only the session's start is signed: its address is its
			// credential.
			if auth[0] != "Bearer tok" || strings.Join(auth[1:], "") != "" {
				t.Fatalf("authorization %q", auth)
			}
		})
	}
}

// An upload never replaces an object: a version's objects are saved once.
func TestUploadNeverReplaces(t *testing.T) {
	w := setup(t)
	w.emu.Put("b-1", "p/shard-000.tar.zst", []byte("there"))
	_, err := w.c.Upload(context.Background(), &tokens{list: []string{"t"}}, "b-1", "p/shard-000.tar.zst", strings.NewReader("new"))
	if err == nil || !strings.Contains(err.Error(), "saved before") {
		t.Fatalf("err = %v", err)
	}
}

// A chunk that fails is sent again from what Cloud Storage says it has.
func TestUploadResumes(t *testing.T) {
	w := setup(t)
	w.emu.Faults().Add(fault.Rule{Method: http.MethodPut, Times: 1, After: true, Respond: fault.Drop})
	w.emu.Faults().Add(fault.Rule{Method: http.MethodPut, Times: 1, Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "busy")})
	data := random(5*chunkUnit + 3)
	if _, err := w.c.Upload(context.Background(), &tokens{list: []string{"t"}}, "b-1", "p/x", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := w.emu.Object("b-1", "p/x"); !bytes.Equal(got, data) {
		t.Fatalf("the emulator has %d bytes", len(got))
	}
}

// An upload finished, but whose answer is cut short, is asked about, and
// found finished.
func TestUploadFinishedButCutShort(t *testing.T) {
	w := setup(t)
	last := func(r *http.Request) bool {
		cr := r.Header.Get("Content-Range")
		return !strings.HasSuffix(cr, "/*") && !strings.HasPrefix(cr, "bytes *")
	}
	w.emu.Faults().Add(fault.Rule{Method: http.MethodPut, Match: last, Times: 1, After: true, Respond: func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Length", "100")
		rw.WriteHeader(http.StatusOK)
		rw.Write([]byte(`{"name": "p`))
	}})
	data := random(2*chunkUnit + 5)
	o, err := w.c.Upload(context.Background(), &tokens{list: []string{"t"}}, "b-1", "p/x", bytes.NewReader(data))
	if err != nil || o.Size != int64(len(data)) {
		t.Fatalf("uploaded %+v, %v", o, err)
	}
}

// The session's address must be Cloud Storage's own, since it's a
// credential, and the upload's start is retried and refused as any
// request is.
func TestUploadStart(t *testing.T) {
	for name, c := range map[string]struct {
		respond func(http.ResponseWriter, *http.Request)
		says    string
	}{
		"a session elsewhere": {fault.Status(http.StatusOK, http.Header{"Location": {"https://elsewhere.example/upload?id=1"}}, ""),
			"without a session at its own address"},
		"no session": {fault.Status(http.StatusOK, nil, ""), "without a session at its own address"},
		"an object there": {gcsemu.JSONError(http.StatusPreconditionFailed, "conditionNotMet", "At least one of the pre-conditions you specified did not hold."),
			"saved before"},
		"a refusal": {gcsemu.JSONError(http.StatusForbidden, "forbidden", "may not create"), "answered 403: may not create"},
	} {
		t.Run(name, func(t *testing.T) {
			w := setup(t)
			var puts int
			w.emu.Faults().Add(fault.Rule{Method: http.MethodPut, Match: func(*http.Request) bool { puts++; return false }})
			w.emu.Faults().Add(fault.Rule{PathPrefix: "/upload/", Respond: c.respond})
			_, err := w.c.Upload(context.Background(), &tokens{list: []string{"tok-1"}}, "b-1", "p/x", strings.NewReader("x"))
			if err == nil || !strings.Contains(err.Error(), c.says) || puts != 0 || len(*w.waits) != 0 {
				t.Fatalf("err = %v, after %d puts and waits %v", err, puts, *w.waits)
			}
		})
	}
	w := setup(t)
	w.emu.Faults().Add(fault.Rule{PathPrefix: "/upload/", Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "busy")})
	if _, err := w.c.Upload(context.Background(), &tokens{list: []string{"t"}}, "b-1", "p/x", strings.NewReader("x")); err == nil || len(*w.waits) != 3 {
		t.Fatalf("err = %v after waits %v", err, *w.waits)
	}
	w = setup(t)
	w.emu.Faults().Add(fault.Rule{PathPrefix: "/upload/", Match: func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer old" },
		Respond: gcsemu.JSONError(http.StatusUnauthorized, "authError", "Invalid Credentials")})
	var auth []string
	w.emu.Faults().Add(fault.Rule{PathPrefix: "/upload/", Match: func(r *http.Request) bool {
		auth = append(auth, r.Header.Get("Authorization"))
		return true
	}, Respond: fault.Status(http.StatusOK, http.Header{"Location": {"https://elsewhere.example/"}}, "")})
	k := &tokens{list: []string{"old", "new"}}
	w.c.Upload(context.Background(), k, "b-1", "p/x", strings.NewReader("x"))
	if k.n != 1 || len(auth) != 1 || auth[0] != "Bearer new" {
		t.Fatalf("an expired token: %d new tokens, then %q", k.n, auth)
	}
}

func TestCRC32C(t *testing.T) {
	// Cloud Storage's own example: the CRC32C of "hello world".
	if got := FormatCRC32C(crc32.Checksum([]byte("hello world"), castagnoli)); got != "yZRlqg==" {
		t.Fatalf("FormatCRC32C = %s", got)
	}
	if v, err := ParseCRC32C("yZRlqg=="); err != nil || FormatCRC32C(v) != "yZRlqg==" {
		t.Fatalf("ParseCRC32C = %x, %v", v, err)
	}
	for _, bad := range []string{"", "AAAA", "AAAAAAA=", "not base64"} {
		if _, err := ParseCRC32C(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// Check confirms an object is there at its generation, as recorded,
// without reading it.
func TestCheck(t *testing.T) {
	w := setup(t)
	o := object(w, "p/shard-000.tar.zst", []byte("data"))
	k := &tokens{list: []string{"t"}}
	if err := w.c.Check(context.Background(), k, "b-1", o); err != nil {
		t.Fatal(err)
	}
	bad := o
	bad.CRC32C ^= 1
	if err := w.c.Check(context.Background(), k, "b-1", bad); err == nil || !strings.Contains(err.Error(), "CRC32C") {
		t.Fatalf("a CRC32C that differs: %v", err)
	}
	bad = o
	bad.Size++
	if err := w.c.Check(context.Background(), k, "b-1", bad); err == nil {
		t.Fatal("a size that differs")
	}
	w.emu.Put("b-1", o.Name, []byte("newer"))
	if err := w.c.Check(context.Background(), k, "b-1", o); err == nil || !strings.Contains(err.Error(), "isn't there at its generation") {
		t.Fatalf("a generation that's gone: %v", err)
	}
}

// An answer that describes an object in a way Cloud Storage doesn't never
// brings what it says, a token perhaps, into an error.
func TestADescriptionThatIsntOne(t *testing.T) {
	const secret = "ya29.secret-token"
	for _, field := range []string{"size", "generation", "crc32c", "name"} {
		w := setup(t)
		o := object(w, "p/shard-000.tar.zst", []byte("data"))
		desc := map[string]string{"name": o.Name, "size": "4", "generation": strconv.FormatInt(o.Generation, 10), "crc32c": FormatCRC32C(o.CRC32C)}
		desc[field] = secret + "\x01"
		b, _ := json.Marshal(desc)
		w.emu.Faults().Add(fault.Rule{Method: http.MethodGet, Respond: fault.Status(http.StatusOK, nil, string(b))})
		err := w.c.Check(context.Background(), &tokens{list: []string{secret}}, "b-1", o)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: %v", field, err)
		}
	}
}
