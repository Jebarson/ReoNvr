package archive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"reonavr/internal/domain"
)

const (
	archiveReserveDuration = 30 * time.Minute
	defaultMainBitrateKbps = 8192
	defaultSubBitrateKbps  = 1024
)

var ErrInsufficientArchiveCapacity = errors.New("archive capacity reserve cannot be restored")

type RetentionStore interface {
	ListCameras(context.Context) ([]domain.Camera, error)
	ListRetentionCandidates(context.Context, int) ([]domain.Recording, error)
	MarkRecordingDeleted(context.Context, string, time.Time) error
}

type CapacityGuard interface {
	EnsureCapacity(context.Context, int64) error
}

type RetentionService struct {
	store       RetentionStore
	archiveRoot string
	freeBytes   func(string) (uint64, error)
	now         func() time.Time
	mu          sync.Mutex
}

func NewRetentionService(store RetentionStore, archiveRoot string) *RetentionService {
	return &RetentionService{store: store, archiveRoot: archiveRoot, freeBytes: archiveFreeBytes, now: time.Now}
}

func (service *RetentionService) WithArchiveMutation(run func() error) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	return run()
}

func (service *RetentionService) EnsureCapacity(ctx context.Context, requiredBytes int64) error {
	if requiredBytes < 0 {
		return errors.New("required archive capacity cannot be negative")
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if err := os.MkdirAll(service.archiveRoot, 0o750); err != nil {
		return fmt.Errorf("create archive root: %w", err)
	}
	cameras, err := service.store.ListCameras(ctx)
	if err != nil {
		return err
	}
	reserve := archiveReserveBytes(cameras)
	target := reserve + uint64(requiredBytes)
	for {
		available, err := service.freeBytes(service.archiveRoot)
		if err != nil {
			return fmt.Errorf("measure archive capacity: %w", err)
		}
		if available >= target {
			return nil
		}
		candidates, err := service.store.ListRetentionCandidates(ctx, 1)
		if err != nil {
			return err
		}
		if len(candidates) == 0 {
			return ErrInsufficientArchiveCapacity
		}
		candidate := candidates[0]
		mediaPath := filepath.Join(service.archiveRoot, filepath.FromSlash(candidate.LocalPath))
		if candidate.LocalPath == "" || !pathWithin(service.archiveRoot, mediaPath) {
			return errors.New("retention candidate escaped the configured archive root")
		}
		for _, artifact := range []string{mediaPath, mediaPath + ".json", mediaPath + ".thumb.jpg"} {
			if err := os.Remove(artifact); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("delete recording artifact: %w", err)
			}
		}
		if err := service.store.MarkRecordingDeleted(ctx, candidate.ID, service.now().UTC()); err != nil {
			return err
		}
	}
}

func archiveReserveBytes(cameras []domain.Camera) uint64 {
	var bitrateKbps uint64
	for _, camera := range cameras {
		if !camera.Enabled || !camera.ArchiveEnabled {
			continue
		}
		bitrate := 0
		if camera.Capabilities != nil {
			if camera.ArchiveStream == domain.StreamSub {
				bitrate = camera.Capabilities.SubBitrateKbps
			} else {
				bitrate = camera.Capabilities.MainBitrateKbps
			}
		}
		if bitrate <= 0 && camera.ArchiveStream == domain.StreamSub {
			bitrate = defaultSubBitrateKbps
		} else if bitrate <= 0 {
			bitrate = defaultMainBitrateKbps
		}
		bitrateKbps += uint64(bitrate)
	}
	return bitrateKbps * 1000 * uint64(archiveReserveDuration/time.Second) / 8
}
