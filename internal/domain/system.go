package domain

import "time"

type RecordingControl struct {
	Enabled      bool      `json:"enabled"`
	UpdatedAtUtc time.Time `json:"updatedAtUtc"`
}
