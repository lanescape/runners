// Package tokens mints OAuth access tokens downscoped to one prefix of the
// caches' bucket, with Google's Credential Access Boundaries, from the
// service's own credentials (specs/0003-disks.md, Caches): a job gets one
// that may only read a version's objects, or one that may only create
// objects under its own version's prefix. A token is a credential: nothing
// here logs one.
package tokens

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/google/downscope"
)

// The roles a token is narrowed to.
const (
	// Reader reads objects, but can't list, write or delete any.
	Reader = "inRole:roles/storage.objectViewer"
	// Creator creates objects, but can't read, overwrite or delete any.
	Creator = "inRole:roles/storage.objectCreator"
)

// prefixPattern is what a version's prefix looks like: <repository
// ID>/<version>/, nothing that could end the condition's string.
var prefixPattern = regexp.MustCompile(`^[1-9][0-9]*/[a-z0-9-]+/$`)

// Downscoper mints downscoped tokens for one bucket.
type Downscoper struct {
	Bucket string
	// Root is the service's own credentials, which may do everything a
	// downscoped token is narrowed from: read, create, list and delete the
	// bucket's objects.
	Root oauth2.TokenSource
}

// New is a Downscoper from the service's default credentials.
func New(ctx context.Context, bucket string) (*Downscoper, error) {
	root, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, err
	}
	return &Downscoper{Bucket: bucket, Root: root}, nil
}

// Read is a token that reads the objects under prefix alone.
func (d *Downscoper) Read(ctx context.Context, prefix string) (string, time.Time, error) {
	return d.mint(ctx, prefix, Reader)
}

// Create is a token that creates objects under prefix alone.
func (d *Downscoper) Create(ctx context.Context, prefix string) (string, time.Time, error) {
	return d.mint(ctx, prefix, Creator)
}

func (d *Downscoper) mint(ctx context.Context, prefix, role string) (string, time.Time, error) {
	rule, err := Rule(d.Bucket, prefix, role)
	if err != nil {
		return "", time.Time{}, err
	}
	ts, err := downscope.NewTokenSource(ctx, downscope.DownscopingConfig{RootSource: d.Root, Rules: []downscope.AccessBoundaryRule{rule}})
	if err != nil {
		return "", time.Time{}, err
	}
	tok, err := ts.Token()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("downscoping a token: %w", err)
	}
	return tok.AccessToken, tok.Expiry, nil
}

// Rule is the access boundary that narrows a token to role on the objects
// under prefix of bucket.
func Rule(bucket, prefix, role string) (downscope.AccessBoundaryRule, error) {
	if !prefixPattern.MatchString(prefix) {
		return downscope.AccessBoundaryRule{}, fmt.Errorf("%q isn't a version's prefix", prefix)
	}
	if role != Reader && role != Creator {
		return downscope.AccessBoundaryRule{}, fmt.Errorf("%q isn't a role a cache's token is narrowed to", role)
	}
	return downscope.AccessBoundaryRule{
		AvailableResource:    "//storage.googleapis.com/projects/_/buckets/" + bucket,
		AvailablePermissions: []string{role},
		Condition: &downscope.AvailabilityCondition{
			Title:      "one cache version",
			Expression: fmt.Sprintf("resource.name.startsWith('projects/_/buckets/%s/objects/%s')", bucket, prefix),
		},
	}, nil
}
