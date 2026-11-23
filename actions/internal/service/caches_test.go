package service

// The emulator's own answers, and, through emulators/fault, answers it
// never gives, to check how the client reads each.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	"github.com/lanescape/runners/emulators/service"

	"github.com/lanescape/runners/actions/internal/gcs"
	"github.com/lanescape/runners/actions/internal/metadata"
)

func answer(e *env, path, body string) {
	e.svc.Faults().Add(fault.Rule{PathPrefix: path, Respond: fault.Status(http.StatusOK, nil, body)})
}

func TestCacheNames(t *testing.T) {
	for n, ok := range map[string]bool{"a": true, "build-out-linux": true, "go-1": true, strings.Repeat("a", 32): true,
		"": false, "1a": false, "a-": false, "-a": false, "A": false, "a_b": false, "a/b": false, strings.Repeat("a", 33): false} {
		if ValidCacheName(n) != ok {
			t.Errorf("%q: %v", n, !ok)
		}
	}
}

func TestSaveCache(t *testing.T) {
	e := setup(t)
	var got []string
	var mu sync.Mutex
	e.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/", Match: func(r *http.Request) bool {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		mu.Lock()
		defer mu.Unlock()
		auth, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b)+" "+audience(auth))
		return false
	}})
	answer(e, "/v1/caches/save", `{"saves": true, "bucket": "lanescape-runners-caches", "prefix": "123/v1/", "token": "ya29.x", "expires": "2026-11-15T12:00:00Z"}`)
	answer(e, "/v1/caches/commit", `{"objects": 3, "bytes": 1024}`)
	s, err := e.c.SaveCache(context.Background(), "build")
	want := CacheSave{Saves: true, Bucket: "lanescape-runners-caches", Prefix: "123/v1/", Token: "ya29.x", Expires: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	if err != nil || s != want {
		t.Fatalf("save = %+v, %v", s, err)
	}
	c, err := e.c.CommitCache(context.Background(), "build")
	if err != nil || c != (CacheCommit{Objects: 3, Bytes: 1024}) {
		t.Fatalf("commit = %+v, %v", c, err)
	}
	aud := e.svc.URL() + " full"
	if w := []string{`POST /v1/caches/save {"cache":"build"} ` + aud, `POST /v1/caches/commit {"cache":"build"} ` + aud}; !slices.Equal(got, w) {
		t.Fatalf("requests = %q, want %q", got, w)
	}

	e = setup(t)
	answer(e, "/v1/caches/save", `{"saves": false, "reason": "the run's scope,\nread, saves nothing"}`)
	if s, err := e.c.SaveCache(context.Background(), "build"); err != nil || s.Saves || s.Reason != "the run's scope, read, saves nothing" {
		t.Fatalf("save = %+v, %v", s, err)
	}
}

func TestRestoreCache(t *testing.T) {
	e := setup(t)
	answer(e, "/v1/caches/restore", `{"restored": "main_exact", "bucket": "b-1", "token": "ya29.y", "expires": "2026-11-15T12:00:00Z",
		"objects": [{"name": "123/v1/shard-000.tar.zst", "size": 10, "crc32c": "AAAAAQ==", "generation": 1731600000000001}]}`)
	r, err := e.c.RestoreCache(context.Background(), "build")
	if err != nil || r.Restored != "main_exact" || r.Bucket != "b-1" || r.Token != "ya29.y" ||
		!slices.Equal(r.Objects, []gcs.Object{{Name: "123/v1/shard-000.tar.zst", Size: 10, CRC32C: 1, Generation: 1731600000000001}}) {
		t.Fatalf("restore = %+v, %v", r, err)
	}
	e = setup(t)
	answer(e, "/v1/caches/restore", `{"restored": "none"}`)
	if r, err := e.c.RestoreCache(context.Background(), "build"); err != nil || r.Restored != "none" || r.Objects != nil {
		t.Fatalf("restore = %+v, %v", r, err)
	}
}

func TestCheckCache(t *testing.T) {
	for _, step := range []string{"main_exact", "pull_request_newest", "none"} {
		e := setup(t)
		answer(e, "/v1/caches/check", `{"found": "`+step+`"}`)
		if found, err := e.c.CheckCache(context.Background(), "build"); err != nil || found != step {
			t.Fatalf("check = %q, %v; want %s", found, err, step)
		}
	}
}

// Against the emulator itself: a save in main, its commit, and a later
// run's restore of it, once the saving job has succeeded.
func TestCachesAgainstTheEmulator(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var r *Refused
	if _, err := e.c.SaveCache(ctx, "undeclared"); !errors.As(err, &r) || !strings.Contains(r.Reason, "doesn't declare") {
		t.Fatalf("an undeclared cache: %v", err)
	}
	s, err := e.c.SaveCache(ctx, "build")
	if err != nil || !s.Saves || s.Bucket != service.CachesBucket || s.Token != service.Token || !strings.HasSuffix(s.Prefix, "/") {
		t.Fatalf("save = %+v, %v", s, err)
	}
	if _, err := e.c.CommitCache(ctx, "build"); !errors.As(err, &r) || !strings.Contains(r.Reason, "nothing is uploaded") {
		t.Fatalf("a commit with nothing uploaded: %v", err)
	}
	gen := e.svc.CacheStorage().Put(s.Bucket, s.Prefix+"dirs.tar.zst", []byte("hello world"))
	if c, err := e.c.CommitCache(ctx, "build"); err != nil || c != (CacheCommit{Objects: 1, Bytes: 11}) {
		t.Fatalf("commit = %+v, %v", c, err)
	}
	// Not restorable until the job has succeeded, and then by a later run.
	if v, err := e.c.RestoreCache(ctx, "build"); err != nil || v.Restored != "none" {
		t.Fatalf("restore before the job finished = %+v, %v", v, err)
	}
	e.svc.FinishJob(e.job, "success")
	j, err := e.svc.StartJob(e.svc.Push(e.repo, "main"), "lanescape-2vcpu-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	c := At(e.svc.URL(), metadata.Server{URL: e.svc.Metadata(j)})
	// A check finds it, and chooses nothing.
	if found, err := c.CheckCache(ctx, "build"); err != nil || found != "main_exact" {
		t.Fatalf("check = %q, %v", found, err)
	}
	if _, err := c.CheckCache(ctx, "undeclared"); !errors.As(err, &r) || !strings.Contains(r.Reason, "doesn't declare") {
		t.Fatalf("a check of an undeclared cache: %v", err)
	}
	v, err := c.RestoreCache(ctx, "build")
	want := []gcs.Object{{Name: s.Prefix + "dirs.tar.zst", Size: 11, CRC32C: 0xc99465aa, Generation: gen}}
	if err != nil || v.Restored != "main_exact" || v.Bucket != service.CachesBucket || !slices.Equal(v.Objects, want) {
		t.Fatalf("restore = %+v, %v", v, err)
	}
}

func TestCacheAnswersOutsideTheInterface(t *testing.T) {
	const obj = `{"name": "p/shard-000.tar.zst", "size": 1, "crc32c": "AAAAAQ==", "generation": 1}`
	for path, bodies := range map[string][]string{
		"/v1/caches/save": {`{}`, `{"saves": false}`, `{"saves": true}`, `{"saves": "yes"}`,
			`{"saves": true, "bucket": "b-1", "prefix": "p", "token": "t", "expires": "2026-11-15T12:00:00Z"}`,
			`{"saves": true, "bucket": "B/1", "prefix": "p/", "token": "t", "expires": "2026-11-15T12:00:00Z"}`,
			`{"saves": true, "bucket": "b-1", "prefix": "p/", "token": "", "expires": "2026-11-15T12:00:00Z"}`,
			`{"saves": true, "bucket": "b-1", "prefix": "p/", "token": "t", "expires": "tomorrow"}`},
		"/v1/caches/commit": {`{}`, `{"objects": 1}`, `{"objects": -1, "bytes": 0}`},
		"/v1/caches/check":  {`{}`, `{"found": "resume"}`, `{"found": ""}`, `{"restored": "main_exact"}`},
		"/v1/caches/restore": {`{}`, `{"restored": "resume"}`, `{"restored": "main_exact"}`,
			`{"restored": "main_exact", "bucket": "b-1", "token": "t", "expires": "2026-11-15T12:00:00Z", "objects": []}`,
			`{"restored": "main_exact", "bucket": "b-1", "token": "t", "expires": "2026-11-15T12:00:00Z", "objects": [` + obj + `,` + obj + `]}`,
			`{"restored": "main_exact", "bucket": "b-1", "token": "t", "expires": "2026-11-15T12:00:00Z",
				"objects": [{"name": "p/a", "size": 1, "crc32c": "AAAA", "generation": 1}]}`,
			`{"restored": "main_exact", "bucket": "b-1", "token": "t", "expires": "2026-11-15T12:00:00Z",
				"objects": [{"name": "p/a", "size": 1, "crc32c": "AAAAAQ==", "generation": 0}]}`},
	} {
		for _, body := range bodies {
			e := setup(t)
			answer(e, path, body)
			n := e.count()
			var err error
			switch path {
			case "/v1/caches/save":
				_, err = e.c.SaveCache(context.Background(), "build")
			case "/v1/caches/commit":
				_, err = e.c.CommitCache(context.Background(), "build")
			case "/v1/caches/check":
				_, err = e.c.CheckCache(context.Background(), "build")
			default:
				_, err = e.c.RestoreCache(context.Background(), "build")
			}
			if err == nil || *n != 1 {
				t.Errorf("%s %s: err = %v after %d requests", path, body, err, *n)
			}
		}
	}
}

// The caches' requests are refused and retried as every request is.
func TestCacheRefusalsAndRetries(t *testing.T) {
	e := setup(t)
	e.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/save", Respond: fault.Refuse("the run's commit doesn't declare the cache build")})
	var r *Refused
	if _, err := e.c.SaveCache(context.Background(), "build"); !errors.As(err, &r) || !strings.Contains(r.Reason, "doesn't declare") {
		t.Fatalf("err = %v", err)
	}
	e = setup(t)
	e.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/restore", Times: 2, Respond: fault.Unavailable(5 * time.Second)})
	answer(e, "/v1/caches/restore", `{"restored": "none"}`)
	if v, err := e.c.RestoreCache(context.Background(), "build"); err != nil || v.Restored != "none" ||
		!slices.Equal(*e.waits, []time.Duration{5 * time.Second, 5 * time.Second}) {
		t.Fatalf("restore = %+v, %v, after waits %v", v, err, *e.waits)
	}
}

// A field the client rejects is never repeated in the error, since the
// service may have put anything in it, the cache's token included.
func TestARejectedFieldIsNeverRepeated(t *testing.T) {
	const secret = "ya29.SECRET-token"
	for path, body := range map[string]string{
		"/v1/caches/restore": `{"restored": "` + secret + `", "token": "` + secret + `"}`,
		"/v1/disks":          `{"state": "` + secret + `", "restored": "none"}`,
		"/v1/disks#restored": `{"state": "ready", "restored": "` + secret + `"}`,
	} {
		e := setup(t)
		answer(e, strings.Split(path, "#")[0], body)
		var err error
		if strings.HasPrefix(path, "/v1/disks") {
			_, err = e.c.Disk(context.Background(), DiskAsk{})
		} else {
			_, err = e.c.RestoreCache(context.Background(), "build")
		}
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: err = %v", path, err)
		}
	}
}
