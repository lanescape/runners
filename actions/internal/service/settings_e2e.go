//go:build e2e

package service

import (
	"os"
	"time"

	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/runners/actions/internal/backoff"
	"github.com/lanescape/runners/actions/internal/gcs"
)

// In a binary built with the e2e tag, for the end-to-end tests alone, the
// service is the test service at LANESCAPE_TEST_SERVICE, the metadata
// server and the mounts are the test's, and waits are milliseconds rather
// than seconds. No action.yml builds with that tag.

func address() string {
	if u := os.Getenv("LANESCAPE_TEST_SERVICE"); u != "" {
		return u
	}
	return endpoints.Service
}

func defaultRetry() backoff.Policy {
	p := DefaultRetry
	p.First, p.Max = time.Millisecond, 10*time.Millisecond
	return p
}

// MakingEvery is how often disk asks again while its work disk is being
// made.
func MakingEvery() time.Duration { return 20 * time.Millisecond }

// MountWait is how long disk waits for /work to be mounted, and MountEvery
// how often it looks.
func MountWait() time.Duration  { return 500 * time.Millisecond }
func MountEvery() time.Duration { return 10 * time.Millisecond }

// MountInfo is the test's stand-in for /proc/self/mountinfo.
func MountInfo() string {
	if p := os.Getenv("LANESCAPE_TEST_MOUNTINFO"); p != "" {
		return p
	}
	return "/proc/self/mountinfo"
}

// MetadataServer is the test's metadata server.
func MetadataServer() string {
	if u := os.Getenv("LANESCAPE_TEST_METADATA"); u != "" {
		return u
	}
	return "http://metadata.google.internal"
}

// Storage is the test's Cloud Storage, at LANESCAPE_TEST_STORAGE, retried
// in milliseconds.
func Storage() *gcs.Client {
	c := &gcs.Client{URL: gcs.DefaultURL, Retry: backoff.Policy{Attempts: 8, First: time.Millisecond, Max: 10 * time.Millisecond}}
	if u := os.Getenv("LANESCAPE_TEST_STORAGE"); u != "" {
		c.URL = u
	}
	return c
}
