package reolink

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"reonavr/internal/domain"
)

type Connection struct {
	Host     string
	Port     int
	Channel  int
	Stream   domain.ArchiveStream
	Timezone string
	Username string
	Password string
}

type TestResult struct {
	Model                 string
	Firmware              string
	Timezone              string
	TimezoneOffsetSeconds int
	TimezoneConfiguration TimezoneConfiguration
	Capabilities          domain.CameraCapabilities
	SnapshotJPEG          []byte
}

type TimezoneConfiguration struct {
	BaseOffsetSeconds int           `json:"baseOffsetSeconds"`
	DstEnabled        bool          `json:"dstEnabled"`
	DstOffsetSeconds  int           `json:"dstOffsetSeconds"`
	DstStart          DstTransition `json:"dstStart"`
	DstEnd            DstTransition `json:"dstEnd"`
}

type DstTransition struct {
	Month   int `json:"month"`
	Week    int `json:"week"`
	Weekday int `json:"weekday"`
	Hour    int `json:"hour"`
	Minute  int `json:"minute"`
	Second  int `json:"second"`
}

type Recording struct {
	ID          string
	NativeName  string
	Stream      domain.ArchiveStream
	StartAtUtc  time.Time
	EndAtUtc    time.Time
	SizeBytes   int64
	EventTypes  []domain.EventType
	RawMetadata json.RawMessage
}

type CommandRunner interface {
	Run(ctx context.Context, password string, arguments ...string) ([]byte, error)
}

type StreamingCommandRunner interface {
	Stream(ctx context.Context, password string, onLine func([]byte) error, arguments ...string) error
}

type ExecRunner struct {
	Path        string
	GatewayAddr string
}

type boundedBuffer struct {
	data []byte
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	const limit = 64 * 1024
	written := len(value)
	buffer.data = append(buffer.data, value...)
	if len(buffer.data) > limit {
		buffer.data = append(buffer.data[:0], buffer.data[len(buffer.data)-limit:]...)
	}
	return written, nil
}

func (buffer *boundedBuffer) String() string {
	return string(buffer.data)
}

func (runner *ExecRunner) Run(ctx context.Context, password string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, runner.Path, arguments...)
	command.Stdin = strings.NewReader(password + "\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("camera command timed out")
		}
		return nil, fmt.Errorf("camera command failed: %s", safeCommandError(stderr.String(), password))
	}
	return stdout.Bytes(), nil
}

func (runner *ExecRunner) Stream(ctx context.Context, password string, onLine func([]byte) error, arguments ...string) error {
	streamContext, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(streamContext, runner.Path, arguments...)
	command.Stdin = strings.NewReader(password + "\n")
	stdout, err := command.StdoutPipe()
	if err != nil {
		return errors.New("open camera event stream")
	}
	var stderr boundedBuffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start camera event stream: %s", safeCommandError(stderr.String(), password))
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var handlerErr error
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if err := onLine(line); err != nil {
			handlerErr = err
			cancel()
			break
		}
	}
	if err := scanner.Err(); err != nil && handlerErr == nil {
		handlerErr = errors.New("read camera event stream")
		cancel()
	}
	waitErr := command.Wait()
	if handlerErr != nil {
		return handlerErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		return fmt.Errorf("camera event stream failed: %s", safeCommandError(stderr.String(), password))
	}
	return nil
}

type Client struct {
	runner              CommandRunner
	timeout             time.Duration
	isolatedGatewayPath string
	captureSnapshot     bool
}

func NewClient(runner CommandRunner, timeout time.Duration) *Client {
	return &Client{runner: runner, timeout: timeout}
}

func NewValidationClient(runner *ExecRunner, gatewayPath string, timeout time.Duration) *Client {
	return &Client{runner: runner, timeout: timeout, isolatedGatewayPath: gatewayPath, captureSnapshot: true}
}

func (client *Client) Test(ctx context.Context, connection Connection) (TestResult, error) {
	if err := validateConnection(connection); err != nil {
		return TestResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	commandClient := client
	if client.isolatedGatewayPath != "" {
		execRunner, ok := client.runner.(*ExecRunner)
		if !ok {
			return TestResult{}, errors.New("isolated validation requires the executable runner")
		}
		isolatedRunner, stopGateway, err := startIsolatedGateway(ctx, execRunner.Path, client.isolatedGatewayPath)
		if err != nil {
			return TestResult{}, err
		}
		defer stopGateway()
		isolatedClient := *client
		isolatedClient.runner = isolatedRunner
		isolatedClient.isolatedGatewayPath = ""
		commandClient = &isolatedClient
	}

	infoBytes, err := commandClient.run(ctx, connection, "info")
	if err != nil {
		return TestResult{}, err
	}
	var info struct {
		Model    string `json:"model"`
		Firmware string `json:"firmware"`
	}
	if err := decodeData(infoBytes, &info); err != nil {
		return TestResult{}, fmt.Errorf("read camera information: %w", err)
	}
	if info.Model == "" {
		return TestResult{}, errors.New("camera returned incomplete device information")
	}

	timezoneBytes, err := commandClient.run(ctx, connection, "config", "get", "time-zone")
	if err != nil {
		return TestResult{}, fmt.Errorf("read camera timezone: %w", err)
	}
	cameraTimezoneOffsetSeconds, err := parseTimezoneOffset(timezoneBytes)
	if err != nil {
		return TestResult{}, err
	}
	dstBytes, err := commandClient.run(ctx, connection, "raw", "106")
	if err != nil {
		return TestResult{}, fmt.Errorf("read camera daylight-saving configuration: %w", err)
	}
	timezoneConfiguration, err := parseDstConfiguration(dstBytes)
	if err != nil {
		return TestResult{}, err
	}
	timezoneConfiguration.BaseOffsetSeconds = -cameraTimezoneOffsetSeconds
	timezone, err := resolveCameraTimezone(timezoneConfiguration, connection.Timezone, time.Now().UTC())
	if err != nil {
		return TestResult{}, err
	}

	result := TestResult{
		Model: info.Model, Firmware: info.Firmware, Timezone: timezone,
		TimezoneOffsetSeconds: timezoneConfiguration.BaseOffsetSeconds,
		TimezoneConfiguration: timezoneConfiguration,
	}
	if payload, err := commandClient.run(ctx, connection, "encode", "get"); err == nil {
		parseEncode(payload, &result.Capabilities)
	}
	if payload, err := commandClient.run(ctx, connection, "record", "config", "get"); err == nil {
		parseRecordConfig(payload, &result.Capabilities)
	}
	if payload, err := commandClient.run(ctx, connection, "storage", "status"); err == nil {
		parseStorage(payload, &result.Capabilities)
	}
	if payload, err := commandClient.run(ctx, connection, "capabilities"); err == nil {
		parseEventCapabilities(payload, &result.Capabilities)
	}
	stream := connection.Stream
	if stream != domain.StreamSub {
		stream = domain.StreamMain
	}
	if payload, err := commandClient.run(ctx, connection, "stream", "url", "--kind", "rtsp", "--stream", strings.ToLower(string(stream))); err == nil {
		result.Capabilities.RtspAvailable = containsRtspURL(payload)
	}
	if len(result.Capabilities.SupportedEventTypes) == 0 {
		result.Capabilities.SupportedEventTypes = []domain.EventType{domain.EventMotion}
	}
	if client.captureSnapshot {
		if payload, err := commandClient.captureSnapshotJPEG(ctx, connection); err == nil {
			result.SnapshotJPEG = payload
		}
	}
	return result, nil
}

func (client *Client) captureSnapshotJPEG(ctx context.Context, connection Connection) ([]byte, error) {
	directory, err := os.MkdirTemp("", "reonavr-snapshot-*")
	if err != nil {
		return nil, errors.New("create private snapshot directory")
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "snapshot.jpg")
	if _, err := client.run(ctx, connection, "snapshot", "--stream", "sub", "--file", path); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, errors.New("read camera snapshot information")
	}
	const maxSnapshotBytes = 8 << 20
	if info.Size() < 4 || info.Size() > maxSnapshotBytes {
		return nil, errors.New("camera snapshot size is invalid")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("read camera snapshot")
	}
	if !isJPEGSnapshot(payload) {
		return nil, errors.New("camera snapshot is not a JPEG image")
	}
	return payload, nil
}

func isJPEGSnapshot(payload []byte) bool {
	const maxSnapshotBytes = 8 << 20
	return len(payload) >= 4 && len(payload) <= maxSnapshotBytes &&
		payload[0] == 0xff && payload[1] == 0xd8 &&
		payload[len(payload)-2] == 0xff && payload[len(payload)-1] == 0xd9
}

func (client *Client) SearchRecordings(ctx context.Context, connection Connection, stream domain.ArchiveStream, from, to time.Time) ([]Recording, error) {
	if err := validateConnection(connection); err != nil {
		return nil, err
	}
	if stream != domain.StreamMain && stream != domain.StreamSub {
		return nil, errors.New("recording stream must be Main or Sub")
	}
	if !from.Before(to) {
		return nil, errors.New("recording search start must be before its end")
	}
	ctx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	location := time.UTC
	if connection.Timezone != "" {
		var err error
		location, err = time.LoadLocation(connection.Timezone)
		if err != nil {
			return nil, errors.New("camera timezone is invalid")
		}
	}
	payload, err := client.run(ctx, connection, "vod", "search",
		"--from", from.In(location).Format("2006-01-02T15:04:05"),
		"--to", to.In(location).Format("2006-01-02T15:04:05"),
		"--stream", strings.ToLower(string(stream)), "--limit", "0")
	if err != nil {
		return nil, err
	}
	return parseRecordings(payload, connection.Host, stream, location)
}

func (client *Client) DownloadRecording(ctx context.Context, connection Connection, nativeName string, stream domain.ArchiveStream, outputPath string) error {
	if err := validateConnection(connection); err != nil {
		return err
	}
	if strings.TrimSpace(nativeName) == "" || strings.TrimSpace(outputPath) == "" {
		return errors.New("recording name and output path are required")
	}
	if stream != domain.StreamMain && stream != domain.StreamSub {
		return errors.New("recording stream must be Main or Sub")
	}
	transferContext, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	_, err := client.run(transferContext, connection, "vod", "download", nativeName,
		"--file", outputPath, "--stream", strings.ToLower(string(stream)))
	return err
}

func (client *Client) RtspURL(ctx context.Context, connection Connection, stream domain.ArchiveStream) (string, error) {
	if err := validateConnection(connection); err != nil {
		return "", err
	}
	if stream != domain.StreamMain && stream != domain.StreamSub {
		return "", errors.New("recording stream must be Main or Sub")
	}
	ctx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	payload, err := client.run(ctx, connection, "stream", "url", "--kind", "rtsp", "--stream", strings.ToLower(string(stream)))
	if err != nil {
		return "", err
	}
	value, ok := findString(payload, func(value string) bool {
		parsed, parseErr := url.Parse(value)
		return parseErr == nil && strings.EqualFold(parsed.Scheme, "rtsp") && parsed.Hostname() != ""
	})
	if !ok {
		return "", errors.New("camera did not return an RTSP stream URL")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", errors.New("camera returned an invalid RTSP stream URL")
	}
	addresses, err := net.LookupIP(parsed.Hostname())
	if err != nil {
		return "", errors.New("RTSP stream host could not be resolved")
	}
	for _, address := range addresses {
		if !address.IsPrivate() && !address.IsLoopback() && !address.IsLinkLocalUnicast() {
			return "", errors.New("RTSP stream host must resolve only to private network addresses")
		}
	}
	parsed.User = nil
	return parsed.String(), nil
}

func (client *Client) StreamEvents(ctx context.Context, connection Connection, onEvent func(domain.EventTransition) error) error {
	if err := validateConnection(connection); err != nil {
		return err
	}
	if onEvent == nil {
		return errors.New("event handler is required")
	}
	runner, ok := client.runner.(StreamingCommandRunner)
	if !ok {
		return errors.New("camera command runner does not support event streaming")
	}
	arguments := client.arguments(connection, "events", "stream")
	return runner.Stream(ctx, connection.Password, func(line []byte) error {
		transition, relevant, err := parseEventTransition(line)
		if err != nil {
			return nil
		}
		if !relevant {
			return nil
		}
		return onEvent(transition)
	}, arguments...)
}

func (client *Client) QueryEvents(ctx context.Context, connection Connection, after time.Time, onEvent func(domain.EventTransition) error) error {
	if err := validateConnection(connection); err != nil {
		return err
	}
	if after.IsZero() || onEvent == nil {
		return errors.New("event replay cursor and handler are required")
	}
	ctx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	payload, err := client.run(ctx, connection, "events", "query", "--after", strconv.FormatInt(after.UTC().Unix(), 10))
	if err != nil {
		return err
	}
	items, err := eventPayloads(payload)
	if err != nil {
		return err
	}
	for _, item := range items {
		transition, relevant, parseErr := parseEventTransition(item)
		if parseErr != nil || !relevant {
			continue
		}
		if err := onEvent(transition); err != nil {
			return err
		}
	}
	return nil
}

func eventPayloads(payload []byte) ([]json.RawMessage, error) {
	var values []json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(payload))
	for decoder.More() {
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("camera returned malformed event query JSON")
		}
		values = append(values, value)
	}
	if len(values) == 0 {
		return nil, errors.New("camera returned empty event query data")
	}
	items := make([]json.RawMessage, 0)
	for _, value := range values {
		var array []json.RawMessage
		if err := json.Unmarshal(value, &array); err == nil {
			items = append(items, array...)
			continue
		}
		var object struct {
			Timestamp int64             `json:"timestamp"`
			EventType string            `json:"eventType"`
			Data      json.RawMessage   `json:"data"`
			Events    []json.RawMessage `json:"events"`
			Items     []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(value, &object); err != nil {
			return nil, errors.New("camera returned unexpected event query data")
		}
		if object.Timestamp > 0 || object.EventType != "" {
			items = append(items, value)
			continue
		}
		nested := object.Events
		if nested == nil {
			nested = object.Items
		}
		if nested == nil && len(object.Data) > 0 && string(object.Data) != "null" {
			if err := json.Unmarshal(object.Data, &nested); err != nil {
				var envelope struct {
					Events []json.RawMessage `json:"events"`
					Items  []json.RawMessage `json:"items"`
				}
				if nestedErr := json.Unmarshal(object.Data, &envelope); nestedErr == nil {
					nested = envelope.Events
					if nested == nil {
						nested = envelope.Items
					}
				}
			}
		}
		if nested == nil {
			if _, relevant, err := parseEventTransition(value); err == nil && relevant {
				items = append(items, value)
				continue
			}
			return nil, errors.New("camera returned unexpected event query data")
		}
		items = append(items, nested...)
	}
	return items, nil
}

func parseEventTransition(payload []byte) (domain.EventTransition, bool, error) {
	var item struct {
		Timestamp int64  `json:"timestamp"`
		EventType string `json:"eventType"`
		Data      struct {
			Status string `json:"status"`
			AIType string `json:"aiType"`
		} `json:"data"`
	}
	data := json.RawMessage(payload)
	if err := json.Unmarshal(data, &item); err != nil {
		return domain.EventTransition{}, false, errors.New("camera returned malformed event JSON")
	}
	if item.Timestamp == 0 && item.EventType == "" {
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(payload, &envelope); err != nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			return domain.EventTransition{}, false, errors.New("camera returned an invalid event item")
		}
		data = envelope.Data
		if err := json.Unmarshal(data, &item); err != nil {
			return domain.EventTransition{}, false, errors.New("camera returned an invalid event item")
		}
	}
	if item.Timestamp <= 0 || strings.TrimSpace(item.EventType) == "" {
		return domain.EventTransition{}, false, errors.New("camera returned an invalid event item")
	}
	transition := domain.EventTransition{
		OccurredAtUtc: time.Unix(item.Timestamp, 0).UTC(),
		Active:        !strings.EqualFold(item.EventType, "none") && !strings.EqualFold(item.Data.Status, "none"),
		RawPayload:    append(json.RawMessage(nil), data...),
	}
	if !transition.Active {
		return transition, true, nil
	}
	switch strings.ToLower(item.EventType) {
	case "motion":
		transition.EventType = domain.EventMotion
	case "people":
		transition.EventType = domain.EventPerson
	case "vehicle":
		transition.EventType = domain.EventVehicle
	case "dog_cat":
		transition.EventType = domain.EventAnimal
	default:
		return domain.EventTransition{}, false, nil
	}
	return transition, true, nil
}

func parseRecordings(payload []byte, cameraKey string, fallbackStream domain.ArchiveStream, location *time.Location) ([]Recording, error) {
	data := json.RawMessage(payload)
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if strings.HasPrefix(strings.TrimSpace(string(payload)), "{") {
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return nil, errors.New("camera returned malformed recording search JSON")
		}
		if len(envelope.Data) != 0 && string(envelope.Data) != "null" {
			data = envelope.Data
		}
	}
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		var result struct {
			Recordings []json.RawMessage `json:"recordings"`
			Items      []json.RawMessage `json:"items"`
		}
		if objectErr := json.Unmarshal(data, &result); objectErr != nil {
			return nil, errors.New("camera returned unexpected recording search data")
		}
		items = result.Recordings
		if items == nil {
			items = result.Items
		}
	}
	recordings := make([]Recording, 0, len(items))
	for _, raw := range items {
		var item struct {
			Name       string          `json:"name"`
			StartTime  string          `json:"startTime"`
			EndTime    string          `json:"endTime"`
			FileSize   json.RawMessage `json:"fileSize"`
			Size       json.RawMessage `json:"size"`
			RecordType string          `json:"recordType"`
			StreamType string          `json:"streamType"`
			Type       string          `json:"type"`
		}
		if err := json.Unmarshal(raw, &item); err != nil || item.Name == "" {
			return nil, errors.New("camera returned an invalid recording entry")
		}
		start, err := parseCameraTime(item.StartTime, location)
		if err != nil {
			return nil, fmt.Errorf("parse recording start: %w", err)
		}
		end, err := parseCameraTime(item.EndTime, location)
		if err != nil {
			return nil, fmt.Errorf("parse recording end: %w", err)
		}
		size, err := parseJSONInt(item.FileSize)
		if err != nil || size == 0 {
			size, err = parseJSONInt(item.Size)
		}
		if err != nil {
			return nil, errors.New("camera returned an invalid recording size")
		}
		stream := normalizeStream(item.StreamType, item.Type, fallbackStream)
		digest := sha256.Sum256([]byte(cameraKey + "\x00" + item.Name))
		recordings = append(recordings, Recording{
			ID: fmt.Sprintf("%x", digest[:16]), NativeName: item.Name, Stream: stream,
			StartAtUtc: start, EndAtUtc: end, SizeBytes: size,
			EventTypes: parseRecordTypes(item.RecordType), RawMetadata: append(json.RawMessage(nil), raw...),
		})
	}
	return recordings, nil
}

func parseCameraTime(value string, location *time.Location) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), nil
	}
	if parsed, err := time.ParseInLocation("2006-01-02T15:04:05", value, location); err == nil {
		return parsed.UTC(), nil
	}
	return time.Time{}, errors.New("unsupported camera timestamp")
}

func parseJSONInt(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, errors.New("missing integer")
	}
	var number int64
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, err
	}
	return strconv.ParseInt(text, 10, 64)
}

func normalizeStream(primary, secondary string, fallback domain.ArchiveStream) domain.ArchiveStream {
	value := strings.ToLower(primary)
	if value == "" {
		value = strings.ToLower(secondary)
	}
	if strings.HasPrefix(value, "sub") {
		return domain.StreamSub
	}
	if strings.HasPrefix(value, "main") {
		return domain.StreamMain
	}
	return fallback
}

func parseRecordTypes(value string) []domain.EventType {
	seen := make(map[domain.EventType]bool)
	result := make([]domain.EventType, 0, 4)
	for _, nativeType := range strings.Split(strings.ToLower(value), ",") {
		var eventType domain.EventType
		switch strings.TrimSpace(nativeType) {
		case "md":
			eventType = domain.EventMotion
		case "people":
			eventType = domain.EventPerson
		case "vehicle":
			eventType = domain.EventVehicle
		case "dog_cat":
			eventType = domain.EventAnimal
		default:
			continue
		}
		if !seen[eventType] {
			seen[eventType] = true
			result = append(result, eventType)
		}
	}
	return result
}

func (client *Client) run(ctx context.Context, connection Connection, command ...string) ([]byte, error) {
	return client.runner.Run(ctx, connection.Password, client.arguments(connection, command...)...)
}

func (client *Client) arguments(connection Connection, command ...string) []string {
	host := connection.Host
	if connection.Port != 9000 {
		host = net.JoinHostPort(connection.Host, strconv.Itoa(connection.Port))
	}
	arguments := []string{
		"--host", host,
		"--user", connection.Username,
		"--password-stdin",
		"--channel", strconv.Itoa(connection.Channel),
		"--protocol", "v20",
		"--output", "json",
	}
	if runner, ok := client.runner.(*ExecRunner); ok && runner.GatewayAddr != "" {
		arguments = append(arguments, "--gateway-addr", runner.GatewayAddr)
	}
	arguments = append(arguments, command...)
	return arguments
}

func validateConnection(connection Connection) error {
	if strings.TrimSpace(connection.Host) == "" {
		return errors.New("host is required")
	}
	if strings.TrimSpace(connection.Username) == "" {
		return errors.New("username is required")
	}
	if connection.Password == "********" {
		return errors.New("masked password placeholders are not accepted")
	}
	if connection.Port < 1 || connection.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if connection.Channel < 0 {
		return errors.New("channel must be zero or greater")
	}
	addresses, err := net.LookupIP(connection.Host)
	if err != nil {
		return errors.New("host could not be resolved")
	}
	for _, address := range addresses {
		if !address.IsPrivate() && !address.IsLoopback() && !address.IsLinkLocalUnicast() {
			return errors.New("host must resolve only to private network addresses")
		}
	}
	return nil
}

func decodeData(payload []byte, target any) error {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return errors.New("camera returned malformed JSON")
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return errors.New("camera returned no data")
	}
	if err := json.Unmarshal(envelope.Data, target); err != nil {
		return errors.New("camera returned unexpected data")
	}
	return nil
}

func parseEncode(payload []byte, capabilities *domain.CameraCapabilities) {
	var data struct {
		Main streamData `json:"main"`
		Sub  streamData `json:"sub"`
	}
	if decodeData(payload, &data) != nil {
		return
	}
	capabilities.MainStream = data.Main.String()
	capabilities.SubStream = data.Sub.String()
	capabilities.MainBitrateKbps = data.Main.Bitrate
	capabilities.SubBitrateKbps = data.Sub.Bitrate
}

func parseTimezoneOffset(payload []byte) (int, error) {
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return 0, errors.New("camera returned malformed timezone data")
	}
	offset, ok := findNumberByKey(decoded, "timeZone")
	if !ok || offset != float64(int(offset)) {
		return 0, errors.New("camera returned incomplete timezone data")
	}
	const maximumTimezoneOffset = 14 * 60 * 60
	if offset < -maximumTimezoneOffset || offset > maximumTimezoneOffset {
		return 0, errors.New("camera returned an invalid timezone offset")
	}
	return int(offset), nil
}

func findNumberByKey(value any, requestedKey string) (float64, bool) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if value, ok := findNumberByKey(item, requestedKey); ok {
				return value, true
			}
		}
	case map[string]any:
		for key, item := range typed {
			if strings.EqualFold(key, requestedKey) {
				value, ok := item.(float64)
				return value, ok
			}
			if value, ok := findNumberByKey(item, requestedKey); ok {
				return value, true
			}
		}
	}
	return 0, false
}

func parseDstConfiguration(payload []byte) (TimezoneConfiguration, error) {
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return TimezoneConfiguration{}, errors.New("camera returned malformed daylight-saving data")
	}
	fields, ok := findObjectByKey(decoded, "Dst")
	if !ok {
		if rawXML, found := findStringValue(decoded, func(value string) bool {
			return strings.Contains(strings.ToLower(value), "<dst")
		}); found {
			fields, ok = dstFieldsFromXML(rawXML)
		}
	}
	if !ok {
		return TimezoneConfiguration{}, errors.New("camera returned incomplete daylight-saving data")
	}
	enabled, ok := objectInt(fields, "enable")
	if !ok || (enabled != 0 && enabled != 1) {
		return TimezoneConfiguration{}, errors.New("camera returned an invalid daylight-saving configuration")
	}
	configuration := TimezoneConfiguration{DstEnabled: enabled == 1}
	if !configuration.DstEnabled {
		return configuration, nil
	}
	offsetHours, ok := objectInt(fields, "offset")
	if !ok || offsetHours < 1 || offsetHours > 3 {
		return TimezoneConfiguration{}, errors.New("camera returned an invalid daylight-saving offset")
	}
	configuration.DstOffsetSeconds = offsetHours * 60 * 60
	var valid bool
	configuration.DstStart, valid = parseDstTransition(fields, "start")
	if !valid {
		return TimezoneConfiguration{}, errors.New("camera returned an invalid daylight-saving start rule")
	}
	configuration.DstEnd, valid = parseDstTransition(fields, "end")
	if !valid {
		return TimezoneConfiguration{}, errors.New("camera returned an invalid daylight-saving end rule")
	}
	return configuration, nil
}

func parseDstTransition(fields map[string]any, prefix string) (DstTransition, bool) {
	month, monthOK := objectInt(fields, prefix+"mon", prefix+"month")
	week, weekOK := objectInt(fields, prefix+"week", prefix+"weekindex")
	weekday, weekdayOK := objectWeekday(fields, prefix+"weekday")
	hour, hourOK := objectInt(fields, prefix+"hour")
	minute, minuteOK := objectInt(fields, prefix+"min", prefix+"minute")
	second, secondOK := objectInt(fields, prefix+"sec", prefix+"second")
	transition := DstTransition{Month: month, Week: week, Weekday: weekday, Hour: hour, Minute: minute, Second: second}
	valid := monthOK && weekOK && weekdayOK && hourOK && minuteOK && secondOK &&
		month >= 1 && month <= 12 && week >= 1 && week <= 5 && weekday >= 0 && weekday <= 6 &&
		hour >= 0 && hour <= 23 && minute >= 0 && minute <= 59 && second >= 0 && second <= 59
	return transition, valid
}

func findObjectByKey(value any, requestedKey string) (map[string]any, bool) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if value, ok := findObjectByKey(item, requestedKey); ok {
				return value, true
			}
		}
	case map[string]any:
		for key, item := range typed {
			if strings.EqualFold(key, requestedKey) {
				value, ok := item.(map[string]any)
				return value, ok
			}
			if value, ok := findObjectByKey(item, requestedKey); ok {
				return value, true
			}
		}
	}
	return nil, false
}

func findStringValue(value any, predicate func(string) bool) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, predicate(typed)
	case []any:
		for _, item := range typed {
			if value, ok := findStringValue(item, predicate); ok {
				return value, true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if value, ok := findStringValue(item, predicate); ok {
				return value, true
			}
		}
	}
	return "", false
}

func dstFieldsFromXML(payload string) (map[string]any, bool) {
	decoder := xml.NewDecoder(strings.NewReader(payload))
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		start, ok := token.(xml.StartElement)
		if !ok || !strings.EqualFold(start.Name.Local, "Dst") {
			continue
		}
		var values struct {
			Inner string `xml:",innerxml"`
		}
		if decoder.DecodeElement(&values, &start) != nil {
			return nil, false
		}
		fields := make(map[string]any)
		inner := xml.NewDecoder(strings.NewReader("<Dst>" + values.Inner + "</Dst>"))
		for {
			token, err := inner.Token()
			if err != nil {
				break
			}
			field, ok := token.(xml.StartElement)
			if !ok || strings.EqualFold(field.Name.Local, "Dst") {
				continue
			}
			var text string
			if inner.DecodeElement(&text, &field) == nil {
				fields[field.Name.Local] = strings.TrimSpace(text)
			}
		}
		return fields, true
	}
}

func objectInt(fields map[string]any, keys ...string) (int, bool) {
	for field, value := range fields {
		for _, key := range keys {
			if !strings.EqualFold(field, key) {
				continue
			}
			switch typed := value.(type) {
			case float64:
				return int(typed), typed == float64(int(typed))
			case string:
				parsed, err := strconv.Atoi(strings.TrimSpace(typed))
				return parsed, err == nil
			}
		}
	}
	return 0, false
}

func objectWeekday(fields map[string]any, key string) (int, bool) {
	if value, ok := objectInt(fields, key); ok {
		return value, true
	}
	weekdays := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	for field, value := range fields {
		if !strings.EqualFold(field, key) {
			continue
		}
		name, ok := value.(string)
		if !ok {
			return 0, false
		}
		for index, weekday := range weekdays {
			if strings.EqualFold(strings.TrimSpace(name), weekday) {
				return index, true
			}
		}
	}
	return 0, false
}

func resolveCameraTimezone(configuration TimezoneConfiguration, preferred string, now time.Time) (string, error) {
	candidates := make([]string, 0, 64)
	seen := make(map[string]bool)
	appendCandidate := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			candidates = append(candidates, name)
		}
	}
	appendCandidate(preferred)
	for _, name := range []string{
		"UTC", "America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles",
		"America/Anchorage", "Pacific/Honolulu", "Europe/London", "Europe/Berlin", "Europe/Moscow",
		"Africa/Johannesburg", "Asia/Dubai", "Asia/Kolkata", "Asia/Shanghai", "Asia/Tokyo",
		"Australia/Perth", "Australia/Adelaide", "Australia/Sydney", "Pacific/Auckland",
	} {
		appendCandidate(name)
	}
	zoneNames := installedTimezoneNames()
	sort.Strings(zoneNames)
	for _, name := range zoneNames {
		appendCandidate(name)
	}
	for _, name := range candidates {
		location, err := time.LoadLocation(name)
		if err == nil && timezoneMatchesConfiguration(location, configuration, now.Year()) {
			return name, nil
		}
	}
	return "", errors.New("camera timezone rules do not match an installed IANA timezone")
}

func installedTimezoneNames() []string {
	for _, path := range []string{"/usr/share/zoneinfo/zone1970.tab", "/usr/share/zoneinfo/zone.tab"} {
		payload, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		names := make([]string, 0, 400)
		for _, line := range strings.Split(string(payload), "\n") {
			if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
				continue
			}
			fields := strings.Split(line, "\t")
			if len(fields) >= 3 {
				names = append(names, fields[2])
			}
		}
		return names
	}
	return nil
}

func timezoneMatchesConfiguration(location *time.Location, configuration TimezoneConfiguration, year int) bool {
	baseOffset := configuration.BaseOffsetSeconds
	if !configuration.DstEnabled {
		for month := time.January; month <= time.December; month++ {
			_, offset := time.Date(year, month, 15, 12, 0, 0, 0, time.UTC).In(location).Zone()
			if offset != baseOffset {
				return false
			}
		}
		return true
	}
	daylightOffset := baseOffset + configuration.DstOffsetSeconds
	for checkedYear := year - 1; checkedYear <= year+1; checkedYear++ {
		startLocal, ok := dstTransitionDate(checkedYear, configuration.DstStart)
		if !ok {
			return false
		}
		endLocal, ok := dstTransitionDate(checkedYear, configuration.DstEnd)
		if !ok {
			return false
		}
		startUTC := startLocal.Add(-time.Duration(baseOffset) * time.Second)
		endUTC := endLocal.Add(-time.Duration(daylightOffset) * time.Second)
		if zoneOffset(location, startUTC.Add(-time.Second)) != baseOffset || zoneOffset(location, startUTC.Add(time.Second)) != daylightOffset ||
			zoneOffset(location, endUTC.Add(-time.Second)) != daylightOffset || zoneOffset(location, endUTC.Add(time.Second)) != baseOffset {
			return false
		}
	}
	return true
}

func dstTransitionDate(year int, transition DstTransition) (time.Time, bool) {
	if transition.Month < 1 || transition.Month > 12 || transition.Week < 1 || transition.Week > 5 || transition.Weekday < 0 || transition.Weekday > 6 {
		return time.Time{}, false
	}
	month := time.Month(transition.Month)
	var day int
	if transition.Week == 5 {
		last := time.Date(year, month+1, 0, transition.Hour, transition.Minute, transition.Second, 0, time.UTC)
		day = last.Day() - (int(last.Weekday())-transition.Weekday+7)%7
	} else {
		first := time.Date(year, month, 1, transition.Hour, transition.Minute, transition.Second, 0, time.UTC)
		day = 1 + (transition.Weekday-int(first.Weekday())+7)%7 + (transition.Week-1)*7
	}
	value := time.Date(year, month, day, transition.Hour, transition.Minute, transition.Second, 0, time.UTC)
	return value, value.Month() == month
}

func zoneOffset(location *time.Location, instant time.Time) int {
	_, offset := instant.In(location).Zone()
	return offset
}

type streamData struct {
	Width   int  `json:"width"`
	Height  int  `json:"height"`
	Bitrate int  `json:"bitrate"`
	FPS     int  `json:"fps"`
	Audio   bool `json:"audio"`
}

func (stream streamData) String() string {
	if stream.Width == 0 || stream.Height == 0 {
		return "Unknown"
	}
	return fmt.Sprintf("%dx%d, %d Kbit/s, %d fps", stream.Width, stream.Height, stream.Bitrate, stream.FPS)
}

func parseRecordConfig(payload []byte, capabilities *domain.CameraCapabilities) {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	var data struct {
		PreRecordSeconds  int `json:"preRecordSecs"`
		PostRecordSeconds int `json:"postRecordSecs"`
	}
	if json.Unmarshal(payload, &envelope) != nil || len(envelope.Data) == 0 || json.Unmarshal(envelope.Data, &data) != nil {
		return
	}
	capabilities.PreRecordSeconds = data.PreRecordSeconds
	capabilities.PostRecordSeconds = data.PostRecordSeconds
	var decoded any
	if json.Unmarshal(envelope.Data, &decoded) == nil {
		capabilities.ContinuousAvailable = hasEnabledTimingSchedule(decoded)
	}
}

func parseStorage(payload []byte, capabilities *domain.CameraCapabilities) {
	var data struct {
		Items []struct {
			Mounted   bool    `json:"mounted"`
			Formatted bool    `json:"formatted"`
			Remaining float64 `json:"remainGB"`
			Total     float64 `json:"totalGB"`
		} `json:"items"`
	}
	if decodeData(payload, &data) != nil || len(data.Items) == 0 {
		capabilities.Storage = "Unavailable"
		return
	}
	item := data.Items[0]
	if !item.Mounted || !item.Formatted {
		capabilities.Storage = "Not ready"
		return
	}
	capabilities.StorageAvailable = true
	capabilities.Storage = fmt.Sprintf("%.2f GB free of %.2f GB", item.Remaining, item.Total)
}

func containsRtspURL(payload []byte) bool {
	_, ok := findString(payload, func(value string) bool {
		return strings.HasPrefix(strings.ToLower(value), "rtsp://")
	})
	return ok
}

func findString(payload []byte, predicate func(string) bool) (string, bool) {
	var decoded any
	if json.Unmarshal(payload, &decoded) != nil {
		return "", false
	}
	return firstString(decoded, predicate)
}

func firstString(value any, predicate func(string) bool) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, predicate(typed)
	case []any:
		for _, item := range typed {
			if value, ok := firstString(item, predicate); ok {
				return value, true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if value, ok := firstString(item, predicate); ok {
				return value, true
			}
		}
	}
	return "", false
}

func hasEnabledTimingSchedule(value any) bool {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if hasEnabledTimingSchedule(item) {
				return true
			}
		}
	case map[string]any:
		for key, item := range typed {
			if strings.EqualFold(key, "timing") && scheduleHasValue(item) {
				return true
			}
			if hasEnabledTimingSchedule(item) {
				return true
			}
		}
	}
	return false
}

func scheduleHasValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case float64:
		return typed != 0
	case string:
		for _, character := range typed {
			if character >= '1' && character <= '9' {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if scheduleHasValue(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if scheduleHasValue(item) {
				return true
			}
		}
	}
	return false
}

func parseEventCapabilities(payload []byte, capabilities *domain.CameraCapabilities) {
	var data struct {
		Item struct {
			Motion string `json:"motion"`
			AIType string `json:"aitype"`
		} `json:"item"`
	}
	if decodeData(payload, &data) != nil {
		return
	}
	aiType, _ := strconv.Atoi(data.Item.AIType)
	if data.Item.Motion != "0" {
		capabilities.SupportedEventTypes = append(capabilities.SupportedEventTypes, domain.EventMotion)
	}
	if aiType&(1<<1) != 0 {
		capabilities.SupportedEventTypes = append(capabilities.SupportedEventTypes, domain.EventPerson)
	}
	if aiType&(1<<2) != 0 {
		capabilities.SupportedEventTypes = append(capabilities.SupportedEventTypes, domain.EventVehicle)
	}
	if aiType&(1<<4) != 0 {
		capabilities.SupportedEventTypes = append(capabilities.SupportedEventTypes, domain.EventAnimal)
	}
}

func safeCommandError(stderr, password string) string {
	message := strings.TrimSpace(stderr)
	if message == "" {
		return "connection failed"
	}
	if password != "" {
		message = strings.ReplaceAll(message, password, "[redacted]")
	}
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}
