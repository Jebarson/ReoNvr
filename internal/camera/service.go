package camera

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"
	"reonavr/internal/repository"
	"reonavr/internal/security"
)

var (
	ErrInvalid              = errors.New("invalid camera configuration")
	ErrRetest               = errors.New("test this connection before saving it")
	ErrNotFound             = errors.New("camera not found")
	ErrDiscoveryBusy        = errors.New("camera discovery is already running")
	ErrDiscoveryUnavailable = errors.New("camera discovery is unavailable")
)

type DiscoveredCamera struct {
	reolink.DiscoveredCamera
	Registered bool `json:"registered"`
}

type Draft struct {
	Name             string                  `json:"name"`
	Host             string                  `json:"host"`
	Port             int                     `json:"port"`
	Channel          int                     `json:"channel"`
	Timezone         string                  `json:"timezone"`
	Username         string                  `json:"username"`
	Password         string                  `json:"password"`
	Enabled          bool                    `json:"enabled"`
	ArchiveEnabled   bool                    `json:"archiveEnabled"`
	ArchiveStream    domain.ArchiveStream    `json:"archiveStream"`
	RecordingSource  domain.RecordingSource  `json:"recordingSource"`
	RecoveryEnabled  bool                    `json:"recoveryEnabled"`
	RecordingMode    domain.RecordingMode    `json:"recordingMode"`
	HistoricalImport domain.HistoricalImport `json:"historicalImport"`
	PreEventSeconds  int                     `json:"preEventSeconds"`
	PostEventSeconds int                     `json:"postEventSeconds"`
	EventTypes       []domain.EventType      `json:"eventTypes"`
	TestID           string                  `json:"testId"`
}

type TestResponse struct {
	TestID                string                        `json:"testId"`
	Model                 string                        `json:"model"`
	Firmware              string                        `json:"firmware"`
	Timezone              string                        `json:"timezone"`
	TimezoneOffsetSeconds int                           `json:"timezoneOffsetSeconds"`
	TimezoneConfiguration reolink.TimezoneConfiguration `json:"timezoneConfiguration"`
	SnapshotDataURL       string                        `json:"snapshotDataUrl,omitempty"`
	domain.CameraCapabilities
}

type Store interface {
	CreateCamera(context.Context, domain.Camera, domain.EncryptedCredential, []domain.EventType) error
	UpdateCamera(context.Context, domain.Camera, *domain.EncryptedCredential, []domain.EventType) error
	ListCameras(context.Context) ([]domain.Camera, error)
	GetCamera(context.Context, string) (domain.Camera, error)
	GetCredential(context.Context, string) (domain.EncryptedCredential, error)
	RetireCamera(context.Context, string, time.Time) error
	UpdateCameraProbe(context.Context, string, string, string, string, domain.CameraCapabilities, string, time.Time, ...domain.Camera) error
	UpdateCameraStatus(context.Context, string, string, time.Time, ...domain.Camera) error
}

type CameraTester interface {
	Test(context.Context, reolink.Connection) (reolink.TestResult, error)
}

type CredentialCipher interface {
	Encrypt(cameraID, password string) (ciphertext, nonce []byte, err error)
	Decrypt(cameraID string, ciphertext, nonce []byte) (string, error)
}

type Service struct {
	store       Store
	tester      CameraTester
	probeTester CameraTester
	cipher      CredentialCipher
	receipts    *receiptStore
	now         func() time.Time
	onChanged   func(domain.Camera)
	onDeleted   func(string)
	discoveryMu sync.Mutex
	mutationMu  sync.Mutex
}

func NewService(store Store, tester CameraTester, cipher CredentialCipher) *Service {
	return &Service{
		store: store, tester: tester, probeTester: tester, cipher: cipher,
		receipts: newReceiptStore(5 * time.Minute), now: time.Now,
	}
}

func (service *Service) SetProbeTester(tester CameraTester) {
	service.probeTester = tester
}

func (service *Service) SetLifecycleHooks(onChanged func(domain.Camera), onDeleted func(string)) {
	service.onChanged = onChanged
	service.onDeleted = onDeleted
}

func (service *Service) List(ctx context.Context) ([]domain.Camera, error) {
	return service.store.ListCameras(ctx)
}

func (service *Service) Discover(ctx context.Context) ([]DiscoveredCamera, error) {
	if !service.discoveryMu.TryLock() {
		return nil, ErrDiscoveryBusy
	}
	defer service.discoveryMu.Unlock()
	discoverer, ok := service.probeTester.(interface {
		Discover(context.Context) ([]reolink.DiscoveredCamera, error)
	})
	if !ok {
		return nil, ErrDiscoveryUnavailable
	}
	devices, err := discoverer.Discover(ctx)
	if err != nil {
		return nil, err
	}
	registered, err := service.store.ListCameras(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]DiscoveredCamera, 0, len(devices))
	for _, device := range devices {
		result := DiscoveredCamera{DiscoveredCamera: device}
		for _, existing := range registered {
			if strings.EqualFold(strings.Trim(existing.Host, "[]"), device.Host) && existing.Port == device.Port {
				result.Registered = true
				break
			}
		}
		results = append(results, result)
	}
	return results, nil
}

func (service *Service) Get(ctx context.Context, id string) (domain.Camera, error) {
	camera, err := service.store.GetCamera(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return domain.Camera{}, ErrNotFound
	}
	return camera, err
}

func (service *Service) Test(ctx context.Context, cameraID string, draft Draft) (TestResponse, error) {
	password, err := service.resolvePassword(ctx, cameraID, draft.Password)
	if err != nil {
		return TestResponse{}, err
	}
	draft.Password = password
	connectionFingerprint := fingerprint(draft, password)
	connection := draft.connection()
	connection.Timezone = ""
	if cameraID != "" {
		stored, err := service.Get(ctx, cameraID)
		if err != nil {
			return TestResponse{}, err
		}
		if strings.EqualFold(strings.Trim(stored.Host, "[]"), strings.Trim(connection.Host, "[]")) && stored.Port == connection.Port && stored.Channel == connection.Channel {
			connection.Timezone = stored.Timezone
		}
	}
	result, err := service.tester.Test(ctx, connection)
	draft.Password = ""
	password = ""
	if err != nil {
		return TestResponse{}, err
	}
	receiptResult := result
	receiptResult.SnapshotJPEG = nil
	id, err := service.receipts.issue(cameraID, connectionFingerprint, receiptResult, service.now())
	if err != nil {
		return TestResponse{}, err
	}
	return TestResponse{
		TestID: id, Model: result.Model, Firmware: result.Firmware, Timezone: result.Timezone,
		TimezoneOffsetSeconds: result.TimezoneOffsetSeconds,
		TimezoneConfiguration: result.TimezoneConfiguration,
		SnapshotDataURL:       snapshotDataURL(result.SnapshotJPEG),
		CameraCapabilities:    result.Capabilities,
	}, nil
}

func snapshotDataURL(snapshot []byte) string {
	if len(snapshot) == 0 {
		return ""
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(snapshot)
}

func (service *Service) Create(ctx context.Context, draft Draft) (domain.Camera, error) {
	service.mutationMu.Lock()
	defer service.mutationMu.Unlock()
	normalizeRecordingPolicy(&draft)
	if err := validateDraft(draft); err != nil {
		return domain.Camera{}, err
	}
	result, ok := service.receipts.consume(draft.TestID, "", fingerprint(draft, draft.Password), service.now())
	if !ok {
		return domain.Camera{}, ErrRetest
	}
	if err := validateEventTypes(draft.EventTypes, result.Capabilities.SupportedEventTypes); err != nil {
		return domain.Camera{}, err
	}
	if err := validateRecordingPolicy(draft, result.Capabilities); err != nil {
		return domain.Camera{}, err
	}
	normalizeRecordingPolicy(&draft)
	id, err := randomID()
	if err != nil {
		return domain.Camera{}, fmt.Errorf("generate camera ID: %w", err)
	}
	ciphertext, nonce, err := service.cipher.Encrypt(id, draft.Password)
	if err != nil {
		return domain.Camera{}, err
	}
	now := service.now().UTC()
	camera := cameraFromDraft(id, draft, result, now, now)
	credential := domain.EncryptedCredential{Username: draft.Username, Ciphertext: ciphertext, Nonce: nonce, KeyVersion: security.KeyVersion}
	if err := service.store.CreateCamera(ctx, camera, credential, result.Capabilities.SupportedEventTypes); err != nil {
		return domain.Camera{}, fmt.Errorf("save camera: %w", err)
	}
	draft.Password = ""
	if service.onChanged != nil {
		service.onChanged(camera)
	}
	return camera, nil
}

func (service *Service) Update(ctx context.Context, id string, draft Draft) (domain.Camera, error) {
	service.mutationMu.Lock()
	defer service.mutationMu.Unlock()
	normalizeRecordingPolicy(&draft)
	if err := validateDraft(draft); err != nil {
		return domain.Camera{}, err
	}
	existing, err := service.Get(ctx, id)
	if err != nil {
		return domain.Camera{}, err
	}
	credentialChanged := draft.Host != existing.Host || draft.Port != existing.Port ||
		draft.Channel != existing.Channel || draft.Username != existing.Username || draft.Password != ""
	requiresTest := credentialChanged || draft.ArchiveStream != existing.ArchiveStream
	result := reolink.TestResult{Model: existing.Model, Firmware: existing.Firmware, Timezone: existing.Timezone}
	if existing.Capabilities != nil {
		result.Capabilities = *existing.Capabilities
	}
	var encrypted *domain.EncryptedCredential
	if requiresTest || draft.TestID != "" {
		password, err := service.resolvePassword(ctx, id, draft.Password)
		if err != nil {
			return domain.Camera{}, err
		}
		var ok bool
		result, ok = service.receipts.consume(draft.TestID, id, fingerprint(draft, password), service.now())
		if !ok {
			return domain.Camera{}, ErrRetest
		}
		if credentialChanged {
			ciphertext, nonce, err := service.cipher.Encrypt(id, password)
			if err != nil {
				return domain.Camera{}, err
			}
			value := domain.EncryptedCredential{Username: draft.Username, Ciphertext: ciphertext, Nonce: nonce, KeyVersion: security.KeyVersion}
			encrypted = &value
		}
		password = ""
	}
	if err := validateEventTypes(draft.EventTypes, result.Capabilities.SupportedEventTypes); err != nil {
		return domain.Camera{}, err
	}
	if err := validateRecordingPolicy(draft, result.Capabilities); err != nil {
		return domain.Camera{}, err
	}
	normalizeRecordingPolicy(&draft)
	now := service.now().UTC()
	camera := cameraFromDraft(id, draft, result, existing.CreatedAtUtc, now)
	if draft.RecordingSource == existing.RecordingSource && draft.HistoricalImport == existing.HistoricalImport {
		camera.ArchiveFromUtc = existing.ArchiveFromUtc
	}
	if !requiresTest && draft.TestID == "" {
		camera.LastTestedAtUtc = existing.LastTestedAtUtc
		camera.ConnectionStatus = existing.ConnectionStatus
		if !existing.Enabled && camera.Enabled {
			camera.ConnectionStatus = "Testing"
		}
	}
	if !camera.Enabled {
		camera.ConnectionStatus = "Offline"
	}
	if err := service.store.UpdateCamera(ctx, camera, encrypted, result.Capabilities.SupportedEventTypes); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return domain.Camera{}, ErrNotFound
		}
		return domain.Camera{}, fmt.Errorf("save camera: %w", err)
	}
	draft.Password = ""
	if service.onChanged != nil {
		service.onChanged(camera)
	}
	return camera, nil
}

func (service *Service) Delete(ctx context.Context, id string) error {
	service.mutationMu.Lock()
	defer service.mutationMu.Unlock()
	err := service.store.RetireCamera(ctx, id, service.now().UTC())
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if err == nil && service.onDeleted != nil {
		service.onDeleted(id)
	}
	return err
}

func (service *Service) Probe(ctx context.Context, id string) error {
	camera, err := service.Get(ctx, id)
	if err != nil {
		return err
	}
	if !camera.Enabled {
		return nil
	}
	credential, err := service.store.GetCredential(ctx, id)
	if err != nil {
		return err
	}
	password, err := service.cipher.Decrypt(id, credential.Ciphertext, credential.Nonce)
	if err != nil {
		return err
	}
	result, probeErr := service.probeTester.Test(ctx, reolink.Connection{
		Host: camera.Host, Port: camera.Port, Channel: camera.Channel,
		Stream: camera.ArchiveStream, Timezone: camera.Timezone,
		Username: credential.Username, Password: password,
	})
	password = ""
	if probeErr != nil {
		return errors.Join(probeErr, service.store.UpdateCameraStatus(ctx, id, "Degraded", service.now().UTC(), camera))
	}
	return service.store.UpdateCameraProbe(ctx, id, result.Model, result.Firmware, result.Timezone, result.Capabilities, "Online", service.now().UTC(), camera)
}

func (service *Service) resolvePassword(ctx context.Context, cameraID, submitted string) (string, error) {
	if submitted != "" {
		if submitted == "********" {
			return "", fmt.Errorf("%w: masked password placeholders are not accepted", ErrInvalid)
		}
		return submitted, nil
	}
	if cameraID == "" {
		return "", nil
	}
	credential, err := service.store.GetCredential(ctx, cameraID)
	if errors.Is(err, repository.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if credential.KeyVersion != security.KeyVersion {
		return "", errors.New("stored credential uses an unsupported key version")
	}
	return service.cipher.Decrypt(cameraID, credential.Ciphertext, credential.Nonce)
}

func (draft Draft) connection() reolink.Connection {
	return reolink.Connection{Host: strings.TrimSpace(draft.Host), Port: draft.Port, Channel: draft.Channel, Stream: draft.ArchiveStream, Timezone: strings.TrimSpace(draft.Timezone), Username: strings.TrimSpace(draft.Username), Password: draft.Password}
}

func validateDraft(draft Draft) error {
	if strings.TrimSpace(draft.Name) == "" {
		return fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if strings.TrimSpace(draft.Host) == "" || strings.TrimSpace(draft.Username) == "" {
		return fmt.Errorf("%w: host and username are required", ErrInvalid)
	}
	if draft.Port < 1 || draft.Port > 65535 || draft.Channel < 0 {
		return fmt.Errorf("%w: port or channel is outside its valid range", ErrInvalid)
	}
	if draft.ArchiveStream != domain.StreamMain && draft.ArchiveStream != domain.StreamSub {
		return fmt.Errorf("%w: archive stream must be Main or Sub", ErrInvalid)
	}
	if draft.RecordingSource != domain.RecordingSourceCameraStorage && draft.RecordingSource != domain.RecordingSourceRtsp {
		return fmt.Errorf("%w: recording source must be CameraStorage or Rtsp", ErrInvalid)
	}
	if draft.RecordingMode != domain.RecordingModeEvents && draft.RecordingMode != domain.RecordingModeContinuous {
		return fmt.Errorf("%w: recording mode must be Events or Continuous", ErrInvalid)
	}
	switch draft.HistoricalImport {
	case domain.HistoricalImportNewOnly, domain.HistoricalImportLast24Hours,
		domain.HistoricalImportLast48Hours, domain.HistoricalImportLast7Days,
		domain.HistoricalImportAll:
	default:
		return fmt.Errorf("%w: historical import range is invalid", ErrInvalid)
	}
	if draft.PreEventSeconds < 0 || draft.PreEventSeconds > 300 || draft.PostEventSeconds < 0 || draft.PostEventSeconds > 300 {
		return fmt.Errorf("%w: pre-event and post-event durations must be between 0 and 300 seconds", ErrInvalid)
	}
	return nil
}

func validateRecordingPolicy(draft Draft, capabilities domain.CameraCapabilities) error {
	if !draft.ArchiveEnabled {
		return nil
	}
	if draft.RecoveryEnabled && !capabilities.StorageAvailable {
		return fmt.Errorf("%w: mounted, formatted camera storage is required for recovery", ErrInvalid)
	}
	if draft.RecoveryEnabled && !capabilities.StoragePlaybackAvailable {
		return fmt.Errorf("%w: camera recording search must be available for recovery", ErrInvalid)
	}
	switch draft.RecordingSource {
	case domain.RecordingSourceCameraStorage:
		if !capabilities.StorageAvailable {
			return fmt.Errorf("%w: camera storage is unavailable", ErrInvalid)
		}
		if draft.RecordingMode == domain.RecordingModeContinuous && !capabilities.ContinuousAvailable {
			return fmt.Errorf("%w: the camera is not configured for continuous recording", ErrInvalid)
		}
	case domain.RecordingSourceRtsp:
		if !capabilities.RtspAvailable {
			return fmt.Errorf("%w: RTSP is unavailable for the selected stream", ErrInvalid)
		}
	}
	if draft.RecordingMode == domain.RecordingModeEvents && len(draft.EventTypes) == 0 {
		return fmt.Errorf("%w: select at least one event category for event recording", ErrInvalid)
	}
	return nil
}

func normalizeRecordingPolicy(draft *Draft) {
	draft.RecordingSource = domain.RecordingSourceRtsp
	draft.HistoricalImport = domain.HistoricalImportNewOnly
}

func validateEventTypes(selected, supported []domain.EventType) error {
	allowed := make(map[domain.EventType]bool, len(supported))
	for _, eventType := range supported {
		allowed[eventType] = true
	}
	for _, eventType := range selected {
		if !allowed[eventType] {
			return fmt.Errorf("%w: event type %s is not supported by this camera", ErrInvalid, eventType)
		}
	}
	return nil
}

func cameraFromDraft(id string, draft Draft, result reolink.TestResult, created, updated time.Time) domain.Camera {
	lastTested := updated
	status := "Online"
	if !draft.Enabled {
		status = "Offline"
	}
	capabilities := result.Capabilities
	return domain.Camera{
		ID: id, Name: strings.TrimSpace(draft.Name), Host: strings.TrimSpace(draft.Host),
		Port: draft.Port, Channel: draft.Channel, Timezone: result.Timezone,
		Username: strings.TrimSpace(draft.Username), Enabled: draft.Enabled,
		ArchiveEnabled: draft.ArchiveEnabled, ArchiveStream: draft.ArchiveStream,
		RecordingSource: draft.RecordingSource, RecordingMode: draft.RecordingMode,
		RecoveryEnabled:  draft.RecoveryEnabled,
		HistoricalImport: draft.HistoricalImport, PreEventSeconds: draft.PreEventSeconds,
		PostEventSeconds: draft.PostEventSeconds, ArchiveFromUtc: historicalBoundary(draft.HistoricalImport, updated),
		EventTypes: append([]domain.EventType(nil), draft.EventTypes...), Model: result.Model,
		Firmware: result.Firmware, Capabilities: &capabilities, ConnectionStatus: status,
		CredentialConfigured: true, LastTestedAtUtc: &lastTested, CreatedAtUtc: created, UpdatedAtUtc: updated,
	}
}

func historicalBoundary(value domain.HistoricalImport, now time.Time) *time.Time {
	var boundary time.Time
	switch value {
	case domain.HistoricalImportLast24Hours:
		boundary = now.Add(-24 * time.Hour)
	case domain.HistoricalImportLast48Hours:
		boundary = now.Add(-48 * time.Hour)
	case domain.HistoricalImportLast7Days:
		boundary = now.Add(-7 * 24 * time.Hour)
	case domain.HistoricalImportAll:
		return nil
	default:
		boundary = now
	}
	boundary = boundary.UTC()
	return &boundary
}

func fingerprint(draft Draft, password string) [32]byte {
	value, _ := json.Marshal([]any{strings.TrimSpace(draft.Host), draft.Port, draft.Channel, draft.ArchiveStream, strings.TrimSpace(draft.Username), password})
	return sha256.Sum256(value)
}

func randomID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

type receipt struct {
	cameraID    string
	fingerprint [32]byte
	result      reolink.TestResult
	expiresAt   time.Time
}

type receiptStore struct {
	mu       sync.Mutex
	lifetime time.Duration
	items    map[string]receipt
}

func newReceiptStore(lifetime time.Duration) *receiptStore {
	return &receiptStore{lifetime: lifetime, items: make(map[string]receipt)}
}

func (store *receiptStore) issue(cameraID string, fingerprint [32]byte, result reolink.TestResult, now time.Time) (string, error) {
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(random)
	store.mu.Lock()
	defer store.mu.Unlock()
	for key, item := range store.items {
		if !item.expiresAt.After(now) {
			delete(store.items, key)
		}
	}
	store.items[id] = receipt{cameraID: cameraID, fingerprint: fingerprint, result: result, expiresAt: now.Add(store.lifetime)}
	return id, nil
}

func (store *receiptStore) consume(id, cameraID string, fingerprint [32]byte, now time.Time) (reolink.TestResult, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	item, ok := store.items[id]
	if !ok || item.cameraID != cameraID || item.fingerprint != fingerprint || !item.expiresAt.After(now) {
		return reolink.TestResult{}, false
	}
	delete(store.items, id)
	return item.result, true
}
