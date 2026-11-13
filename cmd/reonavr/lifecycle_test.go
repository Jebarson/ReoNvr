package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"reonavr/internal/domain"
	"reonavr/internal/workers"
)

type testCameraWorkers struct {
	items   map[string]workers.Spec
	changes int
}

func (registry *testCameraWorkers) Add(spec workers.Spec) {
	registry.items[spec.Name] = spec
	registry.changes++
}

func (registry *testCameraWorkers) Remove(name, _ string) {
	delete(registry.items, name)
	registry.changes++
}

func TestIntentFailureStillAppliesCameraWorkers(t *testing.T) {
	for _, scenario := range []string{"record", "disable", "pause", "disable archive", "disable recovery"} {
		t.Run(scenario, func(t *testing.T) {
			registry := &testCameraWorkers{items: make(map[string]workers.Spec)}
			for _, name := range []string{"CameraLifecycleWorker", "LiveEventWorker", "RecoveryWorker", "DownloadWorker", "RtspRecorderWorker"} {
				registry.Add(workers.Spec{Name: name})
			}
			camera := domain.Camera{ID: "camera", Enabled: true, ArchiveEnabled: true, RecoveryEnabled: true,
				RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeContinuous, EventTypes: []domain.EventType{domain.EventPerson}}
			camera.Enabled = scenario != "disable"
			camera.ArchiveEnabled = scenario != "disable archive"
			camera.RecoveryEnabled = scenario != "disable recovery"
			enabled := scenario != "pause"
			manager := &cameraWorkerManager{registry: registry, enabled: func() bool { return enabled }, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				syncIntent: func(context.Context, domain.Camera) error { return errors.New("bookkeeping temporarily unavailable") }}
			manager.Apply(camera)
			for _, name := range []string{"CameraLifecycleWorker", "LiveEventWorker", "RecoveryWorker", "DownloadWorker", "RtspRecorderWorker"} {
				want := camera.Enabled
				if name != "CameraLifecycleWorker" {
					want = want && enabled && camera.ArchiveEnabled
				}
				if name == "RecoveryWorker" || name == "DownloadWorker" {
					want = want && camera.RecoveryEnabled
				}
				_, present := registry.items[name]
				if present != want {
					t.Fatalf("intent failure left incorrect %s state: present=%t want=%t", name, present, want)
				}
			}
		})
	}
}

func TestIntentRetryContinuesOtherCamerasWithoutRestartingWorkers(t *testing.T) {
	registry := &testCameraWorkers{items: make(map[string]workers.Spec)}
	failing := true
	attempts := make(map[string]int)
	manager := &cameraWorkerManager{registry: registry, enabled: func() bool { return true }, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		syncIntent: func(_ context.Context, camera domain.Camera) error {
			attempts[camera.ID]++
			if failing && camera.ID == "first" {
				return errors.New("bookkeeping temporarily unavailable")
			}
			return nil
		}}
	cameras := []domain.Camera{{ID: "first", Enabled: true}, {ID: "second", Enabled: true}}
	manager.Apply(cameras[0])
	changes := registry.changes
	if err := manager.ReconcileIntents(context.Background(), cameras); err == nil || attempts["second"] != 1 {
		t.Fatal("intent retry did not report failure or skipped another camera")
	}
	failing = false
	if err := manager.ReconcileIntents(context.Background(), cameras); err != nil {
		t.Fatal(err)
	}
	if attempts["first"] != 3 || attempts["second"] != 2 || registry.changes != changes {
		t.Fatal("intent retry failed to recover independently of worker changes")
	}
}
