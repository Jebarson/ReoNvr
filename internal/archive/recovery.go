package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"
	"reonavr/internal/security"
)

type RecoveryStore interface {
	GetCamera(context.Context, string) (domain.Camera, error)
	GetCredential(context.Context, string) (domain.EncryptedCredential, error)
	ListRecoveryPeriods(context.Context, string, time.Time) ([]domain.RecoveryPeriod, error)
	SaveRecoveryScan(context.Context, string, time.Time, []domain.RecoveryGap) error
	ListVerifiedCoverage(context.Context, string, domain.ArchiveStream, domain.TimeRange, bool) ([]domain.TimeRange, error)
	ListRecoveryGaps(context.Context, string, int) ([]domain.RecoveryGap, error)
	UpdateRecoveryGap(context.Context, string, string, float64, time.Time) error
	QueueRecoveryRecordings(context.Context, domain.RecoveryGap, []domain.Recording, time.Time) error
	RecoveryRecordingFinal(context.Context, string) (bool, error)
	ListRequiredEventCoverage(context.Context, domain.RecoveryGap) ([]domain.TimeRange, error)
}

type RecoveryService struct {
	store    RecoveryStore
	searcher Searcher
	cipher   CredentialCipher
	now      func() time.Time
}

func NewRecoveryService(store RecoveryStore, searcher Searcher, cipher CredentialCipher) *RecoveryService {
	return &RecoveryService{store: store, searcher: searcher, cipher: cipher, now: time.Now}
}

func (service *RecoveryService) Reconcile(ctx context.Context, cameraID string) error {
	camera, err := service.store.GetCamera(ctx, cameraID)
	if err != nil {
		return err
	}
	if !camera.Enabled || !camera.ArchiveEnabled || !camera.RecoveryEnabled {
		return nil
	}
	now := service.now().UTC()
	before := now.Add(-2 * time.Minute)
	periods, err := service.store.ListRecoveryPeriods(ctx, cameraID, before)
	if err != nil {
		return err
	}
	for _, period := range periods {
		end := before
		if period.Mode == domain.RecordingModeEvents {
			end = now.Add(-10 * time.Minute)
		}
		if period.EndAtUtc != nil && period.EndAtUtc.Before(end) {
			end = *period.EndAtUtc
		}
		if limit := period.ScannedUntilUtc.Add(30 * time.Minute); limit.Before(end) {
			end = limit
		}
		if !period.ScannedUntilUtc.Before(end) {
			continue
		}
		span := domain.TimeRange{StartAtUtc: period.ScannedUntilUtc, EndAtUtc: end}
		coverage, err := service.store.ListVerifiedCoverage(ctx, cameraID, period.Stream, span, false)
		if err != nil {
			return err
		}
		missing := missingCoverage(span, coverage)
		if period.Mode == domain.RecordingModeEvents {
			missing = []domain.TimeRange{span}
		}
		var gaps []domain.RecoveryGap
		for _, interval := range missing {
			if interval.EndAtUtc.Sub(interval.StartAtUtc) <= coverageTolerance {
				continue
			}
			hash := sha256.Sum256([]byte(period.ID + interval.StartAtUtc.Format(time.RFC3339Nano) + interval.EndAtUtc.Format(time.RFC3339Nano)))
			gaps = append(gaps, domain.RecoveryGap{ID: hex.EncodeToString(hash[:16]), PeriodID: period.ID, CameraID: cameraID,
				Stream: period.Stream, Mode: period.Mode, EventTypes: period.EventTypes, TimeRange: interval, Status: "Pending"})
		}
		if err := service.store.SaveRecoveryScan(ctx, period.ID, end, gaps); err != nil {
			return err
		}
	}
	gaps, err := service.store.ListRecoveryGaps(ctx, cameraID, 4)
	if err != nil || len(gaps) == 0 {
		return err
	}
	if camera.Capabilities == nil || !camera.Capabilities.StorageAvailable {
		return errors.New("camera storage is unavailable for recovery")
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
	connection := reolink.Connection{Host: camera.Host, Port: camera.Port, Channel: camera.Channel, Timezone: camera.Timezone, Username: credential.Username, Password: password}
	password = ""
	for _, gap := range gaps {
		if err := service.recoverGap(ctx, camera, connection, gap, now); err != nil {
			return err
		}
	}
	return nil
}

func (service *RecoveryService) recoverGap(ctx context.Context, camera domain.Camera, connection reolink.Connection, gap domain.RecoveryGap, now time.Time) error {
	coverage, err := service.store.ListVerifiedCoverage(ctx, camera.ID, gap.Stream, gap.TimeRange, false)
	if err != nil {
		return err
	}
	missing := missingCoverage(gap.TimeRange, coverage)
	estimatedClock := reolink.CameraClockEstimated(connection.Timezone)
	if !estimatedClock && gap.Mode == domain.RecordingModeContinuous && missingDuration(missing) <= coverageTolerance {
		return service.store.UpdateRecoveryGap(ctx, gap.ID, "Recovered", 0, now)
	}
	items, err := service.searcher.SearchRecordings(ctx, connection, gap.Stream, gap.StartAtUtc.Add(-coverageTolerance), gap.EndAtUtc.Add(coverageTolerance))
	if err != nil {
		_ = service.store.UpdateRecoveryGap(ctx, gap.ID, "SearchFailed", missingDuration(missing).Seconds(), now)
		return err
	}
	selected := make(map[domain.EventType]bool, len(gap.EventTypes))
	for _, eventType := range gap.EventTypes {
		selected[eventType] = true
	}
	var queued []domain.Recording
	var expectedEvents []domain.TimeRange
	if gap.Mode == domain.RecordingModeEvents {
		expectedEvents, err = service.store.ListRequiredEventCoverage(ctx, gap)
		if err != nil {
			return err
		}
	}
	for _, item := range items {
		if item.Stream != gap.Stream || !item.EndAtUtc.After(gap.StartAtUtc) || !item.StartAtUtc.Before(gap.EndAtUtc) || item.EndAtUtc.After(now.Add(-30*time.Second)) {
			continue
		}
		if gap.Mode == domain.RecordingModeEvents && !hasSelectedType(item.EventTypes, selected) {
			continue
		}
		span := domain.TimeRange{StartAtUtc: item.StartAtUtc, EndAtUtc: item.EndAtUtc}
		if span.StartAtUtc.Before(gap.StartAtUtc) {
			span.StartAtUtc = gap.StartAtUtc
		}
		if span.EndAtUtc.After(gap.EndAtUtc) {
			span.EndAtUtc = gap.EndAtUtc
		}
		if gap.Mode == domain.RecordingModeEvents {
			expectedEvents = append(expectedEvents, span)
		}
		if missingDuration(missingCoverage(span, coverage)) <= coverageTolerance {
			continue
		}
		final, err := service.store.RecoveryRecordingFinal(ctx, recordingID(camera.ID, item.NativeName))
		if err != nil {
			return err
		}
		if final {
			continue
		}
		raw := item.RawMetadata
		if len(raw) == 0 {
			raw = []byte("{}")
		}
		if estimatedClock {
			var metadata map[string]json.RawMessage
			if err := json.Unmarshal(raw, &metadata); err != nil {
				return err
			}
			if metadata == nil {
				metadata = make(map[string]json.RawMessage)
			}
			metadata["_reonvrEstimatedClock"] = json.RawMessage("true")
			raw, err = json.Marshal(metadata)
			if err != nil {
				return err
			}
		}
		queued = append(queued, domain.Recording{ID: recordingID(camera.ID, item.NativeName), CameraID: camera.ID, NativeName: item.NativeName,
			Source: domain.RecordingSourceCameraStorage, Stream: item.Stream, StartAtUtc: item.StartAtUtc, EndAtUtc: item.EndAtUtc,
			SizeBytes: item.SizeBytes, EventTypes: item.EventTypes, State: domain.RecordingQueued, RawMetadata: raw, DiscoveredAtUtc: now, UpdatedAtUtc: now})
	}
	if len(queued) > 0 {
		if err := service.store.QueueRecoveryRecordings(ctx, gap, queued, now); err != nil {
			return err
		}
	}
	status := "Unavailable"
	remaining := missingDuration(missing)
	if gap.Mode == domain.RecordingModeEvents {
		remaining = 0
		sort.Slice(expectedEvents, func(first, second int) bool {
			return expectedEvents[first].StartAtUtc.Before(expectedEvents[second].StartAtUtc)
		})
		var requiredUntil time.Time
		for _, span := range expectedEvents {
			if span.StartAtUtc.Before(requiredUntil) {
				span.StartAtUtc = requiredUntil
			}
			if !span.StartAtUtc.Before(span.EndAtUtc) {
				continue
			}
			requiredUntil = span.EndAtUtc
			remaining += missingDuration(missingCoverage(span, coverage))
		}
		status = "RecoveredEvents"
		if remaining > coverageTolerance {
			status = "Unavailable"
		}
		if len(expectedEvents) == 0 {
			captured, err := service.store.ListVerifiedCoverage(ctx, camera.ID, gap.Stream, gap.TimeRange, true)
			if err != nil {
				return err
			}
			remaining = missingDuration(missingCoverage(gap.TimeRange, captured))
			status = "CheckedEvents"
			if remaining > coverageTolerance {
				status = "Unknown"
			}
		}
	}
	if len(queued) > 0 {
		status = "Downloading"
	} else if remaining > coverageTolerance && len(coverage) > 0 {
		status = "Partial"
	}
	if estimatedClock {
		status = "Provisional"
	}
	return service.store.UpdateRecoveryGap(ctx, gap.ID, status, remaining.Seconds(), now)
}

func missingDuration(ranges []domain.TimeRange) time.Duration {
	var duration time.Duration
	for _, span := range ranges {
		if span.EndAtUtc.Sub(span.StartAtUtc) > coverageTolerance {
			duration += span.EndAtUtc.Sub(span.StartAtUtc)
		}
	}
	return duration
}

func missingCoverage(expected domain.TimeRange, coverage []domain.TimeRange) []domain.TimeRange {
	if !expected.StartAtUtc.Before(expected.EndAtUtc) {
		return nil
	}
	ordered := append([]domain.TimeRange(nil), coverage...)
	sort.Slice(ordered, func(first, second int) bool {
		return ordered[first].StartAtUtc.Before(ordered[second].StartAtUtc)
	})
	cursor := expected.StartAtUtc
	var missing []domain.TimeRange
	for _, covered := range ordered {
		if !covered.StartAtUtc.Before(covered.EndAtUtc) || !covered.EndAtUtc.After(cursor) {
			continue
		}
		if !covered.StartAtUtc.Before(expected.EndAtUtc) {
			break
		}
		if covered.StartAtUtc.After(cursor) {
			missing = append(missing, domain.TimeRange{StartAtUtc: cursor, EndAtUtc: covered.StartAtUtc})
		}
		cursor = covered.EndAtUtc
		if !cursor.Before(expected.EndAtUtc) {
			return missing
		}
	}
	if cursor.Before(expected.EndAtUtc) {
		missing = append(missing, domain.TimeRange{StartAtUtc: cursor, EndAtUtc: expected.EndAtUtc})
	}
	return missing
}
