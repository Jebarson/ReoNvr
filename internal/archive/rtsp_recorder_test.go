package archive

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"
)

type rtspRecordingStore struct {
	camera     domain.Camera
	credential domain.EncryptedCredential
	events     []domain.Event
	recording  domain.Recording
}

func (store *rtspRecordingStore) GetCamera(context.Context, string) (domain.Camera, error) {
	return store.camera, nil
}

func (store *rtspRecordingStore) GetCredential(context.Context, string) (domain.EncryptedCredential, error) {
	return store.credential, nil
}

func (store *rtspRecordingStore) ListEvents(context.Context, string, int) ([]domain.Event, error) {
	return store.events, nil
}

func (store *rtspRecordingStore) SaveRtspRecording(_ context.Context, recording domain.Recording) error {
	store.recording = recording
	return nil
}

type rtspURLProvider struct {
	password string
}

func (provider *rtspURLProvider) RtspURL(_ context.Context, connection reolink.Connection, _ domain.ArchiveStream) (string, error) {
	provider.password = connection.Password
	return "rtsp://10.0.0.152:554/Preview_01_main", nil
}

type rtspSegmenter struct {
	password string
	payload  []byte
	cancel   context.CancelFunc
	segments []RtspSegment
	duration time.Duration
}

func (segmenter *rtspSegmenter) Capture(ctx context.Context, streamURL, _, password, outputPattern string, duration time.Duration, onSegment func(RtspSegment) error) error {
	segmenter.password = password
	segmenter.duration = duration
	if strings.Contains(streamURL, password) {
		return errors.New("provider returned an authenticated URL")
	}
	segments := segmenter.segments
	if len(segments) == 0 {
		segments = []RtspSegment{{StartOffset: 0, EndOffset: time.Minute}}
	}
	for index, segment := range segments {
		segment.Path = strings.Replace(outputPattern, "%06d", fmt.Sprintf("%06d", index), 1)
		if err := os.WriteFile(segment.Path, segmenter.payload, 0o640); err != nil {
			return err
		}
		if err := onSegment(segment); err != nil {
			return err
		}
	}
	segmenter.cancel()
	<-ctx.Done()
	return ctx.Err()
}

func (segmenter *rtspSegmenter) Join(_ context.Context, inputPaths []string, outputPath string) error {
	var joined []byte
	for _, inputPath := range inputPaths {
		payload, err := os.ReadFile(inputPath)
		if err != nil {
			return err
		}
		joined = append(joined, payload...)
	}
	return os.WriteFile(outputPath, joined, 0o640)
}

func (segmenter *rtspSegmenter) ProbeCodec(context.Context, string) (string, error) {
	return "H.265", nil
}

func TestRtspRecorderPublishesContinuousSegment(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := &rtspRecordingStore{
		camera: domain.Camera{
			ID: "camera-one", Host: "10.0.0.152", Port: 9000, Enabled: true, ArchiveEnabled: true,
			ArchiveStream: domain.StreamMain, RecordingSource: domain.RecordingSourceRtsp,
			RecordingMode: domain.RecordingModeContinuous, EventTypes: []domain.EventType{domain.EventPerson},
		},
		credential: domain.EncryptedCredential{Username: "admin", KeyVersion: 1},
		events: []domain.Event{{
			ID: "event-one", CameraID: "camera-one", EventType: domain.EventPerson,
			StartedAtUtc: now.Add(10 * time.Second), CreatedAtUtc: now.Add(10 * time.Second),
		}},
	}
	provider := &rtspURLProvider{}
	archiveRoot := t.TempDir()
	bufferRoot := filepath.Join(archiveRoot, ".rtsp-buffer")
	ctx, cancel := context.WithCancel(context.Background())
	segmenter := &rtspSegmenter{payload: []byte("native-rtsp-video"), cancel: cancel}
	service := NewRtspRecorderService(store, provider, serviceCipher{}, segmenter, archiveRoot, bufferRoot)
	service.now = func() time.Time { return now }
	if err := service.Record(ctx, store.camera.ID); err != nil {
		t.Fatal(err)
	}
	if provider.password != "secret" || segmenter.password != "secret" {
		t.Fatal("decrypted password did not reach the secure RTSP adapters")
	}
	if segmenter.duration != time.Minute {
		t.Fatalf("unexpected continuous segment duration: %s", segmenter.duration)
	}
	if store.recording.Source != domain.RecordingSourceRtsp || store.recording.Codec != "H.265" || store.recording.State != domain.RecordingComplete {
		t.Fatalf("unexpected RTSP recording: %#v", store.recording)
	}
	if len(store.recording.EventTypes) != 1 || store.recording.EventTypes[0] != domain.EventPerson {
		t.Fatalf("continuous recording omitted overlapping event markers: %#v", store.recording.EventTypes)
	}
	mediaPath := filepath.Join(archiveRoot, filepath.FromSlash(store.recording.LocalPath))
	if payload, err := os.ReadFile(mediaPath); err != nil || string(payload) != string(segmenter.payload) {
		t.Fatalf("unexpected archived RTSP media: %q %v", payload, err)
	}
	sidecar, err := os.ReadFile(mediaPath + ".json")
	if err != nil {
		t.Fatal(err)
	}
	expectedHash := fmt.Sprintf("%x", sha256.Sum256(segmenter.payload))
	if store.recording.Sha256 != expectedHash || strings.Contains(string(sidecar), "secret") || !strings.Contains(string(sidecar), `"source": "Rtsp"`) {
		t.Fatalf("unexpected RTSP publication: %#v %s", store.recording, sidecar)
	}
}

func TestRtspRecorderFinalizesCoalescedEventWindow(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	ended := now.Add(5 * time.Second)
	store := &rtspRecordingStore{
		camera: domain.Camera{
			ID: "camera-one", Host: "10.0.0.152", Port: 9000, Enabled: true, ArchiveEnabled: true,
			ArchiveStream: domain.StreamMain, RecordingSource: domain.RecordingSourceRtsp,
			RecordingMode: domain.RecordingModeEvents, PreEventSeconds: 2, PostEventSeconds: 2,
			EventTypes: []domain.EventType{domain.EventPerson, domain.EventVehicle},
		},
		credential: domain.EncryptedCredential{Username: "admin", KeyVersion: 1},
		events: []domain.Event{
			{ID: "person", CameraID: "camera-one", EventType: domain.EventPerson, StartedAtUtc: now.Add(3 * time.Second), EndedAtUtc: &ended, CreatedAtUtc: now},
			{ID: "vehicle", CameraID: "camera-one", EventType: domain.EventVehicle, StartedAtUtc: now.Add(4 * time.Second), EndedAtUtc: &ended, CreatedAtUtc: now},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	segmenter := &rtspSegmenter{
		payload: []byte("segment"), cancel: cancel,
		segments: []RtspSegment{
			{StartOffset: 0, EndOffset: 2 * time.Second},
			{StartOffset: 2 * time.Second, EndOffset: 4 * time.Second},
			{StartOffset: 4 * time.Second, EndOffset: 6 * time.Second},
			{StartOffset: 6 * time.Second, EndOffset: 8 * time.Second},
		},
	}
	archiveRoot := t.TempDir()
	service := NewRtspRecorderService(store, &rtspURLProvider{}, serviceCipher{}, segmenter, archiveRoot, filepath.Join(archiveRoot, ".rtsp-buffer"))
	service.now = func() time.Time { return now }
	if err := service.Record(ctx, store.camera.ID); err != nil {
		t.Fatal(err)
	}
	if store.recording.State != domain.RecordingComplete || !slices.Equal(store.recording.EventTypes, store.camera.EventTypes) {
		t.Fatalf("unexpected event recording: %#v", store.recording)
	}
	if store.recording.StartAtUtc.After(now.Add(time.Second)) || store.recording.EndAtUtc.Before(now.Add(7*time.Second)) {
		t.Fatalf("event recording omitted pre/post media: %#v", store.recording)
	}
}

func TestFFmpegCaptureSpecKeepsCredentialsOutOfArguments(t *testing.T) {
	arguments, input, authenticatedURL, err := ffmpegCaptureSpec(
		"rtsp://10.0.0.152:554/Preview_01_main", "camera-user", "s e/c:r@t", time.Minute, "/buffer/segment-%06d.partial.mkv",
	)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(arguments, " ")
	if strings.Contains(joined, "camera-user") || strings.Contains(joined, "s e/c:r@t") || strings.Contains(joined, authenticatedURL) {
		t.Fatalf("credentials leaked into FFmpeg arguments: %s", joined)
	}
	if !strings.Contains(input, authenticatedURL) || !strings.Contains(input, "option rtsp_transport tcp") {
		t.Fatalf("authenticated RTSP input was not sent through stdin: %q", input)
	}
	message := safeFFmpegError("failed to open "+authenticatedURL+" for camera-user with s e/c:r@t", authenticatedURL, "camera-user", "s e/c:r@t")
	if strings.Contains(message, "camera-user") || strings.Contains(message, "s e/c:r@t") || strings.Contains(message, authenticatedURL) {
		t.Fatalf("FFmpeg error leaked credentials: %q", message)
	}
}

func TestFFconcatDocumentUsesExplicitFileProtocol(t *testing.T) {
	inputPath := filepath.Join(t.TempDir(), "segment-000001.partial.mkv")
	document, err := ffconcatDocument([]string{inputPath})
	if err != nil {
		t.Fatal(err)
	}
	expected := "file 'file:" + filepath.ToSlash(inputPath) + "'"
	if !strings.Contains(document, expected) {
		t.Fatalf("concat document does not qualify the path for pipe input: %q", document)
	}
}

func TestRtspRecoveryRemovesOnlyBufferedPartialFragments(t *testing.T) {
	archiveRoot := t.TempDir()
	bufferRoot := filepath.Join(archiveRoot, ".rtsp-buffer")
	cameraBuffer := filepath.Join(bufferRoot, "camera-one")
	if err := os.MkdirAll(cameraBuffer, 0o750); err != nil {
		t.Fatal(err)
	}
	partialPath := filepath.Join(cameraBuffer, "segment-run-000001.partial.mkv")
	keepPath := filepath.Join(cameraBuffer, "diagnostic.txt")
	for _, path := range []string{partialPath, keepPath} {
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	service := NewRtspRecorderService(nil, nil, nil, nil, archiveRoot, bufferRoot)
	if err := service.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(partialPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial fragment was not removed: %v", err)
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Fatalf("unrelated buffer file was removed: %v", err)
	}
}

func TestParseRtspSegmentRejectsEscapedPath(t *testing.T) {
	_, err := parseRtspSegment(filepath.Join(t.TempDir(), "segment-%06d.partial.mkv"), `../outside.mkv,0.000000,60.000000`)
	if err == nil {
		t.Fatal("expected escaped segment path to be rejected")
	}
}
