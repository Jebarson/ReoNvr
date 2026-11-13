package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/workers"
)

type cameraWorkerRegistry interface {
	Add(workers.Spec)
	Remove(string, string)
}

type cameraWorkerManager struct {
	registry   cameraWorkerRegistry
	enabled    func() bool
	syncIntent func(context.Context, domain.Camera) error
	logger     *slog.Logger
	probe      func(context.Context, string) error
	watch      func(context.Context, string) error
	recover    func(context.Context, string) error
	download   func(context.Context, string) error
	record     func(context.Context, string) error
}

func (manager *cameraWorkerManager) add(name, cameraID string, interval time.Duration, run func(context.Context, string) error) {
	manager.registry.Add(workers.Spec{Name: name, ScopeKey: cameraID, Interval: interval,
		Run: func(ctx context.Context) error { return run(ctx, cameraID) }})
}

func (manager *cameraWorkerManager) Apply(camera domain.Camera) {
	if err := manager.syncIntent(context.Background(), camera); err != nil {
		manager.logger.Error("persist camera recording intent", "cameraId", camera.ID, "error", err)
	}
	manager.registry.Remove("RecoveryWorker", camera.ID)
	if !camera.Enabled {
		for _, name := range []string{"CameraLifecycleWorker", "LiveEventWorker", "ReconciliationWorker", "DownloadWorker", "RtspRecorderWorker"} {
			manager.registry.Remove(name, camera.ID)
		}
		return
	}
	manager.add("CameraLifecycleWorker", camera.ID, time.Minute, manager.probe)
	if !manager.enabled() {
		for _, name := range []string{"LiveEventWorker", "ReconciliationWorker", "DownloadWorker", "RtspRecorderWorker"} {
			manager.registry.Remove(name, camera.ID)
		}
		return
	}
	manager.registry.Remove("LiveEventWorker", camera.ID)
	if camera.ArchiveEnabled && len(camera.EventTypes) > 0 {
		manager.add("LiveEventWorker", camera.ID, 2*time.Second, manager.watch)
	}
	if camera.ArchiveEnabled && camera.RecoveryEnabled && (camera.RecordingMode == domain.RecordingModeContinuous || len(camera.EventTypes) > 0) {
		manager.add("RecoveryWorker", camera.ID, time.Minute, manager.recover)
		manager.add("DownloadWorker", camera.ID, 5*time.Second, manager.download)
	} else {
		manager.registry.Remove("ReconciliationWorker", camera.ID)
		manager.registry.Remove("DownloadWorker", camera.ID)
	}
	manager.registry.Remove("RtspRecorderWorker", camera.ID)
	if camera.ArchiveEnabled && camera.RecordingSource == domain.RecordingSourceRtsp &&
		(camera.RecordingMode == domain.RecordingModeContinuous || (camera.RecordingMode == domain.RecordingModeEvents && len(camera.EventTypes) > 0)) {
		manager.add("RtspRecorderWorker", camera.ID, 5*time.Second, manager.record)
	}
}

func (manager *cameraWorkerManager) ReconcileIntents(ctx context.Context, cameras []domain.Camera) error {
	var result error
	for _, camera := range cameras {
		result = errors.Join(result, manager.syncIntent(ctx, camera))
	}
	return result
}
