package archive

import (
	"context"
	"errors"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"reonavr/internal/domain"
)

type ApplicationSettingsProvider interface {
	GetApplicationSettings(context.Context) (domain.ApplicationSettings, error)
}

func loadApplicationSettings(ctx context.Context, provider ApplicationSettingsProvider) (domain.ApplicationSettings, error) {
	if provider == nil {
		return domain.ApplicationSettings{RecordingFolder: ".", ArchiveLayout: domain.ArchiveLayoutCameraDate}, nil
	}
	return provider.GetApplicationSettings(ctx)
}

func archiveRelativePath(camera domain.Camera, recording domain.Recording, settings domain.ApplicationSettings) (string, error) {
	normalized := strings.ReplaceAll(strings.TrimSpace(recording.NativeName), "\\", "/")
	cleanedName := path.Clean(normalized)
	if cleanedName == "." || path.IsAbs(cleanedName) || cleanedName == ".." || strings.HasPrefix(cleanedName, "../") {
		return "", errors.New("camera returned an unsafe recording name")
	}
	fileName := path.Base(cleanedName)
	if filepath.Ext(fileName) == "" && recording.Source == domain.RecordingSourceCameraStorage {
		fileName += ".mp4"
	}
	folder := path.Clean(strings.ReplaceAll(strings.TrimSpace(settings.RecordingFolder), "\\", "/"))
	if folder == "" {
		folder = "."
	}
	if path.IsAbs(folder) || folder == ".." || strings.HasPrefix(folder, "../") {
		return "", errors.New("recording folder escaped the archive root")
	}
	cameraFolder := safeCameraFolder(camera)
	date := recording.StartAtUtc.UTC()
	dateFolders := []string{date.Format("2006"), date.Format("01"), date.Format("02")}
	parts := make([]string, 0, 6)
	if folder != "." {
		parts = append(parts, folder)
	}
	switch settings.ArchiveLayout {
	case domain.ArchiveLayoutCameraDate:
		parts = append(parts, cameraFolder)
		parts = append(parts, dateFolders...)
	case domain.ArchiveLayoutDateCamera:
		parts = append(parts, dateFolders...)
		parts = append(parts, cameraFolder)
	case domain.ArchiveLayoutCamera:
		parts = append(parts, cameraFolder)
	case domain.ArchiveLayoutDate:
		parts = append(parts, dateFolders...)
		fileName = cameraFolder + "-" + fileName
	default:
		return "", errors.New("unknown archive layout")
	}
	parts = append(parts, fileName)
	return filepath.FromSlash(path.Join(parts...)), nil
}

func safeCameraFolder(camera domain.Camera) string {
	var builder strings.Builder
	lastSeparator := false
	for _, character := range strings.TrimSpace(camera.Name) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '_' || character == '-' {
			builder.WriteRune(character)
			lastSeparator = false
		} else if !lastSeparator {
			builder.WriteByte('-')
			lastSeparator = true
		}
	}
	name := strings.Trim(builder.String(), "-._ ")
	if name == "" {
		name = "Camera"
	}
	id := strings.ReplaceAll(camera.ID, "-", "")
	if len(id) > 8 {
		id = id[:8]
	}
	if id == "" {
		return name
	}
	return name + "-" + id
}
