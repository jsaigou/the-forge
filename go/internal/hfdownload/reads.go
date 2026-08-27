// SPDX-License-Identifier: Apache-2.0

package hfdownload

import (
	"context"
	"fmt"

	"github.com/jsaigou/the-forge/internal/store"
)

// reads.go — read-only passthroughs for the HTTP layer and smith's
// download_status tool. No business logic beyond "ask the store".

func (s *Service) Get(ctx context.Context, jobID int64) (store.ModelDownloadRow, error) {
	return s.d.Store.ModelDownloads().Get(ctx, jobID)
}

func (s *Service) List(ctx context.Context) ([]store.ModelDownloadRow, error) {
	return s.d.Store.ModelDownloads().List(ctx)
}

func (s *Service) ListFiles(ctx context.Context, jobID int64) ([]store.ModelDownloadFileRow, error) {
	return s.d.Store.ModelDownloads().ListFiles(ctx, jobID)
}

// BootReconcile flips every job the store still shows as "running" to
// "paused" — called once at daemon startup, before anything else touches
// the queue. A "running" row surviving to the next boot can only mean the
// previous process died mid-download (a clean Pause/Cancel always
// persists its own terminal state); leaving it claiming to be running
// would be a lie no worker is actually fetching for. Mirrors the same
// boot-reconcile posture cmd/forge/main.go already applies to compressor
// proxies.
func (s *Service) BootReconcile(ctx context.Context) error {
	running, err := s.d.Store.ModelDownloads().ListRunning(ctx)
	if err != nil {
		return fmt.Errorf("hfdownload: boot reconcile: list running: %w", err)
	}
	for _, job := range running {
		if err := s.d.Store.ModelDownloads().UpdateState(ctx, job.ID, "paused", ""); err != nil {
			s.d.logf("hfdownload: boot reconcile: job %d: %v", job.ID, err)
			continue
		}
		s.d.publish(EventStateChanged, map[string]any{"job_id": job.ID, "state": "paused", "error": ""})
		s.d.logf("hfdownload: boot reconcile: job %d (%s) running->paused", job.ID, job.Repo)
	}
	return nil
}
