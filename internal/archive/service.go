package archive

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"
	"reonavr/internal/security"
)

const (
	initialSearchWindow      = 48 * time.Hour
	emptyBackfillWindow      = 30 * 24 * time.Hour
	maxBackfillWindowsPerRun = 4
	searchOverlap            = 5 * time.Minute
	earliestArchiveYear      = 2000
)

type Store interface {
	GetCamera(context.Context, string) (domain.Camera, error)
	GetCredential(context.Context, string) (domain.EncryptedCredential, error)
	GetSearchWatermark(context.Context, string) (*time.Time, error)
	GetBackfillCursor(context.Context, string) (*time.Time, int, error)
	SaveReconciliation(context.Context, domain.IngestionRun, []domain.Recording, time.Time) error
	ListArchivedRecordings(context.Context, string, int) ([]domain.Recording, error)
}

type Searcher interface {
	SearchRecordings(context.Context, reolink.Connection, domain.ArchiveStream, time.Time, time.Time) ([]reolink.Recording, error)
}

type CredentialCipher interface {
	Decrypt(cameraID string, ciphertext, nonce []byte) (string, error)
}

type Service struct {
	store    Store
	searcher Searcher
	cipher   CredentialCipher
	now      func() time.Time
}

func NewService(store Store, searcher Searcher, cipher CredentialCipher) *Service {
	return &Service{store: store, searcher: searcher, cipher: cipher, now: time.Now}
}

func (service *Service) Reconcile(ctx context.Context, cameraID string) error {
	camera, err := service.store.GetCamera(ctx, cameraID)
	if err != nil {
		return err
	}
	if !camera.Enabled || !camera.ArchiveEnabled || camera.RecordingSource != domain.RecordingSourceCameraStorage {
		return nil
	}
	if camera.RecordingMode == domain.RecordingModeEvents && len(camera.EventTypes) == 0 {
		return nil
	}
	credential, err := service.store.GetCredential(ctx, cameraID)
	if err != nil {
		return err
	}
	if credential.KeyVersion != security.KeyVersion {
		return errors.New("stored credential uses an unsupported key version")
	}
	password, err := service.cipher.Decrypt(cameraID, credential.Ciphertext, credential.Nonce)
	if err != nil {
		return err
	}
	now := service.now().UTC()
	from := now.Add(-initialSearchWindow)
	if camera.ArchiveFromUtc != nil {
		from = camera.ArchiveFromUtc.UTC()
	}
	kind := "Startup"
	watermark, err := service.store.GetSearchWatermark(ctx, cameraID)
	if err != nil {
		return err
	}
	if watermark != nil {
		from = watermark.Add(-searchOverlap)
		if camera.ArchiveFromUtc != nil && from.Before(*camera.ArchiveFromUtc) {
			from = camera.ArchiveFromUtc.UTC()
		}
		kind = "Hourly"
	}
	if !from.Before(now) {
		from = now.Add(-time.Second)
	}
	connection := reolink.Connection{
		Host: camera.Host, Port: camera.Port, Channel: camera.Channel, Stream: camera.ArchiveStream, Timezone: camera.Timezone,
		Username: credential.Username, Password: password,
	}
	password = ""
	selected := make(map[domain.EventType]bool, len(camera.EventTypes))
	for _, eventType := range camera.EventTypes {
		selected[eventType] = true
	}
	if _, err := service.reconcileWindow(ctx, camera, connection, selected, from, now, kind, now); err != nil {
		return err
	}
	if camera.HistoricalImport != domain.HistoricalImportAll {
		return nil
	}
	cursor, previousCount, err := service.store.GetBackfillCursor(ctx, cameraID)
	if err != nil {
		return err
	}
	backfillTo := now.Add(-initialSearchWindow)
	if cursor != nil && cursor.Before(backfillTo) {
		backfillTo = cursor.UTC()
	}
	earliest := time.Date(earliestArchiveYear, 1, 1, 0, 0, 0, 0, time.UTC)
	for range maxBackfillWindowsPerRun {
		if !backfillTo.After(earliest) {
			break
		}
		window := initialSearchWindow
		if cursor != nil && previousCount == 0 {
			window = emptyBackfillWindow
		}
		backfillFrom := backfillTo.Add(-window)
		if backfillFrom.Before(earliest) {
			backfillFrom = earliest
		}
		count, err := service.reconcileWindow(ctx, camera, connection, selected, backfillFrom, backfillTo, "Backfill", now)
		if err != nil && window != initialSearchWindow {
			backfillFrom = backfillTo.Add(-initialSearchWindow)
			if backfillFrom.Before(earliest) {
				backfillFrom = earliest
			}
			count, err = service.reconcileWindow(ctx, camera, connection, selected, backfillFrom, backfillTo, "Backfill", now)
		}
		if err != nil {
			return err
		}
		backfillTo = backfillFrom
		previousCount = count
		cursor = &backfillFrom
	}
	return nil
}

func (service *Service) reconcileWindow(ctx context.Context, camera domain.Camera, connection reolink.Connection, selected map[domain.EventType]bool, from, to time.Time, kind string, now time.Time) (int, error) {
	items, err := service.searcher.SearchRecordings(ctx, connection, camera.ArchiveStream, from, to)
	if err != nil {
		return 0, err
	}
	recordings := make([]domain.Recording, 0, len(items))
	for _, item := range items {
		if camera.RecordingMode == domain.RecordingModeEvents && !hasSelectedType(item.EventTypes, selected) {
			continue
		}
		rawMetadata := item.RawMetadata
		if len(rawMetadata) == 0 {
			rawMetadata = []byte("{}")
		}
		state := domain.RecordingDiscovered
		if camera.ArchiveFromUtc == nil || !item.EndAtUtc.Before(*camera.ArchiveFromUtc) {
			state = domain.RecordingQueued
		}
		recordings = append(recordings, domain.Recording{
			ID: recordingID(camera.ID, item.NativeName), CameraID: camera.ID,
			NativeName: item.NativeName, Source: domain.RecordingSourceCameraStorage,
			Stream: item.Stream, StartAtUtc: item.StartAtUtc,
			EndAtUtc: item.EndAtUtc, SizeBytes: item.SizeBytes,
			EventTypes: append([]domain.EventType(nil), item.EventTypes...),
			State:      state, RawMetadata: rawMetadata,
			DiscoveredAtUtc: now, UpdatedAtUtc: now,
		})
	}
	completed := now
	runID, err := randomID()
	if err != nil {
		return 0, err
	}
	run := domain.IngestionRun{
		ID: runID, CameraID: camera.ID, Kind: kind, WindowStartUtc: from,
		WindowEndUtc: to, Status: "Complete", DiscoveredCount: len(recordings),
		StartedAtUtc: now, CompletedAtUtc: &completed,
	}
	if err := service.store.SaveReconciliation(ctx, run, recordings, now); err != nil {
		return 0, err
	}
	return len(recordings), nil
}

func (service *Service) ListRecordings(ctx context.Context, cameraID string, limit int) ([]domain.Recording, error) {
	if _, err := service.store.GetCamera(ctx, cameraID); err != nil {
		return nil, err
	}
	return service.store.ListArchivedRecordings(ctx, cameraID, limit)
}

func (service *Service) ListRecoveryGaps(ctx context.Context, cameraID string, limit int) ([]domain.RecoveryGap, error) {
	if _, err := service.store.GetCamera(ctx, cameraID); err != nil {
		return nil, err
	}
	reader, ok := service.store.(interface {
		ListRecoveryHistory(context.Context, string, int) ([]domain.RecoveryGap, error)
	})
	if !ok {
		return nil, errors.New("recovery history is unavailable")
	}
	return reader.ListRecoveryHistory(ctx, cameraID, limit)
}

func hasSelectedType(types []domain.EventType, selected map[domain.EventType]bool) bool {
	for _, eventType := range types {
		if selected[eventType] {
			return true
		}
	}
	return false
}

func recordingID(cameraID, nativeName string) string {
	digest := sha256.Sum256([]byte(cameraID + "\x00" + nativeName))
	return fmt.Sprintf("%x", digest[:16])
}

func randomID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate ingestion run ID: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
