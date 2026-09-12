package domain

import "time"

type EventType string

const (
	EventMotion  EventType = "Motion"
	EventPerson  EventType = "Person"
	EventVehicle EventType = "Vehicle"
	EventAnimal  EventType = "Animal"
)

type ArchiveStream string

const (
	StreamMain ArchiveStream = "Main"
	StreamSub  ArchiveStream = "Sub"
)

type RecordingSource string

const (
	RecordingSourceCameraStorage RecordingSource = "CameraStorage"
	RecordingSourceRtsp          RecordingSource = "Rtsp"
)

type RecordingMode string

const (
	RecordingModeEvents     RecordingMode = "Events"
	RecordingModeContinuous RecordingMode = "Continuous"
)

type HistoricalImport string

const (
	HistoricalImportNewOnly     HistoricalImport = "NewOnly"
	HistoricalImportLast24Hours HistoricalImport = "Last24Hours"
	HistoricalImportLast48Hours HistoricalImport = "Last48Hours"
	HistoricalImportLast7Days   HistoricalImport = "Last7Days"
	HistoricalImportAll         HistoricalImport = "AllAvailable"
)

type CameraCapabilities struct {
	MainStream          string      `json:"mainStream"`
	SubStream           string      `json:"subStream"`
	MainBitrateKbps     int         `json:"mainBitrateKbps"`
	SubBitrateKbps      int         `json:"subBitrateKbps"`
	PreRecordSeconds    int         `json:"preRecordSeconds"`
	PostRecordSeconds   int         `json:"postRecordSeconds"`
	Storage             string      `json:"storage"`
	StorageAvailable    bool        `json:"storageAvailable"`
	RtspAvailable       bool        `json:"rtspAvailable"`
	ContinuousAvailable bool        `json:"continuousAvailable"`
	SupportedEventTypes []EventType `json:"supportedEventTypes"`
}

type Camera struct {
	ID                   string              `json:"id"`
	Name                 string              `json:"name"`
	Host                 string              `json:"host"`
	Port                 int                 `json:"port"`
	Channel              int                 `json:"channel"`
	Timezone             string              `json:"timezone"`
	Username             string              `json:"username"`
	Enabled              bool                `json:"enabled"`
	ArchiveEnabled       bool                `json:"archiveEnabled"`
	ArchiveStream        ArchiveStream       `json:"archiveStream"`
	RecordingSource      RecordingSource     `json:"recordingSource"`
	RecordingMode        RecordingMode       `json:"recordingMode"`
	HistoricalImport     HistoricalImport    `json:"historicalImport"`
	PreEventSeconds      int                 `json:"preEventSeconds"`
	PostEventSeconds     int                 `json:"postEventSeconds"`
	ArchiveFromUtc       *time.Time          `json:"archiveFromUtc,omitempty"`
	EventTypes           []EventType         `json:"eventTypes"`
	Model                string              `json:"model,omitempty"`
	Firmware             string              `json:"firmware,omitempty"`
	Capabilities         *CameraCapabilities `json:"capabilities,omitempty"`
	ConnectionStatus     string              `json:"connectionStatus"`
	CredentialConfigured bool                `json:"credentialConfigured"`
	LastTestedAtUtc      *time.Time          `json:"lastTestedAtUtc,omitempty"`
	LastEventAtUtc       *time.Time          `json:"lastEventAtUtc,omitempty"`
	RecordingCount       int                 `json:"recordingCount"`
	PendingDownloadCount int                 `json:"pendingDownloadCount"`
	CreatedAtUtc         time.Time           `json:"createdAtUtc"`
	UpdatedAtUtc         time.Time           `json:"updatedAtUtc"`
}

type EncryptedCredential struct {
	Username   string
	Ciphertext []byte
	Nonce      []byte
	KeyVersion int
}

type WorkerState struct {
	WorkerName          string     `json:"workerName"`
	ScopeKey            string     `json:"scopeKey"`
	Stage               string     `json:"stage"`
	LastHeartbeatAtUtc  *time.Time `json:"lastHeartbeatAtUtc,omitempty"`
	LastSuccessAtUtc    *time.Time `json:"lastSuccessAtUtc,omitempty"`
	ConsecutiveFailures int        `json:"consecutiveFailures"`
	NextAttemptAtUtc    *time.Time `json:"nextAttemptAtUtc,omitempty"`
	LastErrorCode       string     `json:"lastErrorCode,omitempty"`
	UpdatedAtUtc        time.Time  `json:"updatedAtUtc"`
}

type OperationalIssue struct {
	ID              string     `json:"id"`
	Severity        string     `json:"severity"`
	Source          string     `json:"source"`
	CameraID        string     `json:"cameraId,omitempty"`
	ErrorCode       string     `json:"errorCode"`
	Message         string     `json:"message"`
	FirstSeenAtUtc  time.Time  `json:"firstSeenAtUtc"`
	LastSeenAtUtc   time.Time  `json:"lastSeenAtUtc"`
	OccurrenceCount int        `json:"occurrenceCount"`
	ResolvedAtUtc   *time.Time `json:"resolvedAtUtc,omitempty"`
}
