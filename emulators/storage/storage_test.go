package storage_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"

	gcsemu "github.com/lanescape/runners/emulators/storage"
)

// These tests drive the emulator with the real SDK, in each of the ways the
// SDK can call it.

func client(t *testing.T, json bool) (*gcsemu.Emulator, *storage.BucketHandle) {
	t.Helper()
	emu := gcsemu.New()
	t.Cleanup(emu.Close)
	emu.CreateBucket("b")
	opts := emu.ClientOptions()
	if json {
		opts = append(opts, storage.WithJSONReads())
	}
	c, err := storage.NewClient(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return emu, c.Bucket("b")
}

func put(t *testing.T, o *storage.ObjectHandle, data string) *storage.ObjectAttrs {
	t.Helper()
	w := o.NewWriter(context.Background())
	w.Metadata = map[string]string{"k": "v"}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("write: %v", err)
	}
	return w.Attrs()
}

func code(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

func TestReadsBothWays(t *testing.T) {
	for _, json := range []bool{false, true} {
		_, b := client(t, json)
		ctx := context.Background()
		a := put(t, b.Object("dir/a b+c"), "hello, world")
		r, err := b.Object("dir/a b+c").NewReader(ctx)
		if err != nil {
			t.Fatalf("json=%v: %v", json, err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(data) != "hello, world" || r.Attrs.Generation != a.Generation {
			t.Fatalf("json=%v: read %q gen %d err %v, want gen %d", json, data, r.Attrs.Generation, err, a.Generation)
		}
		rr, err := b.Object("dir/a b+c").NewRangeReader(ctx, 7, 5)
		if err != nil {
			t.Fatal(err)
		}
		data, _ = io.ReadAll(rr)
		rr.Close()
		if string(data) != "world" {
			t.Fatalf("json=%v: range read %q", json, data)
		}
		if _, err := b.Object("missing").NewReader(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
			t.Fatalf("json=%v: missing read: %v", json, err)
		}
		_, err = b.Object("dir/a b+c").If(storage.Conditions{GenerationMatch: a.Generation + 1}).NewReader(ctx)
		if code(err) != http.StatusPreconditionFailed {
			t.Fatalf("json=%v: read with wrong generation: %v", json, err)
		}
	}
}

func TestPreconditions(t *testing.T) {
	_, b := client(t, false)
	ctx := context.Background()
	o := b.Object("lease")
	a := put(t, o.If(storage.Conditions{DoesNotExist: true}), "1")

	w := o.If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	io.WriteString(w, "2")
	if err := w.Close(); code(err) != http.StatusPreconditionFailed {
		t.Fatalf("second create-only write: %v", err)
	}
	a2 := put(t, o.If(storage.Conditions{GenerationMatch: a.Generation}), "2")
	if a2.Generation <= a.Generation {
		t.Fatalf("generation went from %d to %d", a.Generation, a2.Generation)
	}
	w = o.If(storage.Conditions{GenerationMatch: a.Generation}).NewWriter(ctx)
	io.WriteString(w, "3")
	if err := w.Close(); code(err) != http.StatusPreconditionFailed {
		t.Fatalf("stale write: %v", err)
	}
	if _, err := o.If(storage.Conditions{GenerationNotMatch: a2.Generation}).Attrs(ctx); code(err) != http.StatusNotModified {
		t.Fatalf("attrs with not-match on current generation: %v", err)
	}
	if err := o.If(storage.Conditions{GenerationMatch: a.Generation}).Delete(ctx); code(err) != http.StatusPreconditionFailed {
		t.Fatalf("stale delete: %v", err)
	}
	if err := o.If(storage.Conditions{GenerationMatch: a2.Generation}).Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if err := o.Delete(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Fatalf("delete of missing object: %v", err)
	}
	attrs := put(t, o, "x")
	if attrs.Metadata["k"] != "v" || attrs.Size != 1 || attrs.CRC32C == 0 {
		t.Fatalf("attrs = %+v", attrs)
	}
}

func TestListWithDelimiter(t *testing.T) {
	_, b := client(t, false)
	for _, n := range []string{"a/1", "a/2", "a/sub/3", "b/1"} {
		put(t, b.Object(n), "x")
	}
	it := b.Objects(context.Background(), &storage.Query{Prefix: "a/", Delimiter: "/"})
	var got []string
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if a.Prefix != "" {
			got = append(got, "prefix:"+a.Prefix)
		} else {
			got = append(got, a.Name)
		}
	}
	want := []string{"a/1", "a/2", "prefix:a/sub/"}
	if len(got) != len(want) {
		t.Fatalf("list = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("list = %v, want %v", got, want)
		}
	}
}

func TestMissingBucket(t *testing.T) {
	emu, _ := client(t, false)
	c, err := storage.NewClient(context.Background(), emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Bucket("nope").Attrs(context.Background()); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Fatalf("attrs of missing bucket: %v", err)
	}
}

func TestNotMatchNeedsALiveObject(t *testing.T) {
	_, b := client(t, false)
	w := b.Object("new").If(storage.Conditions{GenerationNotMatch: 1}).NewWriter(context.Background())
	io.WriteString(w, "x")
	if err := w.Close(); code(err) != http.StatusPreconditionFailed {
		t.Fatalf("create with a not-match condition: %v, want 412", err)
	}
	a := put(t, b.Object("old"), "1")
	put(t, b.Object("old").If(storage.Conditions{GenerationNotMatch: a.Generation + 1}), "2")
}

// The SDK uploads anything larger than its chunk size resumably, in chunks:
// the object is whole, with its metadata, and a precondition holds as for
// any write.
func TestAResumableUploadBySDK(t *testing.T) {
	_, b := client(t, true)
	data := bytes.Repeat([]byte("0123456789abcdef"), (1<<20+100)/16)
	w := b.Object("big").If(storage.Conditions{DoesNotExist: true}).NewWriter(context.Background())
	w.ChunkSize, w.ContentType, w.Metadata = 256<<10, "application/zstd", map[string]string{"k": "v"}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("a resumable upload: %v", err)
	}
	a := w.Attrs()
	if a.Size != int64(len(data)) || a.ContentType != "application/zstd" || a.Metadata["k"] != "v" || a.CRC32C != crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)) {
		t.Fatalf("attrs = %+v", a)
	}
	r, err := b.Object("big").NewReader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes back, want %d", len(got), len(data))
	}
	// It may not be made again where it must not exist.
	w = b.Object("big").If(storage.Conditions{DoesNotExist: true}).NewWriter(context.Background())
	w.ChunkSize = 256 << 10
	_, _ = w.Write(data)
	if err := w.Close(); code(err) != http.StatusPreconditionFailed {
		t.Fatalf("a resumable upload of an object that exists, where it mustn't: %v", err)
	}
}

// The protocol itself: chunks of 256 KiB, a question of how much arrived,
// the last chunk, and the object again for any later request; and what it
// refuses.
func TestAResumableUploadsProtocol(t *testing.T) {
	emu, _ := client(t, true)
	do := func(method, url, cr string, body []byte) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		if cr != "" {
			req.Header.Set("Content-Range", cr)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	start := do(http.MethodPost, emu.URL+"/upload/storage/v1/b/b/o?uploadType=resumable&name=p&ifGenerationMatch=0", "", []byte(`{"contentType": "text/plain"}`))
	session := start.Header.Get("Location")
	if start.StatusCode != http.StatusOK || session == "" {
		t.Fatalf("start: %d, Location %q", start.StatusCode, session)
	}
	if r := do(http.MethodPut, session, "bytes */*", nil); r.StatusCode != http.StatusPermanentRedirect || r.Header.Get("Range") != "" {
		t.Fatalf("asking before anything arrived: %d, Range %q", r.StatusCode, r.Header.Get("Range"))
	}
	chunk := bytes.Repeat([]byte("a"), 256<<10)
	if r := do(http.MethodPut, session, "bytes 0-1023/*", chunk[:1024]); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("a chunk that isn't a multiple of 256 KiB: %d", r.StatusCode)
	}
	if r := do(http.MethodPut, session, "bytes 0-262143/*", chunk); r.StatusCode != http.StatusPermanentRedirect || r.Header.Get("Range") != "bytes=0-262143" {
		t.Fatalf("a chunk: %d, Range %q", r.StatusCode, r.Header.Get("Range"))
	}
	if r := do(http.MethodPut, session, "bytes 300000-300009/300010", chunk[:10]); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("a chunk past what arrived: %d", r.StatusCode)
	}
	if r := do(http.MethodPut, session, "bytes 262144-262153/262150", chunk[:10]); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("a chunk past the total: %d", r.StatusCode)
	}
	if r := do(http.MethodPut, session, "bytes 262144-262153/262154", chunk[:10]); r.StatusCode != http.StatusOK {
		t.Fatalf("the last chunk: %d", r.StatusCode)
	}
	if r := do(http.MethodPut, session, "bytes */*", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("asking once it's whole: %d", r.StatusCode)
	}
	if data, _, ok := emu.Object("b", "p"); !ok || len(data) != 262154 {
		t.Fatalf("object = %d bytes, %v", len(data), ok)
	}
	if r := do(http.MethodPut, emu.URL+"/upload/storage/v1/b/b/o?uploadType=resumable&upload_id=nope", "bytes */*", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown session: %d", r.StatusCode)
	}
	if r := do(http.MethodPost, emu.URL+"/upload/storage/v1/b/b/o?uploadType=resumable&name=p&ifGenerationMatch=0", "", nil); r.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("starting an upload of an object that exists, where it mustn't: %d", r.StatusCode)
	}
}

// Recovery: a question that names the total is answered with what arrived;
// a chunk sent again, or overlapping what arrived, keeps what arrived and
// adds only what's new; and checksums the caller sent are checked before
// the object is made.
func TestAResumableUploadsRecovery(t *testing.T) {
	emu, _ := client(t, true)
	do := func(method, url, cr string, body []byte) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		if cr != "" {
			req.Header.Set("Content-Range", cr)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	start := func(name, meta string) string {
		r := do(http.MethodPost, emu.URL+"/upload/storage/v1/b/b/o?uploadType=resumable&name="+name, "", []byte(meta))
		if r.StatusCode != http.StatusOK {
			t.Fatalf("start: %d", r.StatusCode)
		}
		return r.Header.Get("Location")
	}
	const k = 256 << 10
	data := make([]byte, 3*k)
	for i := range data {
		data[i] = byte(i % 251)
	}
	s := start("r", "")
	do(http.MethodPut, s, "bytes 0-262143/*", data[:k])
	if r := do(http.MethodPut, s, fmt.Sprintf("bytes */%d", len(data)), nil); r.StatusCode != http.StatusPermanentRedirect || r.Header.Get("Range") != "bytes=0-262143" {
		t.Fatalf("a question naming the total: %d, Range %q", r.StatusCode, r.Header.Get("Range"))
	}
	do(http.MethodPut, s, "bytes 262144-524287/*", data[k:2*k])
	// The first chunk again: nothing is lost.
	if r := do(http.MethodPut, s, "bytes 0-262143/*", data[:k]); r.StatusCode != http.StatusPermanentRedirect || r.Header.Get("Range") != "bytes=0-524287" {
		t.Fatalf("a chunk sent again: %d, Range %q", r.StatusCode, r.Header.Get("Range"))
	}
	// Overlapping what arrived, to the end.
	if r := do(http.MethodPut, s, fmt.Sprintf("bytes %d-%d/%d", k, len(data)-1, len(data)), data[k:]); r.StatusCode != http.StatusOK {
		t.Fatalf("an overlapping last chunk: %d", r.StatusCode)
	}
	if got, _, ok := emu.Object("b", "r"); !ok || !bytes.Equal(got, data) {
		t.Fatalf("the object isn't what was sent: %d bytes, %v", len(got), ok)
	}

	// A checksum that doesn't match: no object.
	s = start("bad", `{"crc32c": "AAAAAA=="}`)
	if r := do(http.MethodPut, s, "bytes 0-9/10", data[:10]); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("a wrong CRC32C: %d", r.StatusCode)
	}
	if _, _, ok := emu.Object("b", "bad"); ok {
		t.Fatal("an upload with a wrong CRC32C was made")
	}
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32.Checksum(data[:10], crc32.MakeTable(crc32.Castagnoli)))
	s = start("good", `{"crc32c": "`+base64.StdEncoding.EncodeToString(crc[:])+`"}`)
	if r := do(http.MethodPut, s, "bytes 0-9/10", data[:10]); r.StatusCode != http.StatusOK {
		t.Fatalf("the right CRC32C: %d", r.StatusCode)
	}
}

// A resumable upload whose condition isn't a number is refused at its
// start, as an ordinary upload is, rather than made without it.
func TestAResumableUploadsMalformedCondition(t *testing.T) {
	emu, _ := client(t, true)
	resp, err := http.Post(emu.URL+"/upload/storage/v1/b/b/o?uploadType=resumable&name=x&ifGenerationMatch=oops", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a malformed condition: %d; want 400", resp.StatusCode)
	}
}
