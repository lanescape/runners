package storage_test

import (
	"context"
	"errors"
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
