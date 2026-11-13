package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/repository"
)

type rtspPublication struct {
	Version     int                `json:"version"`
	Recording   domain.Recording   `json:"recording"`
	RawMetadata json.RawMessage    `json:"rawMetadata"`
	Coverage    []domain.TimeRange `json:"coverage"`
	BufferName  string             `json:"bufferName"`
}

func (service *RtspRecorderService) publishRtspRecording(ctx context.Context, segment RtspSegment, recording domain.Recording) error {
	service.publicationMu.Lock()
	defer service.publicationMu.Unlock()
	root, err := os.OpenRoot(service.archiveRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	source, err := filepath.Rel(service.archiveRoot, segment.Path)
	if err != nil {
		return err
	}
	if err := syncRtspPath(root, source); err != nil {
		return err
	}
	journal := filepath.Join(filepath.Dir(source), "publication-"+recording.ID+".json")
	pending := rtspPublication{Version: 1, Recording: recording, RawMetadata: recording.RawMetadata, Coverage: segment.Coverage, BufferName: filepath.Base(source)}
	if err := writeRtspJSON(root, journal, pending); err != nil {
		return err
	}
	return service.finishRtspPublication(ctx, root, journal, pending)
}

func (service *RtspRecorderService) replayPendingRtspPublication(ctx context.Context, sourcePath string, expected domain.Recording) (bool, error) {
	service.publicationMu.Lock()
	defer service.publicationMu.Unlock()
	root, err := os.OpenRoot(service.archiveRoot)
	if err != nil {
		return false, err
	}
	defer root.Close()
	source, err := filepath.Rel(service.archiveRoot, sourcePath)
	if err != nil {
		return false, err
	}
	journal := filepath.Join(filepath.Dir(source), "publication-"+expected.ID+".json")
	if _, err := root.Lstat(journal); errors.Is(err, os.ErrNotExist) {
		existing, err := service.store.GetRecording(ctx, expected.ID)
		if errors.Is(err, repository.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if existing.CameraID != expected.CameraID || existing.Source != domain.RecordingSourceRtsp || existing.Stream != expected.Stream ||
			!existing.StartAtUtc.Equal(expected.StartAtUtc) || !existing.EndAtUtc.Equal(expected.EndAtUtc) {
			return true, errors.New("RTSP event publication conflicts with an indexed recording")
		}
		if existing.State == domain.RecordingDeleted || existing.State == domain.RecordingExpired {
			return true, nil
		}
		if existing.State != domain.RecordingComplete {
			return true, errors.New("RTSP event publication is not complete")
		}
		return true, verifyRtspPublicationFile(root, existing.LocalPath, existing)
	} else if err != nil {
		return false, err
	}
	return true, service.recoverRtspPublicationLocked(ctx, root, journal)
}

func (service *RtspRecorderService) recoverCameraPublications(ctx context.Context, cameraID string) error {
	service.publicationMu.Lock()
	defer service.publicationMu.Unlock()
	root, err := os.OpenRoot(service.archiveRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	buffer, err := filepath.Rel(service.archiveRoot, filepath.Join(service.bufferRoot, cameraID))
	if err != nil {
		return err
	}
	directory, err := root.Open(buffer)
	if errors.Is(err, os.ErrNotExist) {
		service.resolvePublicationIssue(ctx, cameraID)
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := directory.ReadDir(-1)
	directory.Close()
	if err != nil {
		return err
	}
	failed := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "publication-") && strings.HasSuffix(entry.Name(), ".json") {
			journal := filepath.Join(buffer, entry.Name())
			if err := service.recoverRtspPublicationLocked(ctx, root, journal); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				service.reportPublicationFailure(ctx, journal)
				failed = true
			}
		}
	}
	if !failed {
		service.resolvePublicationIssue(ctx, cameraID)
	}
	return nil
}

func (service *RtspRecorderService) resolvePublicationIssue(ctx context.Context, cameraID string) {
	if ctx.Err() != nil {
		return
	}
	if resolver, ok := service.store.(interface {
		ResolveIssue(context.Context, string, string, string, time.Time) error
	}); ok {
		if resolver.ResolveIssue(ctx, "RtspPublication", cameraID, "PublicationReplayFailed", service.now().UTC()) != nil {
			slog.Error("Could not resolve RTSP publication issue", "cameraId", cameraID)
		}
	}
}

func (service *RtspRecorderService) reportPublicationFailure(ctx context.Context, journal string) {
	digest := sha256.Sum256([]byte(journal))
	cameraID := filepath.Base(filepath.Dir(journal))
	if !safePathComponent(cameraID) {
		cameraID = ""
	}
	now := service.now().UTC()
	issue := domain.OperationalIssue{
		ID: "rtsp-publication-" + hex.EncodeToString(digest[:16]), Severity: "Error", Source: "RtspPublication",
		CameraID: cameraID, ErrorCode: "PublicationReplayFailed",
		Message:        "An RTSP publication could not be recovered. Its journal and buffered media are retained for retry; other recording and playback can continue.",
		FirstSeenAtUtc: now, LastSeenAtUtc: now,
	}
	slog.Error(issue.Message, "issueId", issue.ID, "cameraId", cameraID)
	if reporter, ok := service.store.(interface {
		ReportIssue(context.Context, domain.OperationalIssue) error
	}); ok {
		if reporter.ReportIssue(ctx, issue) != nil {
			slog.Error("Could not persist RTSP publication issue", "issueId", issue.ID)
		}
	}
}

func (service *RtspRecorderService) recoverRtspPublication(ctx context.Context, root *os.Root, journal string) error {
	service.publicationMu.Lock()
	defer service.publicationMu.Unlock()
	return service.recoverRtspPublicationLocked(ctx, root, journal)
}

func (service *RtspRecorderService) recoverRtspPublicationLocked(ctx context.Context, root *os.Root, journal string) error {
	file, err := root.Open(journal)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	var pending rtspPublication
	decoder := json.NewDecoder(io.LimitReader(file, 1024*1024))
	if err := decoder.Decode(&pending); err != nil {
		return fmt.Errorf("read RTSP publication journal: %w", err)
	}
	return service.finishRtspPublication(ctx, root, journal, pending)
}

func (service *RtspRecorderService) finishRtspPublication(ctx context.Context, root *os.Root, journal string, pending rtspPublication) error {
	recording := pending.Recording
	recording.RawMetadata = pending.RawMetadata
	buffer, err := filepath.Rel(service.archiveRoot, filepath.Join(service.bufferRoot, recording.CameraID))
	if err != nil {
		return err
	}
	digest, digestErr := hex.DecodeString(recording.Sha256)
	if pending.Version != 1 || !safePathComponent(recording.CameraID) || !safePathComponent(recording.ID) ||
		filepath.Dir(journal) != buffer || filepath.Base(journal) != "publication-"+recording.ID+".json" ||
		pending.BufferName != filepath.Base(pending.BufferName) || strings.ContainsAny(pending.BufferName, "/\\") || !strings.HasSuffix(pending.BufferName, ".partial.mkv") ||
		recording.Source != domain.RecordingSourceRtsp || recording.State != domain.RecordingComplete ||
		!recording.StartAtUtc.Before(recording.EndAtUtc) || recording.SizeBytes <= 0 || digestErr != nil || len(digest) != sha256.Size ||
		filepath.IsAbs(recording.LocalPath) || !pathWithin(service.archiveRoot, filepath.Join(service.archiveRoot, recording.LocalPath)) ||
		pathWithin(service.bufferRoot, filepath.Join(service.archiveRoot, recording.LocalPath)) {
		return errors.New("invalid RTSP publication journal")
	}
	existing, err := service.store.GetRecording(ctx, recording.ID)
	indexed := err == nil
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	if indexed {
		if existing.CameraID != recording.CameraID || existing.Source != recording.Source ||
			existing.Sha256 != recording.Sha256 || existing.SizeBytes != recording.SizeBytes ||
			!existing.StartAtUtc.Equal(recording.StartAtUtc) || !existing.EndAtUtc.Equal(recording.EndAtUtc) ||
			(existing.LocalPath != "" && existing.LocalPath != recording.LocalPath) {
			return errors.New("RTSP publication conflicts with an indexed recording")
		}
		if existing.State == domain.RecordingDeleted || existing.State == domain.RecordingExpired {
			if writer, ok := service.store.(coverageWriter); ok && len(pending.Coverage) > 0 {
				if err := writer.SaveRecordingCoverage(ctx, recording.ID, pending.Coverage); err != nil {
					return err
				}
			}
			return removeRtspJournal(root, journal)
		}
		if existing.State != domain.RecordingComplete || existing.LocalPath != recording.LocalPath {
			return errors.New("RTSP publication conflicts with an indexed recording")
		}
	}
	source := filepath.Join(buffer, pending.BufferName)
	if _, err := root.Stat(recording.LocalPath); errors.Is(err, os.ErrNotExist) {
		if err := verifyRtspPublicationFile(root, source, recording); err != nil {
			return err
		}
		if err := root.MkdirAll(filepath.Dir(recording.LocalPath), 0o750); err != nil {
			return err
		}
		if err := root.Rename(source, recording.LocalPath); err != nil {
			return fmt.Errorf("publish RTSP segment: %w", err)
		}
		if err := syncRtspPath(root, filepath.Dir(recording.LocalPath)); err != nil {
			return err
		}
		if err := syncRtspPath(root, buffer); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := verifyRtspPublicationFile(root, recording.LocalPath, recording); err != nil {
		return err
	}
	if err := writeRtspJSON(root, recording.LocalPath+".json", recording); err != nil {
		return err
	}
	if !indexed {
		if err := service.store.SaveRtspRecording(ctx, recording); err != nil {
			return err
		}
	}
	if writer, ok := service.store.(coverageWriter); ok && len(pending.Coverage) > 0 {
		if err := writer.SaveRecordingCoverage(ctx, recording.ID, pending.Coverage); err != nil {
			return err
		}
	}
	return removeRtspJournal(root, journal)
}

func verifyRtspPublicationFile(root *os.Root, path string, recording domain.Recording) error {
	info, err := root.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != recording.SizeBytes {
		return errors.New("RTSP publication media has invalid size or type")
	}
	file, err := root.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != recording.Sha256 {
		return errors.New("RTSP publication media checksum mismatch")
	}
	return file.Sync()
}

func writeRtspJSON(root *os.Root, path string, value any) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	id, err := randomIdentifier()
	if err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(path), ".publication-"+id)
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	if _, err := file.Write(append(payload, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := root.Rename(temporary, path); err != nil {
		return err
	}
	return syncRtspPath(root, filepath.Dir(path))
}

func syncRtspPath(root *os.Root, path string) error {
	file, err := root.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func removeRtspJournal(root *os.Root, journal string) error {
	if err := root.Remove(journal); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRtspPath(root, filepath.Dir(journal))
}
