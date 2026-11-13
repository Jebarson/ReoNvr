package archive

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"reonavr/internal/domain"
)

type playbackStore struct {
	recording domain.Recording
}

func (store playbackStore) GetRecording(context.Context, string) (domain.Recording, error) {
	return store.recording, nil
}

func TestPlaybackOpensOnlyCompletedArchiveMedia(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "camera-one", "clip.mkv")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("native-media"), 0o640); err != nil {
		t.Fatal(err)
	}
	service := NewPlaybackService(playbackStore{recording: domain.Recording{
		ID: "recording-one", State: domain.RecordingComplete, LocalPath: "camera-one/clip.mkv",
	}}, root)
	_, file, err := service.OpenRecording(context.Background(), "recording-one")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	payload, err := io.ReadAll(file)
	if err != nil || string(payload) != "native-media" {
		t.Fatalf("unexpected media: %q %v", payload, err)
	}
}

func TestPlaybackRejectsPathOutsideArchive(t *testing.T) {
	root := t.TempDir()
	service := NewPlaybackService(playbackStore{recording: domain.Recording{
		ID: "recording-one", State: domain.RecordingComplete, LocalPath: "../outside.mkv",
	}}, root)
	if _, _, err := service.OpenRecording(context.Background(), "recording-one"); !errors.Is(err, ErrRecordingUnavailable) {
		t.Fatalf("unexpected unsafe path result: %v", err)
	}
}
