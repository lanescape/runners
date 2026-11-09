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
)

// Stopped records a Spot machine that stopped with its job admitted,
// before it's deleted (specs/0002-machines.md, Preemption): its instance
// ID, zone, the job's ID, runner ID and name, its run and attempt, its
// selector, and whether it was preempted, yes or unknown. If its scope
// saves disks, it then makes a resume image of its work disk. It implements
// machines.Preemptions, and is called again until it returns nil.
func (s *Service) Stopped(ctx context.Context, m machines.Machine, a machines.Admission, preempted bool) error {
	repo := m.RepositoryID
	if !preempted {
		_, _, ok, err := getJSON[Event](ctx, s.Objects, eventKey(m.ID))
		if err != nil {
			return err
		}
		preempted = ok
	}
	evidence := Unknown
	if preempted {
		evidence = Preempted
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
		Preempted: evidence, StoppedAt: stopped})
	if err != nil {
		return err
	}
	if preempted && rec.Preempted != Preempted {
		rec.Preempted = Preempted
		if gen, err = replaceJSON(ctx, s.Objects, key, rec, gen); err != nil {
			return err
		}
	}
	log := s.log().With("machine", m.Name, "instance_id", m.ID, "run_id", a.RunID, "job_id", a.JobID, "preempted", rec.Preempted)
	if !m.Selector.Scope.Saves() || rec.NoDisk {
		log.Info("stopped Spot machine recorded")
		return nil
	}
	if rec.ResumeImage == "" {
		_, err := s.Compute.Disk(ctx, m.Zone, machines.WorkDisk(m.Name))
		if errors.Is(err, compute.ErrNotFound) {
			rec.NoDisk = true
			_, err = replaceJSON(ctx, s.Objects, key, rec, gen)
			log.Info("stopped Spot machine recorded; it had no work disk to resume from")
			return err
		}
		if err != nil {
			return err
		}
		rec.ResumeImage = ResumeName(repo, m.ID)
		if _, err = replaceJSON(ctx, s.Objects, key, rec, gen); err != nil {
			return err
		}
	}
	ikey := imageKey(repo, rec.ResumeImage)
	im, igen, err := createOnce(ctx, s.Objects, ikey, Image{Image: rec.ResumeImage, RepositoryID: repo, Kind: KindResume, State: ImageMaking,
		Arch: m.Selector.Arch, Machine: m.Name, Zone: m.Zone, InstanceID: m.ID, JobID: a.JobID, JobName: a.JobName, RunID: a.RunID,
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
	log.Info("stopped Spot machine recorded, with a resume image", "image", im.Image)
	return nil
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

// RecordEvent records a Compute Engine event in the bucket by instance ID,
// if it's a preemption of one of the service's machines, before the
// reconcile endpoint acknowledges it: Google keeps the zone operation that
// also shows it for as little as an hour.
func (s *Service) RecordEvent(ctx context.Context, data []byte) error {
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
