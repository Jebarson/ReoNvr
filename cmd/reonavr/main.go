package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"reonavr/internal/archive"
	"reonavr/internal/camera"
	"reonavr/internal/control"
	"reonavr/internal/domain"
	"reonavr/internal/httpapi"
	"reonavr/internal/reolink"
	"reonavr/internal/repository"
	"reonavr/internal/security"
	"reonavr/internal/workers"
	"reonavr/webui"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("application stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	credentialCipher, err := security.LoadOrCreateCredentialCipher(environment("REONVR_MASTER_KEY_FILE", "/run/secrets/master.key"))
	if err != nil {
		return err
	}
	store, err := repository.Open(ctx, environment("REONVR_DATABASE_PATH", "/data/reonavr.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.RecoverRecordingWork(ctx, time.Now().UTC()); err != nil {
		return err
	}

	runner := &reolink.ExecRunner{
		Path:        environment("REONVR_CLI_PATH", "/usr/local/bin/reolink-cli"),
		GatewayAddr: environment("REONVR_GATEWAY_ADDR", "gateway:9000"),
	}
	sharedClient := reolink.NewClient(runner, 15*time.Second)
	validationClient := reolink.NewValidationClient(runner, environment("REONVR_VALIDATION_GATEWAY_PATH", "/usr/local/bin/reolink-gateway"), 15*time.Second)
	service := camera.NewService(store, validationClient, credentialCipher)
	service.SetProbeTester(sharedClient)
	recordingControlService := control.NewRecordingService(store)
	recordingControl, err := recordingControlService.Get(ctx)
	if err != nil {
		return err
	}
	var recordingEnabled atomic.Bool
	recordingEnabled.Store(recordingControl.Enabled)
	archiveService := archive.NewService(store, sharedClient, credentialCipher)
	eventService := archive.NewEventService(store, sharedClient, credentialCipher)
	eventService.SetSearcher(sharedClient)
	archiveRoot := environment("REONVR_ARCHIVE_ROOT", "/archive")
	retentionService := archive.NewRetentionService(store, archiveRoot)
	playbackService := archive.NewPlaybackService(store, archiveRoot)
	downloadService := archive.NewDownloadService(store, sharedClient, credentialCipher, archiveRoot)
	downloadService.SetCapacityGuard(retentionService)
	rtspRecorderService := archive.NewRtspRecorderService(
		store, sharedClient, credentialCipher,
		&archive.FFmpegSegmenter{
			Path:      environment("REONVR_FFMPEG_PATH", "/usr/bin/ffmpeg"),
			ProbePath: environment("REONVR_FFPROBE_PATH", "/usr/bin/ffprobe"),
		},
		archiveRoot,
		environment("REONVR_RTSP_BUFFER_ROOT", "/archive/.rtsp-buffer"),
	)
	if err := rtspRecorderService.Recover(ctx); err != nil {
		return err
	}
	rtspRecorderService.SetCapacityGuard(retentionService)
	supervisor := workers.NewSupervisor(store, logger)
	supervisor.Start(ctx)
	supervisor.Add(workers.Spec{Name: "BootstrapWorker", Interval: time.Minute, Run: func(context.Context) error { return nil }})
	supervisor.Add(workers.Spec{Name: "GatewayHealthWorker", Interval: 15 * time.Second, Run: workers.GatewayProbe(runner.GatewayAddr)})
	supervisor.Add(workers.Spec{Name: "DatabaseMaintenanceWorker", Interval: 6 * time.Hour, Run: store.Maintain})
	supervisor.Add(workers.Spec{Name: "RetentionWorker", Interval: time.Minute, Run: func(workerContext context.Context) error {
		return retentionService.EnsureCapacity(workerContext, 0)
	}})
	supervisor.Add(workers.Spec{Name: "HealthAggregationWorker", Interval: 30 * time.Second, Run: func(context.Context) error { return nil }})
	manageCameraWorker := func(item domain.Camera) {
		if !item.Enabled {
			supervisor.Remove("CameraLifecycleWorker", item.ID)
			supervisor.Remove("LiveEventWorker", item.ID)
			supervisor.Remove("ReconciliationWorker", item.ID)
			supervisor.Remove("DownloadWorker", item.ID)
			supervisor.Remove("RtspRecorderWorker", item.ID)
			return
		}
		supervisor.Add(workers.Spec{
			Name: "CameraLifecycleWorker", ScopeKey: item.ID, Interval: time.Minute,
			Run: func(workerContext context.Context) error { return service.Probe(workerContext, item.ID) },
		})
		if !recordingEnabled.Load() {
			supervisor.Remove("LiveEventWorker", item.ID)
			supervisor.Remove("ReconciliationWorker", item.ID)
			supervisor.Remove("DownloadWorker", item.ID)
			supervisor.Remove("RtspRecorderWorker", item.ID)
			return
		}
		supervisor.Remove("LiveEventWorker", item.ID)
		if item.ArchiveEnabled && len(item.EventTypes) > 0 {
			supervisor.Add(workers.Spec{
				Name: "LiveEventWorker", ScopeKey: item.ID, Interval: 2 * time.Second,
				Run: func(workerContext context.Context) error { return eventService.Watch(workerContext, item.ID) },
			})
		}
		if item.ArchiveEnabled && item.RecordingSource == domain.RecordingSourceCameraStorage &&
			(item.RecordingMode == domain.RecordingModeContinuous || len(item.EventTypes) > 0) {
			supervisor.Add(workers.Spec{
				Name: "ReconciliationWorker", ScopeKey: item.ID, Interval: time.Hour,
				Run: func(workerContext context.Context) error { return archiveService.Reconcile(workerContext, item.ID) },
			})
			supervisor.Add(workers.Spec{
				Name: "DownloadWorker", ScopeKey: item.ID, Interval: 5 * time.Second,
				Run: func(workerContext context.Context) error { return downloadService.DownloadNext(workerContext, item.ID) },
			})
		} else {
			supervisor.Remove("ReconciliationWorker", item.ID)
			supervisor.Remove("DownloadWorker", item.ID)
		}
		supervisor.Remove("RtspRecorderWorker", item.ID)
		if item.ArchiveEnabled && item.RecordingSource == domain.RecordingSourceRtsp &&
			(item.RecordingMode == domain.RecordingModeContinuous ||
				(item.RecordingMode == domain.RecordingModeEvents && len(item.EventTypes) > 0)) {
			supervisor.Add(workers.Spec{
				Name: "RtspRecorderWorker", ScopeKey: item.ID, Interval: 5 * time.Second,
				Run: func(workerContext context.Context) error { return rtspRecorderService.Record(workerContext, item.ID) },
			})
		}
	}
	service.SetLifecycleHooks(manageCameraWorker, func(id string) {
		supervisor.Remove("CameraLifecycleWorker", id)
		supervisor.Remove("LiveEventWorker", id)
		supervisor.Remove("ReconciliationWorker", id)
		supervisor.Remove("DownloadWorker", id)
		supervisor.Remove("RtspRecorderWorker", id)
	})
	applyRecordingControl := func(enabled bool) {
		recordingEnabled.Store(enabled)
		if !enabled {
			supervisor.RemoveAll("EventFinalizationWorker", "LiveEventWorker", "ReconciliationWorker", "DownloadWorker", "RtspRecorderWorker")
			return
		}
		supervisor.Add(workers.Spec{Name: "EventFinalizationWorker", Interval: 5 * time.Second, Run: eventService.FinalizeNext})
		items, listErr := service.List(context.Background())
		if listErr != nil {
			logger.Error("restore camera recording workers", "error", listErr)
			return
		}
		for _, item := range items {
			manageCameraWorker(item)
		}
	}
	recordingControlService.SetLifecycleHook(applyRecordingControl)
	registered, err := service.List(ctx)
	if err != nil {
		return err
	}
	for _, item := range registered {
		manageCameraWorker(item)
	}
	if recordingEnabled.Load() {
		supervisor.Add(workers.Spec{Name: "EventFinalizationWorker", Interval: 5 * time.Second, Run: eventService.FinalizeNext})
	}
	assets, err := webui.Files()
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              environment("REONVR_ADDRESS", ":8080"),
		Handler:           httpapi.New(service, archiveService, eventService, playbackService, supervisor, recordingControlService, assets, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errChannel := make(chan error, 1)
	go func() {
		logger.Info("http server started", "address", server.Addr)
		errChannel <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func environment(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
