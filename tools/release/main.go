package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

func validateVersion(version string) error {
	if len(version) > 100 || !versionPattern.MatchString(version) {
		return errors.New("version must be vMAJOR.MINOR.PATCH or vMAJOR.MINOR.PATCH-PRERELEASE")
	}
	if _, prerelease, found := strings.Cut(version, "-"); found {
		for _, identifier := range strings.Split(prerelease, ".") {
			if len(identifier) > 1 && identifier[0] == '0' && strings.Trim(identifier, "0123456789") == "" {
				return errors.New("numeric prerelease identifiers must not have leading zeroes")
			}
		}
	}
	return nil
}

func fileHash(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func buildBundle(root, output, images, version, architecture, revision string) (string, error) {
	if err := validateVersion(version); err != nil {
		return "", err
	}
	if architecture != "amd64" && architecture != "arm64" {
		return "", errors.New("architecture must be amd64 or arm64")
	}
	if matched, _ := regexp.MatchString(`^[0-9a-f]{40}$`, revision); !matched {
		return "", errors.New("revision must be a full lowercase Git commit SHA")
	}
	imageInfo, err := os.Stat(images)
	if err != nil {
		return "", err
	}
	if !imageInfo.Mode().IsRegular() || imageInfo.Size() == 0 {
		return "", errors.New("images archive must be a nonempty regular file")
	}
	if err := os.MkdirAll(output, 0o750); err != nil {
		return "", err
	}
	archivePath := filepath.Join(output, "ReoNvr-"+version+"-linux-"+architecture+".zip")
	file, err := os.Create(archivePath)
	if err != nil {
		return "", err
	}
	finished := false
	defer func() {
		file.Close()
		if !finished {
			os.Remove(archivePath)
		}
	}()
	writer := zip.NewWriter(file)
	defer writer.Close()
	checksums := make(map[string]string)
	writeEntry := func(name string, mode os.FileMode, reader io.Reader, checksum bool) error {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)}
		header.SetMode(mode)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		hash := sha256.New()
		if _, err := io.Copy(io.MultiWriter(entry, hash), reader); err != nil {
			return err
		}
		if checksum {
			checksums[name] = hex.EncodeToString(hash.Sum(nil))
		}
		return nil
	}
	for _, asset := range []struct{ name, source string }{
		{"compose.yaml", "packaging/compose.yaml"},
		{"start.sh", "packaging/start.sh"},
		{"start.ps1", "packaging/start.ps1"},
		{"README.md", "README.md"},
		{"docs/CONTRIBUTING.md", "docs/CONTRIBUTING.md"},
		{"docs/APPLICATION-SPEC.md", "docs/APPLICATION-SPEC.md"},
		{"docs/REOLINK-CAMERA-API-LEARNINGS.md", "docs/REOLINK-CAMERA-API-LEARNINGS.md"},
		{"THIRD-PARTY-NOTICES.md", "packaging/THIRD-PARTY-NOTICES.md"},
	} {
		payload, err := os.ReadFile(filepath.Join(root, asset.source))
		if err != nil {
			return "", err
		}
		mode := os.FileMode(0o644)
		if asset.name == "start.sh" {
			mode = 0o755
		}
		if err := writeEntry(asset.name, mode, strings.NewReader(string(payload)), true); err != nil {
			return "", err
		}
	}
	configuration := fmt.Sprintf("REONVR_VERSION=%s\nREONVR_ARCH=%s\nCOMPOSE_PROJECT_NAME=reonvr\nREONVR_BIND_ADDRESS=127.0.0.1\nREONVR_HTTP_PORT=8080\nREONVR_DISCOVERY_PORT=3000\n# REONVR_RECORDINGS_PATH=/absolute/path/to/recordings\n", version, architecture)
	if err := writeEntry(".env", 0o644, strings.NewReader(configuration), false); err != nil {
		return "", err
	}
	metadata, err := json.MarshalIndent(struct {
		Version      string `json:"version"`
		Architecture string `json:"architecture"`
		Revision     string `json:"revision"`
	}{version, architecture, revision}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeEntry("release.json", 0o644, strings.NewReader(string(metadata)+"\n"), true); err != nil {
		return "", err
	}
	imageFile, err := os.Open(images)
	if err != nil {
		return "", err
	}
	err = writeEntry("images.tar", 0o644, imageFile, true)
	imageFile.Close()
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(checksums))
	for name := range checksums {
		names = append(names, name)
	}
	sort.Strings(names)
	var listing strings.Builder
	for _, name := range names {
		fmt.Fprintf(&listing, "%s  %s\n", checksums[name], name)
	}
	if err := writeEntry("checksums.sha256", 0o644, strings.NewReader(listing.String()), false); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	digest, err := fileHash(archivePath)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(archivePath+".sha256", []byte(digest+"  "+filepath.Base(archivePath)+"\n"), 0o644); err != nil {
		return "", err
	}
	finished = true
	return archivePath, nil
}

func main() {
	version := flag.String("version", "", "SemVer release tag including v prefix")
	architecture := flag.String("arch", "", "amd64 or arm64")
	revision := flag.String("revision", "", "full Git revision")
	images := flag.String("images", "", "docker save archive")
	output := flag.String("output", "runtime/dist", "output directory")
	root := flag.String("root", ".", "repository root")
	checkVersion := flag.Bool("check-version", false, "validate version without creating a bundle")
	flag.Parse()
	if *checkVersion {
		if err := validateVersion(*version); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(*version)
		return
	}
	path, err := buildBundle(*root, *output, *images, *version, *architecture, *revision)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(path)
}
