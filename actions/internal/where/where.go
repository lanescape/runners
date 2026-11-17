// Package where decides whether an action is inside the organization or
// outside it (specs/0007-actions.md, Where an action is). The decision is a
// courtesy, not a guard: the service checks everything for itself.
package where

import (
	"fmt"

	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/runners/actions/internal/event"
)

// Place is where an action runs.
type Place struct {
	// Inside is whether the action uses the service.
	Inside bool
	// Why says why, for the job's summary.
	Why string
	// ForkIntoOrganization is a fork's pull request into one of the
	// organization's repositories: outside, in the organization's
	// repository.
	ForkIntoOrganization bool
}

// Of decides where a run's action is. The organization is known by its
// immutable ID, endpoints.Organization, never by its name.
func Of(r event.Run) Place {
	if r.OwnerID != endpoints.Organization {
		return Place{Why: fmt.Sprintf("the repository's owner, %d, isn't the lanescape organization, %d", r.OwnerID, int64(endpoints.Organization))}
	}
	if r.ForkPullRequest() {
		if r.Name == "pull_request_target" {
			return Place{Inside: true, Why: "a pull_request_target run of a fork's pull request, which the service accepts only once a maintainer approves it"}
		}
		return Place{ForkIntoOrganization: true, Why: "a fork's pull request, which never runs on the service's machines"}
	}
	return Place{Inside: true, Why: "a run of the lanescape organization's own repository"}
}

// Summary is the summary's line saying where.
func (p Place) Summary() string {
	if p.Inside {
		return "Inside the lanescape organization: " + p.Why + "."
	}
	return "Outside the lanescape organization: " + p.Why + "."
}
