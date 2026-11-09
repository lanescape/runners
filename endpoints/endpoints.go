// Package endpoints is where the service and its proxy are, and the one
// organization they serve, as the actions that call them rely on
// (specs/0005-interface.md, Where the service is). An action names this
// module's version in its go.mod, and gets these constants at that version.
//
// Each address is also the audience of every token sent to it.
package endpoints

const (
	// Service is the service's address, Cloud Run's deterministic one for
	// lanescape-runners in project lanescape-dev, and the audience of
	// every token sent to it.
	Service = "https://lanescape-runners-168731703350.northamerica-northeast1.run.app"
	// Proxy is the HTTP proxy's address, and the audience of every token
	// sent to it (specs/0006-proxy.md).
	Proxy = "https://lanescape-runners-proxy-168731703350.northamerica-northeast1.run.app"
	// Organization is the organization's immutable ID, which an action
	// compares github.repository_owner_id with, never its name.
	Organization = 303994381
)
