package archive

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"reonavr/internal/domain"
)

const coverageTolerance = 2 * time.Second

type VideoInspection struct {
	Duration time.Duration
	Coverage []domain.TimeRange
}

type MediaInspector interface {
	InspectVideo(context.Context, string) (VideoInspection, error)
}

type coverageWriter interface {
	SaveRecordingCoverage(context.Context, string, []domain.TimeRange) error
	SaveCaptureCoverage(context.Context, string, domain.ArchiveStream, []domain.TimeRange) error
}

func (segmenter *FFmpegSegmenter) InspectVideo(ctx context.Context, path string) (VideoInspection, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, segmenter.probeExecutable(), "-v", "error", "-select_streams", "v:0",
		"-show_packets", "-show_entries", "packet=pts_time,duration_time,flags", "-of", "json", path)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return VideoInspection{}, err
	}
	if err := command.Start(); err != nil {
		return VideoInspection{}, errors.New("start video coverage probe")
	}
	inspection, inspectErr := inspectVideoPackets(io.LimitReader(stdout, 64*1024*1024))
	if inspectErr != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
	if err := command.Wait(); err != nil {
		return VideoInspection{}, errors.New("video coverage probe failed")
	}
	return inspection, inspectErr
}

func inspectVideoPackets(reader io.Reader) (VideoInspection, error) {
	var payload struct {
		Packets []struct {
			PTS      string `json:"pts_time"`
			Duration string `json:"duration_time"`
			Flags    string `json:"flags"`
		} `json:"packets"`
	}
	if err := json.NewDecoder(reader).Decode(&payload); err != nil {
		return VideoInspection{}, errors.New("invalid video packet metadata")
	}
	type packet struct {
		start, end time.Duration
		keyframe   bool
	}
	var packets []packet
	for _, item := range payload.Packets {
		pts, err := strconv.ParseFloat(item.PTS, 64)
		if err != nil || math.IsNaN(pts) || math.IsInf(pts, 0) || math.Abs(pts) > 4e9 {
			return VideoInspection{}, errors.New("video packet has no valid presentation timestamp")
		}
		duration, err := strconv.ParseFloat(item.Duration, 64)
		if err != nil || duration <= 0 || duration > 2 || math.IsNaN(duration) || math.IsInf(duration, 0) {
			duration = 0
		}
		packets = append(packets, packet{start: time.Duration(pts * float64(time.Second)), end: time.Duration((pts + duration) * float64(time.Second)), keyframe: strings.Contains(item.Flags, "K")})
	}
	if len(packets) < 2 {
		return VideoInspection{}, errors.New("recording has insufficient video packets")
	}
	sort.Slice(packets, func(first, second int) bool { return packets[first].start < packets[second].start })
	origin := packets[0].start
	duration := packets[len(packets)-1].end - origin
	if duration <= 0 || duration > 24*time.Hour {
		return VideoInspection{}, errors.New("recording has an invalid video duration")
	}
	inspection := VideoInspection{Duration: duration}
	var current *domain.TimeRange
	for _, item := range packets {
		start, end := item.start-origin, item.end-origin
		if current != nil && start-time.Duration(current.EndAtUtc.UnixNano()) > coverageTolerance {
			current = nil
		}
		if current == nil {
			if !item.keyframe {
				continue
			}
			inspection.Coverage = append(inspection.Coverage, domain.TimeRange{StartAtUtc: time.Unix(0, int64(start)).UTC(), EndAtUtc: time.Unix(0, int64(end)).UTC()})
			current = &inspection.Coverage[len(inspection.Coverage)-1]
		} else if end > time.Duration(current.EndAtUtc.UnixNano()) {
			current.EndAtUtc = time.Unix(0, int64(end)).UTC()
		}
	}
	if len(inspection.Coverage) == 0 {
		return VideoInspection{}, errors.New("recording has no independently decodable video")
	}
	return inspection, nil
}

func absoluteCoverage(inspection VideoInspection, start, end time.Time) []domain.TimeRange {
	var ranges []domain.TimeRange
	for _, span := range inspection.Coverage {
		from := start.Add(time.Duration(span.StartAtUtc.UnixNano()))
		to := start.Add(time.Duration(span.EndAtUtc.UnixNano()))
		if to.After(end) {
			to = end
		}
		if from.Before(to) {
			ranges = append(ranges, domain.TimeRange{StartAtUtc: from, EndAtUtc: to})
		}
	}
	return ranges
}
