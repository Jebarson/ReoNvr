package httpapi

import (
	"context"
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
)

type rejectingCameraService struct {
	err error
}

type fixtureEventService struct {
	events []domain.Event
}

type fixturePlaybackService struct {
	recording domain.Recording
	filePath  string
}

type fixtureRecordingControl struct {
	state domain.RecordingControl
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
	const password = "camera-secret-value"
	handler := New(
		rejectingCameraService{err: errors.New("camera command failed: password " + password)},
		nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
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
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(`{"name":"Front","host":"10.0.0.152","port":9000,"channel":0,"username":"admin","password":"`+password+`","enabled":true}`))
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

func TestListEventsReturnsDurableEventMetadata(t *testing.T) {
	started := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	handler := New(
		rejectingCameraService{}, nil,
		fixtureEventService{events: []domain.Event{{ID: "event-one", CameraID: "camera-one", EventType: domain.EventPerson, StartedAtUtc: started, CreatedAtUtc: started}}},
		nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
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
	handler := New(rejectingCameraService{}, nil, nil, playback, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
	handler := New(rejectingCameraService{}, nil, nil, playback, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodGet, "/api/recordings/recording-one/media", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("unexpected extensionless MP4 response %d %q", response.Code, response.Header().Get("Content-Type"))
	}
}

func TestRecordingControlCanBeReadAndPaused(t *testing.T) {
	control := &fixtureRecordingControl{state: domain.RecordingControl{Enabled: true, UpdatedAtUtc: time.Now().UTC()}}
	handler := New(rejectingCameraService{}, nil, nil, nil, nil, control, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
