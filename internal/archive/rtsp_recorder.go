package archive

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"
	"reonavr/internal/security"
)

const defaultRtspSegmentDuration = time.Minute
const eventRtspSegmentDuration = 2 * time.Second

type RtspRecordingStore interface {
	GetCamera(context.Context, string) (domain.Camera, error)
	GetCredential(context.Context, string) (domain.EncryptedCredential, error)
	ListEvents(context.Context, string, int) ([]domain.Event, error)
	SaveRtspRecording(context.Context, domain.Recording) error
}

type RtspURLProvider interface {
	RtspURL(context.Context, reolink.Connection, domain.ArchiveStream) (string, error)
}

type RtspSegment struct {
	Path        string
	StartOffset time.Duration
	EndOffset   time.Duration
}

type RtspSegmenter interface {
	Capture(context.Context, string, string, string, string, time.Duration, func(RtspSegment) error) error
	Join(context.Context, []string, string) error
	ProbeCodec(context.Context, string) (string, error)
}

type bufferedRtspSegment struct {
	segment RtspSegment
	startAt time.Time
	endAt   time.Time
}

type rtspEventBuffer struct {
	segments    []bufferedRtspSegment
	recording   bool
	windowStart time.Time
	windowEnd   time.Time
}

type RtspRecorderService struct {
	store       RtspRecordingStore
	provider    RtspURLProvider
	cipher      CredentialCipher
	segmenter   RtspSegmenter
	capacity    CapacityGuard
	archiveRoot string
	bufferRoot  string
	now         func() time.Time
}

func (service *RtspRecorderService) SetCapacityGuard(capacity CapacityGuard) {
	service.capacity = capacity
}

func NewRtspRecorderService(store RtspRecordingStore, provider RtspURLProvider, cipher CredentialCipher, segmenter RtspSegmenter, archiveRoot, bufferRoot string) *RtspRecorderService {
	return &RtspRecorderService{
		store: store, provider: provider, cipher: cipher, segmenter: segmenter,
		archiveRoot: archiveRoot, bufferRoot: bufferRoot, now: time.Now,
	}
}

func (service *RtspRecorderService) Recover(ctx context.Context) error {
	archiveRoot := filepath.Clean(service.archiveRoot)
	bufferRoot := filepath.Clean(service.bufferRoot)
	if archiveRoot == bufferRoot || !pathWithin(archiveRoot, bufferRoot) {
		return errors.New("RTSP buffer must be a child of the archive root")
	}
	err := filepath.WalkDir(bufferRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".partial.mkv") {
			return nil
		}
		if !pathWithin(bufferRoot, path) {
			return errors.New("RTSP recovery path escaped the configured buffer root")
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove abandoned RTSP fragment: %w", err)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("recover RTSP buffer: %w", err)
	}
	return nil
}

func (service *RtspRecorderService) Record(ctx context.Context, cameraID string) error {
	camera, err := service.store.GetCamera(ctx, cameraID)
	if err != nil {
		return err
	}
	if !camera.Enabled || !camera.ArchiveEnabled || camera.RecordingSource != domain.RecordingSourceRtsp {
		return nil
	}
	if camera.RecordingMode != domain.RecordingModeContinuous && camera.RecordingMode != domain.RecordingModeEvents {
		return nil
	}
	if camera.RecordingMode == domain.RecordingModeEvents && len(camera.EventTypes) == 0 {
		return nil
	}
	if !safePathComponent(camera.ID) {
		return errors.New("camera ID is not safe for an archive path")
	}
	credential, err := service.store.GetCredential(ctx, camera.ID)
	if err != nil {
		return err
	}
	if credential.KeyVersion != security.KeyVersion {
		return errors.New("stored credential uses an unsupported key version")
	}
	password, err := service.cipher.Decrypt(camera.ID, credential.Ciphertext, credential.Nonce)
	if err != nil {
		return err
	}
	connection := reolink.Connection{
		Host: camera.Host, Port: camera.Port, Channel: camera.Channel,
		Stream: camera.ArchiveStream, Timezone: camera.Timezone,
		Username: credential.Username, Password: password,
	}
	streamURL, err := service.provider.RtspURL(ctx, connection, camera.ArchiveStream)
	if err != nil {
		password = ""
		return err
	}
	bufferDirectory := filepath.Join(service.bufferRoot, camera.ID)
	if err := os.MkdirAll(bufferDirectory, 0o750); err != nil {
		password = ""
		return fmt.Errorf("create RTSP buffer directory: %w", err)
	}
	runID, err := randomIdentifier()
	if err != nil {
		password = ""
		return err
	}
	outputPattern := filepath.Join(bufferDirectory, "segment-"+runID+"-%06d.partial.mkv")
	defer removeRtspRunFiles(outputPattern)
	captureStartedAt := service.now().UTC()
	segmentDuration := defaultRtspSegmentDuration
	eventBuffer := &rtspEventBuffer{}
	if camera.RecordingMode == domain.RecordingModeEvents {
		segmentDuration = eventRtspSegmentDuration
	}
	err = service.segmenter.Capture(ctx, streamURL, credential.Username, password, outputPattern, segmentDuration, func(segment RtspSegment) error {
		publishContext := ctx
		var cancel context.CancelFunc
		if ctx.Err() != nil {
			publishContext, cancel = context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
		}
		if camera.RecordingMode == domain.RecordingModeEvents {
			return service.handleEventSegment(publishContext, camera, captureStartedAt, eventBuffer, segment, outputPattern)
		}
		return service.publishSegment(publishContext, camera, captureStartedAt, segment)
	})
	password = ""
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("RTSP stream ended")
}

func (service *RtspRecorderService) handleEventSegment(ctx context.Context, camera domain.Camera, captureStartedAt time.Time, buffer *rtspEventBuffer, segment RtspSegment, outputPattern string) error {
	current := bufferedRtspSegment{
		segment: segment,
		startAt: captureStartedAt.Add(segment.StartOffset).UTC(),
		endAt:   captureStartedAt.Add(segment.EndOffset).UTC(),
	}
	buffer.segments = append(buffer.segments, current)
	events, err := service.store.ListEvents(ctx, camera.ID, 1000)
	if err != nil {
		return err
	}
	selected := make(map[domain.EventType]bool, len(camera.EventTypes))
	for _, eventType := range camera.EventTypes {
		selected[eventType] = true
	}
	open := false
	if !buffer.recording {
		for _, event := range events {
			if !selected[event.EventType] || event.StartedAtUtc.After(current.endAt) {
				continue
			}
			windowStart := event.StartedAtUtc.Add(-time.Duration(camera.PreEventSeconds) * time.Second)
			windowEnd := current.endAt
			if event.EndedAtUtc != nil {
				windowEnd = event.EndedAtUtc.Add(time.Duration(camera.PostEventSeconds) * time.Second)
			}
			if windowEnd.Before(current.startAt) {
				continue
			}
			buffer.recording = true
			buffer.windowStart = windowStart
			buffer.windowEnd = windowEnd
			open = event.EndedAtUtc == nil
			break
		}
	}
	if buffer.recording {
		changed := true
		for changed {
			changed = false
			rightEdge := buffer.windowEnd
			if open && current.endAt.After(rightEdge) {
				rightEdge = current.endAt
			}
			open = false
			for _, event := range events {
				if !selected[event.EventType] || event.StartedAtUtc.After(current.endAt) {
					continue
				}
				eventStart := event.StartedAtUtc.Add(-time.Duration(camera.PreEventSeconds) * time.Second)
				eventEnd := current.endAt
				if event.EndedAtUtc != nil {
					eventEnd = event.EndedAtUtc.Add(time.Duration(camera.PostEventSeconds) * time.Second)
				}
				if eventStart.After(rightEdge) || eventEnd.Before(buffer.windowStart) {
					continue
				}
				if eventStart.Before(buffer.windowStart) {
					buffer.windowStart = eventStart
					changed = true
				}
				if event.EndedAtUtc == nil {
					open = true
				} else if eventEnd.After(buffer.windowEnd) {
					buffer.windowEnd = eventEnd
					changed = true
				}
			}
		}
	}

	if !buffer.recording {
		return trimEventBuffer(buffer, current.endAt.Add(-time.Duration(camera.PreEventSeconds)*time.Second))
	}
	if open || current.endAt.Before(buffer.windowEnd) {
		return nil
	}
	paths := make([]string, 0, len(buffer.segments))
	var first, last *bufferedRtspSegment
	for index := range buffer.segments {
		item := &buffer.segments[index]
		if item.endAt.Before(buffer.windowStart) || item.startAt.After(buffer.windowEnd) {
			continue
		}
		paths = append(paths, item.segment.Path)
		if first == nil {
			first = item
		}
		last = item
	}
	if len(paths) == 0 || first == nil || last == nil {
		return errors.New("RTSP event window has no buffered media")
	}
	joinedPath := strings.Replace(outputPattern, "%06d", "event-"+strconv.FormatInt(current.endAt.UnixNano(), 10), 1)
	if service.capacity != nil {
		var requiredBytes int64
		for _, path := range paths {
			info, err := os.Stat(path)
			if err != nil {
				return fmt.Errorf("inspect RTSP event fragment: %w", err)
			}
			requiredBytes += info.Size()
		}
		if err := service.capacity.EnsureCapacity(ctx, requiredBytes); err != nil {
			return err
		}
	}
	if err := service.segmenter.Join(ctx, paths, joinedPath); err != nil {
		return err
	}
	joined := RtspSegment{
		Path: joinedPath, StartOffset: first.startAt.Sub(captureStartedAt),
		EndOffset: last.endAt.Sub(captureStartedAt),
	}
	if err := service.publishSegment(ctx, camera, captureStartedAt, joined); err != nil {
		return err
	}
	for _, path := range paths {
		if path != current.segment.Path {
			_ = os.Remove(path)
		}
	}
	buffer.segments = []bufferedRtspSegment{current}
	buffer.recording = false
	buffer.windowStart = time.Time{}
	buffer.windowEnd = time.Time{}
	return nil
}

func trimEventBuffer(buffer *rtspEventBuffer, retainAfter time.Time) error {
	retained := buffer.segments[:0]
	for _, item := range buffer.segments {
		if item.endAt.Before(retainAfter) {
			if err := os.Remove(item.segment.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("expire RTSP buffer segment: %w", err)
			}
			continue
		}
		retained = append(retained, item)
	}
	buffer.segments = retained
	return nil
}

func removeRtspRunFiles(outputPattern string) {
	pattern := strings.Replace(outputPattern, "%06d", "*", 1)
	paths, _ := filepath.Glob(pattern)
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

func (service *RtspRecorderService) publishSegment(ctx context.Context, camera domain.Camera, captureStartedAt time.Time, segment RtspSegment) error {
	if segment.EndOffset <= segment.StartOffset {
		return errors.New("RTSP segment has an invalid time range")
	}
	bufferDirectory := filepath.Join(service.bufferRoot, camera.ID)
	if !pathWithin(bufferDirectory, segment.Path) {
		return errors.New("RTSP segment escaped the configured buffer root")
	}
	if service.capacity != nil {
		if err := service.capacity.EnsureCapacity(ctx, 0); err != nil {
			return err
		}
	}
	sizeBytes, digest, err := verifyRecording(segment.Path, 0)
	if err != nil {
		return err
	}
	codec, err := service.segmenter.ProbeCodec(ctx, segment.Path)
	if err != nil {
		return err
	}
	startAt := captureStartedAt.Add(segment.StartOffset).UTC()
	endAt := captureStartedAt.Add(segment.EndOffset).UTC()
	eventTypes, err := service.eventTypesForRange(ctx, camera, startAt, endAt)
	if err != nil {
		return err
	}
	idHash := sha256.Sum256([]byte(camera.ID + "\x00" + startAt.Format(time.RFC3339Nano) + "\x00" + filepath.Base(segment.Path)))
	recordingID := hex.EncodeToString(idHash[:16])
	fileName := startAt.Format("20060102T150405.000Z") + "-" + recordingID[:12] + ".mkv"
	nativeName := filepath.ToSlash(filepath.Join("Rtsp", startAt.Format("2006"), startAt.Format("01"), startAt.Format("02"), fileName))
	relativePath, err := archiveRelativePath(camera.ID, nativeName)
	if err != nil {
		return err
	}
	finalPath := filepath.Join(service.archiveRoot, relativePath)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o750); err != nil {
		return fmt.Errorf("create RTSP archive directory: %w", err)
	}
	if err := os.Rename(segment.Path, finalPath); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return errors.New("RTSP buffer and archive must be on the same filesystem")
		}
		return fmt.Errorf("publish RTSP segment: %w", err)
	}
	sidecar := struct {
		CameraID   string                 `json:"cameraId"`
		NativeName string                 `json:"nativeName"`
		Source     domain.RecordingSource `json:"source"`
		Codec      string                 `json:"codec"`
		Stream     domain.ArchiveStream   `json:"stream"`
		StartAtUtc time.Time              `json:"startAtUtc"`
		EndAtUtc   time.Time              `json:"endAtUtc"`
		SizeBytes  int64                  `json:"sizeBytes"`
		EventTypes []domain.EventType     `json:"eventTypes"`
		Sha256     string                 `json:"sha256"`
	}{
		CameraID: camera.ID, NativeName: nativeName, Source: domain.RecordingSourceRtsp,
		Codec: codec, Stream: camera.ArchiveStream, StartAtUtc: startAt, EndAtUtc: endAt,
		SizeBytes: sizeBytes, EventTypes: eventTypes, Sha256: digest,
	}
	if err := writeSidecar(finalPath+".json", sidecar); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(finalPath)); err != nil {
		return err
	}
	rawMetadata, err := json.Marshal(struct {
		Capture             string  `json:"capture"`
		SegmentStartSeconds float64 `json:"segmentStartSeconds"`
		SegmentEndSeconds   float64 `json:"segmentEndSeconds"`
	}{Capture: "rtsp", SegmentStartSeconds: segment.StartOffset.Seconds(), SegmentEndSeconds: segment.EndOffset.Seconds()})
	if err != nil {
		return fmt.Errorf("encode RTSP recording metadata: %w", err)
	}
	completedAt := service.now().UTC()
	return service.store.SaveRtspRecording(ctx, domain.Recording{
		ID: recordingID, CameraID: camera.ID, NativeName: nativeName,
		Source: domain.RecordingSourceRtsp, Stream: camera.ArchiveStream, Codec: codec,
		StartAtUtc: startAt, EndAtUtc: endAt, SizeBytes: sizeBytes,
		EventTypes: eventTypes, State: domain.RecordingComplete,
		RawMetadata: rawMetadata, DiscoveredAtUtc: completedAt, UpdatedAtUtc: completedAt,
		LocalPath: filepath.ToSlash(relativePath), Sha256: digest,
	})
}

func (service *RtspRecorderService) eventTypesForRange(ctx context.Context, camera domain.Camera, startAt, endAt time.Time) ([]domain.EventType, error) {
	events, err := service.store.ListEvents(ctx, camera.ID, 1000)
	if err != nil {
		return nil, err
	}
	matching := make(map[domain.EventType]bool, len(camera.EventTypes))
	for _, event := range events {
		if event.StartedAtUtc.After(endAt) || (event.EndedAtUtc != nil && event.EndedAtUtc.Before(startAt)) {
			continue
		}
		matching[event.EventType] = true
	}
	result := make([]domain.EventType, 0, len(matching))
	for _, eventType := range camera.EventTypes {
		if matching[eventType] {
			result = append(result, eventType)
		}
	}
	return result, nil
}

type FFmpegSegmenter struct {
	Path            string
	ProbePath       string
	SegmentDuration time.Duration
}

func (segmenter *FFmpegSegmenter) Capture(ctx context.Context, streamURL, username, password, outputPattern string, segmentDuration time.Duration, onSegment func(RtspSegment) error) error {
	if segmentDuration <= 0 {
		segmentDuration = segmenter.duration()
	}
	arguments, input, authenticatedURL, err := ffmpegCaptureSpec(streamURL, username, password, segmentDuration, outputPattern)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, segmenter.executable(), arguments...)
	command.Stdin = strings.NewReader(input)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open FFmpeg segment output: %w", err)
	}
	stderr := &boundedBuffer{limit: 16 * 1024}
	command.Stderr = stderr
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return command.Process.Signal(os.Interrupt)
	}
	command.WaitDelay = 5 * time.Second
	if err := command.Start(); err != nil {
		return fmt.Errorf("start FFmpeg RTSP recorder: %w", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	var callbackErr error
	for scanner.Scan() {
		segment, parseErr := parseRtspSegment(outputPattern, scanner.Text())
		if parseErr != nil {
			callbackErr = parseErr
			break
		}
		if err := onSegment(segment); err != nil {
			callbackErr = err
			break
		}
	}
	if callbackErr != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if callbackErr != nil {
		return callbackErr
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return fmt.Errorf("read FFmpeg segment output: %w", scanErr)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		return fmt.Errorf("FFmpeg RTSP stream copy failed: %s", safeFFmpegError(stderr.String(), authenticatedURL, username, password))
	}
	return nil
}

func (segmenter *FFmpegSegmenter) Join(ctx context.Context, inputPaths []string, outputPath string) error {
	if len(inputPaths) == 0 || strings.TrimSpace(outputPath) == "" {
		return errors.New("RTSP event finalization requires input and output paths")
	}
	input, err := ffconcatDocument(inputPaths)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, segmenter.executable(),
		"-nostdin", "-hide_banner", "-loglevel", "error", "-f", "concat", "-safe", "0",
		"-protocol_whitelist", "pipe,file", "-i", "pipe:0", "-map", "0:v:0", "-map", "0:a?",
		"-c", "copy", "-f", "matroska", "-y", outputPath)
	command.Stdin = strings.NewReader(input)
	stderr := &boundedBuffer{limit: 16 * 1024}
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("finalize RTSP event recording: %s", safeFFmpegError(stderr.String(), "", "", ""))
	}
	return nil
}

func ffconcatDocument(inputPaths []string) (string, error) {
	var input strings.Builder
	input.WriteString("ffconcat version 1.0\n")
	for _, inputPath := range inputPaths {
		if !filepath.IsAbs(inputPath) || strings.ContainsAny(inputPath, "\r\n") {
			return "", errors.New("RTSP event segment path is invalid")
		}
		input.WriteString("file 'file:")
		input.WriteString(strings.ReplaceAll(filepath.ToSlash(inputPath), "'", "'\\''"))
		input.WriteString("'\n")
	}
	return input.String(), nil
}

func (segmenter *FFmpegSegmenter) ProbeCodec(ctx context.Context, filePath string) (string, error) {
	command := exec.CommandContext(ctx, segmenter.probeExecutable(), "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_name", "-of", "default=noprint_wrappers=1:nokey=1", filePath)
	output, err := command.Output()
	if err != nil {
		return "", errors.New("probe RTSP segment codec failed")
	}
	switch strings.ToLower(strings.TrimSpace(string(output))) {
	case "h264":
		return "H.264", nil
	case "hevc", "h265":
		return "H.265", nil
	case "":
		return "", errors.New("RTSP segment has no video stream")
	default:
		return strings.ToUpper(strings.TrimSpace(string(output))), nil
	}
}

func (segmenter *FFmpegSegmenter) executable() string {
	if strings.TrimSpace(segmenter.Path) == "" {
		return "/usr/bin/ffmpeg"
	}
	return segmenter.Path
}

func (segmenter *FFmpegSegmenter) probeExecutable() string {
	if strings.TrimSpace(segmenter.ProbePath) == "" {
		return "/usr/bin/ffprobe"
	}
	return segmenter.ProbePath
}

func (segmenter *FFmpegSegmenter) duration() time.Duration {
	if segmenter.SegmentDuration <= 0 {
		return defaultRtspSegmentDuration
	}
	return segmenter.SegmentDuration
}

func ffmpegCaptureSpec(streamURL, username, password string, segmentDuration time.Duration, outputPattern string) ([]string, string, string, error) {
	parsed, err := url.Parse(streamURL)
	if err != nil || !strings.EqualFold(parsed.Scheme, "rtsp") || parsed.Hostname() == "" || parsed.User != nil {
		return nil, "", "", errors.New("RTSP endpoint must be an unauthenticated RTSP URL")
	}
	if username == "" {
		return nil, "", "", errors.New("RTSP username is required")
	}
	if segmentDuration < time.Second {
		return nil, "", "", errors.New("RTSP segment duration must be at least one second")
	}
	if strings.TrimSpace(outputPattern) == "" {
		return nil, "", "", errors.New("RTSP output pattern is required")
	}
	parsed.User = url.UserPassword(username, password)
	authenticatedURL := parsed.String()
	if strings.Contains(authenticatedURL, "'") || strings.ContainsAny(authenticatedURL, "\r\n") {
		return nil, "", "", errors.New("RTSP URL cannot be represented safely")
	}
	input := "ffconcat version 1.0\nfile '" + authenticatedURL + "'\noption rtsp_transport tcp\noption timeout 15000000\n"
	arguments := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-f", "concat", "-safe", "0",
		"-protocol_whitelist", "pipe,file,rtsp,tcp,udp", "-i", "pipe:0",
		"-map", "0:v:0", "-map", "0:a?", "-c", "copy", "-f", "segment",
		"-segment_time", strconv.FormatFloat(segmentDuration.Seconds(), 'f', 3, 64),
		"-reset_timestamps", "1", "-segment_list", "pipe:1", "-segment_list_type", "csv",
		"-segment_format", "matroska", "-n", outputPattern,
	}
	return arguments, input, authenticatedURL, nil
}

func parseRtspSegment(outputPattern, line string) (RtspSegment, error) {
	record, err := csv.NewReader(strings.NewReader(line)).Read()
	if err != nil || len(record) != 3 {
		return RtspSegment{}, errors.New("FFmpeg returned invalid segment metadata")
	}
	startSeconds, err := strconv.ParseFloat(record[1], 64)
	if err != nil || startSeconds < 0 {
		return RtspSegment{}, errors.New("FFmpeg returned an invalid segment start")
	}
	endSeconds, err := strconv.ParseFloat(record[2], 64)
	if err != nil || endSeconds <= startSeconds {
		return RtspSegment{}, errors.New("FFmpeg returned an invalid segment end")
	}
	reportedPath := record[0]
	if !filepath.IsAbs(reportedPath) {
		reportedPath = filepath.Join(filepath.Dir(outputPattern), reportedPath)
	}
	reportedPath = filepath.Clean(reportedPath)
	if !pathWithin(filepath.Dir(outputPattern), reportedPath) {
		return RtspSegment{}, errors.New("FFmpeg segment path escaped the configured output directory")
	}
	return RtspSegment{
		Path: reportedPath, StartOffset: time.Duration(startSeconds * float64(time.Second)),
		EndOffset: time.Duration(endSeconds * float64(time.Second)),
	}, nil
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func safePathComponent(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value && !strings.ContainsAny(value, `/\`)
}

func randomIdentifier() (string, error) {
	value := make([]byte, 8)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate RTSP capture ID: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func safeFFmpegError(message, authenticatedURL, username, password string) string {
	message = strings.TrimSpace(message)
	for _, secret := range []string{authenticatedURL, password, url.QueryEscape(password), username, url.QueryEscape(username)} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	if message == "" {
		return "stream ended unexpectedly"
	}
	if len(message) > 500 {
		return message[:500]
	}
	return message
}

type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	written := len(value)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = buffer.buffer.Write(value)
	}
	return written, nil
}

func (buffer *boundedBuffer) String() string {
	return buffer.buffer.String()
}
