package archive

import (
	"context"
	"errors"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"
	"reonavr/internal/security"
)

type EventStore interface {
	GetCamera(context.Context, string) (domain.Camera, error)
	GetCredential(context.Context, string) (domain.EncryptedCredential, error)
	ApplyEventTransition(context.Context, string, domain.EventTransition) error
	ListEvents(context.Context, string, int) ([]domain.Event, error)
	ListEventsReadyForFinalization(context.Context, time.Time, int) ([]domain.Event, error)
	SaveEventFinalization(context.Context, string, domain.IngestionRun, []domain.Recording, string, time.Time) error
	MarkEventFinalizationMiss(context.Context, string) error
}

type EventStreamer interface {
	StreamEvents(context.Context, reolink.Connection, func(domain.EventTransition) error) error
}

type EventReplayer interface {
	QueryEvents(context.Context, reolink.Connection, time.Time, func(domain.EventTransition) error) error
}

type eventIssueResolver interface {
	ResolveIssue(context.Context, string, string, string, time.Time) error
}

type EventService struct {
	store    EventStore
	streamer EventStreamer
	searcher Searcher
	cipher   CredentialCipher
	now      func() time.Time
}

func NewEventService(store EventStore, streamer EventStreamer, cipher CredentialCipher) *EventService {
	return &EventService{store: store, streamer: streamer, cipher: cipher, now: time.Now}
}

func (service *EventService) SetSearcher(searcher Searcher) {
	service.searcher = searcher
}

func (service *EventService) FinalizeNext(ctx context.Context) error {
	if service.searcher == nil {
		return errors.New("event recording search is not configured")
	}
	now := service.now().UTC()
	events, err := service.store.ListEventsReadyForFinalization(ctx, now, 1)
	if err != nil || len(events) == 0 {
		return err
	}
	event := events[0]
	if event.EndedAtUtc == nil {
		return errors.New("event finalization requires an end time")
	}
	camera, err := service.store.GetCamera(ctx, event.CameraID)
	if err != nil {
		return err
	}
	preRecordSeconds := 0
	postRecordSeconds := 0
	if camera.Capabilities != nil {
		preRecordSeconds = camera.Capabilities.PreRecordSeconds
		postRecordSeconds = camera.Capabilities.PostRecordSeconds
	}
	readyAt := event.EndedAtUtc.Add(time.Duration(postRecordSeconds+15) * time.Second)
	if now.Before(readyAt) {
		return nil
	}
	credential, err := service.store.GetCredential(ctx, camera.ID)
	if err != nil {
		return err
	}
	if credential.KeyVersion != security.KeyVersion {
		return errors.New("stored credential uses an unsupported key version")
	}
	password, err := service.cipher.Decrypt(camera.ID, credential.Ciphertext, credential.Nonce)
	if err != nil {
		return err
	}
	windowStart := event.StartedAtUtc.Add(-time.Duration(preRecordSeconds) * time.Second)
	windowEnd := readyAt
	items, searchErr := service.searcher.SearchRecordings(ctx, reolink.Connection{
		Host: camera.Host, Port: camera.Port, Channel: camera.Channel,
		Stream: camera.ArchiveStream, Timezone: camera.Timezone,
		Username: credential.Username, Password: password,
	}, camera.ArchiveStream, windowStart, windowEnd)
	password = ""
	if searchErr != nil {
		return searchErr
	}
	recordings := make([]domain.Recording, 0, len(items))
	matchedNativeName := ""
	for _, item := range items {
		if item.StartAtUtc.After(windowEnd) || item.EndAtUtc.Before(windowStart) || !hasSelectedType(item.EventTypes, map[domain.EventType]bool{event.EventType: true}) {
			continue
		}
		if matchedNativeName == "" {
			matchedNativeName = item.NativeName
		}
		rawMetadata := item.RawMetadata
		if len(rawMetadata) == 0 {
			rawMetadata = []byte("{}")
		}
		recordings = append(recordings, domain.Recording{
			ID: recordingID(camera.ID, item.NativeName), CameraID: camera.ID,
			NativeName: item.NativeName, Source: domain.RecordingSourceCameraStorage,
			Stream: item.Stream, StartAtUtc: item.StartAtUtc, EndAtUtc: item.EndAtUtc,
			SizeBytes: item.SizeBytes, EventTypes: append([]domain.EventType(nil), item.EventTypes...),
			State: domain.RecordingQueued, RawMetadata: rawMetadata,
			DiscoveredAtUtc: now, UpdatedAtUtc: now,
		})
	}
	if len(recordings) == 0 {
		return service.store.MarkEventFinalizationMiss(ctx, event.ID)
	}
	runID, err := randomID()
	if err != nil {
		return err
	}
	completed := now
	run := domain.IngestionRun{
		ID: runID, CameraID: camera.ID, Kind: "EventFinalization",
		WindowStartUtc: windowStart, WindowEndUtc: windowEnd, Status: "Complete",
		DiscoveredCount: len(recordings), StartedAtUtc: now, CompletedAtUtc: &completed,
	}
	return service.store.SaveEventFinalization(ctx, event.ID, run, recordings, matchedNativeName, windowEnd)
}

func (service *EventService) Watch(ctx context.Context, cameraID string) error {
	camera, err := service.store.GetCamera(ctx, cameraID)
	if err != nil {
		return err
	}
	if !camera.Enabled || !camera.ArchiveEnabled || len(camera.EventTypes) == 0 {
		return nil
	}
	credential, err := service.store.GetCredential(ctx, cameraID)
	if err != nil {
		return err
	}
	if credential.KeyVersion != security.KeyVersion {
		return errors.New("stored credential uses an unsupported key version")
	}
	password, err := service.cipher.Decrypt(cameraID, credential.Ciphertext, credential.Nonce)
	if err != nil {
		return err
	}
	selected := make(map[domain.EventType]bool, len(camera.EventTypes))
	for _, eventType := range camera.EventTypes {
		selected[eventType] = true
	}
	connection := reolink.Connection{
		Host: camera.Host, Port: camera.Port, Channel: camera.Channel,
		Stream: camera.ArchiveStream, Timezone: camera.Timezone,
		Username: credential.Username, Password: password,
	}
	handleTransition := func(transition domain.EventTransition) error {
		if transition.Active && !selected[transition.EventType] {
			return nil
		}
		return service.store.ApplyEventTransition(ctx, cameraID, transition)
	}
	if replayer, ok := service.streamer.(EventReplayer); ok {
		after := camera.CreatedAtUtc
		if camera.LastEventAtUtc != nil {
			after = *camera.LastEventAtUtc
		}
		if err := replayer.QueryEvents(ctx, connection, after.Add(-time.Second), handleTransition); err != nil {
			password = ""
			connection.Password = ""
			return err
		}
		if resolver, ok := service.store.(eventIssueResolver); ok {
			_ = resolver.ResolveIssue(ctx, "LiveEventWorker", cameraID, "WorkerCycleFailed", service.now().UTC())
		}
	}
	err = service.streamer.StreamEvents(ctx, connection, handleTransition)
	password = ""
	connection.Password = ""
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("camera event stream ended")
}

func (service *EventService) ListEvents(ctx context.Context, cameraID string, limit int) ([]domain.Event, error) {
	if _, err := service.store.GetCamera(ctx, cameraID); err != nil {
		return nil, err
	}
	return service.store.ListEvents(ctx, cameraID, limit)
}
