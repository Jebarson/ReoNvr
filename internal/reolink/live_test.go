package reolink

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/testconfig"
)

func TestLiveCameraStorage(test *testing.T) {
	if os.Getenv("REONVR_E2E_LIVE") != "1" {
		test.Skip("opt-in container-only live test; credentials must arrive over stdin")
	}
	var config testconfig.Config
	if json.NewDecoder(os.Stdin).Decode(&config) != nil || config.ValidateLive() != nil {
		test.Fatal("valid local test configuration must arrive over stdin")
	}
	probeCamera, _ := config.Camera(config.ProbeCamera)
	storageCamera, _ := config.Camera(config.StorageCamera)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	runner := &ExecRunner{Path: "/usr/local/bin/reolink-cli", GatewayAddr: "reonvr-fresh-gateway:9000"}
	client := NewClient(runner, 20*time.Second)
	connection := Connection{Host: probeCamera.Host, Port: probeCamera.Port, Channel: probeCamera.Channel, Stream: domain.StreamMain, Username: config.Username, Password: config.Password}
	result, err := client.Test(ctx, connection)
	if err != nil {
		test.Fatal("configured probe camera authentication failed")
	}
	connection.Timezone = result.Timezone
	test.Run("actual camera time is readable", func(test *testing.T) {
		payload, err := client.run(ctx, connection, "config", "get", "system-general")
		if err != nil {
			test.Fatal("read-only camera time query failed")
		}
		var decoded any
		if json.Unmarshal(payload, &decoded) != nil {
			test.Fatal("camera time JSON was malformed")
		}
		if _, ok := cameraCurrentTime(decoded); !ok {
			test.Fatal("camera current time was not decoded")
		}
	})
	test.Run("native SD search download and codec inspection", func(test *testing.T) {
		storageConnection := connection
		storageConnection.Host = storageCamera.Host
		storageConnection.Port = storageCamera.Port
		storageConnection.Channel = storageCamera.Channel
		storageProbe, err := client.Test(ctx, storageConnection)
		if err != nil {
			test.Fatal("storage camera authentication failed")
		}
		storageConnection.Timezone = storageProbe.Timezone
		now := time.Now().UTC()
		recordings, err := client.SearchRecordings(ctx, storageConnection, domain.StreamMain, now.Add(-6*time.Hour), now)
		if err != nil {
			test.Fatal("read-only camera storage search failed")
		}
		var chosen *Recording
		test.Logf("storage=%t search=%t native clips=%d", storageProbe.Capabilities.StorageAvailable, storageProbe.Capabilities.StoragePlaybackAvailable, len(recordings))
		for index := range recordings {
			duration := recordings[index].EndAtUtc.Sub(recordings[index].StartAtUtc)
			if (recordings[index].SizeBytes > 0 && recordings[index].SizeBytes <= 16*1024*1024) ||
				(recordings[index].SizeBytes == 0 && duration > 0 && duration <= 30*time.Second) {
				chosen = &recordings[index]
				break
			}
		}
		if chosen == nil {
			test.Skip("no bounded native clip in the last six hours; SD settings are never changed to manufacture footage")
		}
		path := filepath.Join(test.TempDir(), "native-camera-clip")
		if client.DownloadRecording(ctx, storageConnection, chosen.NativeName, domain.StreamMain, path) != nil {
			test.Fatal("native camera storage download failed")
		}
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			test.Fatal("camera storage returned empty media")
		}
		payload, err := exec.CommandContext(ctx, "/usr/bin/ffprobe", "-v", "error", "-show_entries", "stream=codec_name,codec_type:format=duration", "-of", "json", path).Output()
		if err != nil {
			test.Fatal("downloaded native media could not be inspected")
		}
		var media struct {
			Streams []struct {
				Codec string `json:"codec_name"`
				Kind  string `json:"codec_type"`
			} `json:"streams"`
		}
		if json.Unmarshal(payload, &media) != nil {
			test.Fatal("invalid native media inspection")
		}
		for _, stream := range media.Streams {
			if stream.Kind == "video" && (stream.Codec == "h264" || stream.Codec == "hevc") {
				test.Logf("native %s clip downloaded and inspected (%d bytes); no transcoding or SD deletion", stream.Codec, info.Size())
				return
			}
		}
		test.Fatal("download did not contain a supported native video stream")
	})
}
