package cache

import (
	"context"

	"github.com/lanescape/runners/internal/github"
)

// SettleRecord is settle, for tests: it decides a pending entry from a
// record and generation as an earlier reading saw them.
func (s *Service) SettleRecord(ctx context.Context, e Entry, gen int64, job *github.Job) error {
	return s.settle(ctx, record[Entry]{e, gen}, job)
}
