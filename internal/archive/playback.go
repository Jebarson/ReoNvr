package archive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"reonavr/internal/domain"
	"reonavr/internal/repository"
)

var ErrRecordingUnavailable = errors.New("recording is unavailable")

type PlaybackStore interface {
	GetRecording(context.Context, string) (domain.Recording, error)
}

type PlaybackService struct {
	store       PlaybackStore
	archiveRoot string
}

func NewPlaybackService(store PlaybackStore, archiveRoot string) *PlaybackService {
	return &PlaybackService{store: store, archiveRoot: archiveRoot}
}

func (service *PlaybackService) GetRecording(ctx context.Context, id string) (domain.Recording, error) {
	recording, err := service.store.GetRecording(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.Recording{}, ErrRecordingUnavailable
	}
	return recording, err
}

func (service *PlaybackService) OpenRecording(ctx context.Context, id string) (domain.Recording, *os.File, error) {
	recording, err := service.GetRecording(ctx, id)
	if err != nil {
		return domain.Recording{}, nil, err
	}
	if recording.State != domain.RecordingComplete || strings.TrimSpace(recording.LocalPath) == "" {
		return domain.Recording{}, nil, ErrRecordingUnavailable
	}
	absolutePath := filepath.Join(service.archiveRoot, filepath.FromSlash(recording.LocalPath))
	if !pathWithin(service.archiveRoot, absolutePath) {
		return domain.Recording{}, nil, ErrRecordingUnavailable
	}
	resolvedRoot, err := filepath.EvalSymlinks(service.archiveRoot)
	if err != nil {
		return domain.Recording{}, nil, fmt.Errorf("resolve archive root: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(absolutePath)
	if errors.Is(err, os.ErrNotExist) {
		return domain.Recording{}, nil, ErrRecordingUnavailable
	}
	if err != nil {
		return domain.Recording{}, nil, fmt.Errorf("resolve recording path: %w", err)
	}
	if !pathWithin(resolvedRoot, resolvedPath) {
		return domain.Recording{}, nil, ErrRecordingUnavailable
	}
	file, err := os.Open(resolvedPath)
	if errors.Is(err, os.ErrNotExist) {
		return domain.Recording{}, nil, ErrRecordingUnavailable
	}
	if err != nil {
		return domain.Recording{}, nil, fmt.Errorf("open recording: %w", err)
	}
	return recording, file, nil
}
