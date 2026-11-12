package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"reonavr/internal/archive"
	"reonavr/internal/camera"
	"reonavr/internal/control"
	"reonavr/internal/domain"
	"reonavr/internal/reolink"
)

type CameraService interface {
	List(ctx context.Context) ([]domain.Camera, error)
	Get(ctx context.Context, id string) (domain.Camera, error)
	Test(ctx context.Context, cameraID string, draft camera.Draft) (camera.TestResponse, error)
	Create(ctx context.Context, draft camera.Draft) (domain.Camera, error)
	Update(ctx context.Context, id string, draft camera.Draft) (domain.Camera, error)
	Delete(ctx context.Context, id string) error
}

type SystemStatus interface {
	Ready() bool
	Workers() any
	Issues() any
}

type RecordingControlService interface {
	Get(context.Context) (domain.RecordingControl, error)
	Update(context.Context, bool) (domain.RecordingControl, error)
}

type ApplicationSettingsService interface {
	Get(context.Context) (domain.ApplicationSettings, error)
	Update(context.Context, string, domain.ArchiveLayout) (domain.ApplicationSettings, error)
}

type RecordingService interface {
	ListRecordings(context.Context, string, int) ([]domain.Recording, error)
}

type EventService interface {
	ListEvents(context.Context, string, int) ([]domain.Event, error)
}

type PlaybackService interface {
	GetRecording(context.Context, string) (domain.Recording, error)
	OpenRecording(context.Context, string) (domain.Recording, *os.File, error)
}

type Server struct {
	cameras    CameraService
	recordings RecordingService
	events     EventService
	playback   PlaybackService
	status     SystemStatus
	recording  RecordingControlService
	settings   ApplicationSettingsService
	logger     *slog.Logger
}

func New(cameras CameraService, recordings RecordingService, events EventService, playback PlaybackService, status SystemStatus, recording RecordingControlService, settings ApplicationSettingsService, assets fs.FS, logger *slog.Logger) http.Handler {
	server := &Server{cameras: cameras, recordings: recordings, events: events, playback: playback, status: status, recording: recording, settings: settings, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health/live", server.live)
	mux.HandleFunc("GET /api/health/ready", server.ready)
	mux.HandleFunc("GET /api/system/workers", server.workers)
	mux.HandleFunc("GET /api/system/issues", server.issues)
	mux.HandleFunc("GET /api/system/recording", server.getRecordingControl)
	mux.HandleFunc("PUT /api/system/recording", server.updateRecordingControl)
	mux.HandleFunc("GET /api/system/settings", server.getApplicationSettings)
	mux.HandleFunc("PUT /api/system/settings", server.updateApplicationSettings)
	mux.HandleFunc("GET /api/cameras", server.listCameras)
	mux.HandleFunc("POST /api/cameras/discover", server.discoverCameras)
	mux.HandleFunc("POST /api/cameras/test", server.testNewCamera)
	mux.HandleFunc("POST /api/cameras", server.createCamera)
	mux.HandleFunc("GET /api/cameras/{id}", server.getCamera)
	mux.HandleFunc("PUT /api/cameras/{id}", server.updateCamera)
	mux.HandleFunc("DELETE /api/cameras/{id}", server.deleteCamera)
	mux.HandleFunc("POST /api/cameras/{id}/test", server.testExistingCamera)
	mux.HandleFunc("GET /api/cameras/{id}/status", server.getCamera)
	mux.HandleFunc("GET /api/cameras/{id}/recordings", server.listRecordings)
	mux.HandleFunc("GET /api/cameras/{id}/recovery", server.listRecoveryGaps)
	mux.HandleFunc("GET /api/cameras/{id}/events", server.listEvents)
	mux.HandleFunc("GET /api/recordings/{id}", server.getRecording)
	mux.HandleFunc("GET /api/recordings/{id}/media", server.serveRecording)
	mux.HandleFunc("GET /api/recordings/{id}/original", server.downloadRecording)
	if assets != nil {
		mux.Handle("/", http.FileServer(http.FS(assets)))
	}
	return securityHeaders(mux)
}

func (server *Server) discoverCameras(writer http.ResponseWriter, request *http.Request) {
	discoverer, ok := server.cameras.(interface {
		Discover(context.Context) ([]camera.DiscoveredCamera, error)
	})
	if !ok {
		writeError(writer, http.StatusNotImplemented, "NotAvailable", "Camera discovery is unavailable.")
		return
	}
	devices, err := discoverer.Discover(request.Context())
	if errors.Is(err, camera.ErrDiscoveryBusy) {
		writer.Header().Set("Retry-After", "3")
		writeError(writer, http.StatusConflict, "DiscoveryBusy", "Camera detection is already running. Try again shortly.")
		return
	}
	if errors.Is(err, camera.ErrDiscoveryUnavailable) {
		writeError(writer, http.StatusNotImplemented, "NotAvailable", "Camera discovery is unavailable.")
		return
	}
	if err != nil {
		writeError(writer, http.StatusBadGateway, "DiscoveryFailed", "Camera detection failed. Check local network access and try again.")
		return
	}
	if devices == nil {
		devices = []camera.DiscoveredCamera{}
	}
	writeJSON(writer, http.StatusOK, devices)
}

func (server *Server) getApplicationSettings(writer http.ResponseWriter, request *http.Request) {
	if server.settings == nil {
		writeJSON(writer, http.StatusNotImplemented, map[string]string{"code": "NotAvailable", "message": "Application settings are not available."})
		return
	}
	settings, err := server.settings.Get(request.Context())
	if err != nil {
		server.internalError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, settings)
}

func (server *Server) listRecoveryGaps(writer http.ResponseWriter, request *http.Request) {
	if _, err := server.cameras.Get(request.Context(), request.PathValue("id")); err != nil {
		if errors.Is(err, camera.ErrNotFound) {
			writeError(writer, http.StatusNotFound, "CameraNotFound", "Camera not found.")
			return
		}
		server.internalError(writer, request, err)
		return
	}
	reader, ok := server.recordings.(interface {
		ListRecoveryGaps(context.Context, string, int) ([]domain.RecoveryGap, error)
	})
	if !ok {
		writeError(writer, http.StatusNotImplemented, "NotAvailable", "Recovery history is unavailable.")
		return
	}
	gaps, err := reader.ListRecoveryGaps(request.Context(), request.PathValue("id"), 100)
	if err != nil {
		server.internalError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, gaps)
}

func (server *Server) updateApplicationSettings(writer http.ResponseWriter, request *http.Request) {
	if server.settings == nil {
		writeJSON(writer, http.StatusNotImplemented, map[string]string{"code": "NotAvailable", "message": "Application settings are not available."})
		return
	}
	var input struct {
		RecordingFolder *string               `json:"recordingFolder"`
		ArchiveLayout   *domain.ArchiveLayout `json:"archiveLayout"`
	}
	if !decodeRequest(writer, request, &input) {
		return
	}
	if input.RecordingFolder == nil || input.ArchiveLayout == nil {
		writeError(writer, http.StatusBadRequest, "InvalidApplicationSettings", "Recording folder and archive layout are required.")
		return
	}
	settings, err := server.settings.Update(request.Context(), *input.RecordingFolder, *input.ArchiveLayout)
	if errors.Is(err, control.ErrInvalidApplicationSettings) {
		writeError(writer, http.StatusBadRequest, "InvalidApplicationSettings", err.Error())
		return
	}
	if err != nil {
		server.internalError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, settings)
}

func (server *Server) getRecordingControl(writer http.ResponseWriter, request *http.Request) {
	if server.recording == nil {
		writeJSON(writer, http.StatusNotImplemented, map[string]string{"code": "NotAvailable", "message": "Recording control is not available."})
		return
	}
	state, err := server.recording.Get(request.Context())
	if err != nil {
		server.internalError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, state)
}

func (server *Server) updateRecordingControl(writer http.ResponseWriter, request *http.Request) {
	if server.recording == nil {
		writeJSON(writer, http.StatusNotImplemented, map[string]string{"code": "NotAvailable", "message": "Recording control is not available."})
		return
	}
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeRequest(writer, request, &input) {
		return
	}
	if input.Enabled == nil {
		writeError(writer, http.StatusBadRequest, "InvalidRecordingControl", "Recording enabled state is required.")
		return
	}
	state, err := server.recording.Update(request.Context(), *input.Enabled)
	if err != nil {
		server.internalError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, state)
}

func (server *Server) getRecording(writer http.ResponseWriter, request *http.Request) {
	if server.playback == nil {
		writeJSON(writer, http.StatusNotImplemented, map[string]string{"code": "NotAvailable", "message": "Recording playback is not available."})
		return
	}
	recording, err := server.playback.GetRecording(request.Context(), request.PathValue("id"))
	if errors.Is(err, archive.ErrRecordingUnavailable) {
		writeError(writer, http.StatusNotFound, "RecordingNotFound", "The recording is not available.")
		return
	}
	if err != nil {
		server.internalError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, recording)
}

func (server *Server) serveRecording(writer http.ResponseWriter, request *http.Request) {
	server.openRecording(writer, request, false)
}

func (server *Server) downloadRecording(writer http.ResponseWriter, request *http.Request) {
	server.openRecording(writer, request, true)
}

func (server *Server) openRecording(writer http.ResponseWriter, request *http.Request, attachment bool) {
	if server.playback == nil {
		writeJSON(writer, http.StatusNotImplemented, map[string]string{"code": "NotAvailable", "message": "Recording playback is not available."})
		return
	}
	recording, file, err := server.playback.OpenRecording(request.Context(), request.PathValue("id"))
	if errors.Is(err, archive.ErrRecordingUnavailable) {
		writeError(writer, http.StatusNotFound, "RecordingNotFound", "The recording is not available.")
		return
	}
	if err != nil {
		server.internalError(writer, request, err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		server.internalError(writer, request, err)
		return
	}
	fileName := filepath.Base(recording.LocalPath)
	contentType, err := recordingContentType(fileName, file)
	if err != nil {
		server.internalError(writer, request, err)
		return
	}
	fileName = recordingFileName(fileName, contentType)
	writer.Header().Set("Content-Type", contentType)
	disposition := "inline"
	if attachment {
		disposition = "attachment"
	}
	writer.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": fileName}))
	http.ServeContent(writer, request, fileName, info.ModTime(), file)
}

func recordingContentType(fileName string, file *os.File) (string, error) {
	if contentType := mime.TypeByExtension(filepath.Ext(fileName)); contentType != "" {
		return contentType, nil
	}
	header := make([]byte, 512)
	count, err := file.Read(header)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", errors.New("inspect recording media type")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", errors.New("reset recording after media type inspection")
	}
	header = header[:count]
	if len(header) >= 12 && string(header[4:8]) == "ftyp" {
		return "video/mp4", nil
	}
	detected := http.DetectContentType(header)
	if strings.HasPrefix(detected, "video/") || strings.HasPrefix(detected, "audio/") {
		return detected, nil
	}
	return "application/octet-stream", nil
}

func recordingFileName(fileName, contentType string) string {
	if filepath.Ext(fileName) == "" && contentType == "video/mp4" {
		return fileName + ".mp4"
	}
	return fileName
}

func (server *Server) listEvents(writer http.ResponseWriter, request *http.Request) {
	if server.events == nil {
		writeJSON(writer, http.StatusNotImplemented, map[string]string{"code": "NotAvailable", "message": "Event history is not available."})
		return
	}
	events, err := server.events.ListEvents(request.Context(), request.PathValue("id"), 1000)
	if err != nil {
		server.internalError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, events)
}

func (server *Server) listRecordings(writer http.ResponseWriter, request *http.Request) {
	if server.recordings == nil {
		writeJSON(writer, http.StatusNotImplemented, map[string]string{"code": "NotAvailable", "message": "Recording inventory is not available."})
		return
	}
	recordings, err := server.recordings.ListRecordings(request.Context(), request.PathValue("id"), 1000)
	if err != nil {
		server.internalError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, recordings)
}

func (server *Server) live(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "Live"})
}

func (server *Server) ready(writer http.ResponseWriter, _ *http.Request) {
	if server.status != nil && !server.status.Ready() {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "Degraded"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "Ready"})
}

func (server *Server) workers(writer http.ResponseWriter, _ *http.Request) {
	if server.status == nil {
		writeJSON(writer, http.StatusOK, []any{})
		return
	}
	writeJSON(writer, http.StatusOK, server.status.Workers())
}

func (server *Server) issues(writer http.ResponseWriter, _ *http.Request) {
	if server.status == nil {
		writeJSON(writer, http.StatusOK, []any{})
		return
	}
	writeJSON(writer, http.StatusOK, server.status.Issues())
}

func (server *Server) listCameras(writer http.ResponseWriter, request *http.Request) {
	cameras, err := server.cameras.List(request.Context())
	if err != nil {
		server.internalError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, cameras)
}

func (server *Server) getCamera(writer http.ResponseWriter, request *http.Request) {
	item, err := server.cameras.Get(request.Context(), request.PathValue("id"))
	if err != nil {
		server.serviceError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (server *Server) testNewCamera(writer http.ResponseWriter, request *http.Request) {
	server.testCamera(writer, request, "")
}

func (server *Server) testExistingCamera(writer http.ResponseWriter, request *http.Request) {
	server.testCamera(writer, request, request.PathValue("id"))
}

func (server *Server) testCamera(writer http.ResponseWriter, request *http.Request, cameraID string) {
	var draft camera.Draft
	if !decodeRequest(writer, request, &draft) {
		return
	}
	result, err := server.cameras.Test(request.Context(), cameraID, draft)
	draft.Password = ""
	if err != nil {
		server.serviceError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) createCamera(writer http.ResponseWriter, request *http.Request) {
	var draft camera.Draft
	if !decodeRequest(writer, request, &draft) {
		return
	}
	created, err := server.cameras.Create(request.Context(), draft)
	draft.Password = ""
	if err != nil {
		server.serviceError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusCreated, created)
}

func (server *Server) updateCamera(writer http.ResponseWriter, request *http.Request) {
	var draft camera.Draft
	if !decodeRequest(writer, request, &draft) {
		return
	}
	updated, err := server.cameras.Update(request.Context(), request.PathValue("id"), draft)
	draft.Password = ""
	if err != nil {
		server.serviceError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, updated)
}

func (server *Server) deleteCamera(writer http.ResponseWriter, request *http.Request) {
	if err := server.cameras.Delete(request.Context(), request.PathValue("id")); err != nil {
		server.serviceError(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) serviceError(writer http.ResponseWriter, request *http.Request, err error) {
	var timezoneFailure *reolink.TimezoneMismatchError
	switch {
	case errors.Is(err, camera.ErrInvalid):
		writeError(writer, http.StatusBadRequest, "InvalidCamera", strings.TrimPrefix(err.Error(), camera.ErrInvalid.Error()+": "))
	case errors.Is(err, reolink.ErrInvalidConnection):
		writeError(writer, http.StatusBadRequest, "InvalidCameraConnection", "Use a private camera address, a valid Baichuan port, and a username. Masked password placeholders are not accepted.")
	case errors.Is(err, camera.ErrRetest):
		writeError(writer, http.StatusConflict, "ConnectionTestRequired", camera.ErrRetest.Error())
	case errors.Is(err, camera.ErrNotFound):
		writeError(writer, http.StatusNotFound, "CameraNotFound", camera.ErrNotFound.Error())
	case strings.Contains(err.Error(), "UNIQUE constraint failed"):
		writeError(writer, http.StatusConflict, "CameraConflict", "A camera with this name or host and channel is already registered.")
	case errors.As(err, &timezoneFailure):
		server.logger.Warn("camera timezone could not be matched", "method", request.Method, "path", request.URL.Path, "timezoneConfiguration", timezoneFailure.Configuration)
		writeError(writer, http.StatusUnprocessableEntity, "CameraTimezoneUnsupported", "Connected to the camera, but its timezone and daylight-saving schedule cannot be matched safely. Review the camera's time settings in the Reolink app. No camera settings were changed.")
	case strings.Contains(err.Error(), "camera command timed out"):
		writeError(writer, http.StatusGatewayTimeout, "CameraTestTimeout", "The camera did not respond before the connection test timed out. Check that it is awake and reachable at this address and Baichuan port, then retry.")
	case strings.Contains(err.Error(), "timezone") || strings.Contains(err.Error(), "daylight-saving"):
		writeError(writer, http.StatusBadGateway, "CameraClockUnavailable", "The camera did not provide usable timezone or daylight-saving data. Review its time settings in the Reolink app and retry. No camera settings were changed.")
	case strings.Contains(err.Error(), "camera command") || strings.Contains(err.Error(), "camera returned") || strings.Contains(err.Error(), "host ") || strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "port "):
		writeError(writer, http.StatusBadGateway, "CameraConnectionFailed", "Unable to connect to the camera. Verify its address, Baichuan port, and credentials.")
	default:
		server.internalError(writer, request, err)
	}
}

func (server *Server) internalError(writer http.ResponseWriter, request *http.Request, err error) {
	server.logger.Error("request failed", "method", request.Method, "path", request.URL.Path, "error", err)
	writeError(writer, http.StatusInternalServerError, "InternalError", "The request could not be completed.")
}

func decodeRequest(writer http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	if err := json.NewDecoder(request.Body).Decode(target); err != nil {
		writeError(writer, http.StatusBadRequest, "InvalidJson", "Request body must contain valid JSON.")
		return false
	}
	return true
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]string{"code": code, "message": message})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(writer, request)
	})
}
