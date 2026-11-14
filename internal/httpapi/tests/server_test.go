package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reonavr/internal/camera"
	"reonavr/internal/domain"
	"reonavr/internal/reolink"

	"reonavr/internal/testconfig"
)

type rejectingCameraService struct {
	err error
}

type fixtureDiscoveryService struct {
	rejectingCameraService
	devices      []camera.DiscoveredCamera
	discoveryErr error
}

func (service fixtureDiscoveryService) Discover(context.Context) ([]camera.DiscoveredCamera, error) {
	return service.devices, service.discoveryErr
}

func TestCameraDiscoveryReturnsDevicesAndSafeFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		devices []camera.DiscoveredCamera
		status  int
		body    string
	}{
		{name: "found", devices: []camera.DiscoveredCamera{{DiscoveredCamera: reolink.DiscoveredCamera{Host: testconfig.Host(2), Port: 9000, Name: "Front"}, Registered: true}}, status: http.StatusOK, body: `"registered":true`},
		{name: "empty", status: http.StatusOK, body: `[]`},
		{name: "busy", err: camera.ErrDiscoveryBusy, status: http.StatusConflict, body: `"code":"DiscoveryBusy"`},
		{name: "unavailable", err: camera.ErrDiscoveryUnavailable, status: http.StatusNotImplemented, body: `"code":"NotAvailable"`},
		{name: "failed", err: errors.New("private-diagnostic"), status: http.StatusBadGateway, body: `"code":"DiscoveryFailed"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := New(fixtureDiscoveryService{devices: test.devices, discoveryErr: test.err}, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/cameras/discover", nil))
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.body) || strings.Contains(response.Body.String(), "private-diagnostic") {
				t.Fatalf("unexpected discovery response: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

type fixtureEventService struct {
	events []domain.Event
}

type fixtureRecoveryService struct{ gaps []domain.RecoveryGap }

func (service fixtureRecoveryService) ListRecordings(context.Context, string, int) ([]domain.Recording, error) {
	return nil, nil
}
func (service fixtureRecoveryService) ListRecoveryGaps(context.Context, string, int) ([]domain.RecoveryGap, error) {
	return service.gaps, nil
}

func TestRecoveryHistoryShowsPartialCoverageAndChecksCamera(t *testing.T) {
	for _, missingCamera := range []bool{false, true} {
		var cameraErr error
		if missingCamera {
			cameraErr = camera.ErrNotFound
		}
		handler := New(rejectingCameraService{err: cameraErr}, fixtureRecoveryService{gaps: []domain.RecoveryGap{{ID: "gap-one", Status: "Partial", MissingSeconds: 30}}}, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/cameras/camera-one/recovery", nil))
		if missingCamera {
			if response.Code != http.StatusNotFound {
				t.Fatalf("unexpected status: %d", response.Code)
			}
		} else if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"Partial"`) || !strings.Contains(response.Body.String(), `"missingSeconds":30`) {
			t.Fatalf("unexpected recovery response: %d %s", response.Code, response.Body.String())
		}
	}
}

type fixturePlaybackService struct {
	recording domain.Recording
	filePath  string
}

type fixtureRecordingControl struct {
	state domain.RecordingControl
}

type fixtureApplicationSettings struct {
	settings domain.ApplicationSettings
}

func (service *fixtureApplicationSettings) Get(context.Context) (domain.ApplicationSettings, error) {
	return service.settings, nil
}

func (service *fixtureApplicationSettings) Update(_ context.Context, recordingFolder string, archiveLayout domain.ArchiveLayout) (domain.ApplicationSettings, error) {
	service.settings.RecordingFolder = recordingFolder
	service.settings.ArchiveLayout = archiveLayout
	return service.settings, nil
}

func (control *fixtureRecordingControl) Get(context.Context) (domain.RecordingControl, error) {
	return control.state, nil
}

func (control *fixtureRecordingControl) Update(_ context.Context, enabled bool) (domain.RecordingControl, error) {
	control.state.Enabled = enabled
	control.state.UpdatedAtUtc = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	return control.state, nil
}

func (service fixturePlaybackService) GetRecording(context.Context, string) (domain.Recording, error) {
	return service.recording, nil
}

func (service fixturePlaybackService) OpenRecording(context.Context, string) (domain.Recording, *os.File, error) {
	file, err := os.Open(service.filePath)
	return service.recording, file, err
}

func (service fixtureEventService) ListEvents(context.Context, string, int) ([]domain.Event, error) {
	return service.events, nil
}

func (service rejectingCameraService) List(context.Context) ([]domain.Camera, error) {
	return nil, service.err
}

func (service rejectingCameraService) Get(context.Context, string) (domain.Camera, error) {
	return domain.Camera{}, service.err
}

func (service rejectingCameraService) Test(context.Context, string, camera.Draft) (camera.TestResponse, error) {
	return camera.TestResponse{}, service.err
}

func (service rejectingCameraService) Create(context.Context, camera.Draft) (domain.Camera, error) {
	return domain.Camera{}, service.err
}

func (service rejectingCameraService) Update(context.Context, string, camera.Draft) (domain.Camera, error) {
	return domain.Camera{}, service.err
}

func (service rejectingCameraService) Delete(context.Context, string) error {
	return service.err
}

func TestCameraWriteResponsesNeverExposePassword(t *testing.T) {
	password := testconfig.Password()
	handler := New(
		rejectingCameraService{err: errors.New("camera command failed: password " + password)},
		nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	tests := []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/api/cameras/test"},
		{method: http.MethodPost, path: "/api/cameras"},
		{method: http.MethodPut, path: "/api/cameras/camera-1"},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(testconfig.Expand("{\"name\":\"Front\",\"host\":\"{{host2}}\",\"port\":9000,\"channel\":0,\"username\":\"{{username}}\",\"password\":\"")+password+`","enabled":true}`))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			body := response.Body.String()
			if response.Code != http.StatusBadGateway {
				t.Fatalf("unexpected status %d: %s", response.Code, body)
			}
			if strings.Contains(body, password) || strings.Contains(strings.ToLower(body), `"password"`) {
				t.Fatalf("response exposed password data: %s", body)
			}
			if !strings.Contains(body, "Baichuan port") {
				t.Fatalf("response omitted port guidance: %s", body)
			}
		})
	}
}

func TestCameraTestFailuresGiveSafeGuidance(test *testing.T) {
	password := testconfig.Password()
	const privateDiagnostic = "private-diagnostic"
	for _, scenario := range []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{
			name: "invalid connection", err: errors.Join(errors.New(privateDiagnostic+password), reolink.ErrInvalidConnection),
			status: http.StatusBadRequest, code: "InvalidCameraConnection", message: "private camera address",
		},
		{
			name: "timezone mismatch", err: errors.Join(errors.New(privateDiagnostic+password), &reolink.TimezoneMismatchError{Configuration: reolink.TimezoneConfiguration{BaseOffsetSeconds: -28800, DstEnabled: true}}),
			status: http.StatusUnprocessableEntity, code: "CameraTimezoneUnsupported", message: "No camera settings were changed",
		},
		{
			name: "timeout", err: errors.New("camera command timed out: " + privateDiagnostic + password),
			status: http.StatusGatewayTimeout, code: "CameraTestTimeout", message: "awake and reachable",
		},
		{
			name: "unreadable clock", err: errors.New("camera returned malformed daylight-saving data: " + privateDiagnostic + password),
			status: http.StatusBadGateway, code: "CameraClockUnavailable", message: "Review its time settings",
		},
		{
			name: "connection", err: errors.New("camera command failed: " + privateDiagnostic + password),
			status: http.StatusBadGateway, code: "CameraConnectionFailed", message: "Baichuan port",
		},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			var logs strings.Builder
			handler := New(rejectingCameraService{err: scenario.err}, nil, nil, nil, nil, nil, nil, nil, slog.New(slog.NewJSONHandler(&logs, nil)))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/cameras/test", strings.NewReader(testconfig.Expand("{\"host\":\"{{host1}}\",\"password\":\"")+password+`"}`)))
			var payload struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				test.Fatal(err)
			}
			if response.Code != scenario.status || payload.Code != scenario.code || !strings.Contains(payload.Message, scenario.message) {
				test.Fatalf("unexpected failure guidance: %d %s", response.Code, response.Body.String())
			}
			for _, output := range []string{response.Body.String(), logs.String()} {
				if strings.Contains(output, password) || strings.Contains(output, privateDiagnostic) {
					test.Fatal("response or logs exposed credentials or raw diagnostics")
				}
			}
			if scenario.code == "CameraTimezoneUnsupported" && !strings.Contains(logs.String(), `"baseOffsetSeconds":-28800`) {
				test.Fatal("timezone diagnostics omitted the safe parsed clock configuration")
			}
		})
	}
}

func TestListEventsReturnsDurableEventMetadata(t *testing.T) {
	started := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	handler := New(
		rejectingCameraService{}, nil,
		fixtureEventService{events: []domain.Event{{ID: "event-one", CameraID: "camera-one", EventType: domain.EventPerson, StartedAtUtc: started, CreatedAtUtc: started}}},
		nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	request := httptest.NewRequest(http.MethodGet, "/api/cameras/camera-one/events", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"eventType":"Person"`) {
		t.Fatalf("unexpected event response %d: %s", response.Code, response.Body.String())
	}
}

func TestRecordingMediaSupportsByteRangesAndOriginalDownload(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "recording-*.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("0123456789"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	playback := fixturePlaybackService{
		recording: domain.Recording{ID: "recording-one", LocalPath: "camera-one/clip.mkv", State: domain.RecordingComplete},
		filePath:  file.Name(),
	}
	handler := New(rejectingCameraService{}, nil, nil, playback, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rangeRequest := httptest.NewRequest(http.MethodGet, "/api/recordings/recording-one/media", nil)
	rangeRequest.Header.Set("Range", "bytes=2-5")
	rangeResponse := httptest.NewRecorder()
	handler.ServeHTTP(rangeResponse, rangeRequest)
	if rangeResponse.Code != http.StatusPartialContent || rangeResponse.Body.String() != "2345" {
		t.Fatalf("unexpected range response %d: %q", rangeResponse.Code, rangeResponse.Body.String())
	}

	downloadRequest := httptest.NewRequest(http.MethodGet, "/api/recordings/recording-one/original", nil)
	downloadResponse := httptest.NewRecorder()
	handler.ServeHTTP(downloadResponse, downloadRequest)
	if downloadResponse.Code != http.StatusOK || !strings.HasPrefix(downloadResponse.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("unexpected original response %d: %s", downloadResponse.Code, downloadResponse.Header())
	}
}

func TestRecordingMediaDetectsExtensionlessMp4(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "0120260911194624")
	payload := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', '4', 0, 0, 0, 1, 'i', 's', 'o', '4', 'h', 'v', 'c', '1'}
	if err := os.WriteFile(filePath, payload, 0o640); err != nil {
		t.Fatal(err)
	}
	playback := fixturePlaybackService{
		recording: domain.Recording{ID: "recording-one", LocalPath: "camera-one/0120260911194624", State: domain.RecordingComplete},
		filePath:  filePath,
	}
	handler := New(rejectingCameraService{}, nil, nil, playback, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodGet, "/api/recordings/recording-one/media", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("unexpected extensionless MP4 response %d %q", response.Code, response.Header().Get("Content-Type"))
	}
	downloadRequest := httptest.NewRequest(http.MethodGet, "/api/recordings/recording-one/original", nil)
	downloadResponse := httptest.NewRecorder()
	handler.ServeHTTP(downloadResponse, downloadRequest)
	if disposition := downloadResponse.Header().Get("Content-Disposition"); !strings.Contains(disposition, `filename=0120260911194624.mp4`) {
		t.Fatalf("unexpected extensionless MP4 download disposition %q", disposition)
	}
}

func TestRecordingControlCanBeReadAndPaused(t *testing.T) {
	control := &fixtureRecordingControl{state: domain.RecordingControl{Enabled: true, UpdatedAtUtc: time.Now().UTC()}}
	handler := New(rejectingCameraService{}, nil, nil, nil, nil, control, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	getRequest := httptest.NewRequest(http.MethodGet, "/api/system/recording", nil)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK || !strings.Contains(getResponse.Body.String(), `"enabled":true`) {
		t.Fatalf("unexpected recording control response %d: %s", getResponse.Code, getResponse.Body.String())
	}

	putRequest := httptest.NewRequest(http.MethodPut, "/api/system/recording", strings.NewReader(`{"enabled":false}`))
	putResponse := httptest.NewRecorder()
	handler.ServeHTTP(putResponse, putRequest)
	if putResponse.Code != http.StatusOK || !strings.Contains(putResponse.Body.String(), `"enabled":false`) || control.state.Enabled {
		t.Fatalf("unexpected recording update response %d: %s", putResponse.Code, putResponse.Body.String())
	}
}

func TestApplicationSettingsCanBeReadAndUpdated(t *testing.T) {
	settings := &fixtureApplicationSettings{settings: domain.ApplicationSettings{
		ArchiveRoot: "/archive", RecordingFolder: ".", ArchiveLayout: domain.ArchiveLayoutCameraDate,
	}}
	handler := New(rejectingCameraService{}, nil, nil, nil, nil, nil, settings, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	getRequest := httptest.NewRequest(http.MethodGet, "/api/system/settings", nil)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK || !strings.Contains(getResponse.Body.String(), `"archiveRoot":"/archive"`) {
		t.Fatalf("unexpected application settings response %d: %s", getResponse.Code, getResponse.Body.String())
	}

	putRequest := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"recordingFolder":"recordings","archiveLayout":"DateCamera"}`))
	putResponse := httptest.NewRecorder()
	handler.ServeHTTP(putResponse, putRequest)
	if putResponse.Code != http.StatusOK || settings.settings.RecordingFolder != "recordings" || settings.settings.ArchiveLayout != domain.ArchiveLayoutDateCamera {
		t.Fatalf("unexpected application settings update %d: %s", putResponse.Code, putResponse.Body.String())
	}
}
