package archive

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"
	"reonavr/internal/repository"
	"reonavr/internal/security"

	"reonavr/internal/testconfig"
)

func TestMediaInspectionWithRealFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "discontinuous.mkv")
	command := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=64x64:rate=5",
		"-frames:v", "15", "-vf", "setpts=PTS+5/TB*gte(N\\,5)", "-vsync", "0", "-c:v", "libx264", "-g", "5", "-an", path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate media: %v %s", err, output)
	}
	inspection, err := (&FFmpegSegmenter{}).InspectVideo(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspection.Coverage) != 2 || inspection.Duration < 7*time.Second {
		t.Fatalf("real packet discontinuity not detected: %#v", inspection)
	}
	t.Logf("Detected %d verified ranges in %s of native media", len(inspection.Coverage), inspection.Duration)
	invalidPath := filepath.Join(t.TempDir(), "invalid.mp4")
	if err := os.WriteFile(invalidPath, []byte("not a video"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := (&FFmpegSegmenter{}).InspectVideo(ctx, invalidPath); !errors.Is(err, ErrInvalidMedia) {
		t.Fatalf("corrupt native media was not classified as invalid: %v", err)
	}
	if _, err := (&FFmpegSegmenter{}).InspectVideo(ctx, invalidPath+"-missing"); err == nil || errors.Is(err, ErrInvalidMedia) {
		t.Fatalf("missing-file probe failure was incorrectly terminal: %v", err)
	}
	if _, err := (&FFmpegSegmenter{ProbePath: filepath.Join(t.TempDir(), "missing-probe")}).InspectVideo(ctx, path); err == nil || errors.Is(err, ErrInvalidMedia) {
		t.Fatalf("unavailable probe was incorrectly terminal: %v", err)
	}
}

func TestClockAlignedSegmentsUseRelativeMediaTime(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pattern := filepath.Join(t.TempDir(), "segment-%06d.mkv")
	arguments, _, _, err := ffmpegCaptureSpec(testconfig.Expand("rtsp://{{host2}}:554/Preview_01_sub"), testconfig.Username(), testconfig.Password(), time.Minute, pattern)
	if err != nil {
		t.Fatal(err)
	}
	segmentIndex := slices.Index(arguments, "segment") - 1
	fixtureArguments := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=64x64:rate=5",
		"-frames:v", "15", "-vf", "setpts=PTS+1700000000/TB", "-r", "5", "-c:v", "libx264", "-g", "5", "-an", "-copyts"}
	command := exec.CommandContext(ctx, "ffmpeg", append(fixtureArguments, arguments[segmentIndex:]...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate clock-aligned fixture: %v %s", err, output)
	}
	output, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=start_time,duration", "-of", "json", fmt.Sprintf(pattern, 0)).Output()
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Format struct {
			StartTime float64 `json:"start_time,string"`
			Duration  float64 `json:"duration,string"`
		} `json:"format"`
	}
	if err := json.Unmarshal(output, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Format.StartTime < 0 || probe.Format.StartTime > 1 || probe.Format.Duration < 2 || probe.Format.Duration > 4 {
		t.Fatalf("native segment must use relative presentation time: %s", output)
	}
}

func TestRtspLiveMediaIntegration(t *testing.T) {
	source := os.Getenv("REONVR_INTEGRATION_DATABASE")
	if source == "" {
		t.Skip("explicit isolated camera integration database not configured")
	}
	databasePath := filepath.Join(t.TempDir(), "camera.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		input, err := os.Open(source + suffix)
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		output, err := os.Create(databasePath + suffix)
		if err != nil {
			input.Close()
			t.Fatal(err)
		}
		_, copyErr := io.Copy(output, input)
		input.Close()
		output.Close()
		if copyErr != nil {
			t.Fatal(copyErr)
		}
	}
	ctx := context.Background()
	store, err := repository.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	control, err := store.GetRecordingControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if control.Enabled {
		t.Fatal("integration snapshot must have global recording paused")
	}
	cameras, err := store.ListCameras(ctx)
	if err != nil || len(cameras) == 0 {
		t.Fatalf("no integration camera: %v", err)
	}
	camera := cameras[0]
	camera.Enabled, camera.ArchiveEnabled, camera.RecoveryEnabled = true, true, false
	camera.RecordingSource, camera.RecordingMode = domain.RecordingSourceRtsp, domain.RecordingModeContinuous
	if err := store.UpdateCamera(ctx, camera, nil, camera.EventTypes); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.LoadOrCreateCredentialCipher(os.Getenv("REONVR_INTEGRATION_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	client := reolink.NewClient(&reolink.ExecRunner{Path: "/usr/local/bin/reolink-cli", GatewayAddr: "gateway:9000"}, 15*time.Second)
	root := t.TempDir()
	service := NewRtspRecorderService(store, client, cipher, &FFmpegSegmenter{}, root, filepath.Join(root, ".rtsp-buffer"))
	started := time.Now().UTC()
	captureContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := service.Record(captureContext, camera.ID); err != nil {
		t.Fatal(err)
	}
	recordings, err := store.ListArchivedRecordings(ctx, camera.ID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, recording := range recordings {
		if recording.Source != domain.RecordingSourceRtsp || recording.StartAtUtc.Before(started.Add(-2*time.Second)) {
			continue
		}
		coverage, err := store.ListVerifiedCoverage(ctx, camera.ID, camera.ArchiveStream, domain.TimeRange{StartAtUtc: started.Add(-2 * time.Second), EndAtUtc: time.Now().UTC()}, false)
		if err != nil || len(coverage) == 0 {
			t.Fatalf("live media has no verified coverage: %v", err)
		}
		if recording.EndAtUtc.After(time.Now().Add(2*time.Second)) || recording.EndAtUtc.Sub(recording.StartAtUtc) > 20*time.Second {
			t.Fatalf("invalid wall-clock alignment: %s to %s", recording.StartAtUtc, recording.EndAtUtc)
		}
		t.Logf("Published native %s stream copy with %s duration and verified UTC coverage", recording.Codec, recording.EndAtUtc.Sub(recording.StartAtUtc))
		return
	}
	t.Fatal("bounded live capture did not publish native media")
}

type rtspRecordingStore struct {
	camera      domain.Camera
	credential  domain.EncryptedCredential
	events      []domain.Event
	recording   domain.Recording
	recordings  []domain.Recording
	lookupErr   error
	saveErr     error
	coverageErr error
	coverage    []domain.TimeRange
	issues      []domain.OperationalIssue
	resolutions map[string]int
}

func (store *rtspRecordingStore) ResolveIssue(_ context.Context, source, cameraID, code string, _ time.Time) error {
	if source != "RtspPublication" || code != "PublicationReplayFailed" {
		return errors.New("unexpected publication issue scope")
	}
	if store.resolutions == nil {
		store.resolutions = make(map[string]int)
	}
	store.resolutions[cameraID]++
	return nil
}

func (store *rtspRecordingStore) ReportIssue(_ context.Context, issue domain.OperationalIssue) error {
	store.issues = append(store.issues, issue)
	return nil
}

func TestVideoInspectionRejectsDiscontinuitiesAndWaitsForKeyframe(t *testing.T) {
	payload := `{"packets":[
		{"pts_time":"100.2","duration_time":"0.2","flags":"__"},
		{"pts_time":"100.0","duration_time":"0.2","flags":"K_"},
		{"pts_time":"105.0","duration_time":"0.2","flags":"__"},
		{"pts_time":"106.0","duration_time":"0.2","flags":"K_"},
		{"pts_time":"106.2","duration_time":"0.2","flags":"__"}]}`
	inspection, err := inspectVideoPackets(strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(inspection.Coverage) != 2 || inspection.Coverage[0].EndAtUtc.Sub(inspection.Coverage[0].StartAtUtc) != 400*time.Millisecond || inspection.Coverage[1].StartAtUtc.UnixNano() != int64(6*time.Second) {
		t.Fatalf("packet gap was claimed as covered: %#v", inspection)
	}
	if _, err := inspectVideoPackets(strings.NewReader(`{"packets":[{"pts_time":"NaN"}]}`)); err == nil {
		t.Fatal("invalid timestamp was accepted")
	}
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

func (store *rtspRecordingStore) GetRecording(_ context.Context, id string) (domain.Recording, error) {
	if store.lookupErr != nil {
		return domain.Recording{}, store.lookupErr
	}
	for _, recording := range store.recordings {
		if recording.ID == id {
			return recording, nil
		}
	}
	return domain.Recording{}, repository.ErrNotFound
}

func (store *rtspRecordingStore) SaveRtspRecording(_ context.Context, recording domain.Recording) error {
	if store.saveErr != nil {
		return store.saveErr
	}
	store.recording = recording
	store.recordings = append(store.recordings, recording)
	return nil
}

func (store *rtspRecordingStore) SaveCaptureCoverage(context.Context, string, domain.ArchiveStream, []domain.TimeRange) error {
	return nil
}

func (store *rtspRecordingStore) SaveRecordingCoverage(_ context.Context, _ string, coverage []domain.TimeRange) error {
	if store.coverageErr != nil {
		return store.coverageErr
	}
	store.coverage = append([]domain.TimeRange(nil), coverage...)
	return nil
}

type rtspURLProvider struct {
	password string
}

func (provider *rtspURLProvider) RtspURL(_ context.Context, connection reolink.Connection, _ domain.ArchiveStream) (string, error) {
	provider.password = connection.Password
	return testconfig.Expand("rtsp://{{host2}}:554/Preview_01_main"), nil
}

type rtspSegmenter struct {
	password     string
	payload      []byte
	cancel       context.CancelFunc
	segments     []RtspSegment
	duration     time.Duration
	captureErr   error
	afterSegment func(int)
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
		if segmenter.afterSegment != nil {
			segmenter.afterSegment(index)
		}
	}
	if segmenter.captureErr != nil {
		return segmenter.captureErr
	}
	segmenter.cancel()
	<-ctx.Done()
	return ctx.Err()
}

func (segmenter *rtspSegmenter) Join(ctx context.Context, inputPaths []string, outputPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
			ID: "camera-one", Host: testconfig.Host(2), Port: 9000, Enabled: true, ArchiveEnabled: true,
			ArchiveStream: domain.StreamMain, RecordingSource: domain.RecordingSourceRtsp,
			RecordingMode: domain.RecordingModeContinuous, EventTypes: []domain.EventType{domain.EventPerson},
		},
		credential: domain.EncryptedCredential{Username: testconfig.Username(), KeyVersion: 1},
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
	if provider.password != testconfig.Password() || segmenter.password != testconfig.Password() {
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
	if store.recording.Sha256 != expectedHash || strings.Contains(string(sidecar), testconfig.Password()) || !strings.Contains(string(sidecar), `"source": "Rtsp"`) {
		t.Fatalf("unexpected RTSP publication: %#v %s", store.recording, sidecar)
	}
}

func TestRtspRecorderFinalizesCoalescedEventWindow(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	ended := now.Add(5 * time.Second)
	store := &rtspRecordingStore{
		camera: domain.Camera{
			ID: "camera-one", Host: testconfig.Host(2), Port: 9000, Enabled: true, ArchiveEnabled: true,
			ArchiveStream: domain.StreamMain, RecordingSource: domain.RecordingSourceRtsp,
			RecordingMode: domain.RecordingModeEvents, PreEventSeconds: 2, PostEventSeconds: 2,
			EventTypes: []domain.EventType{domain.EventPerson, domain.EventVehicle},
		},
		credential: domain.EncryptedCredential{Username: testconfig.Username(), KeyVersion: 1},
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

func TestRtspEventCoverageExcludesUnjoinedAdjacentFragment(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	ended := now.Add(5 * time.Second)
	store := &rtspRecordingStore{
		camera: domain.Camera{ID: "camera-one", Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
			RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeEvents, EventTypes: []domain.EventType{domain.EventPerson}},
		credential: domain.EncryptedCredential{KeyVersion: 1},
		events:     []domain.Event{{EventType: domain.EventPerson, StartedAtUtc: ended, EndedAtUtc: &ended}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	segmenter := &rtspSegmenter{payload: []byte("native-fragment"), cancel: cancel, segments: []RtspSegment{
		{EndOffset: 4 * time.Second, Coverage: []domain.TimeRange{{StartAtUtc: now, EndAtUtc: now.Add(4 * time.Second)}}},
		{StartOffset: 4 * time.Second, EndOffset: 6 * time.Second, Coverage: []domain.TimeRange{{StartAtUtc: now.Add(4 * time.Second), EndAtUtc: now.Add(6 * time.Second)}}},
	}}
	root := t.TempDir()
	service := NewRtspRecorderService(store, &rtspURLProvider{}, serviceCipher{}, segmenter, root, filepath.Join(root, ".rtsp-buffer"))
	service.now = func() time.Time { return now }
	if err := service.Record(ctx, store.camera.ID); err != nil {
		t.Fatal(err)
	}
	if len(store.coverage) != 1 || !store.coverage[0].StartAtUtc.Equal(now.Add(4*time.Second)) || !store.coverage[0].EndAtUtc.Equal(now.Add(6*time.Second)) {
		t.Fatalf("coverage includes unjoined footage: %#v", store.coverage)
	}
	payload, err := os.ReadFile(filepath.Join(root, store.recording.LocalPath))
	if err != nil || len(payload) != len(segmenter.payload) {
		t.Fatalf("unexpected joined fragments: %v", err)
	}
}

func TestRtspRecorderFlushesInterruptedEvents(t *testing.T) {
	for _, scenario := range []string{"open event pause", "post-roll pause", "stream failure"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
			store := &rtspRecordingStore{
				camera: domain.Camera{ID: "camera-one", Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
					RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeEvents,
					PreEventSeconds: 2, PostEventSeconds: 10, EventTypes: []domain.EventType{domain.EventPerson}},
				credential: domain.EncryptedCredential{KeyVersion: 1},
				events:     []domain.Event{{EventType: domain.EventPerson, StartedAtUtc: now.Add(time.Second)}},
			}
			if scenario == "post-roll pause" {
				ended := now.Add(3 * time.Second)
				store.events[0].EndedAtUtc = &ended
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			segmenter := &rtspSegmenter{payload: []byte("native-fragment"), cancel: cancel,
				segments: []RtspSegment{{EndOffset: 2 * time.Second}, {StartOffset: 2 * time.Second, EndOffset: 4 * time.Second}}}
			if scenario == "stream failure" {
				segmenter.captureErr = errors.New("stream disconnected")
			}
			root := t.TempDir()
			service := NewRtspRecorderService(store, &rtspURLProvider{}, serviceCipher{}, segmenter, root, filepath.Join(root, ".rtsp-buffer"))
			service.now = func() time.Time { return now }
			err := service.Record(ctx, store.camera.ID)
			if scenario == "stream failure" {
				if !errors.Is(err, segmenter.captureErr) {
					t.Fatalf("capture error lost: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(store.recordings) != 1 || !store.recording.StartAtUtc.Equal(now) || !store.recording.EndAtUtc.Equal(now.Add(4*time.Second)) {
				t.Fatalf("interrupted event footage lost: %#v", store.recordings)
			}
			payload, err := os.ReadFile(filepath.Join(root, store.recording.LocalPath))
			if err != nil || len(payload) != 2*len(segmenter.payload) {
				t.Fatalf("native fragments not retained: %v", err)
			}
		})
	}
}

func TestRtspRecorderCheckpointsOpenEvents(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := &rtspRecordingStore{
		camera: domain.Camera{ID: "camera-one", Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
			RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeEvents, EventTypes: []domain.EventType{domain.EventPerson}},
		credential: domain.EncryptedCredential{KeyVersion: 1},
		events:     []domain.Event{{EventType: domain.EventPerson, StartedAtUtc: now}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	segmenter := &rtspSegmenter{payload: []byte("native-fragment"), cancel: cancel}
	for index := range 35 {
		segmenter.segments = append(segmenter.segments, RtspSegment{StartOffset: time.Duration(index*2) * time.Second, EndOffset: time.Duration((index+1)*2) * time.Second})
	}
	segmenter.afterSegment = func(index int) {
		if index == 29 && len(store.recordings) != 1 {
			t.Error("open event was not checkpointed before capture stopped")
		}
	}
	root := t.TempDir()
	service := NewRtspRecorderService(store, &rtspURLProvider{}, serviceCipher{}, segmenter, root, filepath.Join(root, ".rtsp-buffer"))
	service.now = func() time.Time { return now }
	if err := service.Record(ctx, store.camera.ID); err != nil {
		t.Fatal(err)
	}
	if len(store.recordings) != 2 || !store.recording.EndAtUtc.Equal(now.Add(70*time.Second)) {
		t.Fatalf("checkpoint or final tail missing: %#v", store.recordings)
	}
}

func TestFFmpegCaptureSpecKeepsCredentialsOutOfArguments(t *testing.T) {
	arguments, input, authenticatedURL, err := ffmpegCaptureSpec(
		testconfig.Expand("rtsp://{{host2}}:554/Preview_01_main"), testconfig.Username(), testconfig.SpecialPassword(), time.Minute, "/buffer/segment-%06d.partial.mkv",
	)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(arguments, " ")
	if !strings.Contains(joined, "-segment_time 60.000 -segment_atclocktime 1") {
		t.Fatal("wall-clock timestamps must use clock-aligned segmentation")
	}
	if strings.Contains(joined, testconfig.Username()) || strings.Contains(joined, testconfig.SpecialPassword()) || strings.Contains(joined, authenticatedURL) {
		t.Fatalf("credentials leaked into FFmpeg arguments: %s", joined)
	}
	if !strings.Contains(input, authenticatedURL) || !strings.Contains(input, "option rtsp_transport tcp") {
		t.Fatalf("authenticated RTSP input was not sent through stdin: %q", input)
	}
	message := safeFFmpegError("failed to open "+authenticatedURL+testconfig.Expand(" for {{username}} with {{specialPassword}}"), authenticatedURL, testconfig.Username(), testconfig.SpecialPassword())
	if strings.Contains(message, testconfig.Username()) || strings.Contains(message, testconfig.SpecialPassword()) || strings.Contains(message, authenticatedURL) {
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

type changingJoinSegmenter struct {
	rtspSegmenter
	joins int
}

func (segmenter *changingJoinSegmenter) Join(ctx context.Context, paths []string, output string) error {
	segmenter.joins++
	if err := segmenter.rtspSegmenter.Join(ctx, paths, output); err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintf(file, "join-%d", segmenter.joins)
	return errors.Join(writeErr, file.Close())
}

func TestRtspEventPublicationRetryReplaysWithoutRemux(t *testing.T) {
	for _, stage := range []string{"index", "coverage"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			bufferRoot := filepath.Join(root, ".rtsp-buffer")
			camera := domain.Camera{ID: "camera-one", Name: "Front", Timezone: "UTC", ArchiveStream: domain.StreamMain}
			directory := filepath.Join(bufferRoot, camera.ID)
			if err := os.MkdirAll(directory, 0o750); err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
			buffer := &rtspEventBuffer{recording: true, windowStart: start, windowEnd: start.Add(8 * time.Second)}
			for index := range 2 {
				path := filepath.Join(directory, fmt.Sprintf("run-%06d.partial.mkv", index))
				if err := os.WriteFile(path, []byte("native-fragment"), 0o640); err != nil {
					t.Fatal(err)
				}
				from, to := start.Add(time.Duration(index)*4*time.Second), start.Add(time.Duration(index+1)*4*time.Second)
				buffer.segments = append(buffer.segments, bufferedRtspSegment{startAt: from, endAt: to,
					segment: RtspSegment{Path: path, Coverage: []domain.TimeRange{{StartAtUtc: from, EndAtUtc: to}}}})
			}
			store := &rtspRecordingStore{}
			if stage == "index" {
				store.saveErr = errors.New("index unavailable")
			} else {
				store.coverageErr = errors.New("coverage unavailable")
			}
			segmenter := &changingJoinSegmenter{}
			service := NewRtspRecorderService(store, nil, nil, segmenter, root, bufferRoot)
			pattern := filepath.Join(directory, "run-%06d.partial.mkv")
			if err := service.publishEventBuffer(context.Background(), camera, start, buffer, pattern); err == nil {
				t.Fatal("fault injection did not interrupt publication")
			}
			journals, _ := filepath.Glob(filepath.Join(directory, "publication-*.json"))
			if len(journals) != 1 {
				t.Fatal("failed publication did not retain its journal")
			}
			original, err := os.ReadFile(journals[0])
			if err != nil {
				t.Fatal(err)
			}
			if err := service.publishEventBuffer(context.Background(), camera, start, buffer, pattern); err == nil {
				t.Fatal("retry unexpectedly bypassed the persistent fault")
			}
			unchanged, err := os.ReadFile(journals[0])
			if err != nil || string(unchanged) != string(original) || segmenter.joins != 1 {
				t.Fatal("retry remuxed or replaced the original journal")
			}
			store.saveErr, store.coverageErr = nil, nil
			staleBuffer := *buffer
			staleBuffer.segments = append([]bufferedRtspSegment(nil), buffer.segments...)
			if err := service.publishEventBuffer(context.Background(), camera, start, buffer, pattern); err != nil {
				t.Fatal(err)
			}
			if err := service.publishEventBuffer(context.Background(), camera, start, &staleBuffer, pattern); err != nil {
				t.Fatalf("flush after another replay completed was not idempotent: %v", err)
			}
			if segmenter.joins != 1 || len(store.recordings) != 1 || len(store.coverage) == 0 || buffer.recording {
				t.Fatal("event publication was not recovered exactly once")
			}
			published, err := os.ReadFile(filepath.Join(root, store.recording.LocalPath))
			if err != nil || !strings.HasSuffix(string(published), "join-1") {
				t.Fatal("replay did not preserve the first remux bytes")
			}
		})
	}
}

func TestRtspPublicationRecoversEveryCommitStage(t *testing.T) {
	for _, stage := range []string{"before rename", "after rename", "after indexing", "worker retry", "retained tombstone", "alongside corrupt journal"} {
		t.Run(stage, func(t *testing.T) {
			now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
			root := t.TempDir()
			buffer := filepath.Join(root, ".rtsp-buffer", "camera-one")
			if err := os.MkdirAll(buffer, 0o750); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(buffer, "segment-run-000000.partial.mkv")
			payload := []byte("native-video")
			if err := os.WriteFile(path, payload, 0o640); err != nil {
				t.Fatal(err)
			}
			store := &rtspRecordingStore{}
			failure := errors.New("injected publication failure")
			switch stage {
			case "before rename":
				store.lookupErr = failure
			case "after rename", "worker retry":
				store.saveErr = failure
			default:
				store.coverageErr = failure
			}
			camera := domain.Camera{ID: "camera-one", Name: "Front", ArchiveStream: domain.StreamMain}
			service := NewRtspRecorderService(store, nil, nil, &rtspSegmenter{}, root, filepath.Dir(buffer))
			segment := RtspSegment{Path: path, EndOffset: time.Minute, Coverage: []domain.TimeRange{{StartAtUtc: now, EndAtUtc: now.Add(time.Minute)}}}
			if err := service.publishSegment(context.Background(), camera, now, segment); !errors.Is(err, failure) {
				t.Fatalf("fault not injected: %v", err)
			}
			removeRtspRunFiles(filepath.Join(buffer, "segment-run-%06d.partial.mkv"))
			if stage == "before rename" {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("journaled source removed: %v", err)
				}
			}
			if stage == "retained tombstone" {
				store.recordings[0].State = domain.RecordingDeleted
				store.recordings[0].LocalPath = ""
				if err := os.Remove(filepath.Join(root, store.recording.LocalPath)); err != nil {
					t.Fatal(err)
				}
			}
			store.lookupErr, store.saveErr, store.coverageErr = nil, nil, nil
			if stage == "alongside corrupt journal" {
				if err := os.WriteFile(filepath.Join(buffer, "publication-corrupt.json"), []byte("invalid"), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			restarted := NewRtspRecorderService(store, nil, nil, &rtspSegmenter{}, root, filepath.Dir(buffer))
			if stage == "worker retry" {
				if err := restarted.recoverCameraPublications(context.Background(), camera.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := restarted.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := restarted.Recover(context.Background()); err != nil {
				t.Fatalf("replay not idempotent: %v", err)
			}
			if len(store.recordings) != 1 {
				t.Fatalf("publication lost or duplicated: %#v", store.recordings)
			}
			journals, _ := filepath.Glob(filepath.Join(buffer, "publication-*.json"))
			expectedJournals := 0
			if stage == "alongside corrupt journal" {
				expectedJournals = 1
				if len(store.issues) == 0 {
					t.Fatal("corrupt journal was not reported")
				}
			}
			if len(journals) != expectedJournals {
				t.Fatal("committed journal retained")
			}
			if stage == "retained tombstone" {
				if _, err := os.Stat(filepath.Join(root, store.recording.LocalPath)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("retention tombstone resurrected")
				}
				if len(store.coverage) != 1 || !store.coverage[0].StartAtUtc.Equal(now) || !store.coverage[0].EndAtUtc.Equal(now.Add(time.Minute)) {
					t.Fatal("retention discarded pending verified coverage")
				}
			} else {
				actual, err := os.ReadFile(filepath.Join(root, store.recording.LocalPath))
				if err != nil || string(actual) != string(payload) || len(store.coverage) != 1 {
					t.Fatalf("native media or coverage lost: %v", err)
				}
			}
		})
	}
}

func TestRtspPublicationIssuesResolveOnlyAfterCameraPassHasNoFailures(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	bufferRoot := filepath.Join(root, ".rtsp-buffer")
	for _, id := range []string{"camera-one", "camera-two"} {
		if err := os.MkdirAll(filepath.Join(bufferRoot, id), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bufferRoot, id, "publication-corrupt.json"), []byte("invalid"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(bufferRoot, "camera-one", "run-000000.partial.mkv")
	if err := os.WriteFile(path, []byte("native-video"), 0o640); err != nil {
		t.Fatal(err)
	}
	store := &rtspRecordingStore{saveErr: errors.New("index unavailable")}
	service := NewRtspRecorderService(store, nil, nil, &rtspSegmenter{}, root, bufferRoot)
	camera := domain.Camera{ID: "camera-one", Name: "Front", ArchiveStream: domain.StreamMain}
	if err := service.publishSegment(ctx, camera, time.Now().UTC(), RtspSegment{Path: path, EndOffset: time.Minute}); err == nil {
		t.Fatal("index failure was not injected")
	}
	if err := service.Recover(ctx); err != nil || len(store.resolutions) != 0 {
		t.Fatal("failed journals were not isolated or their issue was cleared")
	}
	store.saveErr = nil
	if err := service.recoverCameraPublications(ctx, camera.ID); err != nil || len(store.recordings) != 1 || len(store.resolutions) != 0 {
		t.Fatal("one successful journal cleared another outstanding failure")
	}
	if err := os.Remove(filepath.Join(bufferRoot, camera.ID, "publication-corrupt.json")); err != nil {
		t.Fatal(err)
	}
	if err := service.recoverCameraPublications(ctx, camera.ID); err != nil || store.resolutions[camera.ID] != 1 || store.resolutions["camera-two"] != 0 {
		t.Fatal("successful camera pass did not resolve only that camera's issue")
	}
	if err := os.Remove(filepath.Join(bufferRoot, "camera-two", "publication-corrupt.json")); err != nil {
		t.Fatal(err)
	}
	if err := service.Recover(ctx); err != nil || store.resolutions["camera-two"] == 0 {
		t.Fatal("startup did not resolve an issue after all its journals were cleared")
	}
}

func TestRtspPublicationRejectsEscapedPathsAndCorruption(t *testing.T) {
	for _, scenario := range []string{"escaped archive", "escaped buffer", "symlink", "checksum"} {
		t.Run(scenario, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			buffer := filepath.Join(root, ".rtsp-buffer", "camera-one")
			if err := os.MkdirAll(buffer, 0o750); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			digest := sha256.Sum256([]byte("native-video"))
			pending := rtspPublication{Version: 1, BufferName: "segment-run-000000.partial.mkv", Recording: domain.Recording{
				ID: "recording-one", CameraID: "camera-one", Source: domain.RecordingSourceRtsp, State: domain.RecordingComplete,
				StartAtUtc: now, EndAtUtc: now.Add(time.Minute), SizeBytes: 12, Sha256: fmt.Sprintf("%x", digest), LocalPath: "camera-one/video.mkv"}}
			if err := os.WriteFile(filepath.Join(buffer, pending.BufferName), []byte("native-video"), 0o640); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "escaped archive":
				pending.Recording.LocalPath = "../outside.mkv"
			case "escaped buffer":
				pending.BufferName = "../outside.partial.mkv"
			case "symlink":
				if err := os.Symlink(outside, filepath.Join(root, "camera-one")); err != nil {
					t.Fatal(err)
				}
			case "checksum":
				pending.Recording.Sha256 = strings.Repeat("0", 64)
			}
			if err := writeSidecar(filepath.Join(buffer, "publication-recording-one.json"), pending); err != nil {
				t.Fatal(err)
			}
			store := &rtspRecordingStore{}
			service := NewRtspRecorderService(store, nil, nil, nil, root, filepath.Dir(buffer))
			if err := service.Recover(context.Background()); err != nil {
				t.Fatalf("unsafe publication blocked startup: %v", err)
			}
			if len(store.recordings) != 0 {
				t.Fatal("unsafe media indexed")
			}
			if len(store.issues) != 1 {
				t.Fatal("unsafe publication was not reported")
			}
			if _, err := os.Stat(filepath.Join(buffer, pending.BufferName)); err != nil && scenario != "escaped buffer" {
				t.Fatal("failed journal source was not preserved")
			}
			if err := service.recoverCameraPublications(context.Background(), "camera-one"); err != nil {
				t.Fatal("unsafe journal blocked camera retry")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatal("publication escaped archive root")
			}
		})
	}
}

func TestParseRtspSegmentRejectsEscapedPath(t *testing.T) {
	_, err := parseRtspSegment(filepath.Join(t.TempDir(), "segment-%06d.partial.mkv"), `../outside.mkv,0.000000,60.000000`)
	if err == nil {
		t.Fatal("expected escaped segment path to be rejected")
	}
}
