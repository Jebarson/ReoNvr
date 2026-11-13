package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseVersionValidation(t *testing.T) {
	for _, version := range []string{"v0.0.0", "v1.2.3", "v1.2.3-rc.1", "v1.2.3-ci.123"} {
		if err := validateVersion(version); err != nil {
			t.Fatalf("valid release rejected: %s: %v", version, err)
		}
	}
	for _, version := range []string{"", "1.2.3", "v01.2.3", "v1.2", "v1.2.3-01", "v1.2.3+build", "v1.2.3;exit", "v1.2.3-rc.01"} {
		if err := validateVersion(version); err == nil {
			t.Fatalf("invalid release accepted: %s", version)
		}
	}
}

func TestReleaseBundleIncludesOnlyDistributionFilesAndChecksums(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		"packaging/compose.yaml", "packaging/start.sh", "packaging/start.ps1", "packaging/THIRD-PARTY-NOTICES.md", "README.md",
		"docs/images/cameras.png", "docs/images/automatic-detection.png", "docs/images/camera-setup.png",
		"docs/CONTRIBUTING.md", "docs/APPLICATION-SPEC.md", "docs/REOLINK-CAMERA-API-LEARNINGS.md", "runtime/test-config.json",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	images := filepath.Join(root, "images.tar")
	if err := os.WriteFile(images, []byte("native Docker images"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, architecture := range []string{"amd64", "arm64"} {
		path, err := buildBundle(root, filepath.Join(root, "output"), images, "v1.2.3-rc.1", architecture, strings.Repeat("a", 40))
		if err != nil {
			t.Fatal(err)
		}
		archive, err := zip.OpenReader(path)
		if err != nil {
			t.Fatal(err)
		}
		entries := make(map[string][]byte)
		for _, entry := range archive.File {
			reader, err := entry.Open()
			if err != nil {
				t.Fatal(err)
			}
			entries[entry.Name], err = io.ReadAll(reader)
			reader.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		archive.Close()
		if len(entries) != 15 || entries["runtime/test-config.json"] != nil || !strings.Contains(string(entries[".env"]), "REONVR_ARCH="+architecture) {
			t.Fatal("bundle includes unexpected data or wrong architecture")
		}
		for _, name := range []string{"docs/images/cameras.png", "docs/images/automatic-detection.png", "docs/images/camera-setup.png"} {
			if string(entries[name]) != "fixture" {
				t.Fatalf("manual screenshot missing or changed: %s", name)
			}
		}
		for _, line := range strings.Split(strings.TrimSpace(string(entries["checksums.sha256"])), "\n") {
			digest, name, found := strings.Cut(line, "  ")
			actual := sha256.Sum256(entries[name])
			if !found || digest != hex.EncodeToString(actual[:]) {
				t.Fatalf("invalid bundled checksum: %s", name)
			}
		}
		digest, err := fileHash(path)
		if err != nil {
			t.Fatal(err)
		}
		sidecar, err := os.ReadFile(path + ".sha256")
		if err != nil || !strings.HasPrefix(string(sidecar), digest+"  ") {
			t.Fatal("ZIP checksum is missing or incorrect")
		}
	}
	if _, err := buildBundle(root, root, images, "v1.0.0", "armv7", strings.Repeat("a", 40)); err == nil {
		t.Fatal("unsupported architecture accepted")
	}
}
