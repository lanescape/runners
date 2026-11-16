// Command save-cache is the save-cache action's program
// (specs/0007-actions.md, save-cache): it asks the runner service for a
// version of a cache to save, walks a directory, uploads its files and
// symlinks in shards, and its directories, as compressed archives to
// Cloud Storage, all at once, and then commits the version.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func run(ctx context.Context, rt *actions.Runtime, m machine, svc *service.Client, st *gcs.Client) error {
	name := rt.Input("name")
	if !service.ValidCacheName(name) {
		return fmt.Errorf("input name is %q, which isn't a cache's name: 1 to 32 lowercase letters, digits and hyphens, "+
			"starting with a letter and not ending with a hyphen", name)
	}
	dir := rt.Input("path")
	if dir == "" {
		return errors.New("input path is empty: it's the directory to save")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	var exclude []string
	for line := range strings.SplitSeq(rt.Input("exclude"), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if _, err := archive.CleanRel(strings.TrimPrefix(line, "./")); err != nil {
			return fmt.Errorf("input exclude has %q, which isn't a path relative to %s, with no . or .. in it", line, dir)
		}
		exclude = append(exclude, line)
	}
	r, err := event.Load(rt.Getenv)
	if err != nil {
		return err
	}
	title := fmt.Sprintf("## Saving the cache `%s`\n\n", name)
	place := where.Of(r)
	if !place.Inside {
		rt.Logf("save-cache does nothing here: %s.", place.Why)
		return rt.Summary(title + place.Summary() + " save-cache did nothing.\n\n")
	}
	if !m.OnService(ctx) {
		rt.Logf("save-cache does nothing here: this isn't one of the runner service's machines.")
		return rt.Summary(title + place.Summary() + " This isn't one of the runner service's machines, so save-cache did nothing.\n\n")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("input path is %s, which isn't a directory", dir)
	}
	svc.Logf, st.Logf = rt.Logf, rt.Logf
	s, err := svc.SaveCache(ctx, name)
	if err != nil {
		var refused *service.Refused
		if errors.As(err, &refused) {
			return fmt.Errorf("the runner service refused to save the cache %s: %s", name, refused.Reason)
		}
		return fmt.Errorf("asking to save the cache %s: %w", name, err)
	}
	if !s.Saves {
		rt.Logf("The run's scope saves nothing, so the cache %s isn't saved: %s.", name, s.Reason)
		return rt.Summary(fmt.Sprintf("%s%s\n\nThe run's scope saves nothing, so the cache wasn't saved: %s.\n\n",
			title, place.Summary(), s.Reason))
	}
	rt.Mask(s.Token)
	tokens := gcs.NewRefreshing(s.Token, s.Expires, func(ctx context.Context) (string, time.Time, error) {
		again, err := svc.SaveCache(ctx, name)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("asking again to save the cache %s: %w", name, err)
		}
		if !again.Saves || again.Bucket != s.Bucket || again.Prefix != s.Prefix {
			return "", time.Time{}, fmt.Errorf("asked again, the runner service gave another version of the cache %s to save", name)
		}
		rt.Mask(again.Token)
		return again.Token, again.Expires, nil
	})

	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("saving the cache %s: %w", name, err)
	}
	defer root.Close()
	start := time.Now()
	tree, err := archive.Walk(root, exclude)
	if err != nil {
		return fmt.Errorf("saving the cache %s: %w", name, err)
	}
	rt.Logf("Found %d files and symlinks, %d bytes, and %d directories under %s in %s.",
		len(tree.Files), tree.Bytes, len(tree.Dirs), dir, time.Since(start).Round(time.Millisecond))
	if tree.Skipped > 0 {
		rt.Logf("Left out %d sockets, devices and named pipes, which a cache doesn't keep.", tree.Skipped)
	}

	var mu sync.Mutex
	var uploaded []gcs.Object
	g, gctx := errgroup.WithContext(ctx)
	upload := func(object string, entries []archive.Entry) error {
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(archive.Write(pw, root, entries)) }()
		o, err := st.Upload(gctx, tokens, s.Bucket, s.Prefix+object, pr)
		if err != nil {
			// Whatever's still archiving stops.
			pr.CloseWithError(err)
			return err
		}
		pr.Close()
		mu.Lock()
		uploaded = append(uploaded, o)
		mu.Unlock()
		return nil
	}
	for i, shard := range archive.Shard(tree.Files, archive.Shards) {
		if len(shard) > 0 {
			g.Go(func() error { return upload(fmt.Sprintf("shard-%03d.tar.zst", i), shard) })
		}
	}
	g.Go(func() error { return upload("dirs.tar.zst", tree.Dirs) })
	if err := g.Wait(); err != nil {
		return fmt.Errorf("saving the cache %s: %w", name, err)
	}
	var bytes int64
	for _, o := range uploaded {
		bytes += o.Size
	}
	c, err := svc.CommitCache(ctx, name)
	if err != nil {
		var refused *service.Refused
		if errors.As(err, &refused) {
			return fmt.Errorf("the runner service refused to commit the cache %s: %s", name, refused.Reason)
		}
		return fmt.Errorf("committing the cache %s: %w", name, err)
	}
	if c.Objects != len(uploaded) || c.Bytes != bytes {
		return fmt.Errorf("the runner service committed %d objects of %d bytes for the cache %s, where save-cache uploaded %d of %d bytes",
			c.Objects, c.Bytes, name, len(uploaded), bytes)
	}
	took := time.Since(start).Round(time.Millisecond)
	rt.Logf("Saved the cache %s: %d objects, %d bytes, in %s. It's kept if the job succeeds.", name, c.Objects, c.Bytes, took)
	return rt.Summary(fmt.Sprintf("%s%s\n\nSaved `%s`: %d files and symlinks, %d bytes, and %d directories, as %d objects of %d bytes, "+
		"in %s. The runner service keeps the version if the job succeeds.\n\n", title, place.Summary(), dir,
		len(tree.Files), tree.Bytes, len(tree.Dirs), c.Objects, c.Bytes, took))
}
