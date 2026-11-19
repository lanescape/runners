// Command cache is the cache action's program (specs/0007-actions.md,
// cache): it asks the runner service which version of a cache the run
// restores, downloads its objects from Cloud Storage, checking each, and
// extracts them under a directory, the files' archives at once and the
// directories' archive last. With check-only, it only asks where a restore
// would come from, and restores nothing.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/lanescape/runners/actions/internal/actions"
	"github.com/lanescape/runners/actions/internal/archive"
	"github.com/lanescape/runners/actions/internal/event"
	"github.com/lanescape/runners/actions/internal/gcs"
	"github.com/lanescape/runners/actions/internal/metadata"
	"github.com/lanescape/runners/actions/internal/service"
	"github.com/lanescape/runners/actions/internal/where"
)

func main() {
	md := metadata.Server{URL: service.MetadataServer()}
	if err := run(context.Background(), actions.Default(), md, service.New(md), service.Storage()); err != nil {
		actions.Fail(err)
	}
}

// machine tells whether this is one of the service's machines.
type machine interface {
	OnService(ctx context.Context) bool
}

// shardName is an archive of files' object, under the version's prefix.
var shardName = regexp.MustCompile(`^shard-[0-9]{3}\.tar\.zst$`)

// dirsName is the directories' archive's object.
const dirsName = "dirs.tar.zst"

func run(ctx context.Context, rt *actions.Runtime, m machine, svc *service.Client, st *gcs.Client) error {
	name := rt.Input("name")
	if !service.ValidCacheName(name) {
		return fmt.Errorf("input name is %q, which isn't a cache's name: 1 to 32 lowercase letters, digits and hyphens, "+
			"starting with a letter and not ending with a hyphen", name)
	}
	checkOnly, err := rt.BoolInput("check-only", false)
	if err != nil {
		return err
	}
	dir := rt.Input("path")
	if !checkOnly {
		if dir == "" {
			return errors.New("input path is empty: it's the directory to restore the cache under")
		}
		if dir, err = filepath.Abs(dir); err != nil {
			return err
		}
	}
	r, err := event.Load(rt.Getenv)
	if err != nil {
		return err
	}
	title := fmt.Sprintf("## Cache `%s`\n\n", name)
	place := where.Of(r)
	if !place.Inside {
		rt.Logf("cache does nothing here: %s. Nothing is restored.", place.Why)
		if err := rt.SetOutput("restored", "none"); err != nil {
			return err
		}
		return rt.Summary(title + place.Summary() + " cache did nothing, so nothing was restored.\n\n")
	}
	if !m.OnService(ctx) {
		rt.Logf("cache does nothing here: this isn't one of the runner service's machines. Nothing is restored.")
		if err := rt.SetOutput("restored", "none"); err != nil {
			return err
		}
		return rt.Summary(title + place.Summary() + " This isn't one of the runner service's machines, so cache did nothing, " +
			"and nothing was restored.\n\n")
	}
	svc.Logf, st.Logf = rt.Logf, rt.Logf
	if checkOnly {
		return check(ctx, rt, svc, name, title+place.Summary())
	}
	v, err := svc.RestoreCache(ctx, name)
	if err != nil {
		var refused *service.Refused
		if errors.As(err, &refused) {
			return fmt.Errorf("the runner service refused to restore the cache %s: %s", name, refused.Reason)
		}
		return fmt.Errorf("asking to restore the cache %s: %w", name, err)
	}
	if v.Restored == "none" {
		rt.Logf("There's no version of the cache %s to restore.", name)
		if err := rt.SetOutput("restored", "none"); err != nil {
			return err
		}
		return rt.Summary(fmt.Sprintf("%s%s\n\nThere's no version of the cache to restore, so nothing was restored (`none`).\n\n",
			title, place.Summary()))
	}
	rt.Mask(v.Token)
	var files []gcs.Object
	var dirs *gcs.Object
	for i, o := range v.Objects {
		switch base := path.Base(o.Name); {
		case shardName.MatchString(base):
			files = append(files, o)
		case base == dirsName && dirs == nil:
			dirs = &v.Objects[i]
		default:
			return fmt.Errorf("the version of the cache %s has an object save-cache doesn't make", name)
		}
	}
	if dirs == nil {
		return fmt.Errorf("the version of the cache %s has no directories' archive, %s", name, dirsName)
	}
	tokens := gcs.NewRefreshing(v.Token, v.Expires, func(ctx context.Context) (string, time.Time, error) {
		again, err := svc.RestoreCache(ctx, name)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("asking again to restore the cache %s: %w", name, err)
		}
		if again.Restored != v.Restored || again.Bucket != v.Bucket || !sameObjects(again.Objects, v.Objects) {
			return "", time.Time{}, fmt.Errorf("asked again, the runner service gave another version of the cache %s", name)
		}
		rt.Mask(again.Token)
		return again.Token, again.Expires, nil
	})

	rt.Logf("Restoring the cache %s under %s, from %s: %d objects.", name, dir, service.CacheSteps[v.Restored], len(v.Objects))
	start := time.Now()
	// Every object is there as the version recorded it before anything is
	// extracted, so that a version that can't be restored isn't restored in
	// part.
	check, cctx := errgroup.WithContext(ctx)
	for _, o := range v.Objects {
		check.Go(func() error { return st.Check(cctx, tokens, v.Bucket, o) })
	}
	if err := check.Wait(); err != nil {
		return fmt.Errorf("the version of the cache %s can't be restored: %w", name, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	x, err := archive.NewRestorer(root, os.Geteuid() == 0)
	if err != nil {
		return err
	}
	defer x.Close()
	g, gctx := errgroup.WithContext(ctx)
	for _, o := range files {
		g.Go(func() error { return extract(gctx, st, tokens, v.Bucket, o, x.Files) })
	}
	if err := g.Wait(); err != nil {
		return err
	}
	var ndirs int
	if err := extract(ctx, st, tokens, v.Bucket, *dirs, func(r io.Reader) (err error) {
		ndirs, err = x.Dirs(r)
		return err
	}); err != nil {
		return err
	}
	var bytes int64
	for _, o := range v.Objects {
		bytes += o.Size
	}
	entries, size := x.Counts()
	took := time.Since(start).Round(time.Millisecond)
	rt.Logf("Restored %d files and symlinks, %d bytes, and %d directories in %s.", entries, size, ndirs, took)
	if err := rt.SetOutput("restored", v.Restored); err != nil {
		return err
	}
	return rt.Summary(fmt.Sprintf("%s%s\n\nRestored `%s` from %s (`%s`): %d files and symlinks, %d bytes, and %d directories, "+
		"from %d objects of %d bytes, in %s.\n\n", title, place.Summary(), dir, service.CacheSteps[v.Restored], v.Restored,
		entries, size, ndirs, len(v.Objects), bytes, took))
}

// check asks where a restore of the cache name would come from, and says so,
// restoring nothing: the service records no choice, so the run's first
// restore still chooses.
func check(ctx context.Context, rt *actions.Runtime, svc *service.Client, name, title string) error {
	found, err := svc.CheckCache(ctx, name)
	if err != nil {
		var refused *service.Refused
		if errors.As(err, &refused) {
			return fmt.Errorf("the runner service refused to check the cache %s: %s", name, refused.Reason)
		}
		return fmt.Errorf("asking to check the cache %s: %w", name, err)
	}
	rt.Logf("A restore of the cache %s would come from %s (%s). Only checked: nothing is restored.", name, service.CacheSteps[found], found)
	if err := rt.SetOutput("restored", found); err != nil {
		return err
	}
	return rt.Summary(fmt.Sprintf("%s\n\nOnly checked, so nothing was restored: a restore would come from %s (`%s`).\n\n",
		title, service.CacheSteps[found], found))
}

// extract downloads o and extracts it with into, then reads the rest of
// o, so that all of it is checked against its CRC32C.
func extract(ctx context.Context, st *gcs.Client, tokens gcs.Tokens, bucket string, o gcs.Object, into func(io.Reader) error) error {
	rc := st.Open(ctx, tokens, bucket, o)
	defer rc.Close()
	if err := into(rc); err != nil {
		return fmt.Errorf("restoring %s: %w", gcs.Label(o.Name), err)
	}
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return err
	}
	return nil
}

// sameObjects is whether a and b are the same objects, in any order.
func sameObjects(a, b []gcs.Object) bool {
	by := func(x, y gcs.Object) int { return strings.Compare(x.Name, y.Name) }
	a, b = slices.Clone(a), slices.Clone(b)
	slices.SortFunc(a, by)
	slices.SortFunc(b, by)
	return slices.Equal(a, b)
}
