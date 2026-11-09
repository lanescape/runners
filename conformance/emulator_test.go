package conformance_test

import (
	"testing"

	"github.com/lanescape/runners/conformance"
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
