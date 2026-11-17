// Package event is what the actions read of a run's event: the job's
// GITHUB_ variables and the event's payload, at GITHUB_EVENT_PATH.
package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Repo is a repository as a payload names it.
type Repo struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Owner    struct {
		ID int64 `json:"id"`
	} `json:"owner"`
}

// PullRequest is the payload's pull request.
type PullRequest struct {
	Number int `json:"number"`
	Head   struct {
		SHA string `json:"sha"`
		// Repo is nil once the head repository is deleted.
		Repo *Repo `json:"repo"`
	} `json:"head"`
}

// Payload is the part of an event's payload the actions read.
type Payload struct {
	Repository  *Repo        `json:"repository"`
	PullRequest *PullRequest `json:"pull_request"`
}

// Run is the job's run, as GitHub describes it to the job.
type Run struct {
	// Name is GITHUB_EVENT_NAME, such as push or pull_request_target.
	Name string
	// Repository is GITHUB_REPOSITORY, owner/name.
	Repository string
	// RepositoryID is GITHUB_REPOSITORY_ID.
	RepositoryID int64
	// OwnerID is GITHUB_REPOSITORY_OWNER_ID, the immutable ID that
	// github.repository_owner_id is.
	OwnerID int64
	// SHA is GITHUB_SHA, the run's own commit.
	SHA string
	// APIURL is GITHUB_API_URL, with no trailing slash.
	APIURL  string
	Payload Payload
}

// Load reads the run from the job's environment.
func Load(getenv func(string) string) (Run, error) {
	r := Run{
		Name:       getenv("GITHUB_EVENT_NAME"),
		Repository: getenv("GITHUB_REPOSITORY"),
		SHA:        getenv("GITHUB_SHA"),
		APIURL:     strings.TrimRight(getenv("GITHUB_API_URL"), "/"),
	}
	var errs []error
	for name, v := range map[string]string{"GITHUB_EVENT_NAME": r.Name, "GITHUB_REPOSITORY": r.Repository, "GITHUB_SHA": r.SHA, "GITHUB_API_URL": r.APIURL} {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s isn't set", name))
		}
	}
	path := getenv("GITHUB_EVENT_PATH")
	if path == "" {
		errs = append(errs, errors.New("GITHUB_EVENT_PATH isn't set"))
	} else {
		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("the event's payload: %w", err))
		} else if err := json.Unmarshal(data, &r.Payload); err != nil {
			errs = append(errs, fmt.Errorf("the event's payload isn't JSON: %w", err))
		}
	}
	r.OwnerID = id(getenv("GITHUB_REPOSITORY_OWNER_ID"))
	r.RepositoryID = id(getenv("GITHUB_REPOSITORY_ID"))
	if repo := r.Payload.Repository; repo != nil {
		if r.OwnerID == 0 {
			r.OwnerID = repo.Owner.ID
		}
		if r.RepositoryID == 0 {
			r.RepositoryID = repo.ID
		}
	}
	if r.OwnerID == 0 {
		errs = append(errs, errors.New("GITHUB_REPOSITORY_OWNER_ID isn't set"))
	}
	if r.RepositoryID == 0 {
		errs = append(errs, errors.New("GITHUB_REPOSITORY_ID isn't set"))
	}
	return r, errors.Join(errs...)
}

func id(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// ForkPullRequest reports whether the event is a pull request's whose head
// isn't this repository: a fork's, or one whose head repository is gone.
func (r Run) ForkPullRequest() bool {
	pr := r.Payload.PullRequest
	if pr == nil {
		return false
	}
	return pr.Head.Repo == nil || pr.Head.Repo.ID != r.RepositoryID
}

// Contribution is a pull_request_target run's pull request: its number, its
// head repository and its head commit, as the event says. It fails for any
// other event, or a payload that doesn't name them.
func (r Run) Contribution() (number int, head Repo, sha string, err error) {
	if r.Name != "pull_request_target" {
		return 0, Repo{}, "", fmt.Errorf("this is a %s run, not a pull_request_target run", r.Name)
	}
	pr := r.Payload.PullRequest
	switch {
	case pr == nil || pr.Number <= 0:
		return 0, Repo{}, "", errors.New("the pull_request_target event names no pull request")
	case pr.Head.Repo == nil || pr.Head.Repo.ID <= 0 || pr.Head.Repo.FullName == "":
		return 0, Repo{}, "", fmt.Errorf("pull request %d's head repository is gone", pr.Number)
	case !IsSHA(pr.Head.SHA):
		return 0, Repo{}, "", fmt.Errorf("pull request %d's head commit isn't a commit SHA", pr.Number)
	}
	return pr.Number, *pr.Head.Repo, pr.Head.SHA, nil
}

// IsSHA reports whether s is a full commit SHA, 40 lowercase hex digits,
// as GitHub gives them.
func IsSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
