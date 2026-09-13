package domain

import "time"

type RecordingControl struct {
	Enabled      bool      `json:"enabled"`
	UpdatedAtUtc time.Time `json:"updatedAtUtc"`
}

type ArchiveLayout string

const (
	ArchiveLayoutCameraDate ArchiveLayout = "CameraDate"
	ArchiveLayoutDateCamera ArchiveLayout = "DateCamera"
	ArchiveLayoutCamera     ArchiveLayout = "Camera"
	ArchiveLayoutDate       ArchiveLayout = "Date"
)

type ApplicationSettings struct {
	ArchiveRoot     string        `json:"archiveRoot,omitempty"`
	RecordingFolder string        `json:"recordingFolder"`
	ArchiveLayout   ArchiveLayout `json:"archiveLayout"`
	UpdatedAtUtc    time.Time     `json:"updatedAtUtc"`
}
