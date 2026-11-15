package domain

import (
	"encoding/json"
	"time"
)

type TimeRange struct {
	StartAtUtc time.Time `json:"startAtUtc"`
	EndAtUtc   time.Time `json:"endAtUtc"`
}

type RecoveryPeriod struct {
	ID               string
	CameraID         string
	Stream           ArchiveStream
	Mode             RecordingMode
	EventTypes       []EventType
	PreEventSeconds  int
	PostEventSeconds int
	StartAtUtc       time.Time
	EndAtUtc         *time.Time
	ScannedUntilUtc  time.Time
}

type RecoveryGap struct {
	ID         string        `json:"id"`
	PeriodID   string        `json:"-"`
	CameraID   string        `json:"cameraId"`
	Stream     ArchiveStream `json:"stream"`
	Mode       RecordingMode `json:"mode"`
	EventTypes []EventType   `json:"eventTypes"`
	TimeRange
	Status           string     `json:"status"`
	MissingSeconds   float64    `json:"missingSeconds"`
	LastCheckedAtUtc *time.Time `json:"lastCheckedAtUtc,omitempty"`
	SearchTimezone   string     `json:"-"`
	SearchEndpoint   string     `json:"-"`
	ClockAmbiguous   bool       `json:"-"`
	SearchCursorUtc  *time.Time `json:"-"`
	NextSearchAtUtc  *time.Time `json:"-"`
}

type RecordingState string

const (
	RecordingDiscovered  RecordingState = "Discovered"
	RecordingQueued      RecordingState = "Queued"
	RecordingRecording   RecordingState = "Recording"
	RecordingDownloading RecordingState = "Downloading"
	RecordingVerifying   RecordingState = "Verifying"
	RecordingComplete    RecordingState = "Complete"
	RecordingRetryWait   RecordingState = "RetryWait"
	RecordingFailed      RecordingState = "Failed"
	RecordingExpired     RecordingState = "Expired"
	RecordingDeleted     RecordingState = "Deleted"
)

type Recording struct {
	ID              string          `json:"id"`
	CameraID        string          `json:"cameraId"`
	NativeName      string          `json:"nativeName"`
	Source          RecordingSource `json:"source"`
	Stream          ArchiveStream   `json:"stream"`
	Codec           string          `json:"codec,omitempty"`
	StartAtUtc      time.Time       `json:"startAtUtc"`
	EndAtUtc        time.Time       `json:"endAtUtc"`
	SizeBytes       int64           `json:"sizeBytes"`
	EventTypes      []EventType     `json:"eventTypes"`
	State           RecordingState  `json:"state"`
	RawMetadata     json.RawMessage `json:"-"`
	DiscoveredAtUtc time.Time       `json:"discoveredAtUtc"`
	UpdatedAtUtc    time.Time       `json:"updatedAtUtc"`
	LocalPath       string          `json:"localPath,omitempty"`
	Sha256          string          `json:"sha256,omitempty"`
}

type EventTransition struct {
	OccurredAtUtc time.Time       `json:"occurredAtUtc"`
	EventType     EventType       `json:"eventType,omitempty"`
	Active        bool            `json:"active"`
	RawPayload    json.RawMessage `json:"-"`
}

type Event struct {
	ID                  string          `json:"id"`
	CameraID            string          `json:"cameraId"`
	EventType           EventType       `json:"eventType"`
	StartedAtUtc        time.Time       `json:"startedAtUtc"`
	EndedAtUtc          *time.Time      `json:"endedAtUtc,omitempty"`
	NativeRecordingName string          `json:"nativeRecordingName,omitempty"`
	RawPayload          json.RawMessage `json:"-"`
	CreatedAtUtc        time.Time       `json:"createdAtUtc"`
}

type IngestionRun struct {
	SearchClock     string     `json:"-"`
	SearchEndpoint  string     `json:"-"`
	ID              string     `json:"id"`
	CameraID        string     `json:"cameraId"`
	Kind            string     `json:"kind"`
	WindowStartUtc  time.Time  `json:"windowStartUtc"`
	WindowEndUtc    time.Time  `json:"windowEndUtc"`
	Status          string     `json:"status"`
	DiscoveredCount int        `json:"discoveredCount"`
	StartedAtUtc    time.Time  `json:"startedAtUtc"`
	CompletedAtUtc  *time.Time `json:"completedAtUtc,omitempty"`
}
