//go:build !e2e

package service

import (
	"time"

	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/runners/actions/internal/backoff"
	"github.com/lanescape/runners/actions/internal/gcs"
)

// address is always endpoints.Service in the actions' own builds.
func address() string { return endpoints.Service }

func defaultRetry() backoff.Policy { return DefaultRetry }

// MakingEvery is how often disk asks again while its work disk is being
// made (specs/0007-actions.md, disk).
func MakingEvery() time.Duration { return 5 * time.Second }

// MountWait is how long disk waits for its work disk to be mounted at
// /work once it's ready, and MountEvery how often it looks.
func MountWait() time.Duration  { return 2 * time.Minute }
func MountEvery() time.Duration { return time.Second }

// MountInfo is where the action's own process sees its mounts.
func MountInfo() string { return "/proc/self/mountinfo" }

// MetadataServer is the machine's metadata server.
func MetadataServer() string { return "http://metadata.google.internal" }

// Storage is the Cloud Storage client the cache actions use, at Cloud
// Storage's own address.
func Storage() *gcs.Client { return &gcs.Client{URL: gcs.DefaultURL} }
