package reolink

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

const cameraClockPrefix = "Camera/"

func approximateCameraClock(payload []byte, configuration TimezoneConfiguration, started, finished time.Time) TimezoneConfiguration {
	configuration.Estimated = true
	configuration.DstEnabled = false
	if offset, ok := observedCameraOffset(payload, started, finished); ok {
		configuration.BaseOffsetSeconds = offset
	}
	return configuration
}

func observedCameraOffset(payload []byte, started, finished time.Time) (int, bool) {
	var decoded any
	if json.Unmarshal(payload, &decoded) != nil {
		return 0, false
	}
	cameraTime, ok := cameraCurrentTime(decoded)
	if !ok {
		return 0, false
	}
	midpoint := started.Add(finished.Sub(started) / 2).UTC()
	offset := cameraTime.Sub(midpoint).Round(15 * time.Minute)
	if offset >= -14*time.Hour && offset <= 14*time.Hour {
		return int(offset / time.Second), true
	}
	return 0, false
}

func priorCameraClockCompatible(name string, baseOffset int, baseKnown bool, payload []byte, started, finished time.Time) bool {
	if name == "" {
		return false
	}
	location, err := cameraLocation(name)
	if err != nil {
		return false
	}
	configuration, customErr := decodeCameraClock(name)
	_, currentKnown := observedCameraOffset(payload, started, finished)
	if CameraClockEstimated(name) {
		if currentKnown {
			return false
		}
		return !baseKnown || configuration.EstimatedBaseOffsetSeconds == nil || *configuration.EstimatedBaseOffsetSeconds == baseOffset
	}
	if baseKnown {
		priorBase := configuration.BaseOffsetSeconds
		if customErr != nil {
			instant := finished.In(location)
			for transitions := 0; instant.IsDST(); transitions++ {
				_, next := instant.ZoneBounds()
				if next.IsZero() || transitions >= 8 {
					return false
				}
				instant = next.In(location)
			}
			_, priorBase = instant.Zone()
		}
		if priorBase != baseOffset {
			return false
		}
	}
	return !currentKnown || !cameraClockDiffers(payload, name, started, finished)
}

func cameraClockDiffers(payload []byte, timezone string, started, finished time.Time) bool {
	location, err := cameraLocation(timezone)
	if err != nil {
		return false
	}
	var decoded any
	if json.Unmarshal(payload, &decoded) != nil {
		return false
	}
	cameraTime, ok := cameraCurrentTime(decoded)
	if !ok {
		return false
	}
	for _, instant := range []time.Time{started, finished} {
		local := instant.In(location)
		expected := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), 0, time.UTC)
		difference := cameraTime.Sub(expected)
		if difference >= -time.Minute && difference <= time.Minute {
			return false
		}
	}
	return true
}

func cameraCurrentTime(value any) (time.Time, bool) {
	switch fields := value.(type) {
	case map[string]any:
		year, yearOK := objectInt(fields, "year")
		month, monthOK := objectInt(fields, "month", "mon")
		day, dayOK := objectInt(fields, "day")
		hour, hourOK := objectInt(fields, "hour")
		minute, minuteOK := objectInt(fields, "minute", "min")
		second, secondOK := objectInt(fields, "second", "sec")
		if yearOK && monthOK && dayOK && hourOK && minuteOK && secondOK {
			instant := time.Date(year, time.Month(month), day, hour, minute, second, 0, time.UTC)
			valid := year >= 1 && year <= 9999 && instant.Year() == year && int(instant.Month()) == month && instant.Day() == day && instant.Hour() == hour && instant.Minute() == minute && instant.Second() == second
			return instant, valid
		}
		for _, item := range fields {
			if instant, ok := cameraCurrentTime(item); ok {
				return instant, true
			}
		}
	case []any:
		for _, item := range fields {
			if instant, ok := cameraCurrentTime(item); ok {
				return instant, true
			}
		}
	}
	return time.Time{}, false
}

func customCameraTimezone(configuration TimezoneConfiguration) (string, error) {
	payload, err := json.Marshal(configuration)
	if err != nil {
		return "", err
	}
	name := cameraClockPrefix + base64.RawURLEncoding.EncodeToString(payload)
	_, err = cameraLocation(name)
	return name, err
}

func decodeCameraClock(name string) (TimezoneConfiguration, error) {
	var configuration TimezoneConfiguration
	if len(name) > 4096 || !strings.HasPrefix(name, cameraClockPrefix) {
		return configuration, errors.New("camera clock rules are unavailable")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(name, cameraClockPrefix))
	if err != nil || json.Unmarshal(payload, &configuration) != nil {
		return configuration, errors.New("camera clock rules are unavailable")
	}
	return configuration, nil
}

func CameraEndpointIdentity(host string, port, channel int) string {
	payload, _ := json.Marshal(struct {
		Host    string
		Port    int
		Channel int
	}{strings.ToLower(strings.TrimSpace(host)), port, channel})
	digest := sha256.Sum256(payload)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func CameraLocation(name string) (*time.Location, error) {
	return cameraLocation(name)
}

func CameraClockEstimated(name string) bool {
	if !strings.HasPrefix(name, cameraClockPrefix) {
		return false
	}
	configuration, err := decodeCameraClock(name)
	return err != nil || configuration.Estimated
}

func CameraRecordingTimes(name string, payload []byte) (time.Time, time.Time, error) {
	var metadata struct {
		Start string `json:"startTime"`
		End   string `json:"endTime"`
	}
	if err := json.Unmarshal(payload, &metadata); err != nil {
		return time.Time{}, time.Time{}, errors.New("invalid camera recording metadata")
	}
	start, startQualified, err := parseRecordedCameraTime(name, metadata.Start)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, endQualified, err := parseRecordedCameraTime(name, metadata.End)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !start.Before(end) {
		return time.Time{}, time.Time{}, errors.New("invalid camera recording time range")
	}
	if !startQualified {
		if err := ValidateCameraTimeRange(name, start, start.Add(time.Nanosecond)); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if !endQualified {
		if err := ValidateCameraTimeRange(name, end, end.Add(time.Nanosecond)); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	return start, end, nil
}

func parseRecordedCameraTime(name, value string) (time.Time, bool, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC(), true, nil
	}
	location, err := cameraLocation(name)
	if err != nil {
		return time.Time{}, false, err
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		wall, err := time.Parse(layout, value)
		if err != nil {
			continue
		}
		parsed, err := time.ParseInLocation(layout, value, location)
		if err != nil {
			continue
		}
		local := parsed.In(location)
		roundTrip := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), time.UTC)
		if !roundTrip.Equal(wall) {
			return time.Time{}, false, errors.New("camera recording wall time does not exist")
		}
		return parsed.UTC(), false, nil
	}
	return time.Time{}, false, errors.New("unsupported camera recording timestamp")
}

func ValidateCameraTimeRange(name string, from, to time.Time) error {
	if !from.Before(to) {
		return errors.New("invalid camera recording time range")
	}
	if err := validateCameraSearchClock(name, from, to); err != nil {
		return err
	}
	location, err := cameraLocation(name)
	if err != nil {
		return err
	}
	for cursor, limit := from.Add(-24*time.Hour), to.Add(24*time.Hour); cursor.Before(limit); {
		_, offset := cursor.In(location).Zone()
		_, next := cursor.In(location).ZoneBounds()
		if next.IsZero() || next.After(limit) {
			break
		}
		_, nextOffset := next.In(location).Zone()
		repeated := time.Duration(offset-nextOffset) * time.Second
		if repeated > 0 && from.Before(next.Add(repeated)) && to.After(next.Add(-repeated)) {
			return errors.New("camera recording timestamps are ambiguous during the daylight-saving clock change")
		}
		cursor = next
	}
	return nil
}

func cameraLocation(name string) (*time.Location, error) {
	if !strings.HasPrefix(name, cameraClockPrefix) {
		return time.LoadLocation(name)
	}
	configuration, err := decodeCameraClock(name)
	if err != nil {
		return nil, err
	}
	if configuration.BaseOffsetSeconds < -14*3600 || configuration.BaseOffsetSeconds > 14*3600 {
		return nil, errors.New("camera clock offset is outside its valid range")
	}
	if !configuration.DstEnabled {
		return time.FixedZone("Camera clock", configuration.BaseOffsetSeconds), nil
	}
	if configuration.DstOffsetSeconds < 3600 || configuration.DstOffsetSeconds > 3*3600 {
		return nil, errors.New("camera daylight-saving offset is outside its valid range")
	}
	for _, rule := range []DstTransition{configuration.DstStart, configuration.DstEnd} {
		if rule.Hour < 0 || rule.Hour > 23 || rule.Minute < 0 || rule.Minute > 59 || rule.Second < 0 || rule.Second > 59 {
			return nil, errors.New("camera daylight-saving transition time is invalid")
		}
	}
	type transition struct {
		instant time.Time
		kind    byte
	}
	transitions := make([]transition, 0, 606)
	for year := 1899; year <= 2201; year++ {
		start, startOK := dstTransitionDate(year, configuration.DstStart)
		end, endOK := dstTransitionDate(year, configuration.DstEnd)
		if !startOK || !endOK {
			return nil, errors.New("camera daylight-saving transition date is invalid")
		}
		transitions = append(transitions,
			transition{start.Add(-time.Duration(configuration.BaseOffsetSeconds) * time.Second), 1},
			transition{end.Add(-time.Duration(configuration.BaseOffsetSeconds+configuration.DstOffsetSeconds) * time.Second), 0},
		)
	}
	sort.Slice(transitions, func(first, second int) bool { return transitions[first].instant.Before(transitions[second].instant) })
	for index := 1; index < len(transitions); index++ {
		if transitions[index-1].kind == transitions[index].kind || !transitions[index-1].instant.Before(transitions[index].instant) {
			return nil, errors.New("camera daylight-saving transitions are inconsistent")
		}
	}
	header := func(count, kinds, characters uint32) []byte {
		data := append([]byte{'T', 'Z', 'i', 'f', '2'}, make([]byte, 15)...)
		for _, value := range []uint32{0, 0, 0, count, kinds, characters} {
			data = binary.BigEndian.AppendUint32(data, value)
		}
		return data
	}
	payload := header(0, 1, 4)
	payload = binary.BigEndian.AppendUint32(payload, uint32(int32(configuration.BaseOffsetSeconds)))
	payload = append(payload, 0, 0, 'S', 'T', 'D', 0)
	payload = append(payload, header(uint32(len(transitions)), 2, 8)...)
	for _, item := range transitions {
		payload = binary.BigEndian.AppendUint64(payload, uint64(item.instant.Unix()))
	}
	for _, item := range transitions {
		payload = append(payload, item.kind)
	}
	payload = binary.BigEndian.AppendUint32(payload, uint32(int32(configuration.BaseOffsetSeconds)))
	payload = append(payload, 0, 0)
	payload = binary.BigEndian.AppendUint32(payload, uint32(int32(configuration.BaseOffsetSeconds+configuration.DstOffsetSeconds)))
	payload = append(payload, 1, 4)
	payload = append(payload, []byte("STD\x00DST\x00\n\n")...)
	return time.LoadLocationFromTZData("Camera clock", payload)
}

func validateCameraSearchClock(name string, from, to time.Time) error {
	if !strings.HasPrefix(name, cameraClockPrefix) {
		return nil
	}
	configuration, err := decodeCameraClock(name)
	if err != nil {
		return err
	}
	if from.Year() < 1900 || to.Year() > 2200 {
		return errors.New("camera clock conversion is unavailable outside 1900-2200")
	}
	if configuration.DstEnabled {
		for year := from.Year() - 1; year <= to.Year()+1; year++ {
			end, ok := dstTransitionDate(year, configuration.DstEnd)
			if !ok {
				return errors.New("camera daylight-saving transition date is invalid")
			}
			end = end.Add(-time.Duration(configuration.BaseOffsetSeconds+configuration.DstOffsetSeconds) * time.Second)
			repeated := time.Duration(configuration.DstOffsetSeconds) * time.Second
			if from.Before(end.Add(repeated)) && to.After(end.Add(-repeated)) {
				return errors.New("camera recording timestamps are ambiguous during the daylight-saving clock change")
			}
		}
	}
	return nil
}
