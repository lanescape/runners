package machines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/store"
)

// GitHub lists a repository's runs by status from its search index, which
// can leave a queued run out for a while, so a pass that read only those
// lists would give its jobs no machine. So the service also keeps, from
// each queued job's workflow_job delivery, a hint naming the job and its
// run, and a pass looks up by ID each hinted run the lists left out
// (specs/0001-architecture.md, Passes). A hint only says where to look:
// what the pass does still comes from what GitHub reports of the run.

// HintLimit is how long a hint is kept: GitHub cancels a job that has
// waited queued for 24 hours.
const HintLimit = 25 * time.Hour

// HintDeadline is the most of a pass's queue reading the hinted runs may
// take, after every repository's listed runs have been read.
const HintDeadline = 30 * time.Second

// hint is what the service keeps of a queued job's delivery.
type hint struct {
	RunID      int64     `json:"run_id"`
	JobID      int64     `json:"job_id"`
	RecordedAt time.Time `json:"recorded_at"`
}

// hintKey names a job's hint: one object per job, written once, so the jobs
// of a matrix and a push delivered again never write the same object twice.
func hintKey(repo, run, job int64) string { return fmt.Sprintf("queued/%d/%d/%d.json", repo, run, job) }

// queuedDelivery is what the service reads of GitHub's workflow_job
// delivery.
type queuedDelivery struct {
	Action      string `json:"action"`
	WorkflowJob *struct {
		ID     int64    `json:"id"`
		RunID  int64    `json:"run_id"`
		Labels []string `json:"labels"`
	} `json:"workflow_job"`
	Repository *struct {
		ID    int64 `json:"id"`
		Owner *struct {
			ID int64 `json:"id"`
		} `json:"owner"`
	} `json:"repository"`
}

// RecordQueued keeps a hint for a queued job that names one of the
// service's selectors, from GitHub's signed workflow_job delivery, before
// the delivery's push is acknowledged. A delivery of anything else, or of
// another organization's repository, by ID, records nothing. A rerun's jobs
// are new jobs, with hints of their own.
func (s *Service) RecordQueued(ctx context.Context, data []byte) error {
	var d queuedDelivery
	if err := json.Unmarshal(data, &d); err != nil {
		return nil
	}
	if d.Action != "queued" || d.WorkflowJob == nil || d.WorkflowJob.RunID <= 0 || d.WorkflowJob.ID <= 0 || d.Repository == nil ||
		d.Repository.ID <= 0 || d.Repository.Owner == nil || d.Repository.Owner.ID != s.Config.Organization.ID {
		return nil
	}
	ours := false
	for _, l := range d.WorkflowJob.Labels {
		if strings.HasPrefix(strings.ToLower(l), "lanescape-") {
			ours = true
		}
	}
	if !ours {
		return nil
	}
	data, err := json.Marshal(hint{RunID: d.WorkflowJob.RunID, JobID: d.WorkflowJob.ID, RecordedAt: s.now()})
	if err != nil {
		return err
	}
	// Written once: a push delivered again finds it there.
	_, err = s.Objects.Create(ctx, hintKey(d.Repository.ID, d.WorkflowJob.RunID, d.WorkflowJob.ID), data)
	if errors.Is(err, store.ErrPrecondition) {
		return nil
	}
	return err
}

// hintObject is a hint as its name and the bucket's listing give it.
type hintObject struct {
	name    string
	gen     int64
	run     int64
	updated time.Time
}

// hintName reads a hint's name, queued/<repository>/<run>/<job>.json.
func hintName(name string) (repo, run int64, ok bool) {
	parts := strings.Split(strings.TrimPrefix(name, "queued/"), "/")
	if !strings.HasPrefix(name, "queued/") || len(parts) != 3 || !strings.HasSuffix(parts[2], ".json") {
		return 0, 0, false
	}
	repo, err1 := strconv.ParseInt(parts[0], 10, 64)
	run, err2 := strconv.ParseInt(parts[1], 10, 64)
	job, err3 := strconv.ParseInt(strings.TrimSuffix(parts[2], ".json"), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || repo <= 0 || run <= 0 || job <= 0 {
		return 0, 0, false
	}
	return repo, run, true
}

// readHintObjects lists every hint, by repository and run, from the
// bucket's listing alone, and drops those past HintLimit or misnamed, a
// departed repository's among them. complete is false if they couldn't be
// listed.
func (s *Service) readHintObjects(ctx context.Context) (byRepo map[int64]map[int64][]hintObject, complete bool) {
	objs, err := s.Objects.List(ctx, "queued/")
	if err != nil {
		s.log().Error("listing queued jobs' hints", "err", err.Error())
		return nil, false
	}
	byRepo = map[int64]map[int64][]hintObject{}
	for _, o := range objs {
		repo, run, ok := hintName(o.Name)
		if !ok || s.now().Sub(o.Updated) > HintLimit {
			s.dropHint(ctx, o.Name, o.Generation)
			continue
		}
		if byRepo[repo] == nil {
			byRepo[repo] = map[int64][]hintObject{}
		}
		byRepo[repo][run] = append(byRepo[repo][run], hintObject{name: o.Name, gen: o.Generation, run: run, updated: o.Updated})
	}
	return byRepo, true
}

// readHints adds to q each hinted run that GitHub's lists by status left
// out and that's still queued or in progress, with its jobs, read right
// after its lookup so that a slow lookup after it can't leave them unread.
// It looks each run up once. A finished run's hints go, after every lookup;
// those of a run that's listed, or waiting, pending or requested, stay until
// it ends or they expire. A run it can't look up, GitHub's 404 among them,
// since GitHub can answer so for a run it has while the App's access
// changes, keeps its hints until they expire and leaves q incomplete, so
// that cleanup keeps what its jobs recorded.
func (s *Service) readHints(ctx context.Context, q *queue, runs map[int64][]hintObject) {
	var done [][]hintObject
	for runID, hints := range runs {
		if _, ok := q.runs[runID]; ok {
			continue
		}
		run, err := s.GitHub.WorkflowRun(ctx, q.repo, runID)
		if err != nil {
			if !errors.Is(err, github.ErrNotFound) {
				s.log().Error("reading a hinted run", "repository_id", q.repo.ID, "run_id", runID, "err", err.Error())
			}
			q.complete = false
			continue
		}
		switch run.Status {
		case "queued", "in_progress":
			s.log().Info("a run GitHub's lists by status left out", "repository_id", q.repo.ID, "run_id", run.ID, "status", run.Status)
			q.runs[run.ID] = run
			s.readJobs(ctx, q, []int64{run.ID})
		case "completed":
			done = append(done, hints)
		}
	}
	for _, hints := range done {
		s.dropHints(ctx, hints)
	}
}

func (s *Service) dropHints(ctx context.Context, hints []hintObject) {
	for _, h := range hints {
		s.dropHint(ctx, h.name, h.gen)
	}
}

// dropHint deletes a hint at the generation listed.
func (s *Service) dropHint(ctx context.Context, name string, gen int64) {
	err := s.Objects.Delete(ctx, name, gen)
	if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrPrecondition) {
		s.log().Warn("dropping a queued job's hint", "hint", name, "err", err.Error())
	}
}

// Recorder records a pushed message before it's acknowledged, as the
// server's reconcile endpoint asks.
type Recorder interface {
	RecordEvent(ctx context.Context, attributes map[string]string, data []byte) error
}

// Events routes pushed messages to what records them: a workflow_job
// delivery's queued job to the machines' hints, everything else to Others,
// the disks' records. The service and its test harness both use it.
type Events struct {
	Machines *Service
	Others   Recorder
}

// RecordEvent implements Recorder.
func (e Events) RecordEvent(ctx context.Context, attributes map[string]string, data []byte) error {
	if attributes["event"] == "workflow_job" {
		return e.Machines.RecordQueued(ctx, data)
	}
	return e.Others.RecordEvent(ctx, attributes, data)
}
