// Command disk is the disk action's program (specs/0007-actions.md, disk):
// it asks the runner service for the job's work disk, asks again while
// the disk is being made, and waits until it's mounted at /work.
package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/lanescape/runners/actions/internal/actions"
	"github.com/lanescape/runners/actions/internal/event"
	"github.com/lanescape/runners/actions/internal/metadata"
	"github.com/lanescape/runners/actions/internal/mount"
	"github.com/lanescape/runners/actions/internal/service"
	"github.com/lanescape/runners/actions/internal/where"
)

// Work is where the machine mounts the job's work disk.
const Work = "/work"

func main() {
	md := metadata.Server{URL: service.MetadataServer()}
	err := run(context.Background(), actions.Default(), md, service.New(md), timing{
		making: service.MakingEvery(), mountWait: service.MountWait(), mountEvery: service.MountEvery(), mountinfo: service.MountInfo(),
	})
	if err != nil {
		actions.Fail(err)
	}
}

// timing is how often the program asks and looks, and where it looks.
type timing struct {
	// making is how often it asks again while the disk is being made.
	making time.Duration
	// mountWait is how long it waits for the disk to be mounted, and
	// mountEvery how often it looks, in mountinfo.
	mountWait, mountEvery time.Duration
	mountinfo             string
}

// machine tells whether this is one of the service's machines.
type machine interface {
	OnService(ctx context.Context) bool
}

func run(ctx context.Context, rt *actions.Runtime, m machine, svc *service.Client, tm timing) error {
	ask, err := asked(rt)
	if err != nil {
		return err
	}
	r, err := event.Load(rt.Getenv)
	if err != nil {
		return err
	}
	place := where.Of(r)
	if !place.Inside {
		rt.Logf("disk does nothing here: %s. %s is whatever the runner has there.", place.Why, Work)
		return rt.Summary(fmt.Sprintf("## Work disk\n\n%s disk did nothing, so `%s` is whatever the runner has there.\n\n", place.Summary(), Work))
	}
	if !m.OnService(ctx) {
		rt.Logf("disk does nothing here: this isn't one of the runner service's machines. %s is whatever the runner has there.", Work)
		return rt.Summary(fmt.Sprintf("## Work disk\n\n%s This isn't one of the runner service's machines, so disk did nothing, "+
			"and `%s` is whatever the runner has there.\n\n", place.Summary(), Work))
	}
	svc.Logf = rt.Logf
	rt.Logf("Asking the runner service for a work disk of %s.", describe(ask))
	var d service.Disk
	for said := false; ; said = true {
		if d, err = svc.Disk(ctx, ask); err != nil {
			return explain(err)
		}
		if d.State == service.Ready {
			break
		}
		if !said {
			rt.Logf("The runner service is making the work disk; asking again every %s.", tm.making)
		}
		if err := pause(ctx, tm.making); err != nil {
			return err
		}
	}
	rt.Logf("The work disk is ready; it came from %s. Waiting for it at %s.", service.Steps[d.Restored], Work)
	if err := waitForMount(ctx, tm); err != nil {
		return err
	}
	rt.Logf("The work disk is mounted at %s.", Work)
	return rt.Summary(fmt.Sprintf("## Work disk\n\n%s\n\nThe job's work disk is mounted at `%s`, a disk of %s. It came from %s (`%s`).\n\n",
		place.Summary(), Work, describe(ask), service.Steps[d.Restored], d.Restored))
}

// asked is the work disk the action's inputs ask for: size-gb, iops and
// throughput-mibps, each a positive whole number, or empty for the
// service's default.
func asked(rt *actions.Runtime) (service.DiskAsk, error) {
	var ask service.DiskAsk
	for _, in := range []struct {
		name string
		to   *int64
	}{{"size-gb", &ask.SizeGB}, {"iops", &ask.IOPS}, {"throughput-mibps", &ask.ThroughputMiBps}} {
		v := rt.Input(in.name)
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 || strconv.FormatInt(n, 10) != v {
			return service.DiskAsk{}, fmt.Errorf("input %s is %q; it must be a positive whole number, or empty for the runner service's default", in.name, v)
		}
		*in.to = n
	}
	return ask, nil
}

// describe says what a request asks for, for the job's log and summary.
func describe(ask service.DiskAsk) string {
	part := func(v int64, unit, what string) string {
		if v == 0 {
			return "the runner service's default " + what
		}
		return fmt.Sprintf("%d %s", v, unit)
	}
	return fmt.Sprintf("%s, %s and %s", part(ask.SizeGB, "GB", "size"), part(ask.IOPS, "IOPS", "IOPS"),
		part(ask.ThroughputMiBps, "MiB/s", "throughput"))
}

// waitForMount waits until /work, as this process sees it, is a mount
// point rather than the empty directory under it.
func waitForMount(ctx context.Context, tm timing) error {
	deadline := time.Now().Add(tm.mountWait)
	for {
		ok, err := mount.Mounted(tm.mountinfo, Work)
		if ok {
			return nil
		}
		if !time.Now().Before(deadline) {
			why := ""
			if err != nil {
				why = fmt.Sprintf(" (%v)", err)
			}
			return fmt.Errorf("the runner service made the work disk, but it isn't mounted at %s after %s%s", Work, tm.mountWait, why)
		}
		if err := pause(ctx, min(tm.mountEvery, time.Until(deadline))); err != nil {
			return err
		}
	}
}

func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(max(d, 0))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// explain is an error of the service's, for the job's log.
func explain(err error) error {
	var refused *service.Refused
	if errors.As(err, &refused) {
		return fmt.Errorf("the runner service refused the work disk: %s", refused.Reason)
	}
	return fmt.Errorf("the work disk: %w", err)
}
