package server

import "context"

// runExclusive runs one pass of a background job unless another instance
// sharing the database is running it. That instance does the work, so this
// pass is skipped rather than waited for. Jobs that act on shared state, such
// as eviction against max_size, would otherwise each do all of it.
func (s *Server) runExclusive(ctx context.Context, job string, run func()) {
	release, ok, err := s.db.TryLock(ctx, "job:"+job)
	if err != nil {
		s.logger.Warn("background job: failed to take lock, skipping this pass", "job", job, "error", err)
		return
	}
	if !ok {
		s.logger.Debug("background job: running on another instance, skipping this pass", "job", job)
		return
	}
	defer release()
	run()
}
