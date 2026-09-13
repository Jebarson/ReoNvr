package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"
	"reonavr/internal/security"
)

type DownloadStore interface {
	GetCamera(context.Context, string) (domain.Camera, error)
	GetCredential(context.Context, string) (domain.EncryptedCredential, error)
	ClaimNextDownload(context.Context, string, time.Time) (*domain.Recording, error)
	UpdateRecordingState(context.Context, string, domain.RecordingState, time.Time) error
	CompleteRecording(context.Context, string, string, string, int64, time.Time) error
}

type NativeDownloader interface {
	DownloadRecording(context.Context, reolink.Connection, string, domain.ArchiveStream, string) error
}

type DownloadService struct {
	store       DownloadStore
	downloader  NativeDownloader
	cipher      CredentialCipher
	capacity    CapacityGuard
	settings    ApplicationSettingsProvider
	archiveRoot string
	now         func() time.Time
}

func (service *DownloadService) SetCapacityGuard(capacity CapacityGuard) {
	service.capacity = capacity
}

func (service *DownloadService) SetApplicationSettingsProvider(settings ApplicationSettingsProvider) {
	service.settings = settings
}

func NewDownloadService(store DownloadStore, downloader NativeDownloader, cipher CredentialCipher, archiveRoot string) *DownloadService {
	return &DownloadService{store: store, downloader: downloader, cipher: cipher, archiveRoot: archiveRoot, now: time.Now}
}

func (service *DownloadService) DownloadNext(ctx context.Context, cameraID string) error {
	camera, err := service.store.GetCamera(ctx, cameraID)
	if err != nil {
		return err
	}
	if !camera.Enabled || !camera.ArchiveEnabled || camera.RecordingSource != domain.RecordingSourceCameraStorage {
		return nil
	}
	now := service.now().UTC()
	recording, err := service.store.ClaimNextDownload(ctx, cameraID, now)
	if err != nil || recording == nil {
		return err
	}
	if err := service.download(ctx, camera, recording); err != nil {
		state := domain.RecordingRetryWait
		if recordingExpired(err) {
			state = domain.RecordingExpired
		}
		updateContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if stateErr := service.store.UpdateRecordingState(updateContext, recording.ID, state, service.now().UTC()); stateErr != nil {
			return errors.Join(err, stateErr)
		}
		return err
	}
	return nil
}

func (service *DownloadService) download(ctx context.Context, camera domain.Camera, recording *domain.Recording) error {
	if service.capacity != nil {
		if err := service.capacity.EnsureCapacity(ctx, recording.SizeBytes); err != nil {
			return err
		}
	}
	settings, err := loadApplicationSettings(ctx, service.settings)
	if err != nil {
		return err
	}
	relativePath, err := archiveRelativePath(camera, *recording, settings)
	if err != nil {
		return err
	}
	finalPath := filepath.Join(service.archiveRoot, relativePath)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o750); err != nil {
		return fmt.Errorf("create recording directory: %w", err)
	}
	if _, err := os.Stat(finalPath); errors.Is(err, os.ErrNotExist) {
		credential, credentialErr := service.store.GetCredential(ctx, camera.ID)
		if credentialErr != nil {
			return credentialErr
		}
		if credential.KeyVersion != security.KeyVersion {
			return errors.New("stored credential uses an unsupported key version")
		}
		password, decryptErr := service.cipher.Decrypt(camera.ID, credential.Ciphertext, credential.Nonce)
		if decryptErr != nil {
			return decryptErr
		}
		partialPath := finalPath + ".partial"
		downloadErr := service.downloader.DownloadRecording(ctx, reolink.Connection{
			Host: camera.Host, Port: camera.Port, Channel: camera.Channel,
			Stream: recording.Stream, Timezone: camera.Timezone,
			Username: credential.Username, Password: password,
		}, recording.NativeName, recording.Stream, partialPath)
		password = ""
		if downloadErr != nil {
			return downloadErr
		}
		if err := service.store.UpdateRecordingState(ctx, recording.ID, domain.RecordingVerifying, service.now().UTC()); err != nil {
			return err
		}
		if _, _, err := verifyRecording(partialPath, recording.SizeBytes); err != nil {
			return err
		}
		if err := os.Rename(partialPath, finalPath); err != nil {
			return fmt.Errorf("publish recording: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("inspect recording path: %w", err)
	} else if err := service.store.UpdateRecordingState(ctx, recording.ID, domain.RecordingVerifying, service.now().UTC()); err != nil {
		return err
	}

	sizeBytes, digest, err := verifyRecording(finalPath, recording.SizeBytes)
	if err != nil {
		return err
	}
	sidecar := struct {
		CameraID   string                 `json:"cameraId"`
		NativeName string                 `json:"nativeName"`
		Source     domain.RecordingSource `json:"source"`
		Codec      string                 `json:"codec,omitempty"`
		Stream     domain.ArchiveStream   `json:"stream"`
		StartAtUtc time.Time              `json:"startAtUtc"`
		EndAtUtc   time.Time              `json:"endAtUtc"`
		SizeBytes  int64                  `json:"sizeBytes"`
		EventTypes []domain.EventType     `json:"eventTypes"`
		Sha256     string                 `json:"sha256"`
	}{
		CameraID: camera.ID, NativeName: recording.NativeName, Source: recording.Source,
		Codec: recording.Codec, Stream: recording.Stream, StartAtUtc: recording.StartAtUtc,
		EndAtUtc: recording.EndAtUtc, SizeBytes: sizeBytes,
		EventTypes: recording.EventTypes, Sha256: digest,
	}
	if err := writeSidecar(finalPath+".json", sidecar); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(finalPath)); err != nil {
		return err
	}
	return service.store.CompleteRecording(ctx, recording.ID, filepath.ToSlash(relativePath), digest, sizeBytes, service.now().UTC())
}

func verifyRecording(filePath string, expectedBytes int64) (int64, string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, "", fmt.Errorf("open recording for verification: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, "", fmt.Errorf("inspect recording: %w", err)
	}
	if expectedBytes > 0 && info.Size() < expectedBytes {
		return 0, "", fmt.Errorf("recording is shorter than expected: expected at least %d bytes, received %d", expectedBytes, info.Size())
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return 0, "", fmt.Errorf("hash recording: %w", err)
	}
	if err := file.Sync(); err != nil {
		return 0, "", fmt.Errorf("flush recording: %w", err)
	}
	return info.Size(), hex.EncodeToString(hash.Sum(nil)), nil
}

func writeSidecar(filePath string, value any) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode recording sidecar: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(filePath), ".sidecar-")
	if err != nil {
		return fmt.Errorf("create recording sidecar: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(append(payload, '\n')); err != nil {
		temporary.Close()
		return fmt.Errorf("write recording sidecar: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("flush recording sidecar: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close recording sidecar: %w", err)
	}
	if err := os.Rename(temporaryPath, filePath); err != nil {
		return fmt.Errorf("publish recording sidecar: %w", err)
	}
	return nil
}

func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open recording directory: %w", err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("flush recording directory: %w", err)
	}
	return nil
}

func recordingExpired(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not found") || strings.Contains(message, "404") || strings.Contains(message, "overwritten")
}
