package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"reonavr/internal/domain"
)

type applicationSettingsStore struct {
	settings domain.ApplicationSettings
	err      error
}

func (store *applicationSettingsStore) GetApplicationSettings(context.Context) (domain.ApplicationSettings, error) {
	return store.settings, store.err
}

func (store *applicationSettingsStore) SetApplicationSettings(_ context.Context, settings domain.ApplicationSettings) error {
	if store.err != nil {
		return store.err
	}
	store.settings = settings
	return nil
}

func TestApplicationSettingsValidateAndPersistWritableFolder(t *testing.T) {
	root := t.TempDir()
	store := &applicationSettingsStore{}
	service := NewApplicationSettingsService(store, root)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	settings, err := service.Update(context.Background(), `daily\recordings`, domain.ArchiveLayoutDateCamera)
	if err != nil {
		t.Fatal(err)
	}
	if settings.ArchiveRoot != root || settings.RecordingFolder != "daily/recordings" || settings.ArchiveLayout != domain.ArchiveLayoutDateCamera || !settings.UpdatedAtUtc.Equal(now) {
		t.Fatalf("unexpected settings: %#v", settings)
	}
	if info, err := os.Stat(filepath.Join(root, "daily", "recordings")); err != nil || !info.IsDir() {
		t.Fatalf("recording folder was not created: %v", err)
	}
}

func TestApplicationSettingsRejectUnsafeFolderAndUnknownLayout(t *testing.T) {
	service := NewApplicationSettingsService(&applicationSettingsStore{}, t.TempDir())
	if _, err := service.Update(context.Background(), "../outside", domain.ArchiveLayoutCameraDate); !errors.Is(err, ErrInvalidApplicationSettings) {
		t.Fatalf("expected unsafe folder rejection, got %v", err)
	}
	if _, err := service.Update(context.Background(), ".", domain.ArchiveLayout("Unknown")); !errors.Is(err, ErrInvalidApplicationSettings) {
		t.Fatalf("expected unknown layout rejection, got %v", err)
	}
}
