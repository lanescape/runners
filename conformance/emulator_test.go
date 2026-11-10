package conformance_test

import (
	"testing"

	"github.com/lanescape/runners/conformance"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/service"
)

// The emulator answers every case as the service does.
func TestTheEmulator(t *testing.T) {
	conformance.Run(t, func(t *testing.T) service.World {
		e := service.New()
		t.Cleanup(e.Close)
		return e
	})
}

// So does the emulator that takes its view of GitHub from a GitHub
// emulator, as a consumer's test that runs one does.
func TestTheEmulatorOnGitHub(t *testing.T) {
	conformance.Run(t, func(t *testing.T) service.World {
		gh := ghemu.New()
		t.Cleanup(gh.Close)
		e := service.New(service.WithGitHub(gh))
		t.Cleanup(e.Close)
		return e
	})
}
