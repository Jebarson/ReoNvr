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
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	if err := os.MkdirAll(output, 0o755); err != nil {
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
		{"THIRD-PARTY-LICENSES.tar", "runtime/third-party-notices.tar"},
	} {
		payload, err := os.ReadFile(filepath.Join(root, asset.source))
		if err != nil {
			return "", err
		}
		mode := os.FileMode(0o644)
		if asset.name == "start.sh" {
			mode = 0o755
			payload = []byte(strings.ReplaceAll(string(payload), "\r\n", "\n"))
		}
		if err := writeEntry(asset.name, mode, strings.NewReader(string(payload)), true); err != nil {
			return "", err
		}
	}
	configuration := fmt.Sprintf("REONVR_VERSION=%s\nREONVR_ARCH=%s\nCOMPOSE_PROJECT_NAME=reonvr-release\nREONVR_BIND_ADDRESS=127.0.0.1\nREONVR_HTTP_PORT=8080\nREONVR_DISCOVERY_PORT=3000\n# COMPOSE_PROFILES=discovery\n# REONVR_RECORDINGS_PATH=/absolute/path/to/recordings\n", version, architecture)
	if err := writeEntry(".env", 0o644, strings.NewReader(configuration), false); err != nil {
		return "", err
	}
	release := struct {
		Version      string   `json:"version"`
		Architecture string   `json:"architecture"`
		Revision     string   `json:"revision"`
		Images       []string `json:"images"`
	}{version, architecture, revision, []string{"reonvr/app:" + version + "-" + architecture, "reonvr/gateway:" + version + "-" + architecture, "reonvr/discovery-relay:" + version + "-" + architecture}}
	metadata, err := json.MarshalIndent(release, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeEntry("release.json", 0o644, strings.NewReader(string(metadata)+"\n"), true); err != nil {
		return "", err
	}
	if err := writeEntry("images.txt", 0o644, strings.NewReader(strings.Join(release.Images, "\n")+"\n"), true); err != nil {
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

type releaseTagPolicy struct {
	ID           int                `json:"id"`
	Target       string             `json:"target"`
	Enforcement  string             `json:"enforcement"`
	BypassActors *[]json.RawMessage `json:"bypass_actors"`
	Conditions   struct {
		RefName struct {
			Include []string `json:"include"`
			Exclude []string `json:"exclude"`
		} `json:"ref_name"`
	} `json:"conditions"`
	Rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			AllowsMerge *bool `json:"update_allows_fetch_and_merge"`
		} `json:"parameters"`
	} `json:"rules"`
}

func (policy releaseTagPolicy) immutable() bool {
	if policy.Target != "tag" || policy.Enforcement != "active" || policy.BypassActors == nil || len(*policy.BypassActors) != 0 || len(policy.Conditions.RefName.Exclude) != 0 {
		return false
	}
	if !slices.Contains(policy.Conditions.RefName.Include, "refs/tags/v*") {
		return false
	}
	updatesBlocked, deletionsBlocked := false, false
	for _, rule := range policy.Rules {
		if rule.Type == "update" && rule.Parameters.AllowsMerge != nil && !*rule.Parameters.AllowsMerge {
			updatesBlocked = true
		}
		if rule.Type == "deletion" {
			deletionsBlocked = true
		}
	}
	return updatesBlocked && deletionsBlocked
}

func checkPublication(client *http.Client, api, token, repository, version, revision string, requireTagPolicy bool) error {
	if err := validateVersion(version); err != nil {
		return err
	}
	if token == "" || !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repository) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(revision) {
		return errors.New("publication requires a token, repository and tested full revision")
	}
	read := func(path string, target any) error {
		request, err := http.NewRequest(http.MethodGet, api+"/repos/"+repository+path, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
		request.Header.Set("Cache-Control", "no-cache")
		response, err := client.Do(request)
		if err != nil {
			return errors.New("unable to read GitHub publication state")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("GitHub publication check failed (HTTP %d)", response.StatusCode)
		}
		payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			return err
		}
		return json.Unmarshal(payload, target)
	}
	if requireTagPolicy {
		protected := false
		for page := 1; page <= 100; page++ {
			var policies []releaseTagPolicy
			if err := read(fmt.Sprintf("/rulesets?targets=tag&includes_parents=true&per_page=100&page=%d", page), &policies); err != nil {
				return err
			}
			for _, summary := range policies {
				if summary.Target != "tag" || summary.Enforcement != "active" {
					continue
				}
				var policy releaseTagPolicy
				if err := read(fmt.Sprintf("/rulesets/%d", summary.ID), &policy); err != nil {
					return err
				}
				if policy.immutable() {
					protected = true
					break
				}
			}
			if protected || len(policies) < 100 {
				break
			}
		}
		if !protected {
			return errors.New("publication requires a verified active tag update/deletion ruleset with no bypass actors; policy permissions must be visible")
		}
	}
	var reference struct {
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if err := read("/git/ref/tags/"+version, &reference); err != nil {
		return err
	}
	for depth := 0; reference.Object.Type == "tag" && depth < 8; depth++ {
		if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(reference.Object.SHA) {
			return errors.New("invalid remote tag object")
		}
		if err := read("/git/tags/"+reference.Object.SHA, &reference); err != nil {
			return err
		}
	}
	if reference.Object.Type != "commit" || reference.Object.SHA != revision {
		return errors.New("remote release tag no longer matches the tested revision")
	}
	return nil
}

func main() {
	version := flag.String("version", "", "SemVer release tag including v prefix")
	architecture := flag.String("arch", "", "amd64 or arm64")
	revision := flag.String("revision", "", "full Git revision")
	images := flag.String("images", "", "docker save archive")
	output := flag.String("output", "runtime/dist", "output directory")
	root := flag.String("root", ".", "repository root")
	checkVersion := flag.Bool("check-version", false, "validate version without creating a bundle")
	publication := flag.Bool("check-publication", false, "verify remote tag and required policy before publication")
	requireTagPolicy := flag.Bool("require-tag-policy", true, "require an immutable release-tag ruleset before publication")
	repository := flag.String("repository", "", "GitHub owner/repository for publication")
	flag.Parse()
	if *publication {
		if err := checkPublication(&http.Client{Timeout: 20 * time.Second}, "https://api.github.com", os.Getenv("GITHUB_TOKEN"), *repository, *version, *revision, *requireTagPolicy); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("Remote tag and publication requirements verified")
		return
	}
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
