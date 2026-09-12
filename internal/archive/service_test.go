package archive

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"
)

type serviceStore struct {
	camera     domain.Camera
	credential domain.EncryptedCredential
	watermark  *time.Time
	run        domain.IngestionRun
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

func (store *serviceStore) SaveReconciliation(_ context.Context, run domain.IngestionRun, recordings []domain.Recording, _ time.Time) error {
	store.run = run
	store.recordings = recordings
	return nil
}

func (store *serviceStore) ListRecordings(context.Context, string, int) ([]domain.Recording, error) {
	return store.recordings, nil
}

type serviceSearcher struct {
	connection reolink.Connection
	from       time.Time
	to         time.Time
	items      []reolink.Recording
}

func (searcher *serviceSearcher) SearchRecordings(_ context.Context, connection reolink.Connection, _ domain.ArchiveStream, from, to time.Time) ([]reolink.Recording, error) {
	searcher.connection = connection
	searcher.from = from
	searcher.to = to
	return searcher.items, nil
}

type serviceCipher struct{}

func (serviceCipher) Decrypt(string, []byte, []byte) (string, error) { return "secret", nil }

func TestReconcileFiltersAndPersistsSelectedRecordings(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := &serviceStore{
		camera: domain.Camera{
			ID: "camera-one", Host: "10.0.0.152", Port: 9000, Channel: 0, Timezone: "UTC",
			Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
			RecordingSource: domain.RecordingSourceCameraStorage, RecordingMode: domain.RecordingModeEvents,
			HistoricalImport: domain.HistoricalImportLast48Hours, ArchiveFromUtc: timePointer(now.Add(-48 * time.Hour)),
			EventTypes: []domain.EventType{domain.EventPerson},
		},
		credential: domain.EncryptedCredential{Username: "admin", KeyVersion: 1},
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
	if searcher.connection.Password != "secret" || searcher.connection.Timezone != "UTC" {
		t.Fatalf("unexpected search connection: %#v", searcher.connection)
	}
}

func TestReconcileOverlapsExistingWatermark(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	watermark := now.Add(-time.Hour)
	store := &serviceStore{
		camera:     domain.Camera{ID: "camera-one", Host: "10.0.0.152", Port: 9000, Timezone: "UTC", Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain, RecordingSource: domain.RecordingSourceCameraStorage, RecordingMode: domain.RecordingModeEvents, EventTypes: []domain.EventType{domain.EventMotion}},
		credential: domain.EncryptedCredential{Username: "admin", KeyVersion: 1}, watermark: &watermark,
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
			ID: "camera-one", Host: "10.0.0.152", Port: 9000, Timezone: "UTC",
			Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
			RecordingSource: domain.RecordingSourceCameraStorage, RecordingMode: domain.RecordingModeContinuous,
			HistoricalImport: domain.HistoricalImportNewOnly, ArchiveFromUtc: timePointer(now.Add(-time.Minute)),
		},
		credential: domain.EncryptedCredential{Username: "admin", KeyVersion: 1},
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

func timePointer(value time.Time) *time.Time { return &value }
