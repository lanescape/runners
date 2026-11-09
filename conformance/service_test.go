package conformance_test

import (
	"testing"

	"github.com/lanescape/runners/conformance"
	"github.com/lanescape/runners/emulators/service"
	"github.com/lanescape/runners/internal/servicetest"
)

// The real service, wired to this repository's emulators, answers every
// case as the emulator does.
func TestTheService(t *testing.T) {
	conformance.Run(t, func(t *testing.T) service.World { return servicetest.New(t, nil) })
}
