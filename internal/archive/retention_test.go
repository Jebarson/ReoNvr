package archive

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"reonavr/internal/domain"
)

type retentionStore struct {
	cameras    []domain.Camera
	candidates []domain.Recording
	deleted    []string
}

func (store *retentionStore) ListCameras(context.Context) ([]domain.Camera, error) {
	return store.cameras, nil
}

func (store *retentionStore) ListRetentionCandidates(context.Context, int) ([]domain.Recording, error) {
	if len(store.candidates) == 0 {
		return nil, nil
	}
	return store.candidates[:1], nil
}

func (store *retentionStore) MarkRecordingDeleted(_ context.Context, id string, _ time.Time) error {
	store.deleted = append(store.deleted, id)
	store.candidates = store.candidates[1:]
	return nil
}

func TestRetentionDeletesOldestCompleteRecordingUntilReserveIsRestored(t *testing.T) {
	root := t.TempDir()
	mediaPath := filepath.Join(root, "camera-one", "old.mkv")
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaPath, []byte("video"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaPath+".json", []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	store := &retentionStore{
		cameras: []domain.Camera{{
			ID: "camera-one", Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
			Capabilities: &domain.CameraCapabilities{MainBitrateKbps: 6144},
		}},
		candidates: []domain.Recording{{ID: "oldest", LocalPath: "camera-one/old.mkv", State: domain.RecordingComplete}},
	}
	service := NewRetentionService(store, root)
	service.freeBytes = func(string) (uint64, error) {
		if len(store.deleted) == 0 {
			return 0, nil
		}
		return archiveReserveBytes(store.cameras), nil
	}
	if err := service.EnsureCapacity(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "oldest" {
		t.Fatalf("unexpected tombstones: %#v", store.deleted)
	}
	if _, err := os.Stat(mediaPath); !os.IsNotExist(err) {
		t.Fatalf("media was not deleted: %v", err)
	}
	if _, err := os.Stat(mediaPath + ".json"); !os.IsNotExist(err) {
		t.Fatalf("sidecar was not deleted: %v", err)
	}
}

func TestArchiveReserveUsesSelectedStreamBitrates(t *testing.T) {
	cameras := []domain.Camera{
		{Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain, Capabilities: &domain.CameraCapabilities{MainBitrateKbps: 6144}},
		{Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamSub, Capabilities: &domain.CameraCapabilities{SubBitrateKbps: 256}},
		{Enabled: false, ArchiveEnabled: true, ArchiveStream: domain.StreamMain, Capabilities: &domain.CameraCapabilities{MainBitrateKbps: 9999}},
	}
	expected := uint64(6400 * 1000 * 1800 / 8)
	if actual := archiveReserveBytes(cameras); actual != expected {
		t.Fatalf("unexpected archive reserve: got %d want %d", actual, expected)
	}
}
