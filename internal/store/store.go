// Package store keeps the service's records in its Cloud Storage bucket,
// with generation preconditions so that concurrent writers never overwrite
// each other unknowingly.
package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"cloud.google.com/go/storage"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// ErrNotFound means the object doesn't exist.
var ErrNotFound = errors.New("object not found")

// ErrPrecondition means the object's generation wasn't the one given: it
// exists when it should not, or changed since it was read.
var ErrPrecondition = errors.New("object generation precondition failed")

// maxObject bounds what Get reads, since records are small.
const maxObject = 1 << 20

// Object is an object's name and generation.
type Object struct {
	Name       string
	Generation int64
	Updated    time.Time
}

// Objects is the service's bucket. Every write states the generation it
// expects, 0 meaning that the object must not exist yet.
type Objects interface {
	// Get returns an object's contents and generation.
	Get(ctx context.Context, name string) ([]byte, int64, error)
	// Create writes an object only if it doesn't exist.
	Create(ctx context.Context, name string, data []byte) (int64, error)
	// Replace writes an object only if its generation is still generation.
	Replace(ctx context.Context, name string, data []byte, generation int64) (int64, error)
	// Delete removes an object only if its generation is still generation.
	Delete(ctx context.Context, name string, generation int64) error
	// List returns the objects whose names start with prefix, in order.
	List(ctx context.Context, prefix string) ([]Object, error)
}

// GCS is Objects in a Cloud Storage bucket.
type GCS struct {
	client *storage.Client
	bucket *storage.BucketHandle
}

// NewGCS connects to a bucket. opts are passed to the Cloud Storage client,
// such as an emulator's endpoint in tests.
func NewGCS(ctx context.Context, bucket string, opts ...option.ClientOption) (*GCS, error) {
	c, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("cloud storage client: %w", err)
	}
	// Every write here carries a generation precondition, so the SDK may
	// retry them all. Short backoff, since every call is already capped by
	// its context.
	c.SetRetry(storage.WithBackoff(gax.Backoff{Initial: 100 * time.Millisecond, Max: 5 * time.Second, Multiplier: 2}))
	return &GCS{client: c, bucket: c.Bucket(bucket)}, nil
}

// Close releases the client.
func (g *GCS) Close() error { return g.client.Close() }

// uploadID is a resumable upload's session in a URI, a credential.
var uploadID = regexp.MustCompile(`upload_id=[^&\s"']+`)

// redacted is an error with any upload session's ID taken out, so that no
// caller can log one.
type redacted struct {
	msg string
	err error
}

func (r *redacted) Error() string { return r.msg }
func (r *redacted) Unwrap() error { return r.err }

func classify(err error) error {
	if err == nil {
		return nil
	}
	if msg := err.Error(); uploadID.MatchString(msg) {
		err = &redacted{msg: uploadID.ReplaceAllString(msg, "upload_id=REDACTED"), err: err}
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case http.StatusPreconditionFailed:
			return fmt.Errorf("%w: %w", ErrPrecondition, err)
		case http.StatusNotFound:
			return fmt.Errorf("%w: %w", ErrNotFound, err)
		}
	}
	return err
}

// Get implements Objects.
func (g *GCS) Get(ctx context.Context, name string) ([]byte, int64, error) {
	r, err := g.bucket.Object(name).NewReader(ctx)
	if err != nil {
		return nil, 0, classify(err)
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, maxObject+1))
	if err != nil {
		return nil, 0, classify(err)
	}
	if len(data) > maxObject {
		return nil, 0, fmt.Errorf("object %s is larger than %d bytes", name, maxObject)
	}
	return data, r.Attrs.Generation, nil
}

func (g *GCS) write(ctx context.Context, obj *storage.ObjectHandle, data []byte) (int64, error) {
	w := obj.NewWriter(ctx)
	w.ContentType = "application/json"
	// An object goes in one chunk, which the SDK buffers and sends as one
	// request with its checksum, and can retry. With no chunking at all it
	// streams, and can't. A chunk smaller than the object would start a
	// resumable upload, whose session URI is a credential that a failed
	// chunk's error would carry, so the chunk is the object's size, in
	// the multiples the SDK takes, and at least the smallest.
	w.ChunkSize = max(googleapi.MinUploadChunkSize, (len(data)+googleapi.MinUploadChunkSize-1)/googleapi.MinUploadChunkSize*googleapi.MinUploadChunkSize)
	if _, err := io.Copy(w, bytes.NewReader(data)); err != nil {
		_ = w.CloseWithError(err)
		return 0, classify(err)
	}
	if err := w.Close(); err != nil {
		return 0, classify(err)
	}
	return w.Attrs().Generation, nil
}

// Create implements Objects.
func (g *GCS) Create(ctx context.Context, name string, data []byte) (int64, error) {
	return g.write(ctx, g.bucket.Object(name).If(storage.Conditions{DoesNotExist: true}), data)
}

// Replace implements Objects.
func (g *GCS) Replace(ctx context.Context, name string, data []byte, generation int64) (int64, error) {
	if generation <= 0 {
		return 0, fmt.Errorf("replace %s: generation %d isn't an existing object's", name, generation)
	}
	return g.write(ctx, g.bucket.Object(name).If(storage.Conditions{GenerationMatch: generation}), data)
}

// Delete implements Objects.
func (g *GCS) Delete(ctx context.Context, name string, generation int64) error {
	if generation <= 0 {
		return fmt.Errorf("delete %s: generation %d isn't an existing object's", name, generation)
	}
	return classify(g.bucket.Object(name).If(storage.Conditions{GenerationMatch: generation}).Delete(ctx))
}

// List implements Objects.
func (g *GCS) List(ctx context.Context, prefix string) ([]Object, error) {
	q := &storage.Query{Prefix: prefix}
	if err := q.SetAttrSelection([]string{"Name", "Generation", "Updated"}); err != nil {
		return nil, err
	}
	it := g.bucket.Objects(ctx, q)
	var out []Object
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, classify(err)
		}
		out = append(out, Object{Name: a.Name, Generation: a.Generation, Updated: a.Updated})
	}
}
