// Package endpoints is where the service is, and the one organization it
// serves, as the actions that call it rely on (specs/0005-interface.md,
// Where the service is). An action names this module's version in its
// go.mod, and gets these constants at that version.
package endpoints

const (
	// Service is the service's address, Cloud Run's deterministic one for
	// lanescape-runners in project lanescape-dev, and the audience of
	// every token sent to it.
	Service = "https://lanescape-runners-168731703350.northamerica-northeast1.run.app"
	// Organization is the organization's immutable ID, which an action
	// compares github.repository_owner_id with, never its name.
	Organization = 303994381
)
