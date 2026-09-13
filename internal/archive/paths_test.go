package archive

import (
	"path/filepath"
	"testing"
	"time"

	"reonavr/internal/domain"
)

func TestArchiveRelativePathLayouts(t *testing.T) {
	camera := domain.Camera{ID: "a35b97be-59a3-4ce4-8302-facf86b80d48", Name: "Front Camera"}
	recording := domain.Recording{
		ID: "recording-one", NativeName: "Mp4Record/0120260911194542",
		Source:     domain.RecordingSourceCameraStorage,
		StartAtUtc: time.Date(2026, 9, 12, 2, 45, 42, 0, time.UTC),
	}
	tests := map[domain.ArchiveLayout]string{
		domain.ArchiveLayoutCameraDate: filepath.Join("recordings", "Front-Camera-a35b97be", "2026", "09", "12", "0120260911194542.mp4"),
		domain.ArchiveLayoutDateCamera: filepath.Join("recordings", "2026", "09", "12", "Front-Camera-a35b97be", "0120260911194542.mp4"),
		domain.ArchiveLayoutCamera:     filepath.Join("recordings", "Front-Camera-a35b97be", "0120260911194542.mp4"),
		domain.ArchiveLayoutDate:       filepath.Join("recordings", "2026", "09", "12", "Front-Camera-a35b97be-0120260911194542.mp4"),
	}
	for layout, expected := range tests {
		actual, err := archiveRelativePath(camera, recording, domain.ApplicationSettings{RecordingFolder: "recordings", ArchiveLayout: layout})
		if err != nil {
			t.Fatalf("%s: %v", layout, err)
		}
		if actual != expected {
			t.Fatalf("%s: expected %q, got %q", layout, expected, actual)
		}
	}
}

func TestArchiveRelativePathLayoutsForRtsp(t *testing.T) {
	camera := domain.Camera{ID: "a35b97be-59a3-4ce4-8302-facf86b80d48", Name: "Front Camera"}
	recording := domain.Recording{
		ID: "recording-one", NativeName: "Rtsp/2026/09/12/20260912T024542.000Z-recording.mkv",
		Source:     domain.RecordingSourceRtsp,
		StartAtUtc: time.Date(2026, 9, 12, 2, 45, 42, 0, time.UTC),
	}
	tests := map[domain.ArchiveLayout]string{
		domain.ArchiveLayoutCameraDate: filepath.Join("recordings", "Front-Camera-a35b97be", "2026", "09", "12", "20260912T024542.000Z-recording.mkv"),
		domain.ArchiveLayoutDateCamera: filepath.Join("recordings", "2026", "09", "12", "Front-Camera-a35b97be", "20260912T024542.000Z-recording.mkv"),
		domain.ArchiveLayoutCamera:     filepath.Join("recordings", "Front-Camera-a35b97be", "20260912T024542.000Z-recording.mkv"),
		domain.ArchiveLayoutDate:       filepath.Join("recordings", "2026", "09", "12", "Front-Camera-a35b97be-20260912T024542.000Z-recording.mkv"),
	}
	for layout, expected := range tests {
		actual, err := archiveRelativePath(camera, recording, domain.ApplicationSettings{RecordingFolder: "recordings", ArchiveLayout: layout})
		if err != nil {
			t.Fatalf("%s: %v", layout, err)
		}
		if actual != expected {
			t.Fatalf("%s: expected %q, got %q", layout, expected, actual)
		}
	}
}

func TestArchiveRelativePathRejectsUnsafeInput(t *testing.T) {
	camera := domain.Camera{ID: "camera-one", Name: "Front"}
	recording := domain.Recording{NativeName: "../secret.mp4", Source: domain.RecordingSourceCameraStorage, StartAtUtc: time.Now()}
	if _, err := archiveRelativePath(camera, recording, domain.ApplicationSettings{RecordingFolder: ".", ArchiveLayout: domain.ArchiveLayoutCameraDate}); err == nil {
		t.Fatal("expected unsafe native path rejection")
	}
	recording.NativeName = "clip.mp4"
	if _, err := archiveRelativePath(camera, recording, domain.ApplicationSettings{RecordingFolder: "../outside", ArchiveLayout: domain.ArchiveLayoutCameraDate}); err == nil {
		t.Fatal("expected unsafe recording folder rejection")
	}
}
