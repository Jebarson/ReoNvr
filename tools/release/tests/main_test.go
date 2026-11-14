package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const immutablePolicyFixture = `{"id":42,"target":"tag","enforcement":"active","bypass_actors":[],"conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},"rules":[{"type":"update","parameters":{"update_allows_fetch_and_merge":false}},{"type":"deletion"}]}`

func TestReleaseTagPolicyRequiresCompleteEnforcement(t *testing.T) {
	for _, scenario := range []string{"valid", "disabled", "evaluate", "branch", "wrong-pattern", "excluded-tag", "bypass", "hidden-bypass", "missing-deletion", "allows-merge", "unknown-update-permissions"} {
		t.Run(scenario, func(t *testing.T) {
			var policy releaseTagPolicy
			if err := json.Unmarshal([]byte(immutablePolicyFixture), &policy); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "disabled":
				policy.Enforcement = "disabled"
			case "evaluate":
				policy.Enforcement = "evaluate"
			case "branch":
				policy.Target = "branch"
			case "wrong-pattern":
				policy.Conditions.RefName.Include = []string{"refs/tags/v1.2.3"}
			case "excluded-tag":
				policy.Conditions.RefName.Exclude = []string{"refs/tags/v1.2.3"}
			case "bypass":
				actors := []json.RawMessage{json.RawMessage(`{"actor_type":"RepositoryRole","actor_id":5}`)}
				policy.BypassActors = &actors
			case "hidden-bypass":
				policy.BypassActors = nil
			case "missing-deletion":
				policy.Rules = policy.Rules[:1]
			case "allows-merge":
				*policy.Rules[0].Parameters.AllowsMerge = true
			case "unknown-update-permissions":
				policy.Rules[0].Parameters.AllowsMerge = nil
			}
			if policy.immutable() != (scenario == "valid") {
				t.Fatal("unsafe release tag policy accepted")
			}
		})
	}
}

func TestPublicationRechecksRemoteTagAndPolicy(t *testing.T) {
	revision := strings.Repeat("a", 40)
	for _, scenario := range []string{"lightweight", "annotated", "paginated", "moved", "deleted", "forbidden", "missing-policy", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/repos/owner/repo/rulesets":
					if scenario == "forbidden" {
						response.WriteHeader(http.StatusForbidden)
						return
					}
					if scenario == "missing-policy" {
						io.WriteString(response, "[]")
						return
					}
					if scenario == "paginated" && request.URL.Query().Get("page") == "1" {
						json.NewEncoder(response).Encode(make([]releaseTagPolicy, 100))
						return
					}
					io.WriteString(response, "["+immutablePolicyFixture+"]")
				case "/repos/owner/repo/rulesets/42":
					if scenario == "malformed" {
						io.WriteString(response, "{")
						return
					}
					io.WriteString(response, immutablePolicyFixture)
				case "/repos/owner/repo/git/ref/tags/v1.2.3":
					if scenario == "deleted" {
						response.WriteHeader(http.StatusNotFound)
						return
					}
					kind, target := "commit", revision
					if scenario == "annotated" {
						kind, target = "tag", strings.Repeat("b", 40)
					}
					if scenario == "moved" {
						target = strings.Repeat("c", 40)
					}
					json.NewEncoder(response).Encode(map[string]any{"object": map[string]string{"type": kind, "sha": target}})
				case "/repos/owner/repo/git/tags/" + strings.Repeat("b", 40):
					json.NewEncoder(response).Encode(map[string]any{"object": map[string]string{"type": "commit", "sha": revision}})
				default:
					response.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			err := checkPublication(server.Client(), server.URL, "test-token", "owner/repo", "v1.2.3", revision)
			valid := scenario == "lightweight" || scenario == "annotated" || scenario == "paginated"
			if (err == nil) != valid {
				t.Fatalf("incorrect publication result: %v", err)
			}
		})
	}
}

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
		"runtime/third-party-notices.tar",
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
	script, err := os.ReadFile(filepath.Join("..", "..", "packaging", "start.sh"))
	if err != nil {
		t.Fatal(err)
	}
	normalizedScript := strings.ReplaceAll(string(script), "\r\n", "\n")
	if err := os.WriteFile(filepath.Join(root, "packaging/start.sh"), []byte(strings.ReplaceAll(normalizedScript, "\n", "\r\n")), 0o640); err != nil {
		t.Fatal(err)
	}
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
		expected := []string{".env", "checksums.sha256", "compose.yaml", "images.tar", "images.txt", "release.json", "start.ps1", "start.sh", "THIRD-PARTY-LICENSES.tar"}
		if len(entries) != len(expected) || entries["runtime/test-config.json"] != nil || !strings.Contains(string(entries[".env"]), "REONVR_ARCH="+architecture) {
			t.Fatal("bundle includes unexpected data or wrong architecture")
		}
		for _, name := range expected {
			if _, found := entries[name]; !found {
				t.Fatalf("distribution asset missing: %s", name)
			}
		}
		if string(entries["THIRD-PARTY-LICENSES.tar"]) != "fixture" {
			t.Fatal("required third-party licenses missing or changed")
		}
		if !strings.Contains(string(entries[".env"]), "COMPOSE_PROJECT_NAME=reonvr-release\n") {
			t.Fatal("release project collides with source deployment")
		}
		if string(entries["start.sh"]) != normalizedScript {
			t.Fatal("packaged shell script is not normalized to LF")
		}
		var metadata struct {
			Version      string   `json:"version"`
			Architecture string   `json:"architecture"`
			Images       []string `json:"images"`
		}
		if err := json.Unmarshal(entries["release.json"], &metadata); err != nil {
			t.Fatal(err)
		}
		if metadata.Version != "v1.2.3-rc.1" || metadata.Architecture != architecture || len(metadata.Images) != 3 || string(entries["images.txt"]) != strings.Join(metadata.Images, "\n")+"\n" {
			t.Fatal("image manifest does not match release metadata")
		}
		for imageIndex, service := range []string{"app", "gateway", "discovery-relay"} {
			if metadata.Images[imageIndex] != "reonvr/"+service+":"+metadata.Version+"-"+architecture {
				t.Fatal("unexpected release image identity")
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
		extracted := t.TempDir()
		for name, payload := range entries {
			if err := os.WriteFile(filepath.Join(extracted, name), payload, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		mock := `docker() { case "$1" in info) printf "%s\n" "$TEST_ARCH";; compose) case "$*" in *--images*) cat images.txt;; *" up "*) printf "started\n";; esac;; load) printf "loaded\n";; esac; }; . ./start.sh`
		for _, tampered := range []bool{false, true} {
			if tampered {
				if err := os.WriteFile(filepath.Join(extracted, "images.tar"), []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command("/bin/sh", "-c", mock, "./start.sh")
			command.Dir = extracted
			command.Env = append(os.Environ(), "TEST_ARCH="+architecture)
			output, err := command.CombinedOutput()
			reachedStartup := strings.Contains(string(output), "loaded\n") || strings.Contains(string(output), "started\n")
			if tampered && (err == nil || reachedStartup) {
				t.Fatal("tampered extracted ZIP reached Docker operations")
			}
			if !tampered && (err != nil || !strings.Contains(string(output), "loaded\nstarted\n")) {
				t.Fatalf("valid extracted ZIP did not start: %v: %s", err, output)
			}
		}
	}
	if _, err := buildBundle(root, root, images, "v1.0.0", "armv7", strings.Repeat("a", 40)); err == nil {
		t.Fatal("unsupported architecture accepted")
	}
}
