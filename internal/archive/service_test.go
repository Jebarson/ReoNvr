package archive

import (
	"context"
	"encoding/json"
	"path/filepath"
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
}

func (searcher *serviceSearcher) SearchRecordings(_ context.Context, connection reolink.Connection, _ domain.ArchiveStream, from, to time.Time) ([]reolink.Recording, error) {
	searcher.connection = connection
	searcher.from = from
	searcher.to = to
	searcher.windows = append(searcher.windows, [2]time.Time{from, to})
	return searcher.items, nil
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
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "partial"}[partial], func(t *testing.T) {
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
			service := NewRecoveryService(store, searcher, serviceCipher{})
			service.now = func() time.Time { return start.Add(20 * time.Minute) }
			if err := service.Reconcile(ctx, camera.ID); err != nil {
				t.Fatal(err)
			}
			recordings, err := store.ListRecordings(ctx, camera.ID, 100)
			if err != nil || len(recordings) != 3 {
				t.Fatalf("unrelated recordings queued: %#v %v", recordings, err)
			}
			duration := 210 * time.Second
			if partial {
				duration = time.Minute
			}
			downloader := NewDownloadService(store, &fileDownloader{payload: []byte("native-video")}, serviceCipher{}, t.TempDir())
			downloader.SetMediaInspector(recoveryInspection{duration: duration})
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

func TestEventRecoveryDistinguishesQuietCaptureFromUnknownOutage(t *testing.T) {
	for _, scenario := range []string{"quiet", "outage", "unpublished selected event", "overlapping selected events", "unselected event"} {
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
			service := NewRecoveryService(store, &serviceSearcher{}, serviceCipher{})
			service.now = func() time.Time { return start.Add(time.Hour) }
			if err := service.Reconcile(ctx, camera.ID); err != nil {
				t.Fatal(err)
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
