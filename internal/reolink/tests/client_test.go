package reolink

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"reonavr/internal/domain"

	"reonavr/internal/testconfig"
)

type fixtureRunner struct {
	responses      map[string]string
	snapshot       []byte
	calls          [][]string
	passwords      []string
	blockedCommand string
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

func (runner *fixtureRunner) Run(ctx context.Context, password string, arguments ...string) ([]byte, error) {
	runner.calls = append(runner.calls, slices.Clone(arguments))
	runner.passwords = append(runner.passwords, password)
	if runner.blockedCommand != "" && strings.HasSuffix(strings.Join(arguments, " "), runner.blockedCommand) {
		<-ctx.Done()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
		if key == "vod search" && slices.Contains(arguments, "vod") && slices.Contains(arguments, "search") {
			return []byte(response), nil
		}
		if strings.HasSuffix(strings.Join(arguments, " "), key) {
			return []byte(response), nil
		}
	}
	return nil, context.DeadlineExceeded
}

func TestClientDiscoveryIsBoundedAndDoesNotUseCredentials(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		wantErr bool
	}{
		{name: "empty scan", payload: `{"ok":true,"data":{"count":0,"devices":[]}}`},
		{name: "missing devices", payload: `{"ok":true,"data":{}}`, wantErr: true},
		{name: "invalid response", payload: `not-json`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fixtureRunner{responses: map[string]string{"discover --timeout-secs 3 --output json": test.payload}}
			devices, err := NewClient(runner, time.Second).Discover(context.Background())
			if (err != nil) != test.wantErr {
				t.Fatalf("unexpected result: devices=%#v err=%v", devices, err)
			}
			if len(runner.calls) != 1 || runner.passwords[0] != "" || slices.Contains(runner.calls[0], "--host") {
				t.Fatalf("discovery used camera credentials or registration: %#v", runner.calls)
			}
		})
	}
}

func TestClientDiscoveryNormalizesNativeReplyAndRejectsUnsafeEndpoints(t *testing.T) {
	runner := &fixtureRunner{responses: map[string]string{"discover --timeout-secs 3 --output json": testconfig.Expand("{\"ok\":true,\"data\":{\"devices\":[\n\t\t{\"host\":\"{{host2}}:9000\",\"transport\":\"tcp\",\"uid\":\"private-identifier\",\"mac\":\"private-mac\",\"scopes\":[\"reolink-lan://name/Front%20Door\"]},\n\t\t{\"host\":\"{{host2}}:9000\",\"transport\":\"tcp\"},\n\t\t{\"host\":\"{{host3}}:9001\",\"transport\":\"tcp\"},\n\t\t{\"host\":\"8.8.8.8:9000\",\"transport\":\"tcp\"},\n\t\t{\"host\":\"camera.example:9000\",\"transport\":\"tcp\"},\n\t\t{\"host\":\"{{host4}}:0\",\"transport\":\"tcp\"},\n\t\t{\"host\":\"{{host5}}:9000\",\"transport\":\"udp\"},\n\t\t{\"host\":\"invalid\",\"transport\":\"tcp\"}\n\t]}}")}}
	devices, err := NewClient(runner, time.Second).Discover(context.Background())
	if err != nil || len(devices) != 2 || devices[0].Host != testconfig.Host(2) || devices[0].Port != 9000 || devices[0].Name != "Front Door" || devices[1].Port != 9001 {
		t.Fatalf("unexpected normalized discovery: %#v %v", devices, err)
	}
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
		"vod search":                           `{"data":{"items":[]}}`,
		"capabilities":                         `{"data":{"item":{"motion":"1","aitype":"1847"}}}`,
		"stream url --kind rtsp --stream main": testconfig.Expand("{\"data\":{\"urls\":[\"rtsp://{{host2}}/Preview_01_main\"]}}"),
	}}
	client := NewClient(runner, time.Second)
	client.captureSnapshot = true
	result, err := client.Test(context.Background(), Connection{Host: testconfig.Host(2), Port: 9000, Timezone: "America/Los_Angeles", Username: testconfig.Username(), Password: testconfig.Password()})
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "RLC-810A" || result.Timezone != "America/Los_Angeles" || result.TimezoneOffsetSeconds != -28800 || result.Capabilities.PreRecordSeconds != 10 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if !result.TimezoneConfiguration.DstEnabled || result.TimezoneConfiguration.DstStart.Month != 3 || result.TimezoneConfiguration.DstEnd.Month != 11 {
		t.Fatalf("unexpected timezone configuration: %#v", result.TimezoneConfiguration)
	}
	if !result.Capabilities.StorageAvailable || !result.Capabilities.StoragePlaybackAvailable || !result.Capabilities.RtspAvailable || result.Capabilities.ContinuousAvailable {
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
		if slices.Contains(call, testconfig.Password()) {
			t.Fatalf("password present in command arguments: %#v", call)
		}
		if !slices.Contains(call, "--password-stdin") || runner.passwords[index] != testconfig.Password() {
			t.Fatalf("password was not sent through stdin: %#v", call)
		}
	}
}

func TestConnectionTestKeepsStreamsAvailableWhenClockIsUnverified(test *testing.T) {
	runner := &fixtureRunner{responses: map[string]string{
		"info":                                 `{"data":{"model":"Doorbell","firmware":"test"}}`,
		"config get time-zone":                 `{"data":{"timeZone":28800}}`,
		"raw 106":                              `{"data":{}}`,
		"storage status":                       `{"data":{"items":[{"mounted":true,"formatted":true,"remainGB":1,"totalGB":32}]}}`,
		"vod search":                           `{"data":{"items":[]}}`,
		"stream url --kind rtsp --stream main": testconfig.Expand("{\"data\":{\"urls\":[\"rtsp://{{host1}}/Preview_01_main\"]}}"),
	}}
	result, err := NewClient(runner, time.Second).Test(context.Background(), Connection{Host: testconfig.Host(1), Port: 9000, Username: testconfig.Username(), Password: testconfig.Password()})
	if err != nil || !result.Capabilities.RtspAvailable || result.Capabilities.ClockWarning == "" {
		test.Fatalf("unverified clock blocked a valid connection or omitted its warning: %#v %v", result, err)
	}
	location, err := cameraLocation(result.Timezone)
	if err != nil || zoneOffset(location, time.Now()) != -28800 {
		test.Fatalf("camera base offset was not retained for best-effort searches: %v", err)
	}
	if !result.Capabilities.StoragePlaybackAvailable {
		test.Fatal("missing daylight-saving rules blocked usable camera storage")
	}
	searched := false
	for _, call := range runner.calls {
		if slices.Contains(call, "vod") {
			searched = true
		}
	}
	if !searched {
		test.Fatal("camera storage was not probed with the best-effort clock")
	}
}

func TestConnectionTestSlowCurrentTimePreservesStorageAndSnapshot(test *testing.T) {
	for _, fixture := range []struct {
		name string
		dst  string
	}{
		{name: "verified IANA clock", dst: `{"data":{"Dst":{"enable":0}}}`},
		{name: "estimated clock", dst: `{"data":{}}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			snapshot := []byte{0xff, 0xd8, 0x01, 0x02, 0xff, 0xd9}
			runner := &fixtureRunner{blockedCommand: "config get system-general", snapshot: snapshot, responses: map[string]string{
				"info":                 `{"data":{"model":"Doorbell","firmware":"test"}}`,
				"config get time-zone": `{"data":{"timeZone":28800}}`,
				"raw 106":              fixture.dst,
				"storage status":       `{"data":{"items":[{"mounted":true,"formatted":true,"remainGB":1,"totalGB":32}]}}`,
				"vod search":           `{"data":{"items":[]}}`,
			}}
			client := NewClient(runner, time.Second)
			client.captureSnapshot = true
			result, err := client.Test(context.Background(), Connection{Host: testconfig.Host(1), Port: 9000, Timezone: "Etc/GMT+8", Username: testconfig.Username(), Password: testconfig.Password()})
			if err != nil || !result.Capabilities.StoragePlaybackAvailable || !slices.Equal(result.SnapshotJPEG, snapshot) {
				test.Fatalf("optional clock query consumed the storage/snapshot budget: storage=%t snapshot=%t err=%v", result.Capabilities.StoragePlaybackAvailable, slices.Equal(result.SnapshotJPEG, snapshot), err)
			}
			if fixture.name == "estimated clock" && result.Capabilities.ClockWarning == "" {
				test.Fatal("estimated clock warning was lost after the optional query timed out")
			}
		})
	}
}

func TestConnectionTestContinuesWhenAllClockQueriesAreUnavailable(test *testing.T) {
	runner := &fixtureRunner{responses: map[string]string{
		"info":           `{"data":{"model":"Doorbell","firmware":"test"}}`,
		"storage status": `{"data":{"items":[{"mounted":true,"formatted":true,"remainGB":1,"totalGB":32}]}}`,
		"vod search":     `{"data":{"items":[]}}`,
	}}
	result, err := NewClient(runner, time.Second).Test(context.Background(), Connection{Host: testconfig.Host(1), Port: 9000, Username: testconfig.Username(), Password: testconfig.Password()})
	if err != nil || result.Capabilities.ClockWarning == "" || !result.Capabilities.StoragePlaybackAvailable {
		test.Fatalf("unavailable clock queries blocked best-effort storage: %#v %v", result, err)
	}
	if _, err := cameraLocation(result.Timezone); err != nil {
		test.Fatalf("clock fallback is unusable: %v", err)
	}
}

func TestApproximateCameraClockUsesObservedOffsetAndKeepsFallback(test *testing.T) {
	started := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	for _, fixture := range []struct {
		name    string
		payload string
		offset  int
	}{
		{name: "active daylight saving", payload: `{"data":{"Time":{"year":2026,"month":7,"day":15,"hour":5,"minute":0,"second":0}}}`, offset: -7 * 3600},
		{name: "quarter hour zone", payload: `{"data":{"Time":{"year":2026,"month":7,"day":15,"hour":17,"minute":45,"second":20}}}`, offset: 20700},
		{name: "malformed time", payload: `not-json`, offset: -8 * 3600},
		{name: "missing time", payload: `{"data":{}}`, offset: -8 * 3600},
		{name: "invalid calendar", payload: `{"year":2026,"month":2,"day":30,"hour":5,"minute":0,"second":0}`, offset: -8 * 3600},
		{name: "large clock skew", payload: `{"year":2025,"month":7,"day":15,"hour":5,"minute":0,"second":0}`, offset: -8 * 3600},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			configuration := approximateCameraClock([]byte(fixture.payload), TimezoneConfiguration{BaseOffsetSeconds: -8 * 3600}, started, started.Add(time.Second))
			if configuration.BaseOffsetSeconds != fixture.offset || configuration.DstEnabled {
				test.Fatalf("unexpected estimated clock: %#v", configuration)
			}
		})
	}
}

func TestCustomCameraClockPreservesRulesAcrossSeasons(test *testing.T) {
	configuration := TimezoneConfiguration{
		BaseOffsetSeconds: -8 * 3600, DstEnabled: true, DstOffsetSeconds: 3600,
		DstStart: DstTransition{Month: 3, Week: 2, Weekday: 0, Hour: 1},
		DstEnd:   DstTransition{Month: 11, Week: 1, Weekday: 0, Hour: 3},
	}
	name, err := customCameraTimezone(configuration)
	if err != nil {
		test.Fatal(err)
	}
	location, err := cameraLocation(name)
	if err != nil {
		test.Fatal(err)
	}
	for _, sample := range []struct {
		instant string
		offset  int
	}{
		{"2026-01-15T12:00:00Z", -8 * 3600},
		{"2026-07-15T12:00:00Z", -7 * 3600},
		{"2026-03-08T08:59:59Z", -8 * 3600},
		{"2026-03-08T09:00:00Z", -7 * 3600},
		{"2026-11-01T09:59:59Z", -7 * 3600},
		{"2026-11-01T10:00:00Z", -8 * 3600},
	} {
		instant, err := time.Parse(time.RFC3339, sample.instant)
		if err != nil || zoneOffset(location, instant) != sample.offset {
			test.Errorf("custom offset at %s: got %d, want %d (%v)", sample.instant, zoneOffset(location, instant), sample.offset, err)
		}
	}
	local, err := time.ParseInLocation("2006-01-02T15:04:05", "2026-07-15T05:00:00", location)
	if err != nil || local.UTC().Format(time.RFC3339) != "2026-07-15T12:00:00Z" {
		test.Fatalf("custom recording timestamp conversion failed: %v %v", local, err)
	}
	from := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	if validateCameraSearchClock(name, from, from.Add(time.Minute)) == nil {
		test.Fatal("ambiguous repeated local timestamps must not be guessed")
	}
	if _, err := cameraLocation("Camera/Unverified"); err == nil {
		test.Fatal("unverified clock must not silently become UTC")
	}
}

func TestCustomCameraClockHandlesSouthernAndFixedOffsets(test *testing.T) {
	for _, fixture := range []struct {
		name          string
		configuration TimezoneConfiguration
		samples       map[string]int
	}{
		{
			name: "southern daylight saving",
			configuration: TimezoneConfiguration{
				BaseOffsetSeconds: 37800, DstEnabled: true, DstOffsetSeconds: 3600,
				DstStart: DstTransition{Month: 10, Week: 1, Weekday: 0, Hour: 1},
				DstEnd:   DstTransition{Month: 4, Week: 1, Weekday: 0, Hour: 3},
			},
			samples: map[string]int{
				"2026-01-15T12:00:00Z": 41400,
				"2026-07-15T12:00:00Z": 37800,
				"2026-04-04T15:29:59Z": 41400,
				"2026-04-04T15:30:00Z": 37800,
				"2026-10-03T14:29:59Z": 37800,
				"2026-10-03T14:30:00Z": 41400,
			},
		},
		{
			name:          "nonstandard fixed offset",
			configuration: TimezoneConfiguration{BaseOffsetSeconds: 12345},
			samples: map[string]int{
				"2026-01-15T12:00:00Z": 12345,
				"2026-07-15T12:00:00Z": 12345,
			},
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			name, err := customCameraTimezone(fixture.configuration)
			if err != nil {
				test.Fatal(err)
			}
			location, err := cameraLocation(name)
			if err != nil {
				test.Fatal(err)
			}
			for sample, expected := range fixture.samples {
				instant, err := time.Parse(time.RFC3339, sample)
				if err != nil || zoneOffset(location, instant) != expected {
					test.Errorf("custom offset at %s: got %d, want %d (%v)", sample, zoneOffset(location, instant), expected, err)
				}
			}
		})
	}
}

func TestConnectionTestAcceptsCustomClockAndStorageSearch(test *testing.T) {
	runner := &fixtureRunner{responses: map[string]string{
		"info":                                 `{"data":{"model":"Doorbell","firmware":"test"}}`,
		"config get time-zone":                 `{"data":{"timeZone":28800}}`,
		"raw 106":                              `{"data":{"Dst":{"enable":1,"offset":1,"startMon":3,"startWeek":2,"startWeekday":0,"startHour":1,"startMin":0,"startSec":0,"endMon":11,"endWeek":1,"endWeekday":0,"endHour":3,"endMin":0,"endSec":0}}}`,
		"storage status":                       `{"data":{"items":[{"mounted":true,"formatted":true,"remainGB":1,"totalGB":32}]}}`,
		"stream url --kind rtsp --stream main": testconfig.Expand("{\"data\":{\"urls\":[\"rtsp://{{host1}}/Preview_01_main\"]}}"),
		"vod search":                           `{"data":{"items":[]}}`,
	}}
	result, err := NewClient(runner, time.Second).Test(context.Background(), Connection{Host: testconfig.Host(1), Port: 9000, Username: testconfig.Username(), Password: testconfig.Password()})
	if err != nil || !result.Capabilities.RtspAvailable || result.Capabilities.ClockWarning != "" || !strings.HasPrefix(result.Timezone, cameraClockPrefix) {
		test.Fatalf("custom clock blocked the connection: %#v %v", result, err)
	}
	configuration, err := decodeCameraClock(result.Timezone)
	if err != nil || configuration.DstStart.Hour != 1 || configuration.DstEnd.Hour != 3 {
		test.Fatalf("custom clock rules were not preserved: %#v %v", configuration, err)
	}
	client := NewClient(runner, time.Second)
	from := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	_, err = client.SearchRecordings(context.Background(), Connection{Host: testconfig.Host(1), Port: 9000, Username: testconfig.Username(), Password: testconfig.Password(), Timezone: result.Timezone}, domain.StreamMain, from, from.Add(time.Minute))
	if err != nil {
		test.Fatal(err)
	}
	call := runner.calls[len(runner.calls)-1]
	if !slices.Contains(call, "2026-07-15T05:00:00") || !slices.Contains(call, "2026-07-15T05:01:00") {
		test.Fatalf("search did not use the reported daylight-saving offset: %v", call)
	}
}

func TestCustomClockWarningRequiresCurrentTimeMismatch(test *testing.T) {
	for _, fixture := range []struct {
		name    string
		skew    time.Duration
		missing bool
		warning bool
	}{
		{name: "matching time"},
		{name: "small clock drift", skew: 30 * time.Second},
		{name: "camera ahead", skew: 3 * time.Minute, warning: true},
		{name: "camera behind", skew: -3 * time.Minute, warning: true},
		{name: "unreadable time", missing: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			configuration := TimezoneConfiguration{BaseOffsetSeconds: -8 * 3600, DstEnabled: true, DstOffsetSeconds: 3600,
				DstStart: DstTransition{Month: 3, Week: 2, Weekday: 0, Hour: 1}, DstEnd: DstTransition{Month: 11, Week: 1, Weekday: 0, Hour: 3}}
			name, err := customCameraTimezone(configuration)
			if err != nil {
				test.Fatal(err)
			}
			location, err := cameraLocation(name)
			if err != nil {
				test.Fatal(err)
			}
			local := time.Now().Add(fixture.skew).In(location)
			payload, err := json.Marshal(map[string]any{"data": map[string]any{"Time": map[string]int{
				"year": local.Year(), "month": int(local.Month()), "day": local.Day(), "hour": local.Hour(), "minute": local.Minute(), "second": local.Second(),
			}}})
			if err != nil {
				test.Fatal(err)
			}
			if fixture.missing {
				payload = []byte(`{"data":{}}`)
			}
			runner := &fixtureRunner{responses: map[string]string{
				"info":                                 `{"data":{"model":"Doorbell","firmware":"test"}}`,
				"config get time-zone":                 `{"data":{"timeZone":28800}}`,
				"raw 106":                              `{"data":{"Dst":{"enable":1,"offset":1,"startMon":3,"startWeek":2,"startWeekday":0,"startHour":1,"startMin":0,"startSec":0,"endMon":11,"endWeek":1,"endWeekday":0,"endHour":3,"endMin":0,"endSec":0}}}`,
				"config get system-general":            string(payload),
				"stream url --kind rtsp --stream main": testconfig.Expand("{\"data\":{\"urls\":[\"rtsp://{{host1}}/Preview_01_main\"]}}"),
			}}
			result, err := NewClient(runner, time.Second).Test(context.Background(), Connection{Host: testconfig.Host(1), Port: 9000, Username: testconfig.Username(), Password: testconfig.Password()})
			if err != nil || !result.Capabilities.RtspAvailable {
				test.Fatalf("clock comparison blocked the connection: %v", err)
			}
			expected := ""
			if fixture.warning {
				expected = "Your camera's time doesn't match your machine. ReoNvr will use the camera's clock."
			}
			if result.Capabilities.ClockWarning != expected {
				test.Fatalf("unexpected clock warning: got %q, want %q", result.Capabilities.ClockWarning, expected)
			}
		})
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

func TestTimezoneMismatchRetainsOnlyParsedClockConfiguration(test *testing.T) {
	configuration := TimezoneConfiguration{BaseOffsetSeconds: 12345}
	_, err := resolveCameraTimezone(configuration, "UTC", time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC))
	var mismatch *TimezoneMismatchError
	if !errors.As(err, &mismatch) || mismatch.Configuration != configuration {
		test.Fatalf("expected a typed clock mismatch with its parsed configuration: %v", err)
	}
}

func TestClientRequiresPrivateAddress(t *testing.T) {
	client := NewClient(&fixtureRunner{}, time.Second)
	_, err := client.Test(context.Background(), Connection{Host: "8.8.8.8", Port: 9000, Username: testconfig.Username(), Password: testconfig.Password()})
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
	message := safeCommandError(testconfig.Expand("login failed for password {{password}}"), testconfig.Password())
	if strings.Contains(message, testconfig.Password()) || !strings.Contains(message, "[redacted]") {
		t.Fatalf("password was not redacted: %q", message)
	}
}

func TestClientGetsRtspURLWithoutReturningEmbeddedCredentials(t *testing.T) {
	runner := &fixtureRunner{responses: map[string]string{
		"stream url --kind rtsp --stream sub": testconfig.Expand("{\"data\":{\"urls\":[\"rtsp://{{username}}:{{password}}@{{host2}}:554/Preview_01_sub\"]}}"),
	}}
	client := NewClient(runner, time.Second)
	streamURL, err := client.RtspURL(context.Background(), Connection{
		Host: testconfig.Host(2), Port: 9000, Username: testconfig.Username(), Password: testconfig.Password(),
	}, domain.StreamSub)
	if err != nil {
		t.Fatal(err)
	}
	if streamURL != testconfig.Expand("rtsp://{{host2}}:554/Preview_01_sub") {
		t.Fatalf("unexpected RTSP URL: %q", streamURL)
	}
	if len(runner.calls) != 1 || slices.Contains(runner.calls[0], testconfig.Password()) || runner.passwords[0] != testconfig.Password() {
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
		Host: testconfig.Host(2), Port: 9000, Username: testconfig.Username(), Password: testconfig.Password(),
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
	if len(runner.calls) != 1 || slices.Contains(runner.calls[0], testconfig.Password()) || runner.passwords[0] != testconfig.Password() {
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
		Host: testconfig.Host(2), Port: 9000, Username: testconfig.Username(), Password: testconfig.Password(),
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
	if slices.Contains(runner.calls[0], testconfig.Password()) || runner.passwords[0] != testconfig.Password() {
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
		Host: testconfig.Host(2), Port: 9000, Username: testconfig.Username(), Password: testconfig.Password(),
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
	if slices.Contains(runner.calls[0], testconfig.Password()) || runner.passwords[0] != testconfig.Password() {
		t.Fatal("search did not keep the password on stdin")
	}
}

func TestClientDownloadsNativeRecordingWithoutPasswordArgument(t *testing.T) {
	runner := &fixtureRunner{responses: map[string]string{
		"vod download clip.mp4 --file /archive/clip.mp4.partial --stream main": `{}`,
	}}
	client := NewClient(runner, time.Second)
	err := client.DownloadRecording(context.Background(), Connection{
		Host: testconfig.Host(2), Port: 9000, Username: testconfig.Username(), Password: testconfig.Password(),
	}, "clip.mp4", domain.StreamMain, "/archive/clip.mp4.partial")
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || slices.Contains(runner.calls[0], testconfig.Password()) || runner.passwords[0] != testconfig.Password() {
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
