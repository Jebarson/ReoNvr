package archive

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"

	"reonavr/internal/testconfig"
)

type downloadStore struct {
	camera     domain.Camera
	credential domain.EncryptedCredential
	recording  *domain.Recording
	states     []domain.RecordingState
	completed  domain.Recording
}

func (store *downloadStore) GetCamera(context.Context, string) (domain.Camera, error) {
	return store.camera, nil
}

func (store *downloadStore) GetCredential(context.Context, string) (domain.EncryptedCredential, error) {
	return store.credential, nil
}

func (store *downloadStore) ClaimNextDownload(context.Context, string, time.Time) (*domain.Recording, error) {
	recording := store.recording
	store.recording = nil
	return recording, nil
}

func (store *downloadStore) UpdateRecordingState(_ context.Context, _ string, state domain.RecordingState, _ time.Time) error {
	store.states = append(store.states, state)
	return nil
}

func (store *downloadStore) CompleteRecording(_ context.Context, id, localPath, digest string, size int64, _ time.Time) error {
	store.completed = domain.Recording{ID: id, LocalPath: localPath, Sha256: digest, SizeBytes: size, State: domain.RecordingComplete}
	return nil
}

type fileDownloader struct {
	payload  []byte
	password string
	called   bool
}

type mediaInspectorFunc func(context.Context, string) (VideoInspection, error)

func (inspect mediaInspectorFunc) InspectVideo(ctx context.Context, path string) (VideoInspection, error) {
	return inspect(ctx, path)
}

func TestDownloadInspectionDistinguishesInvalidMediaFromTransientFailure(t *testing.T) {
	for _, invalid := range []bool{true, false} {
		t.Run(map[bool]string{true: "invalid", false: "transient"}[invalid], func(t *testing.T) {
			now := time.Now().UTC()
			store := &downloadStore{
				camera:     domain.Camera{ID: "camera-one", Name: "Front", Timezone: "UTC", Enabled: true, ArchiveEnabled: true, RecordingSource: domain.RecordingSourceCameraStorage},
				credential: domain.EncryptedCredential{KeyVersion: 1},
				recording: &domain.Recording{ID: "clip", NativeName: "clip.mp4", Source: domain.RecordingSourceCameraStorage, Stream: domain.StreamMain,
					StartAtUtc: now.Add(-time.Minute), EndAtUtc: now, SizeBytes: 4},
			}
			service := NewDownloadService(store, &fileDownloader{payload: []byte("video")}, serviceCipher{}, t.TempDir())
			service.SetMediaInspector(mediaInspectorFunc(func(context.Context, string) (VideoInspection, error) {
				if invalid {
					return VideoInspection{}, fmt.Errorf("%w: no decodable frames", ErrInvalidMedia)
				}
				return VideoInspection{}, errors.New("probe temporarily unavailable")
			}))
			if err := service.DownloadNext(context.Background(), store.camera.ID); err == nil {
				t.Fatal("inspection error was ignored")
			}
			want := domain.RecordingRetryWait
			if invalid {
				want = domain.RecordingFailed
			}
			if store.states[len(store.states)-1] != want || store.completed.State == domain.RecordingComplete {
				t.Fatal("inspection failure was misclassified or invalid media was published")
			}
		})
	}
	for _, metadata := range []struct {
		payload string
		invalid bool
	}{
		{payload: `{"packets":[],"error":{"code":-1094995529}}`, invalid: true},
		{payload: `{"packets":[]}`, invalid: true},
		{payload: `{"error":{"code":-5}}`},
		{payload: `{"packets":`},
	} {
		_, err := inspectVideoPackets(strings.NewReader(metadata.payload))
		if err == nil || errors.Is(err, ErrInvalidMedia) != metadata.invalid {
			t.Fatal("probe metadata classification did not distinguish invalid media from incomplete or I/O results")
		}
	}
}

type rejectingCapacityGuard struct{}

func (rejectingCapacityGuard) EnsureCapacity(context.Context, int64) error {
	return ErrInsufficientArchiveCapacity
}

func (downloader *fileDownloader) DownloadRecording(_ context.Context, connection reolink.Connection, _ string, _ domain.ArchiveStream, outputPath string) error {
	downloader.called = true
	downloader.password = connection.Password
	return os.WriteFile(outputPath, downloader.payload, 0o640)
}

func TestDownloadNextPublishesVerifiedNativeRecording(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	payload := []byte("native-video")
	store := &downloadStore{
		camera:     domain.Camera{ID: "camera-one", Name: "Front", Host: testconfig.Host(2), Port: 9000, Timezone: "UTC", Enabled: true, ArchiveEnabled: true, RecordingSource: domain.RecordingSourceCameraStorage},
		credential: domain.EncryptedCredential{Username: testconfig.Username(), KeyVersion: 1},
		recording: &domain.Recording{
			ID: "recording-one", CameraID: "camera-one", NativeName: "Mp4Record/2026/clip.mp4",
			Source: domain.RecordingSourceCameraStorage, Stream: domain.StreamMain,
			StartAtUtc: now.Add(-time.Minute), EndAtUtc: now, SizeBytes: int64(len(payload)),
			EventTypes: []domain.EventType{domain.EventPerson}, State: domain.RecordingDownloading,
		},
	}
	downloader := &fileDownloader{payload: payload}
	root := t.TempDir()
	service := NewDownloadService(store, downloader, serviceCipher{}, root)
	service.now = func() time.Time { return now }
	if err := service.DownloadNext(context.Background(), store.camera.ID); err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(root, "Front-cameraon", "2026", "09", "11", "clip.mp4")
	if downloaded, err := os.ReadFile(finalPath); err != nil || string(downloaded) != string(payload) {
		t.Fatalf("unexpected archived media: %q %v", downloaded, err)
	}
	sidecar, err := os.ReadFile(finalPath + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sidecar), testconfig.Password()) || !strings.Contains(string(sidecar), `"source": "CameraStorage"`) {
		t.Fatalf("unexpected sidecar: %s", sidecar)
	}
	expectedHash := fmt.Sprintf("%x", sha256.Sum256(payload))
	if downloader.password != testconfig.Password() || store.completed.Sha256 != expectedHash || store.completed.LocalPath != "Front-cameraon/2026/09/11/clip.mp4" {
		t.Fatalf("unexpected completion: %#v", store.completed)
	}
	if len(store.states) != 1 || store.states[0] != domain.RecordingVerifying {
		t.Fatalf("unexpected states: %#v", store.states)
	}
}

func TestVerifyRecordingAcceptsGrowthButRejectsTruncation(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "clip.mp4")
	payload := []byte("camera-file-grew")
	if err := os.WriteFile(filePath, payload, 0o640); err != nil {
		t.Fatal(err)
	}
	if size, _, err := verifyRecording(filePath, int64(len("camera-file"))); err != nil || size != int64(len(payload)) {
		t.Fatalf("larger complete recording was rejected: size=%d err=%v", size, err)
	}
	if _, _, err := verifyRecording(filePath, int64(len(payload)+1)); err == nil {
		t.Fatal("truncated recording was accepted")
	}
}

func TestDownloadNextRejectsUnsafeNativeName(t *testing.T) {
	store := &downloadStore{
		camera:    domain.Camera{ID: "camera-one", Enabled: true, ArchiveEnabled: true, RecordingSource: domain.RecordingSourceCameraStorage},
		recording: &domain.Recording{ID: "recording-one", NativeName: "../../outside.mp4", Source: domain.RecordingSourceCameraStorage},
	}
	downloader := &fileDownloader{}
	service := NewDownloadService(store, downloader, serviceCipher{}, t.TempDir())
	err := service.DownloadNext(context.Background(), store.camera.ID)
	if err == nil || downloader.called {
		t.Fatalf("unsafe name was not rejected before download: %v", err)
	}
	if len(store.states) != 1 || store.states[0] != domain.RecordingRetryWait {
		t.Fatalf("unsafe download did not enter retry state: %#v", store.states)
	}
}

func TestDownloadNextDoesNotTransferWithoutCapacityReserve(t *testing.T) {
	store := &downloadStore{
		camera: domain.Camera{ID: "camera-one", Enabled: true, ArchiveEnabled: true, RecordingSource: domain.RecordingSourceCameraStorage},
		recording: &domain.Recording{
			ID: "recording-one", NativeName: "clip.mp4", Source: domain.RecordingSourceCameraStorage,
			SizeBytes: 1024,
		},
	}
	downloader := &fileDownloader{}
	service := NewDownloadService(store, downloader, serviceCipher{}, t.TempDir())
	service.SetCapacityGuard(rejectingCapacityGuard{})
	if err := service.DownloadNext(context.Background(), store.camera.ID); !errors.Is(err, ErrInsufficientArchiveCapacity) {
		t.Fatalf("unexpected capacity result: %v", err)
	}
	if downloader.called || len(store.states) != 1 || store.states[0] != domain.RecordingRetryWait {
		t.Fatalf("capacity failure started transfer or omitted retry state: called=%v states=%#v", downloader.called, store.states)
	}
}
