package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"reonavr/internal/domain"
)

var ErrInvalidApplicationSettings = errors.New("invalid application settings")

type ApplicationSettingsStore interface {
	GetApplicationSettings(context.Context) (domain.ApplicationSettings, error)
	SetApplicationSettings(context.Context, domain.ApplicationSettings) error
}

type ApplicationSettingsService struct {
	store       ApplicationSettingsStore
	archiveRoot string
	now         func() time.Time
}

func NewApplicationSettingsService(store ApplicationSettingsStore, archiveRoot string) *ApplicationSettingsService {
	return &ApplicationSettingsService{store: store, archiveRoot: filepath.Clean(archiveRoot), now: time.Now}
}

func (service *ApplicationSettingsService) Get(ctx context.Context) (domain.ApplicationSettings, error) {
	settings, err := service.store.GetApplicationSettings(ctx)
	if err != nil {
		return domain.ApplicationSettings{}, err
	}
	settings.ArchiveRoot = service.archiveRoot
	return settings, nil
}

func (service *ApplicationSettingsService) Update(ctx context.Context, recordingFolder string, archiveLayout domain.ArchiveLayout) (domain.ApplicationSettings, error) {
	folder, err := normalizeRecordingFolder(recordingFolder)
	if err != nil {
		return domain.ApplicationSettings{}, err
	}
	if !validArchiveLayout(archiveLayout) {
		return domain.ApplicationSettings{}, fmt.Errorf("%w: unknown archive layout", ErrInvalidApplicationSettings)
	}
	if err := service.verifyWritableFolder(folder); err != nil {
		return domain.ApplicationSettings{}, err
	}
	settings := domain.ApplicationSettings{
		ArchiveRoot: service.archiveRoot, RecordingFolder: folder,
		ArchiveLayout: archiveLayout, UpdatedAtUtc: service.now().UTC(),
	}
	if err := service.store.SetApplicationSettings(ctx, settings); err != nil {
		return domain.ApplicationSettings{}, err
	}
	return settings, nil
}

func normalizeRecordingFolder(value string) (string, error) {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	if normalized == "" {
		normalized = "."
	}
	cleaned := path.Clean(normalized)
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: recording folder must stay inside the archive root", ErrInvalidApplicationSettings)
	}
	return cleaned, nil
}

func validArchiveLayout(layout domain.ArchiveLayout) bool {
	switch layout {
	case domain.ArchiveLayoutCameraDate, domain.ArchiveLayoutDateCamera, domain.ArchiveLayoutCamera, domain.ArchiveLayoutDate:
		return true
	default:
		return false
	}
}

func (service *ApplicationSettingsService) verifyWritableFolder(folder string) error {
	root, err := filepath.Abs(service.archiveRoot)
	if err != nil {
		return fmt.Errorf("resolve archive root: %w", err)
	}
	directory := filepath.Join(root, filepath.FromSlash(folder))
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("%w: create recording folder: %v", ErrInvalidApplicationSettings, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("%w: resolve archive root: %v", ErrInvalidApplicationSettings, err)
	}
	resolvedDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("%w: resolve recording folder: %v", ErrInvalidApplicationSettings, err)
	}
	relative, err := filepath.Rel(resolvedRoot, resolvedDirectory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: recording folder escaped the archive root", ErrInvalidApplicationSettings)
	}
	probe, err := os.CreateTemp(resolvedDirectory, ".reonvr-write-test-")
	if err != nil {
		return fmt.Errorf("%w: recording folder is not writable: %v", ErrInvalidApplicationSettings, err)
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		os.Remove(probePath)
		return fmt.Errorf("%w: close recording folder write test: %v", ErrInvalidApplicationSettings, err)
	}
	if err := os.Remove(probePath); err != nil {
		return fmt.Errorf("%w: remove recording folder write test: %v", ErrInvalidApplicationSettings, err)
	}
	return nil
}
