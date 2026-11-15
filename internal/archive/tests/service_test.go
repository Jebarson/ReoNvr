package archive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"
	"reonavr/internal/repository"

	"reonavr/internal/testconfig"
)

type serviceStore struct {
	camera     domain.Camera
	credential domain.EncryptedCredential
	watermark  *time.Time
	backfill   *time.Time
	backfilled int
	run        domain.IngestionRun
	runs       []domain.IngestionRun
	recordings []domain.Recording
}

func (store *serviceStore) GetCamera(context.Context, string) (domain.Camera, error) {
	return store.camera, nil
}

func (store *serviceStore) GetCredential(context.Context, string) (domain.EncryptedCredential, error) {
	return store.credential, nil
}

func (store *serviceStore) GetSearchWatermark(context.Context, string) (*time.Time, error) {
	return store.watermark, nil
}

func (store *serviceStore) GetBackfillCursor(context.Context, string) (*time.Time, int, error) {
	return store.backfill, store.backfilled, nil
}

func (store *serviceStore) SaveReconciliation(_ context.Context, run domain.IngestionRun, recordings []domain.Recording, _ time.Time) error {
	store.run = run
	store.runs = append(store.runs, run)
	store.recordings = recordings
	return nil
}

func (store *serviceStore) ListArchivedRecordings(context.Context, string, int) ([]domain.Recording, error) {
	return store.recordings, nil
}

type serviceSearcher struct {
	connection reolink.Connection
	from       time.Time
	to         time.Time
	items      []reolink.Recording
	windows    [][2]time.Time
	onSearch   func()
}

func (searcher *serviceSearcher) SearchRecordings(_ context.Context, connection reolink.Connection, _ domain.ArchiveStream, from, to time.Time) ([]reolink.Recording, error) {
	searcher.connection = connection
	searcher.from = from
	searcher.to = to
	searcher.windows = append(searcher.windows, [2]time.Time{from, to})
	if searcher.onSearch != nil {
		searcher.onSearch()
	}
	items := append([]reolink.Recording(nil), searcher.items...)
	for index := range items {
		if len(items[index].RawMetadata) == 0 {
			payload, err := json.Marshal(map[string]string{"startTime": items[index].StartAtUtc.Format(time.RFC3339Nano), "endTime": items[index].EndAtUtc.Format(time.RFC3339Nano)})
			if err != nil {
				return nil, err
			}
			items[index].RawMetadata = payload
		}
	}
	return items, nil
}

type serviceCipher struct{}

func (serviceCipher) Decrypt(string, []byte, []byte) (string, error) {
	return testconfig.Password(), nil
}

func TestReconcileFiltersAndPersistsSelectedRecordings(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := &serviceStore{
		camera: domain.Camera{
			ID: "camera-one", Host: testconfig.Host(2), Port: 9000, Channel: 0, Timezone: "UTC",
			Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
			RecordingSource: domain.RecordingSourceCameraStorage, RecordingMode: domain.RecordingModeEvents,
			HistoricalImport: domain.HistoricalImportLast48Hours, ArchiveFromUtc: timePointer(now.Add(-48 * time.Hour)),
			EventTypes: []domain.EventType{domain.EventPerson},
		},
		credential: domain.EncryptedCredential{Username: testconfig.Username(), KeyVersion: 1},
	}
	searcher := &serviceSearcher{items: []reolink.Recording{
		{NativeName: "person.mp4", Stream: domain.StreamMain, StartAtUtc: now.Add(-time.Minute), EndAtUtc: now, SizeBytes: 10, EventTypes: []domain.EventType{domain.EventMotion, domain.EventPerson}, RawMetadata: json.RawMessage(`{"name":"person.mp4"}`)},
		{NativeName: "vehicle.mp4", Stream: domain.StreamMain, StartAtUtc: now.Add(-time.Minute), EndAtUtc: now, SizeBytes: 20, EventTypes: []domain.EventType{domain.EventVehicle}},
	}}
	service := NewService(store, searcher, serviceCipher{})
	service.now = func() time.Time { return now }
	if err := service.Reconcile(context.Background(), store.camera.ID); err != nil {
		t.Fatal(err)
	}
	if len(store.recordings) != 1 || store.recordings[0].NativeName != "person.mp4" || store.recordings[0].State != domain.RecordingQueued || store.recordings[0].Source != domain.RecordingSourceCameraStorage {
		t.Fatalf("unexpected recordings: %#v", store.recordings)
	}
	if store.run.Kind != "Startup" || store.run.DiscoveredCount != 1 || !searcher.from.Equal(now.Add(-48*time.Hour)) {
		t.Fatalf("unexpected ingestion run: %#v from %s", store.run, searcher.from)
	}
	if searcher.connection.Password != testconfig.Password() || searcher.connection.Timezone != "UTC" {
		t.Fatalf("unexpected search connection: %#v", searcher.connection)
	}
}

func TestReconcileOverlapsExistingWatermark(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	watermark := now.Add(-time.Hour)
	store := &serviceStore{
		camera:     domain.Camera{ID: "camera-one", Host: testconfig.Host(2), Port: 9000, Timezone: "UTC", Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain, RecordingSource: domain.RecordingSourceCameraStorage, RecordingMode: domain.RecordingModeEvents, EventTypes: []domain.EventType{domain.EventMotion}},
		credential: domain.EncryptedCredential{Username: testconfig.Username(), KeyVersion: 1}, watermark: &watermark,
	}
	searcher := &serviceSearcher{}
	service := NewService(store, searcher, serviceCipher{})
	service.now = func() time.Time { return now }
	if err := service.Reconcile(context.Background(), store.camera.ID); err != nil {
		t.Fatal(err)
	}
	if store.run.Kind != "Hourly" || !searcher.from.Equal(watermark.Add(-5*time.Minute)) {
		t.Fatalf("unexpected overlapping search: %#v from %s", store.run, searcher.from)
	}
}

func TestReconcileContinuousModeKeepsUntaggedNativeRecordings(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := &serviceStore{
		camera: domain.Camera{
			ID: "camera-one", Host: testconfig.Host(2), Port: 9000, Timezone: "UTC",
			Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
			RecordingSource: domain.RecordingSourceCameraStorage, RecordingMode: domain.RecordingModeContinuous,
			HistoricalImport: domain.HistoricalImportNewOnly, ArchiveFromUtc: timePointer(now.Add(-time.Minute)),
		},
		credential: domain.EncryptedCredential{Username: testconfig.Username(), KeyVersion: 1},
	}
	searcher := &serviceSearcher{items: []reolink.Recording{{
		NativeName: "scheduled.mp4", Stream: domain.StreamMain,
		StartAtUtc: now.Add(-30 * time.Second), EndAtUtc: now, SizeBytes: 10,
	}}}
	service := NewService(store, searcher, serviceCipher{})
	service.now = func() time.Time { return now }
	if err := service.Reconcile(context.Background(), store.camera.ID); err != nil {
		t.Fatal(err)
	}
	if len(store.recordings) != 1 || store.recordings[0].State != domain.RecordingQueued {
		t.Fatalf("continuous recording was not retained: %#v", store.recordings)
	}
}

func TestReconcileAllAvailableSearchesRecentThenBoundedOlderWindows(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := &serviceStore{
		camera: domain.Camera{
			ID: "camera-one", Host: testconfig.Host(2), Port: 9000, Timezone: "UTC",
			Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
			RecordingSource: domain.RecordingSourceCameraStorage, RecordingMode: domain.RecordingModeContinuous,
			HistoricalImport: domain.HistoricalImportAll,
		},
		credential: domain.EncryptedCredential{Username: testconfig.Username(), KeyVersion: 1},
	}
	searcher := &serviceSearcher{}
	service := NewService(store, searcher, serviceCipher{})
	service.now = func() time.Time { return now }
	if err := service.Reconcile(context.Background(), store.camera.ID); err != nil {
		t.Fatal(err)
	}
	if len(searcher.windows) != 1+maxBackfillWindowsPerRun {
		t.Fatalf("expected recent search and bounded backfill batch, got %#v", searcher.windows)
	}
	if searcher.windows[0][0] != now.Add(-initialSearchWindow) || searcher.windows[0][1] != now {
		t.Fatalf("unexpected recent search window: %#v", searcher.windows[0])
	}
	if searcher.windows[1][0] != now.Add(-2*initialSearchWindow) || searcher.windows[1][1] != now.Add(-initialSearchWindow) {
		t.Fatalf("unexpected first backfill search window: %#v", searcher.windows[1])
	}
	for index, window := range searcher.windows[1:] {
		if !window[0].Before(window[1]) || window[1].Sub(window[0]) > emptyBackfillWindow {
			t.Fatalf("backfill window %d is invalid: %#v", index, window)
		}
		if index > 0 && window[1] != searcher.windows[index][0] {
			t.Fatalf("backfill windows are not newest-first and contiguous: %#v", searcher.windows)
		}
	}
	if len(store.runs) != 1+maxBackfillWindowsPerRun || store.runs[1].Kind != "Backfill" {
		t.Fatalf("backfill progress was not persisted: %#v", store.runs)
	}
}

func timePointer(value time.Time) *time.Time { return &value }

type recoveryInspection struct{ duration time.Duration }

func (inspection recoveryInspection) InspectVideo(context.Context, string) (VideoInspection, error) {
	return VideoInspection{Duration: inspection.duration, Coverage: []domain.TimeRange{{StartAtUtc: time.Unix(0, 0).UTC(), EndAtUtc: time.Unix(0, int64(inspection.duration)).UTC()}}}, nil
}

func TestRecoveryDownloadsOnlyMissingStreamAndKeepsPartialGaps(t *testing.T) {
	for _, scenario := range []string{"complete", "partial", "invalid newest clip"} {
		t.Run(scenario, func(t *testing.T) {
			partial := scenario == "partial"
			ctx := context.Background()
			store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "recovery.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			start := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
			camera := domain.Camera{ID: "camera-one", Name: "Front", Host: "camera", Port: 9000, Timezone: "UTC", Enabled: true, ArchiveEnabled: true, RecoveryEnabled: true,
				RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeContinuous, ArchiveStream: domain.StreamMain,
				Capabilities: &domain.CameraCapabilities{StorageAvailable: true, RtspAvailable: true}, CreatedAtUtc: start, UpdatedAtUtc: start}
			if err := store.CreateCamera(ctx, camera, domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyVersion: 1}, nil); err != nil {
				t.Fatal(err)
			}
			if err := store.SyncRecordingIntent(ctx, camera, true, start); err != nil {
				t.Fatal(err)
			}
			if err := store.CloseRecordingIntents(ctx, camera.ID, start.Add(10*time.Minute)); err != nil {
				t.Fatal(err)
			}
			for index, span := range []domain.TimeRange{{StartAtUtc: start, EndAtUtc: start.Add(time.Minute)}, {StartAtUtc: start.Add(200 * time.Second), EndAtUtc: start.Add(10 * time.Minute)}} {
				id := []string{"rtsp-before", "rtsp-after"}[index]
				if err := store.SaveRtspRecording(ctx, domain.Recording{ID: id, CameraID: camera.ID, NativeName: id + ".mkv", Source: domain.RecordingSourceRtsp, Stream: domain.StreamMain,
					StartAtUtc: span.StartAtUtc, EndAtUtc: span.EndAtUtc, State: domain.RecordingComplete, LocalPath: id + ".mkv", DiscoveredAtUtc: start, UpdatedAtUtc: start}); err != nil {
					t.Fatal(err)
				}
				if err := store.SaveRecordingCoverage(ctx, id, []domain.TimeRange{span}); err != nil {
					t.Fatal(err)
				}
			}
			searcher := &serviceSearcher{items: []reolink.Recording{
				{NativeName: "catchup.mp4", Stream: domain.StreamMain, StartAtUtc: start.Add(30 * time.Second), EndAtUtc: start.Add(4 * time.Minute), SizeBytes: 4},
				{NativeName: "wrong-stream.mp4", Stream: domain.StreamSub, StartAtUtc: start, EndAtUtc: start.Add(5 * time.Minute)},
				{NativeName: "older.mp4", Stream: domain.StreamMain, StartAtUtc: start.Add(-time.Hour), EndAtUtc: start.Add(-time.Minute)},
			}}
			if scenario == "invalid newest clip" {
				searcher.items = append(searcher.items, reolink.Recording{NativeName: "invalid.mp4", Stream: domain.StreamMain,
					StartAtUtc: start.Add(time.Minute), EndAtUtc: start.Add(4 * time.Minute), SizeBytes: 4})
			}
			service := NewRecoveryService(store, searcher, serviceCipher{})
			recoveryNow := start.Add(20 * time.Minute)
			service.now = func() time.Time { return recoveryNow }
			if err := service.Reconcile(ctx, camera.ID); err != nil {
				t.Fatal(err)
			}
			recordings, err := store.ListRecordings(ctx, camera.ID, 100)
			wantRecordings := 3
			if scenario == "invalid newest clip" {
				wantRecordings = 4
			}
			if err != nil || len(recordings) != wantRecordings {
				t.Fatalf("unrelated recordings queued: %#v %v", recordings, err)
			}
			duration := 210 * time.Second
			if partial {
				duration = time.Minute
			}
			downloader := NewDownloadService(store, &fileDownloader{payload: []byte("native-video")}, serviceCipher{}, t.TempDir())
			downloader.SetMediaInspector(mediaInspectorFunc(func(ctx context.Context, path string) (VideoInspection, error) {
				if strings.Contains(filepath.Base(path), "invalid.mp4") {
					return VideoInspection{}, ErrInvalidMedia
				}
				return (recoveryInspection{duration: duration}).InspectVideo(ctx, path)
			}))
			if scenario == "invalid newest clip" {
				if err := downloader.DownloadNext(ctx, camera.ID); !errors.Is(err, ErrInvalidMedia) {
					t.Fatalf("newest invalid clip was not rejected: %v", err)
				}
				invalidID := recordingID(camera.ID, "invalid.mp4")
				invalid, err := store.GetRecording(ctx, invalidID)
				if err != nil || invalid.State != domain.RecordingFailed {
					t.Fatalf("invalid clip did not become terminal: %v", err)
				}
				if final, err := store.RecoveryRecordingFinal(ctx, invalidID); err != nil || !final {
					t.Fatalf("recovery would requeue invalid clip: %v", err)
				}
				recoveryNow = recoveryNow.Add(time.Second)
				if err := service.Reconcile(ctx, camera.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := downloader.DownloadNext(ctx, camera.ID); err != nil {
				t.Fatal(err)
			}
			if err := service.Reconcile(ctx, camera.ID); err != nil {
				t.Fatal(err)
			}
			gaps, err := store.ListRecoveryGaps(ctx, camera.ID, 100)
			if err != nil {
				t.Fatal(err)
			}
			if !partial && len(gaps) != 0 {
				t.Fatalf("verified gap not closed: %#v", gaps)
			}
			if partial && (len(gaps) != 1 || gaps[0].Status != "Partial" || gaps[0].MissingSeconds < 100) {
				t.Fatalf("partial recovery incorrectly closed: %#v", gaps)
			}
			if err := service.Reconcile(ctx, camera.ID); err != nil {
				t.Fatal(err)
			}
			queued, err := store.ClaimNextDownload(ctx, camera.ID, start.Add(time.Hour))
			if err != nil || queued != nil {
				t.Fatalf("verified file downloaded twice: %#v %v", queued, err)
			}
		})
	}
}

func TestEstimatedClockRecoveryRemainsRetryableUntilVerified(t *testing.T) {
	ctx := context.Background()
	store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "estimated.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	start := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	configuration, err := json.Marshal(reolink.TimezoneConfiguration{Estimated: true})
	if err != nil {
		t.Fatal(err)
	}
	camera := domain.Camera{ID: "estimated", Name: "Estimated", Host: "camera", Port: 9000,
		Timezone: "Camera/" + base64.RawURLEncoding.EncodeToString(configuration),
		Enabled:  true, ArchiveEnabled: true, RecoveryEnabled: true,
		RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeEvents, ArchiveStream: domain.StreamMain,
		EventTypes: []domain.EventType{domain.EventPerson}, Capabilities: &domain.CameraCapabilities{StorageAvailable: true}, CreatedAtUtc: start, UpdatedAtUtc: start}
	if err := store.CreateCamera(ctx, camera, domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyVersion: 1}, camera.EventTypes); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncRecordingIntent(ctx, camera, true, start); err != nil {
		t.Fatal(err)
	}
	end := start.Add(time.Minute)
	if err := store.CloseRecordingIntents(ctx, camera.ID, end); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCaptureCoverage(ctx, camera.ID, camera.ArchiveStream, []domain.TimeRange{{StartAtUtc: start, EndAtUtc: end}}); err != nil {
		t.Fatal(err)
	}
	searcher := &serviceSearcher{}
	service := NewRecoveryService(store, searcher, serviceCipher{})
	service.now = func() time.Time { return start.Add(time.Hour) }
	if err := service.Reconcile(ctx, camera.ID); err != nil {
		t.Fatal(err)
	}
	gaps, err := store.ListRecoveryGaps(ctx, camera.ID, 100)
	if err != nil || len(gaps) != 1 || gaps[0].Status != "Provisional" {
		t.Fatalf("estimated empty search finalized the gap: %#v %v", gaps, err)
	}
	if err := store.UpdateCameraProbe(ctx, camera.ID, "Fixture", "test", "UTC", domain.CameraCapabilities{StorageAvailable: true}, "Online", start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := service.Reconcile(ctx, camera.ID); err != nil {
		t.Fatal(err)
	}
	gaps, err = store.ListRecoveryGaps(ctx, camera.ID, 100)
	if err != nil || len(gaps) != 0 || len(searcher.windows) != 2 {
		t.Fatalf("verified clock did not rescan the provisional gap: %#v searches=%d err=%v", gaps, len(searcher.windows), err)
	}
}

func TestRecoveryClockChangeDuringSearchRemainsRetryable(t *testing.T) {
	ctx := context.Background()
	store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "search-clock-race.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	start := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	camera := domain.Camera{ID: "changing-clock", Name: "Changing clock", Host: "camera", Port: 9000, Timezone: "UTC",
		Enabled: true, ArchiveEnabled: true, RecoveryEnabled: true, ArchiveStream: domain.StreamMain,
		RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeEvents,
		EventTypes: []domain.EventType{domain.EventPerson}, Capabilities: &domain.CameraCapabilities{StorageAvailable: true}, CreatedAtUtc: start, UpdatedAtUtc: start}
	if err := store.CreateCamera(ctx, camera, domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyVersion: 1}, camera.EventTypes); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncRecordingIntent(ctx, camera, true, start); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseRecordingIntents(ctx, camera.ID, end); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCaptureCoverage(ctx, camera.ID, camera.ArchiveStream, []domain.TimeRange{{StartAtUtc: start, EndAtUtc: end}}); err != nil {
		t.Fatal(err)
	}
	configuration, err := json.Marshal(reolink.TimezoneConfiguration{Estimated: true})
	if err != nil {
		t.Fatal(err)
	}
	searcher := &serviceSearcher{onSearch: func() {
		if err := store.UpdateCameraProbe(ctx, camera.ID, "Fixture", "test", "Camera/"+base64.RawURLEncoding.EncodeToString(configuration), *camera.Capabilities, "Online", end); err != nil {
			t.Fatal(err)
		}
	}}
	service := NewRecoveryService(store, searcher, serviceCipher{})
	service.now = func() time.Time { return start.Add(time.Hour) }
	if err := service.Reconcile(ctx, camera.ID); err != nil {
		t.Fatal(err)
	}
	gaps, err := store.ListRecoveryGaps(ctx, camera.ID, 10)
	if err != nil || len(gaps) != 1 {
		t.Fatal("a changed-clock search became terminal")
	}
	searcher.onSearch = nil
	if err := store.UpdateCameraProbe(ctx, camera.ID, "Fixture", "test", "UTC", *camera.Capabilities, "Online", end); err != nil {
		t.Fatal(err)
	}
	if err := service.Reconcile(ctx, camera.ID); err != nil {
		t.Fatal(err)
	}
	gaps, err = store.ListRecoveryGaps(ctx, camera.ID, 10)
	if err != nil || len(gaps) != 0 || len(searcher.windows) != 2 {
		t.Fatal("changed-clock search did not rescan on reliable rules")
	}
}

func TestProvisionalRecoveryBacklogIsBoundedAndRescanned(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "linked recordings"}[linked], func(t *testing.T) {
			ctx := context.Background()
			databasePath := filepath.Join(t.TempDir(), "backlog.db")
			store, err := repository.Open(ctx, databasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			start := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
			configuration, err := json.Marshal(reolink.TimezoneConfiguration{Estimated: true})
			if err != nil {
				t.Fatal(err)
			}
			camera := domain.Camera{ID: "backlog", Name: "Backlog", Host: "camera", Port: 9000,
				Timezone: "Camera/" + base64.RawURLEncoding.EncodeToString(configuration),
				Enabled:  true, ArchiveEnabled: true, RecoveryEnabled: true, ArchiveStream: domain.StreamMain,
				RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeEvents,
				EventTypes: []domain.EventType{domain.EventPerson}, Capabilities: &domain.CameraCapabilities{StorageAvailable: true}, CreatedAtUtc: start, UpdatedAtUtc: start}
			if err := store.CreateCamera(ctx, camera, domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyVersion: 1}, camera.EventTypes); err != nil {
				t.Fatal(err)
			}
			if err := store.SyncRecordingIntent(ctx, camera, true, start); err != nil {
				t.Fatal(err)
			}
			searcher := &serviceSearcher{}
			service := NewRecoveryService(store, searcher, serviceCipher{})
			now := start.Add(10 * time.Minute)
			service.now = func() time.Time { return now }
			for cycle := 0; cycle < 36; cycle++ {
				now = now.Add(5 * time.Minute)
				if linked {
					clipStart := now.Add(-15 * time.Minute)
					searcher.items = []reolink.Recording{{NativeName: now.Format("150405") + ".mp4", Stream: camera.ArchiveStream,
						StartAtUtc: clipStart, EndAtUtc: clipStart.Add(time.Minute), EventTypes: camera.EventTypes}}
				}
				if err := service.Reconcile(ctx, camera.ID); err != nil {
					t.Fatal(err)
				}
			}
			gaps, err := store.ListRecoveryGaps(ctx, camera.ID, 1000)
			if err != nil || len(gaps) != 1 || gaps[0].Status != "Provisional" {
				t.Fatalf("unchanged estimate accumulated a backlog: rows=%d err=%v", len(gaps), err)
			}
			end := now.Add(-10 * time.Minute)
			if !gaps[0].StartAtUtc.Equal(start) || !gaps[0].EndAtUtc.Equal(end) {
				t.Fatal("compaction lost affected history")
			}
			if linked {
				required, err := store.ListRequiredEventCoverage(ctx, gaps[0])
				if err != nil || len(required) != 36 {
					t.Fatal("compaction lost recording links")
				}
				if gaps[0].MissingSeconds != 36*time.Minute.Seconds() {
					t.Fatalf("bounded retry lost full-gap event accounting: %.0f", gaps[0].MissingSeconds)
				}
			}
			if len(searcher.windows) > 55 {
				t.Fatalf("unchanged estimates queried too often: %d", len(searcher.windows))
			}
			if err := store.CloseRecordingIntents(ctx, camera.ID, end); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = repository.Open(ctx, databasePath)
			if err != nil {
				t.Fatal(err)
			}
			service.store = store
			previousCalls := len(searcher.windows)
			if err := service.Reconcile(ctx, camera.ID); err != nil {
				t.Fatal(err)
			}
			if len(searcher.windows) != previousCalls {
				t.Fatal("restart lost unchanged-clock backoff")
			}
			configuration, err = json.Marshal(reolink.TimezoneConfiguration{BaseOffsetSeconds: 3600, Estimated: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateCameraProbe(ctx, camera.ID, "Fixture", "test", "Camera/"+base64.RawURLEncoding.EncodeToString(configuration), domain.CameraCapabilities{StorageAvailable: true}, "Online", now); err != nil {
				t.Fatal(err)
			}
			if err := service.Reconcile(ctx, camera.ID); err != nil {
				t.Fatal(err)
			}
			if len(searcher.windows) != previousCalls+1 {
				t.Fatal("changed estimated offset did not invalidate backoff")
			}
			previousCalls = len(searcher.windows)
			if err := store.SaveCaptureCoverage(ctx, camera.ID, camera.ArchiveStream, []domain.TimeRange{{StartAtUtc: start, EndAtUtc: end}}); err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateCameraProbe(ctx, camera.ID, "Fixture", "test", "UTC", domain.CameraCapabilities{StorageAvailable: true}, "Online", now); err != nil {
				t.Fatal(err)
			}
			searcher.items = nil
			for cycle := 0; cycle < 12; cycle++ {
				if err := service.Reconcile(ctx, camera.ID); err != nil {
					t.Fatal(err)
				}
			}
			for _, window := range searcher.windows[previousCalls:] {
				if window[1].Sub(window[0]) > 30*time.Minute+2*coverageTolerance {
					t.Fatal("clock correction caused an unbounded native search")
				}
			}
			if !linked {
				gaps, err = store.ListRecoveryGaps(ctx, camera.ID, 1000)
				if err != nil || len(gaps) != 0 {
					t.Fatalf("reliable clock did not completely rescan provisional history: %d %v", len(gaps), err)
				}
				var covered []domain.TimeRange
				for _, window := range searcher.windows[previousCalls:] {
					covered = append(covered, domain.TimeRange{StartAtUtc: window[0], EndAtUtc: window[1]})
				}
				if missingDuration(missingCoverage(domain.TimeRange{StartAtUtc: start, EndAtUtc: end}, covered)) != 0 {
					t.Fatal("reliable rescan skipped historical intervals")
				}
			}
		})
	}
}

func TestRecoveryRescanKeepsAmbiguousDSTResultsProvisional(t *testing.T) {
	for _, withClip := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty search", true: "downloaded clip"}[withClip], func(t *testing.T) {
			ctx := context.Background()
			store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "ambiguous-rescan.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			start := time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC)
			end := start.Add(time.Minute)
			configuration, err := json.Marshal(reolink.TimezoneConfiguration{BaseOffsetSeconds: -7 * 3600, Estimated: true})
			if err != nil {
				t.Fatal(err)
			}
			camera := domain.Camera{ID: "ambiguous", Name: "Ambiguous", Host: "camera", Port: 9000,
				Timezone: "Camera/" + base64.RawURLEncoding.EncodeToString(configuration),
				Enabled:  true, ArchiveEnabled: true, RecoveryEnabled: true, ArchiveStream: domain.StreamMain,
				RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeEvents,
				EventTypes: []domain.EventType{domain.EventPerson}, Capabilities: &domain.CameraCapabilities{StorageAvailable: true}, CreatedAtUtc: start, UpdatedAtUtc: start}
			if err := store.CreateCamera(ctx, camera, domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyVersion: 1}, camera.EventTypes); err != nil {
				t.Fatal(err)
			}
			if err := store.SyncRecordingIntent(ctx, camera, true, start); err != nil {
				t.Fatal(err)
			}
			if err := store.CloseRecordingIntents(ctx, camera.ID, end); err != nil {
				t.Fatal(err)
			}
			if err := store.SaveCaptureCoverage(ctx, camera.ID, camera.ArchiveStream, []domain.TimeRange{{StartAtUtc: start, EndAtUtc: end}}); err != nil {
				t.Fatal(err)
			}
			searcher := &serviceSearcher{}
			if withClip {
				searcher.items = []reolink.Recording{{NativeName: "ambiguous.mp4", Stream: domain.StreamMain, StartAtUtc: start, EndAtUtc: end,
					EventTypes: []domain.EventType{domain.EventPerson}, RawMetadata: json.RawMessage(`{"startTime":"2026-11-01T01:30:00","endTime":"2026-11-01T01:31:00"}`)}}
			}
			service := NewRecoveryService(store, searcher, serviceCipher{})
			recoveryNow := start.Add(time.Hour)
			service.now = func() time.Time { return recoveryNow }
			if err := service.Reconcile(ctx, camera.ID); err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateCameraProbe(ctx, camera.ID, "Fixture", "test", "America/Los_Angeles", domain.CameraCapabilities{StorageAvailable: true}, "Online", recoveryNow); err != nil {
				t.Fatal(err)
			}
			recoveryNow = recoveryNow.Add(time.Second)
			if err := service.Reconcile(ctx, camera.ID); err != nil {
				t.Fatal(err)
			}
			if withClip {
				recording, err := store.GetRecording(ctx, recordingID(camera.ID, "ambiguous.mp4"))
				if err != nil || !strings.Contains(string(recording.RawMetadata), `"_reonvrEstimatedClock":true`) {
					t.Fatal("reliable-clock rescan erased ambiguous recording uncertainty")
				}
				downloader := NewDownloadService(store, &fileDownloader{payload: []byte("native-video")}, serviceCipher{}, t.TempDir())
				downloader.SetMediaInspector(recoveryInspection{duration: time.Minute})
				if err := downloader.DownloadNext(ctx, camera.ID); err != nil {
					t.Fatal(err)
				}
				if err := service.Reconcile(ctx, camera.ID); err != nil {
					t.Fatal(err)
				}
				coverage, err := store.ListVerifiedCoverage(ctx, camera.ID, camera.ArchiveStream, domain.TimeRange{StartAtUtc: start, EndAtUtc: end}, false)
				if err != nil || len(coverage) != 0 {
					t.Fatal("ambiguous native footage became trusted coverage")
				}
			}
			gaps, err := store.ListRecoveryGaps(ctx, camera.ID, 100)
			if err != nil || len(gaps) != 1 || gaps[0].Status != "Provisional" {
				t.Fatalf("ambiguous rescan became conclusive: %#v %v", gaps, err)
			}
			calls := len(searcher.windows)
			for cycle := 0; cycle < 6; cycle++ {
				recoveryNow = recoveryNow.Add(10 * time.Minute)
				if err := service.Reconcile(ctx, camera.ID); err != nil {
					t.Fatal(err)
				}
			}
			if len(searcher.windows) != calls {
				t.Fatal("unchanged reliable rules repeatedly queried an unresolvable DST interval")
			}
		})
	}
}

func TestReliableAmbiguousRecoveryCompactsWithoutRejectedNativeSearch(t *testing.T) {
	configuration, err := json.Marshal(reolink.TimezoneConfiguration{BaseOffsetSeconds: -8 * 3600, DstEnabled: true, DstOffsetSeconds: 3600,
		DstStart: reolink.DstTransition{Month: 3, Week: 2, Weekday: 0, Hour: 2}, DstEnd: reolink.DstTransition{Month: 11, Week: 1, Weekday: 0, Hour: 2}})
	if err != nil {
		t.Fatal(err)
	}
	for _, timezone := range []string{"America/Los_Angeles", "Camera/" + base64.RawURLEncoding.EncodeToString(configuration)} {
		t.Run(map[bool]string{false: "IANA", true: "custom"}[strings.HasPrefix(timezone, "Camera/")], func(t *testing.T) {
			ctx := context.Background()
			store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "reliable-ambiguity.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			start := time.Date(2026, 11, 1, 8, 15, 0, 0, time.UTC)
			end := start.Add(time.Hour)
			camera := domain.Camera{ID: "reliable-ambiguity", Name: "Reliable ambiguity", Host: "camera", Port: 9000, Timezone: timezone,
				Enabled: true, ArchiveEnabled: true, RecoveryEnabled: true, ArchiveStream: domain.StreamMain,
				RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeEvents,
				EventTypes: []domain.EventType{domain.EventPerson}, Capabilities: &domain.CameraCapabilities{StorageAvailable: true}, CreatedAtUtc: start, UpdatedAtUtc: start}
			if err := store.CreateCamera(ctx, camera, domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyVersion: 1}, camera.EventTypes); err != nil {
				t.Fatal(err)
			}
			if err := store.SyncRecordingIntent(ctx, camera, true, start); err != nil {
				t.Fatal(err)
			}
			if err := store.CloseRecordingIntents(ctx, camera.ID, end); err != nil {
				t.Fatal(err)
			}
			periods, err := store.ListRecoveryPeriods(ctx, camera.ID, end.Add(time.Hour))
			if err != nil || len(periods) != 1 {
				t.Fatal("missing recovery intent")
			}
			var gaps []domain.RecoveryGap
			for index := 0; index < 4; index++ {
				from := start.Add(time.Duration(index) * 15 * time.Minute)
				gaps = append(gaps, domain.RecoveryGap{ID: from.Format("150405"), PeriodID: periods[0].ID, CameraID: camera.ID,
					TimeRange: domain.TimeRange{StartAtUtc: from, EndAtUtc: from.Add(15 * time.Minute)}})
			}
			if err := store.SaveRecoveryScan(ctx, periods[0].ID, end, gaps); err != nil {
				t.Fatal(err)
			}
			service := NewRecoveryService(store, reolink.NewClient(nil, time.Second), serviceCipher{})
			now := end.Add(time.Hour)
			service.now = func() time.Time { return now }
			if err := service.Reconcile(ctx, camera.ID); err != nil {
				t.Fatal(err)
			}
			pending, err := store.ListRecoveryGaps(ctx, camera.ID, 10)
			if err != nil || len(pending) != 1 || pending[0].Status != "Provisional" || !pending[0].ClockAmbiguous {
				t.Fatal("reliable ambiguity was not durably compacted")
			}
			if pending[0].NextSearchAtUtc == nil || pending[0].NextSearchAtUtc.Before(now.Add(23*time.Hour)) {
				t.Fatal("reliable ambiguity has no long retry cooldown")
			}
			now = now.Add(time.Hour)
			if err := service.Reconcile(ctx, camera.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLateVerifiedCoverageClosesEstimatedClockGap(t *testing.T) {
	ctx := context.Background()
	store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "late-coverage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	start := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	configuration, err := json.Marshal(reolink.TimezoneConfiguration{Estimated: true})
	if err != nil {
		t.Fatal(err)
	}
	camera := domain.Camera{ID: "late-coverage", Name: "Late coverage", Host: "camera", Port: 9000,
		Timezone: "Camera/" + base64.RawURLEncoding.EncodeToString(configuration),
		Enabled:  true, ArchiveEnabled: true, RecoveryEnabled: true, ArchiveStream: domain.StreamMain,
		RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeContinuous,
		Capabilities: &domain.CameraCapabilities{StorageAvailable: true}, CreatedAtUtc: start, UpdatedAtUtc: start}
	if err := store.CreateCamera(ctx, camera, domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyVersion: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncRecordingIntent(ctx, camera, true, start); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseRecordingIntents(ctx, camera.ID, end); err != nil {
		t.Fatal(err)
	}
	searcher := &serviceSearcher{}
	service := NewRecoveryService(store, searcher, serviceCipher{})
	service.now = func() time.Time { return start.Add(time.Hour) }
	if err := service.Reconcile(ctx, camera.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRtspRecording(ctx, domain.Recording{ID: "late-rtsp", CameraID: camera.ID, NativeName: "late.mkv",
		Source: domain.RecordingSourceRtsp, Stream: domain.StreamMain, StartAtUtc: start, EndAtUtc: end,
		State: domain.RecordingComplete, LocalPath: "late.mkv", DiscoveredAtUtc: end, UpdatedAtUtc: end}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRecordingCoverage(ctx, "late-rtsp", []domain.TimeRange{{StartAtUtc: start, EndAtUtc: end}}); err != nil {
		t.Fatal(err)
	}
	if err := service.Reconcile(ctx, camera.ID); err != nil {
		t.Fatal(err)
	}
	gaps, err := store.ListRecoveryGaps(ctx, camera.ID, 100)
	if err != nil || len(gaps) != 0 || len(searcher.windows) != 1 {
		t.Fatalf("trusted late footage still caused provisional searches: gaps=%d searches=%d err=%v", len(gaps), len(searcher.windows), err)
	}
}

func TestCorrectedDSTDurationReinspectsRetainedMedia(t *testing.T) {
	ctx := context.Background()
	store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "duration-revalidation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	start := time.Date(2026, 11, 1, 7, 45, 0, 0, time.UTC)
	configuration, err := json.Marshal(reolink.TimezoneConfiguration{BaseOffsetSeconds: -7 * 3600, Estimated: true})
	if err != nil {
		t.Fatal(err)
	}
	camera := domain.Camera{ID: "duration", Name: "Duration", Host: "camera", Port: 9000,
		Timezone: "Camera/" + base64.RawURLEncoding.EncodeToString(configuration), Enabled: true, ArchiveEnabled: true,
		ArchiveStream: domain.StreamMain, RecordingSource: domain.RecordingSourceCameraStorage, RecordingMode: domain.RecordingModeContinuous,
		CreatedAtUtc: start, UpdatedAtUtc: start}
	if err := store.CreateCamera(ctx, camera, domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyVersion: 1}, nil); err != nil {
		t.Fatal(err)
	}
	recording := domain.Recording{ID: "duration-clip", CameraID: camera.ID, NativeName: "duration.mp4", Source: domain.RecordingSourceCameraStorage,
		Stream: camera.ArchiveStream, StartAtUtc: start, EndAtUtc: start.Add(90 * time.Minute), State: domain.RecordingQueued,
		RawMetadata: json.RawMessage(`{"startTime":"2026-11-01T00:45:00","endTime":"2026-11-01T02:15:00"}`), DiscoveredAtUtc: start, UpdatedAtUtc: start}
	if err := store.SaveReconciliation(ctx, domain.IngestionRun{ID: "duration-run", CameraID: camera.ID, Status: "Complete", StartedAtUtc: start}, []domain.Recording{recording}, start); err != nil {
		t.Fatal(err)
	}
	archiveRoot := t.TempDir()
	downloader := NewDownloadService(store, &fileDownloader{payload: []byte("native-video")}, serviceCipher{}, archiveRoot)
	downloader.SetMediaInspector(recoveryInspection{duration: 150 * time.Minute})
	if err := downloader.DownloadNext(ctx, camera.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateCameraProbe(ctx, camera.ID, "Fixture", "test", "America/Los_Angeles", domain.CameraCapabilities{}, "Online", start.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	pending, err := store.GetRecording(ctx, recording.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed, err := store.CompleteSidecarRepair(ctx, pending, nil); err == nil || completed {
		t.Fatal("corrected coverage was promoted without reinspection")
	}
	retention := NewRetentionService(store, archiveRoot)
	downloader.SetCapacityGuard(retention)
	downloader.SetMediaInspector(mediaInspectorFunc(func(context.Context, string) (VideoInspection, error) {
		if retention.mu.TryLock() {
			retention.mu.Unlock()
			t.Fatal("retention could delete files during sidecar publication")
		}
		return recoveryInspection{duration: 150 * time.Minute}.InspectVideo(ctx, "")
	}))
	if err := downloader.RepairSidecars(ctx); err != nil {
		t.Fatal(err)
	}
	span := domain.TimeRange{StartAtUtc: start, EndAtUtc: start.Add(150 * time.Minute)}
	coverage, err := store.ListVerifiedCoverage(ctx, camera.ID, camera.ArchiveStream, span, false)
	if err != nil || missingDuration(missingCoverage(span, coverage)) != 0 {
		t.Fatal("corrected duration retained only estimated clipped coverage")
	}
}

func TestEstimatedRecordingCoverageRevalidatesOnClockCorrection(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "estimated-coverage.db")
	store, err := repository.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	start := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	configuration, err := json.Marshal(reolink.TimezoneConfiguration{BaseOffsetSeconds: 3600, Estimated: true})
	if err != nil {
		t.Fatal(err)
	}
	camera := domain.Camera{ID: "estimated-coverage", Name: "Estimated", Host: "camera", Port: 9000,
		Timezone: "Camera/" + base64.RawURLEncoding.EncodeToString(configuration),
		Enabled:  true, ArchiveEnabled: true, RecoveryEnabled: true,
		RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeContinuous, ArchiveStream: domain.StreamMain,
		Capabilities: &domain.CameraCapabilities{StorageAvailable: true}, CreatedAtUtc: start, UpdatedAtUtc: start}
	if err := store.CreateCamera(ctx, camera, domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyVersion: 1}, camera.EventTypes); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncRecordingIntent(ctx, camera, true, start); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseRecordingIntents(ctx, camera.ID, end); err != nil {
		t.Fatal(err)
	}
	searcher := &serviceSearcher{items: []reolink.Recording{{NativeName: "estimated.mp4", Stream: domain.StreamMain,
		StartAtUtc: start, EndAtUtc: end, RawMetadata: json.RawMessage(`{"startTime":"2026-11-11T11:00:00","endTime":"2026-11-11T11:01:00"}`)}}}
	service := NewRecoveryService(store, searcher, serviceCipher{})
	recoveryNow := start.Add(2 * time.Hour)
	service.now = func() time.Time { return recoveryNow }
	archiveRoot := t.TempDir()
	downloader := NewDownloadService(store, &fileDownloader{payload: []byte("native-video")}, serviceCipher{}, archiveRoot)
	downloader.SetMediaInspector(recoveryInspection{duration: time.Minute})
	if err := service.Reconcile(ctx, camera.ID); err != nil {
		t.Fatal(err)
	}
	if err := downloader.DownloadNext(ctx, camera.ID); err != nil {
		t.Fatal(err)
	}
	span := domain.TimeRange{StartAtUtc: start, EndAtUtc: end}
	coverage, err := store.ListVerifiedCoverage(ctx, camera.ID, camera.ArchiveStream, span, false)
	if err != nil || len(coverage) != 0 {
		t.Fatalf("estimated recording became conclusive coverage: %#v %v", coverage, err)
	}
	if err := service.Reconcile(ctx, camera.ID); err != nil {
		t.Fatal(err)
	}
	gaps, err := store.ListRecoveryGaps(ctx, camera.ID, 100)
	if err != nil || len(gaps) != 1 || gaps[0].Status != "Provisional" {
		t.Fatalf("estimated download finalized the gap: %#v %v", gaps, err)
	}
	if err := store.UpdateCameraProbe(ctx, camera.ID, "Fixture", "test", "UTC", domain.CameraCapabilities{StorageAvailable: true}, "Online", start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	estimated, err := store.GetRecording(ctx, recordingID(camera.ID, "estimated.mp4"))
	if err != nil || estimated.State != domain.RecordingComplete || !estimated.StartAtUtc.Equal(start.Add(time.Hour)) {
		t.Fatalf("downloaded native timestamps were not corrected: %#v %v", estimated, err)
	}
	coverage, err = store.ListVerifiedCoverage(ctx, camera.ID, camera.ArchiveStream,
		domain.TimeRange{StartAtUtc: start.Add(time.Hour), EndAtUtc: end.Add(time.Hour)}, false)
	if err != nil || len(coverage) != 0 {
		t.Fatal("coverage became conclusive before its sidecar repair completed")
	}
	stale := estimated
	stale.StartAtUtc = start
	if completed, err := store.CompleteSidecarRepair(ctx, stale, nil); err != nil || completed {
		t.Fatal("stale sidecar acknowledgement cleared pending repair")
	}
	broken := NewDownloadService(store, nil, serviceCipher{}, filepath.Join(archiveRoot, "unavailable"))
	if err := broken.RepairSidecars(ctx); err == nil {
		t.Fatal("missing archive root did not fail the repair")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = repository.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	service.store = store
	downloader = NewDownloadService(store, &fileDownloader{payload: []byte("native-video")}, serviceCipher{}, archiveRoot)
	downloader.SetMediaInspector(recoveryInspection{duration: time.Minute})
	pending, err := store.ListSidecarRepairs(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatal("failed sidecar repair did not survive restart")
	}
	rescan := pending[0]
	rescan.RawMetadata = json.RawMessage(`{"startTime":"2026-11-11T11:00:00","endTime":"2026-11-11T11:01:00"}`)
	if err := store.SaveReconciliation(ctx, domain.IngestionRun{ID: "pending-sidecar-rescan", CameraID: camera.ID, Kind: "Recovery", Status: "Complete", StartedAtUtc: end}, []domain.Recording{rescan}, end); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListSidecarRepairs(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatal("rescan erased a durable pending sidecar repair")
	}
	if err := downloader.RepairSidecars(ctx); err != nil {
		t.Fatal(err)
	}
	sidecarData, err := os.ReadFile(filepath.Join(archiveRoot, filepath.FromSlash(estimated.LocalPath)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var sidecar struct {
		Start time.Time `json:"startAtUtc"`
		End   time.Time `json:"endAtUtc"`
	}
	if err := json.Unmarshal(sidecarData, &sidecar); err != nil {
		t.Fatal(err)
	}
	if !sidecar.Start.Equal(estimated.StartAtUtc) || !sidecar.End.Equal(estimated.EndAtUtc) {
		t.Fatal("sidecar timestamps disagree with the corrected recording index")
	}
	coverage, err = store.ListVerifiedCoverage(ctx, camera.ID, camera.ArchiveStream,
		domain.TimeRange{StartAtUtc: start.Add(time.Hour), EndAtUtc: end.Add(time.Hour)}, false)
	if err != nil || len(coverage) != 1 || !coverage[0].StartAtUtc.Equal(start.Add(time.Hour)) {
		t.Fatalf("verified media coverage was not realigned: %#v %v", coverage, err)
	}
	searcher.items = nil
	if err := service.Reconcile(ctx, camera.ID); err != nil {
		t.Fatal(err)
	}
	gaps, err = store.ListRecoveryGaps(ctx, camera.ID, 100)
	if err != nil || len(gaps) != 1 {
		t.Fatalf("unrelated corrected footage closed the original gap: %#v %v", gaps, err)
	}
	searcher.items = []reolink.Recording{{NativeName: "correct.mp4", Stream: domain.StreamMain, StartAtUtc: start, EndAtUtc: end,
		RawMetadata: json.RawMessage(`{"startTime":"2026-11-11T10:00:00","endTime":"2026-11-11T10:01:00"}`)}}
	recoveryNow = recoveryNow.Add(time.Second)
	if err := service.Reconcile(ctx, camera.ID); err != nil {
		t.Fatal(err)
	}
	if err := downloader.DownloadNext(ctx, camera.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Reconcile(ctx, camera.ID); err != nil {
		t.Fatal(err)
	}
	gaps, err = store.ListRecoveryGaps(ctx, camera.ID, 100)
	if err != nil || len(gaps) != 0 {
		t.Fatalf("accurately timed verified footage did not close the gap: %#v %v", gaps, err)
	}
}

func TestEventRecoveryDistinguishesQuietCaptureFromUnknownOutage(t *testing.T) {
	for _, scenario := range []string{"quiet", "outage", "unpublished selected event", "overlapping selected events", "unselected event", "disappeared queued native clip"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "events.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			start := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
			camera := domain.Camera{ID: "events", Name: "Events", Host: "camera", Port: 9000, Timezone: "UTC", Enabled: true, ArchiveEnabled: true, RecoveryEnabled: true,
				RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeEvents, ArchiveStream: domain.StreamMain,
				PreEventSeconds: 3, PostEventSeconds: 7,
				EventTypes: []domain.EventType{domain.EventPerson}, Capabilities: &domain.CameraCapabilities{StorageAvailable: true}, CreatedAtUtc: start, UpdatedAtUtc: start}
			if err := store.CreateCamera(ctx, camera, domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyVersion: 1}, camera.EventTypes); err != nil {
				t.Fatal(err)
			}
			if err := store.SyncRecordingIntent(ctx, camera, true, start); err != nil {
				t.Fatal(err)
			}
			end := start.Add(time.Minute)
			if err := store.CloseRecordingIntents(ctx, camera.ID, end); err != nil {
				t.Fatal(err)
			}
			if scenario != "outage" {
				if err := store.SaveCaptureCoverage(ctx, camera.ID, camera.ArchiveStream, []domain.TimeRange{{StartAtUtc: start, EndAtUtc: end}}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "unpublished selected event" || scenario == "overlapping selected events" || scenario == "unselected event" {
				eventType := domain.EventPerson
				if scenario == "unselected event" {
					eventType = domain.EventMotion
				}
				if err := store.ApplyEventTransition(ctx, camera.ID, domain.EventTransition{OccurredAtUtc: start.Add(5 * time.Second), EventType: eventType, Active: true}); err != nil {
					t.Fatal(err)
				}
				if err := store.ApplyEventTransition(ctx, camera.ID, domain.EventTransition{OccurredAtUtc: start.Add(15 * time.Second)}); err != nil {
					t.Fatal(err)
				}
				if scenario == "overlapping selected events" {
					if err := store.ApplyEventTransition(ctx, camera.ID, domain.EventTransition{OccurredAtUtc: start.Add(16 * time.Second), EventType: eventType, Active: true}); err != nil {
						t.Fatal(err)
					}
					if err := store.ApplyEventTransition(ctx, camera.ID, domain.EventTransition{OccurredAtUtc: start.Add(20 * time.Second)}); err != nil {
						t.Fatal(err)
					}
				}
			}
			searcher := &serviceSearcher{}
			if scenario == "disappeared queued native clip" {
				searcher.items = []reolink.Recording{{NativeName: "selected-native.mp4", Stream: domain.StreamMain,
					StartAtUtc: start.Add(5 * time.Second), EndAtUtc: start.Add(15 * time.Second), EventTypes: []domain.EventType{domain.EventPerson}}}
			}
			service := NewRecoveryService(store, searcher, serviceCipher{})
			service.now = func() time.Time { return start.Add(time.Hour) }
			if err := service.Reconcile(ctx, camera.ID); err != nil {
				t.Fatal(err)
			}
			if scenario == "disappeared queued native clip" {
				searcher.items = nil
				if err := service.Reconcile(ctx, camera.ID); err != nil {
					t.Fatal(err)
				}
			}
			gaps, err := store.ListRecoveryGaps(ctx, camera.ID, 100)
			if err != nil {
				t.Fatal(err)
			}
			if (scenario == "quiet" || scenario == "unselected event") && len(gaps) != 0 {
				t.Fatalf("quiet time treated as missing event: %#v", gaps)
			}
			if scenario == "outage" && (len(gaps) != 1 || gaps[0].Status != "Unknown") {
				t.Fatalf("outage declared recovered without evidence: %#v", gaps)
			}
			if scenario == "unpublished selected event" && (len(gaps) != 1 || gaps[0].Status == "CheckedEvents" || gaps[0].MissingSeconds != 20) {
				t.Fatalf("known missing event footage was declared quiet: %#v", gaps)
			}
			if scenario == "overlapping selected events" && (len(gaps) != 1 || gaps[0].MissingSeconds != 25) {
				t.Fatalf("overlapping pre/post windows were counted twice: %#v", gaps)
			}
			if scenario == "disappeared queued native clip" {
				if len(gaps) != 1 || gaps[0].MissingSeconds != 10 {
					t.Fatalf("empty later search erased known missing native footage: %#v", gaps)
				}
				claimed, err := store.ClaimNextDownload(ctx, camera.ID, start.Add(time.Hour))
				if err != nil || claimed == nil || claimed.NativeName != "selected-native.mp4" {
					t.Fatalf("linked native recording was made ineligible: %v", err)
				}
			}
		})
	}
}

func TestRecoveryUsesSavedModeForProvisionalWindow(t *testing.T) {
	for _, test := range []struct {
		name         string
		previousMode domain.RecordingMode
		currentMode  domain.RecordingMode
		wantGaps     int
	}{
		{name: "event period remains provisional", previousMode: domain.RecordingModeEvents, currentMode: domain.RecordingModeContinuous, wantGaps: 0},
		{name: "continuous period is ready", previousMode: domain.RecordingModeContinuous, currentMode: domain.RecordingModeEvents, wantGaps: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "policy.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			start := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
			camera := domain.Camera{ID: "camera-one", Name: "Front", Host: "camera", Port: 9000, Timezone: "UTC", Enabled: true, ArchiveEnabled: true, RecoveryEnabled: true,
				RecordingSource: domain.RecordingSourceRtsp, RecordingMode: test.currentMode, ArchiveStream: domain.StreamMain,
				EventTypes: []domain.EventType{domain.EventPerson}, Capabilities: &domain.CameraCapabilities{StorageAvailable: true}, CreatedAtUtc: start, UpdatedAtUtc: start}
			if err := store.CreateCamera(ctx, camera, domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyVersion: 1}, camera.EventTypes); err != nil {
				t.Fatal(err)
			}
			priorPolicy := camera
			priorPolicy.RecordingMode = test.previousMode
			if err := store.SyncRecordingIntent(ctx, priorPolicy, true, start); err != nil {
				t.Fatal(err)
			}
			if err := store.CloseRecordingIntents(ctx, camera.ID, start.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			service := NewRecoveryService(store, &serviceSearcher{}, serviceCipher{})
			service.now = func() time.Time { return start.Add(7 * time.Minute) }
			if err := service.Reconcile(ctx, camera.ID); err != nil {
				t.Fatal(err)
			}
			gaps, err := store.ListRecoveryHistory(ctx, camera.ID, 100)
			if err != nil || len(gaps) != test.wantGaps {
				t.Fatalf("saved policy maturity changed with current mode: gaps=%#v err=%v", gaps, err)
			}
		})
	}
}

func TestMissingCoverageUsesUnionAndClipsToExpectedPeriod(t *testing.T) {
	start := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	span := func(from, to time.Duration) domain.TimeRange {
		return domain.TimeRange{StartAtUtc: start.Add(from * time.Second), EndAtUtc: start.Add(to * time.Second)}
	}
	tests := []struct {
		name     string
		expected domain.TimeRange
		coverage []domain.TimeRange
		missing  []domain.TimeRange
	}{
		{name: "empty", expected: span(0, 100), missing: []domain.TimeRange{span(0, 100)}},
		{name: "overlap", expected: span(0, 100), coverage: []domain.TimeRange{span(60, 120), span(-10, 30), span(20, 40)}, missing: []domain.TimeRange{span(40, 60)}},
		{name: "complete", expected: span(0, 100), coverage: []domain.TimeRange{span(50, 100), span(0, 50)}},
		{name: "partial recovery", expected: span(0, 100), coverage: []domain.TimeRange{span(0, 25), span(75, 100)}, missing: []domain.TimeRange{span(25, 75)}},
		{name: "outside", expected: span(0, 100), coverage: []domain.TimeRange{span(-20, -10), span(110, 120)}, missing: []domain.TimeRange{span(0, 100)}},
		{name: "invalid", expected: span(10, 0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := missingCoverage(test.expected, test.coverage)
			if len(actual) != len(test.missing) {
				t.Fatalf("expected %#v, got %#v", test.missing, actual)
			}
			for index := range actual {
				if actual[index] != test.missing[index] {
					t.Fatalf("expected %#v, got %#v", test.missing, actual)
				}
			}
		})
	}
}
