package disks

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/scope"
)

// Stopped records a machine that stopped with its job admitted and
// unfinished, before it's deleted (specs/0002-machines.md, Preemption): a
// Spot machine, preempted or perhaps, or any machine the service stopped at
// its age limit, which ageLimit says. The record holds its instance ID,
// zone, the job's ID, runner ID and name, its run and attempt, its
// selector, whether it was preempted, yes, unknown, or no for a standard
// machine, and whether it was stopped at its age limit. If its scope saves
// disks, it then makes a resume image of its work disk. It implements
// machines.Preemptions, and is called again until it returns nil.
func (s *Service) Stopped(ctx context.Context, m machines.Machine, a machines.Admission, preempted, ageLimit bool) error {
	repo := m.RepositoryID
	// Only Spot machines are preempted.
	evidence := NotPreempted
	if m.Spot {
		if !preempted {
			_, _, ok, err := getJSON[Event](ctx, s.Objects, eventKey(m.ID))
			if err != nil {
				return err
			}
			preempted = ok
		}
		evidence = Unknown
		if preempted {
			evidence = Preempted
		}
	}
	// The day a resume image is kept, and evidence looked for, counts from
	// when Compute Engine stopped the machine, not from when a pass saw it.
	stopped := m.LastStop.UTC()
	if stopped.IsZero() || stopped.After(s.now()) {
		stopped = s.now()
	}
	key := preemptionKey(repo, a.RunID, m.ID)
	rec, gen, err := createOnce(ctx, s.Objects, key, Preemption{InstanceID: m.ID, Machine: m.Name, Zone: m.Zone, RepositoryID: repo,
		JobID: a.JobID, RunnerID: m.RunnerID, JobName: a.JobName, RunID: a.RunID, RunAttempt: a.RunAttempt, Selector: a.Selector,
		Arch: m.Selector.Arch, Family: m.Selector.Family, Preempted: evidence, AgeLimit: ageLimit, StoppedAt: stopped})
	if err != nil {
		return err
	}
	if preempted && rec.Preempted == Unknown {
		rec.Preempted = Preempted
		if gen, err = replaceJSON(ctx, s.Objects, key, rec, gen); err != nil {
			return err
		}
	}
	log := s.log().With("machine", m.Name, "instance_id", m.ID, "run_id", a.RunID, "job_id", a.JobID, "preempted", rec.Preempted,
		"age_limit", rec.AgeLimit)
	// done records that nothing more is to be made for the job: its run is
	// rerun only then, so that its rerun's job never asks for its work disk
	// before the resume image it would start from is ready.
	done := func(msg string, args ...any) error {
		if !rec.Done {
			rec.Done = true
			if _, err := replaceJSON(ctx, s.Objects, key, rec, gen); err != nil {
				return err
			}
		}
		log.Info(msg, args...)
		return nil
	}
	if rec.Done {
		// Recorded already, with its resume image ready or none to make:
		// nothing is made again, whatever went wrong after.
		log.Info("stopped machine recorded")
		return nil
	}
	if !m.Selector.Scope.Saves() || rec.NoDisk || rec.Family == "" {
		return done("stopped machine recorded")
	}
	// A resume image not ready a day after its machine stopped could no
	// longer resume the job: the service stops trying to make it, and the
	// machine goes.
	if s.now().Sub(rec.StoppedAt) >= ResumeKept {
		log.Warn("no resume image: it wasn't made within a day of its machine stopping")
		return done("stopped machine recorded")
	}
	// A repository transferred out of the organization gets no resume
	// image of the organization's.
	if repo, err := s.GitHub.RepositoryByID(ctx, m.RepositoryID); err != nil {
		return err
	} else if repo.OwnerID != s.Config.Organization.ID {
		log.Warn("no resume image: the repository is no longer the organization's")
		return done("stopped machine recorded")
	}
	if rec.ResumeImage == "" {
		_, err := s.Compute.Disk(ctx, m.Zone, machines.WorkDisk(m.Name))
		if errors.Is(err, compute.ErrNotFound) {
			rec.NoDisk = true
			return done("stopped machine recorded; it had no work disk to resume from")
		}
		if err != nil {
			return err
		}
		rec.ResumeImage = ResumeName(repo, m.ID)
		if gen, err = replaceJSON(ctx, s.Objects, key, rec, gen); err != nil {
			return err
		}
	}
	ikey := imageKey(repo, rec.ResumeImage)
	im, igen, err := createOnce(ctx, s.Objects, ikey, Image{Image: rec.ResumeImage, RepositoryID: repo, Kind: KindResume, State: ImageMaking,
		Arch: rec.Arch, Family: rec.Family, Machine: m.Name, Zone: m.Zone, InstanceID: m.ID, JobID: a.JobID, JobName: a.JobName, RunID: a.RunID,
		RunAttempt: a.RunAttempt, Selector: a.Selector, SizeGB: 0, SavedAt: rec.StoppedAt, LastUsed: rec.StoppedAt})
	if err != nil {
		return err
	}
	if im.SizeGB == 0 && im.State == ImageMaking {
		// A resume image's disk is made at least this size, so it's never
		// ready without it.
		d, err := s.Compute.Disk(ctx, m.Zone, machines.WorkDisk(m.Name))
		if err != nil {
			return err
		}
		im.SizeGB = d.SizeGB
		if igen, err = replaceJSON(ctx, s.Objects, ikey, im, igen); err != nil {
			return err
		}
	}
	if im, _, err = s.make(ctx, ikey, im, igen); err != nil {
		return err
	}
	return done("stopped machine recorded, with a resume image", "image", im.Image)
}

// logEntry is what the service reads of a Cloud Logging entry the log sink
// publishes: a Compute Engine system event of an instance.
type logEntry struct {
	Timestamp    time.Time `json:"timestamp"`
	ProtoPayload struct {
		MethodName   string `json:"methodName"`
		ResourceName string `json:"resourceName"`
	} `json:"protoPayload"`
	Resource struct {
		Type   string `json:"type"`
		Labels struct {
			InstanceID string `json:"instance_id"`
			ProjectID  string `json:"project_id"`
			Zone       string `json:"zone"`
		} `json:"labels"`
	} `json:"resource"`
}

const preemptedMethod = "compute.instances.preempted"

var instanceResource = regexp.MustCompile(`^projects/([^/]+)/zones/([^/]+)/instances/(lsr-[1-9][0-9]*-[a-z0-9]{8})$`)

// ParseEvent reads a message of the Compute Engine events topic as a
// preemption of one of the service's machines in its project and zones. It
// reports false for any other message, such as a guest's own shutdown.
func (s *Service) ParseEvent(data []byte) (Event, bool) {
	var e logEntry
	if json.Unmarshal(data, &e) != nil || e.Resource.Type != "gce_instance" || e.ProtoPayload.MethodName != preemptedMethod {
		return Event{}, false
	}
	m := instanceResource.FindStringSubmatch(e.ProtoPayload.ResourceName)
	id, ok := compute.ParseID(e.Resource.Labels.InstanceID)
	if m == nil || !ok || m[1] != s.Project || e.Resource.Labels.ProjectID != s.Project || m[2] != e.Resource.Labels.Zone ||
		!slices.Contains(s.Config.Shapes.Zones, m[2]) {
		return Event{}, false
	}
	return Event{InstanceID: id, Zone: m[2], Machine: m[3], Method: preemptedMethod, At: e.Timestamp.UTC()}, true
}

// RecordEvent records what a pushed message says that a pass can't read
// back later, before the reconcile endpoint acknowledges it: a Compute
// Engine event by instance ID, if it's a preemption of one of the service's
// machines, since Google keeps the zone operation that also shows it for as
// little as an hour; and a pull request's closing, since GitHub no longer
// shows it once the pull request is reopened.
func (s *Service) RecordEvent(ctx context.Context, attributes map[string]string, data []byte) error {
	if attributes["event"] == "pull_request" {
		return s.recordClosing(ctx, data)
	}
	ev, ok := s.ParseEvent(data)
	if !ok {
		return nil
	}
	ev.RecordedAt = s.now()
	if _, _, err := createOnce(ctx, s.Objects, eventKey(ev.InstanceID), ev); err != nil {
		return err
	}
	s.log().Info("preemption event recorded", "instance_id", ev.InstanceID, "machine", ev.Machine, "zone", ev.Zone)
	return nil
}

// closing is what the service reads of GitHub's pull_request delivery.
type closing struct {
	Action      string `json:"action"`
	PullRequest *struct {
		Number   int       `json:"number"`
		ClosedAt time.Time `json:"closed_at"`
	} `json:"pull_request"`
	Repository *struct {
		ID    int64 `json:"id"`
		Owner *struct {
			ID int64 `json:"id"`
		} `json:"owner"`
	} `json:"repository"`
}

// recordClosing records a pull request's closing, from GitHub's signed
// delivery, as the last time it was seen closed: the second after GitHub's
// closed_at, which it gives to the second, so a run bound in that second
// counts as bound before. A pass may see the pull request open again by the
// time it runs; its disks saved by runs bound before are retired all the
// same.
func (s *Service) recordClosing(ctx context.Context, data []byte) error {
	var c closing
	if json.Unmarshal(data, &c) != nil || c.Action != "closed" || c.PullRequest == nil || c.PullRequest.Number <= 0 ||
		c.PullRequest.ClosedAt.IsZero() || c.Repository == nil || c.Repository.Owner == nil ||
		c.Repository.Owner.ID != s.Config.Organization.ID || c.Repository.ID <= 0 {
		return nil
	}
	sc := scope.PullRequest(c.PullRequest.Number).String()
	seen := c.PullRequest.ClosedAt.UTC().Truncate(time.Second).Add(time.Second)
	if _, err := s.closeScope(ctx, c.Repository.ID, sc, seen, c.PullRequest.ClosedAt.UTC()); err != nil {
		return err
	}
	s.log().Info("pull request closing recorded", "repository_id", c.Repository.ID, "scope", sc)
	return nil
}

// closeScope records that a pull request scope's pull request was seen
// closed at seen, under the disks' lease, which every choice and mark takes,
// keeping whichever time is later, the one recorded or this, and returns
// what's recorded. The delivery and the passes both write it this way, so
// neither can move it back, nor change it while a step reads it.
func (s *Service) closeScope(ctx context.Context, repo int64, sc string, seen, closedAt time.Time) (Closure, error) {
	key := closureKey(repo, sc)
	var out Closure
	err := s.step(ctx, repo, func(ctx context.Context) error {
		cur, gen, found, err := getJSON[Closure](ctx, s.Objects, key)
		if err != nil {
			return err
		}
		if found && !cur.SeenAt.Before(seen) {
			out = cur
			return nil
		}
		rec := Closure{RepositoryID: repo, Scope: sc, ClosedAt: closedAt, SeenAt: seen}
		if found && closedAt.IsZero() {
			rec.ClosedAt = cur.ClosedAt
		}
		if found {
			_, err = replaceJSON(ctx, s.Objects, key, rec, gen)
		} else {
			_, _, err = createOnce(ctx, s.Objects, key, rec)
		}
		if err != nil {
			return err
		}
		out = rec
		return nil
	})
	return out, err
}
