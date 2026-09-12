package reolink

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"reonavr/internal/domain"
)

type fixtureRunner struct {
	responses map[string]string
	snapshot  []byte
	calls     [][]string
	passwords []string
}

type streamingFixtureRunner struct {
	fixtureRunner
	lines [][]byte
}

func (runner *streamingFixtureRunner) Stream(_ context.Context, password string, onLine func([]byte) error, arguments ...string) error {
	runner.calls = append(runner.calls, slices.Clone(arguments))
	runner.passwords = append(runner.passwords, password)
	for _, line := range runner.lines {
		if err := onLine(line); err != nil {
			return err
		}
	}
	return nil
}

func (runner *fixtureRunner) Run(_ context.Context, password string, arguments ...string) ([]byte, error) {
	runner.calls = append(runner.calls, slices.Clone(arguments))
	runner.passwords = append(runner.passwords, password)
	if slices.Contains(arguments, "snapshot") && len(runner.snapshot) > 0 {
		fileIndex := slices.Index(arguments, "--file")
		if fileIndex < 0 || fileIndex+1 >= len(arguments) {
			return nil, errors.New("snapshot file argument missing")
		}
		if err := os.WriteFile(arguments[fileIndex+1], runner.snapshot, 0o600); err != nil {
			return nil, err
		}
		return []byte(`{"ok":true}`), nil
	}
	for key, response := range runner.responses {
		if strings.HasSuffix(strings.Join(arguments, " "), key) {
			return []byte(response), nil
		}
	}
	return nil, context.DeadlineExceeded
}

func TestClientNormalizesCameraCapabilitiesWithoutPasswordArgument(t *testing.T) {
	snapshot := string([]byte{0xff, 0xd8, 0x01, 0x02, 0xff, 0xd9})
	runner := &fixtureRunner{snapshot: []byte(snapshot), responses: map[string]string{
		"info":                                 `{"data":{"model":"RLC-810A","firmware":"v3.1.0.956"}}`,
		"config get time-zone":                 `{"data":{"timeZone":28800}}`,
		"raw 106":                              `{"data":{"bodyXml":"<body><Dst><enable>1</enable><offset>1</offset><startMon>3</startMon><startWeek>2</startWeek><startWeekday>0</startWeekday><startHour>2</startHour><startMin>0</startMin><startSec>0</startSec><endMon>11</endMon><endWeek>1</endWeek><endWeekday>0</endWeekday><endHour>2</endHour><endMin>0</endMin><endSec>0</endSec></Dst></body>"}}`,
		"encode get":                           `{"data":{"main":{"width":3840,"height":2160,"bitrate":6144,"fps":25},"sub":{"width":640,"height":360,"bitrate":256,"fps":10}}}`,
		"record config get":                    `{"data":{"preRecordSecs":10,"postRecordSecs":15,"schedule":{"TIMING":"00000000"}}}`,
		"storage status":                       `{"data":{"items":[{"mounted":true,"formatted":true,"remainGB":0.779296875,"totalGB":238}]}}`,
		"capabilities":                         `{"data":{"item":{"motion":"1","aitype":"1847"}}}`,
		"stream url --kind rtsp --stream main": `{"data":{"urls":["rtsp://10.0.0.152/Preview_01_main"]}}`,
	}}
	client := NewClient(runner, time.Second)
	client.captureSnapshot = true
	result, err := client.Test(context.Background(), Connection{Host: "10.0.0.152", Port: 9000, Timezone: "America/Los_Angeles", Username: "admin", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "RLC-810A" || result.Timezone != "America/Los_Angeles" || result.TimezoneOffsetSeconds != -28800 || result.Capabilities.PreRecordSeconds != 10 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if !result.TimezoneConfiguration.DstEnabled || result.TimezoneConfiguration.DstStart.Month != 3 || result.TimezoneConfiguration.DstEnd.Month != 11 {
		t.Fatalf("unexpected timezone configuration: %#v", result.TimezoneConfiguration)
	}
	if !result.Capabilities.StorageAvailable || !result.Capabilities.RtspAvailable || result.Capabilities.ContinuousAvailable {
		t.Fatalf("unexpected recording capabilities: %#v", result.Capabilities)
	}
	if result.Capabilities.MainBitrateKbps != 6144 || result.Capabilities.SubBitrateKbps != 256 {
		t.Fatalf("unexpected bitrate capabilities: %#v", result.Capabilities)
	}
	if string(result.SnapshotJPEG) != snapshot {
		t.Fatalf("unexpected snapshot: %x", result.SnapshotJPEG)
	}
	if snapshotCall := runner.calls[len(runner.calls)-1]; !slices.Contains(snapshotCall, "--file") {
		t.Fatalf("snapshot did not use a private output file: %#v", snapshotCall)
	}
	expectedEvents := []domain.EventType{domain.EventMotion, domain.EventPerson, domain.EventVehicle, domain.EventAnimal}
	if !slices.Equal(result.Capabilities.SupportedEventTypes, expectedEvents) {
		t.Fatalf("unexpected events: %#v", result.Capabilities.SupportedEventTypes)
	}
	for index, call := range runner.calls {
		if slices.Contains(call, "secret") {
			t.Fatalf("password present in command arguments: %#v", call)
		}
		if !slices.Contains(call, "--password-stdin") || runner.passwords[index] != "secret" {
			t.Fatalf("password was not sent through stdin: %#v", call)
		}
	}
}

func TestParseRecordConfigDetectsExistingContinuousSchedule(t *testing.T) {
	var capabilities domain.CameraCapabilities
	parseRecordConfig([]byte(`{"data":{"preRecordSecs":10,"postRecordSecs":15,"schedule":{"table":{"TIMING":[0,0,1,0]}}}}`), &capabilities)
	if !capabilities.ContinuousAvailable {
		t.Fatal("expected nonempty TIMING schedule to enable camera-storage continuous mode")
	}
}

func TestParseTimezoneOffsetRequiresExplicitField(t *testing.T) {
	if _, err := parseTimezoneOffset([]byte(`{"data":{"offset":0}}`)); err == nil {
		t.Fatal("expected missing timeZone field to be rejected")
	}
	offset, err := parseTimezoneOffset([]byte(`{"data":{"configuration":{"timeZone":-28800}}}`))
	if err != nil || offset != -28800 {
		t.Fatalf("unexpected timezone offset: %d, %v", offset, err)
	}
}

func TestResolveCameraTimezoneRejectsMismatchedPreference(t *testing.T) {
	configuration := TimezoneConfiguration{
		BaseOffsetSeconds: -8 * 60 * 60, DstEnabled: true, DstOffsetSeconds: 60 * 60,
		DstStart: DstTransition{Month: 3, Week: 2, Weekday: 0, Hour: 2},
		DstEnd:   DstTransition{Month: 11, Week: 1, Weekday: 0, Hour: 2},
	}
	resolved, err := resolveCameraTimezone(configuration, "UTC", time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC))
	if err != nil || resolved != "America/Los_Angeles" {
		t.Fatalf("unexpected resolved timezone: %q, %v", resolved, err)
	}
}

func TestClientRequiresPrivateAddress(t *testing.T) {
	client := NewClient(&fixtureRunner{}, time.Second)
	_, err := client.Test(context.Background(), Connection{Host: "8.8.8.8", Port: 9000, Username: "admin", Password: "secret"})
	if err == nil || !strings.Contains(err.Error(), "private network") {
		t.Fatalf("expected private-network error, got %v", err)
	}
}

func TestDecodeDataRejectsMalformedPayload(t *testing.T) {
	var target any
	if err := decodeData([]byte("not-json"), &target); err == nil {
		t.Fatal("expected malformed JSON error")
	}
}

func TestSafeCommandErrorRedactsPassword(t *testing.T) {
	message := safeCommandError("login failed for password camera-secret", "camera-secret")
	if strings.Contains(message, "camera-secret") || !strings.Contains(message, "[redacted]") {
		t.Fatalf("password was not redacted: %q", message)
	}
}

func TestClientGetsRtspURLWithoutReturningEmbeddedCredentials(t *testing.T) {
	runner := &fixtureRunner{responses: map[string]string{
		"stream url --kind rtsp --stream sub": `{"data":{"urls":["rtsp://camera-user:camera-secret@10.0.0.152:554/Preview_01_sub"]}}`,
	}}
	client := NewClient(runner, time.Second)
	streamURL, err := client.RtspURL(context.Background(), Connection{
		Host: "10.0.0.152", Port: 9000, Username: "camera-user", Password: "camera-secret",
	}, domain.StreamSub)
	if err != nil {
		t.Fatal(err)
	}
	if streamURL != "rtsp://10.0.0.152:554/Preview_01_sub" {
		t.Fatalf("unexpected RTSP URL: %q", streamURL)
	}
	if len(runner.calls) != 1 || slices.Contains(runner.calls[0], "camera-secret") || runner.passwords[0] != "camera-secret" {
		t.Fatalf("RTSP lookup did not preserve password-stdin handling: %#v", runner.calls)
	}
}

func TestClientStreamsNormalizedEventTransitionsWithoutPasswordArgument(t *testing.T) {
	runner := &streamingFixtureRunner{lines: [][]byte{
		[]byte(`{"timestamp":1776765505,"channel":0,"eventType":"people","data":{"status":"MD","aiType":"people"}}`),
		[]byte(`not-json`),
		[]byte(`{"data":{"timestamp":1776765510,"channel":0,"eventType":"none","data":{"status":"none"}}}`),
		[]byte(`{"timestamp":1776765511,"channel":0,"eventType":"raw.cmd123","data":{}}`),
	}}
	client := NewClient(runner, time.Second)
	var transitions []domain.EventTransition
	err := client.StreamEvents(context.Background(), Connection{
		Host: "10.0.0.152", Port: 9000, Username: "admin", Password: "secret",
	}, func(transition domain.EventTransition) error {
		transitions = append(transitions, transition)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(transitions) != 2 || transitions[0].EventType != domain.EventPerson || !transitions[0].Active || transitions[1].Active {
		t.Fatalf("unexpected transitions: %#v", transitions)
	}
	if len(runner.calls) != 1 || slices.Contains(runner.calls[0], "secret") || runner.passwords[0] != "secret" {
		t.Fatalf("event stream exposed or omitted the password: %#v", runner.calls)
	}
	if !slices.Contains(runner.calls[0], "--password-stdin") {
		t.Fatalf("event stream omitted secure arguments: %#v", runner.calls[0])
	}
}

func TestClientReplaysBufferedEventsAfterCursor(t *testing.T) {
	runner := &fixtureRunner{responses: map[string]string{
		"events query --after 1776765504": `{"data":[{"timestamp":1776765505,"eventType":"people","data":{"status":"MD"}},{"bad":true},{"timestamp":1776765510,"eventType":"none","data":{"status":"none"}}]}`,
	}}
	client := NewClient(runner, time.Second)
	var transitions []domain.EventTransition
	err := client.QueryEvents(context.Background(), Connection{
		Host: "10.0.0.152", Port: 9000, Username: "admin", Password: "secret",
	}, time.Unix(1776765504, 0), func(transition domain.EventTransition) error {
		transitions = append(transitions, transition)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(transitions) != 2 || transitions[0].EventType != domain.EventPerson || transitions[1].Active {
		t.Fatalf("unexpected replay transitions: %#v", transitions)
	}
	if slices.Contains(runner.calls[0], "secret") || runner.passwords[0] != "secret" {
		t.Fatal("event replay did not keep the password on stdin")
	}
}

func TestEventPayloadsAcceptsNestedQueryEnvelope(t *testing.T) {
	items, err := eventPayloads([]byte(`{"data":{"events":[{"timestamp":1776765505,"eventType":"vehicle","data":{"status":"MD"}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("unexpected nested event payloads: %#v", items)
	}
}

func TestParseEventTransitionNormalizesSelectedTypes(t *testing.T) {
	tests := map[string]domain.EventType{
		"motion": domain.EventMotion, "people": domain.EventPerson,
		"vehicle": domain.EventVehicle, "dog_cat": domain.EventAnimal,
	}
	for nativeType, expected := range tests {
		transition, relevant, err := parseEventTransition([]byte(`{"timestamp":1776765505,"eventType":"` + nativeType + `","data":{"status":"MD"}}`))
		if err != nil || !relevant || transition.EventType != expected || !transition.Active {
			t.Fatalf("unexpected %s transition: %#v relevant=%v err=%v", nativeType, transition, relevant, err)
		}
	}
}

func TestClientSearchRecordingsNormalizesCLIResults(t *testing.T) {
	runner := &fixtureRunner{responses: map[string]string{
		"vod search --from 2026-09-11T08:00:00 --to 2026-09-11T10:00:00 --stream main --limit 0": `{"data":{"recordings":[{"channel":0,"startTime":"2026-09-11T08:59:33","endTime":"2026-09-11T09:00:03","fileSize":25186873,"name":"0120260911085933","recordType":"md,vehicle","streamType":"mainStream"}]}}`,
	}}
	client := NewClient(runner, time.Second)
	items, err := client.SearchRecordings(context.Background(), Connection{
		Host: "10.0.0.152", Port: 9000, Username: "admin", Password: "secret",
	}, domain.StreamMain, time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC), time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].NativeName != "0120260911085933" || items[0].SizeBytes != 25186873 {
		t.Fatalf("unexpected recordings: %#v", items)
	}
	expectedTypes := []domain.EventType{domain.EventMotion, domain.EventVehicle}
	if !slices.Equal(items[0].EventTypes, expectedTypes) || items[0].Stream != domain.StreamMain {
		t.Fatalf("unexpected normalized metadata: %#v", items[0])
	}
	if len(items[0].ID) != 32 {
		t.Fatalf("expected stable recording ID, got %q", items[0].ID)
	}
	if slices.Contains(runner.calls[0], "secret") || runner.passwords[0] != "secret" {
		t.Fatal("search did not keep the password on stdin")
	}
}

func TestClientDownloadsNativeRecordingWithoutPasswordArgument(t *testing.T) {
	runner := &fixtureRunner{responses: map[string]string{
		"vod download clip.mp4 --file /archive/clip.mp4.partial --stream main": `{}`,
	}}
	client := NewClient(runner, time.Second)
	err := client.DownloadRecording(context.Background(), Connection{
		Host: "10.0.0.152", Port: 9000, Username: "admin", Password: "secret",
	}, "clip.mp4", domain.StreamMain, "/archive/clip.mp4.partial")
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || slices.Contains(runner.calls[0], "secret") || runner.passwords[0] != "secret" {
		t.Fatalf("download exposed or omitted the password: %#v", runner.calls)
	}
}

func TestParseRecordingsAcceptsStringSizeAndTopLevelArray(t *testing.T) {
	items, err := parseRecordings([]byte(`[{"startTime":"2026-09-11T08:59:33Z","endTime":"2026-09-11T09:00:03Z","size":"1024","name":"clip.mp4","recordType":"people,dog_cat","type":"sub"}]`), "camera", domain.StreamMain, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SizeBytes != 1024 || items[0].Stream != domain.StreamSub {
		t.Fatalf("unexpected recordings: %#v", items)
	}
	expectedTypes := []domain.EventType{domain.EventPerson, domain.EventAnimal}
	if !slices.Equal(items[0].EventTypes, expectedTypes) {
		t.Fatalf("unexpected event types: %#v", items[0].EventTypes)
	}
}

func TestParseRecordingsConvertsCameraLocalTimeToUTC(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	items, err := parseRecordings([]byte(`[{"startTime":"2026-09-11T08:00:00","endTime":"2026-09-11T08:01:00","size":1024,"name":"clip.mp4"}]`), "camera", domain.StreamMain, location)
	if err != nil {
		t.Fatal(err)
	}
	expected := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	if len(items) != 1 || !items[0].StartAtUtc.Equal(expected) {
		t.Fatalf("unexpected UTC start: %v", items)
	}
}
