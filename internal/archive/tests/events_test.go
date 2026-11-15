package archive

import (
	"context"
	"errors"
	"testing"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"

	"reonavr/internal/testconfig"
)

type eventStore struct {
	camera      domain.Camera
	credential  domain.EncryptedCredential
	transitions []domain.EventTransition
	events      []domain.Event
	run         domain.IngestionRun
	recordings  []domain.Recording
	missed      bool
	resolved    bool
}

func (store *eventStore) GetCamera(context.Context, string) (domain.Camera, error) {
	return store.camera, nil
}

func (store *eventStore) GetCredential(context.Context, string) (domain.EncryptedCredential, error) {
	return store.credential, nil
}

func (store *eventStore) ApplyEventTransition(_ context.Context, _ string, transition domain.EventTransition) error {
	store.transitions = append(store.transitions, transition)
	return nil
}

func (store *eventStore) ListEvents(context.Context, string, int) ([]domain.Event, error) {
	return store.events, nil
}

func (store *eventStore) ListArchivedEvents(context.Context, string, int) ([]domain.Event, error) {
	return store.events, nil
}

func (store *eventStore) ListEventsReadyForFinalization(context.Context, time.Time, int) ([]domain.Event, error) {
	return store.events, nil
}

func (store *eventStore) SaveEventFinalization(_ context.Context, _ string, run domain.IngestionRun, recordings []domain.Recording, nativeName string, _ time.Time) error {
	store.run = run
	store.recordings = recordings
	store.events[0].NativeRecordingName = nativeName
	return nil
}

func (store *eventStore) MarkEventFinalizationMiss(context.Context, string) error {
	store.missed = true
	return nil
}

func (store *eventStore) ResolveIssue(context.Context, string, string, string, time.Time) error {
	store.resolved = true
	return nil
}

type eventStreamer struct {
	connection  reolink.Connection
	transitions []domain.EventTransition
}

type replayingEventStreamer struct {
	eventStreamer
	replayAfter time.Time
	replay      []domain.EventTransition
}

func (streamer *replayingEventStreamer) QueryEvents(_ context.Context, _ reolink.Connection, after time.Time, onEvent func(domain.EventTransition) error) error {
	streamer.replayAfter = after
	for _, transition := range streamer.replay {
		if err := onEvent(transition); err != nil {
			return err
		}
	}
	return nil
}

func (streamer *eventStreamer) StreamEvents(_ context.Context, connection reolink.Connection, onEvent func(domain.EventTransition) error) error {
	streamer.connection = connection
	for _, transition := range streamer.transitions {
		if err := onEvent(transition); err != nil {
			return err
		}
	}
	return context.Canceled
}

func TestEventServicePersistsOnlySelectedStartsAndAllEndTransitions(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := &eventStore{
		camera: domain.Camera{
			ID: "camera-one", Host: testconfig.Host(2), Port: 9000, Channel: 1,
			Timezone: "UTC", Enabled: true, ArchiveEnabled: true,
			ArchiveStream: domain.StreamMain, EventTypes: []domain.EventType{domain.EventPerson},
		},
		credential: domain.EncryptedCredential{Username: testconfig.Username(), KeyVersion: 1},
	}
	streamer := &eventStreamer{transitions: []domain.EventTransition{
		{OccurredAtUtc: now, EventType: domain.EventMotion, Active: true},
		{OccurredAtUtc: now.Add(time.Second), EventType: domain.EventPerson, Active: true},
		{OccurredAtUtc: now.Add(2 * time.Second), Active: false},
	}}
	service := NewEventService(store, streamer, serviceCipher{})
	if err := service.Watch(context.Background(), store.camera.ID); !errorsIs(err, context.Canceled) {
		t.Fatalf("unexpected stream result: %v", err)
	}
	if len(store.transitions) != 2 || store.transitions[0].EventType != domain.EventPerson || store.transitions[1].Active {
		t.Fatalf("unexpected persisted transitions: %#v", store.transitions)
	}
	if streamer.connection.Password != testconfig.Password() || streamer.connection.Channel != 1 {
		t.Fatalf("unexpected stream connection: %#v", streamer.connection)
	}
}

func TestEventServiceReplaysFromOverlappingDurableCursorBeforeStreaming(t *testing.T) {
	cursor := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := &eventStore{
		camera: domain.Camera{
			ID: "camera-one", Host: testconfig.Host(2), Port: 9000, Enabled: true, ArchiveEnabled: true,
			ArchiveStream: domain.StreamMain, EventTypes: []domain.EventType{domain.EventPerson}, LastEventAtUtc: &cursor,
		},
		credential: domain.EncryptedCredential{Username: testconfig.Username(), KeyVersion: 1},
	}
	streamer := &replayingEventStreamer{
		eventStreamer: eventStreamer{},
		replay:        []domain.EventTransition{{OccurredAtUtc: cursor.Add(time.Second), EventType: domain.EventPerson, Active: true}},
	}
	service := NewEventService(store, streamer, serviceCipher{})
	if err := service.Watch(context.Background(), store.camera.ID); !errorsIs(err, context.Canceled) {
		t.Fatalf("unexpected stream result: %v", err)
	}
	if !streamer.replayAfter.Equal(cursor.Add(-time.Second)) || len(store.transitions) != 1 || !store.resolved {
		t.Fatalf("unexpected replay cursor or transitions: %v %#v", streamer.replayAfter, store.transitions)
	}
}

func TestEventServiceFinalizesCameraStorageRecordingAfterPostDelay(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 1, 0, 0, time.UTC)
	started := now.Add(-time.Minute)
	ended := now.Add(-30 * time.Second)
	store := &eventStore{
		camera: domain.Camera{
			ID: "camera-one", Host: testconfig.Host(2), Port: 9000, Timezone: "UTC",
			Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
			RecordingSource: domain.RecordingSourceCameraStorage, RecordingMode: domain.RecordingModeEvents,
			Capabilities: &domain.CameraCapabilities{PreRecordSeconds: 10, PostRecordSeconds: 5},
		},
		credential: domain.EncryptedCredential{Username: testconfig.Username(), KeyVersion: 1},
		events: []domain.Event{{
			ID: "event-one", CameraID: "camera-one", EventType: domain.EventPerson,
			StartedAtUtc: started, EndedAtUtc: &ended, CreatedAtUtc: started,
		}},
	}
	searcher := &serviceSearcher{items: []reolink.Recording{{
		NativeName: "person.mp4", Stream: domain.StreamMain,
		StartAtUtc: started.Add(-10 * time.Second), EndAtUtc: ended.Add(5 * time.Second),
		SizeBytes: 1024, EventTypes: []domain.EventType{domain.EventPerson},
	}}}
	service := NewEventService(store, &eventStreamer{}, serviceCipher{})
	service.SetSearcher(searcher)
	service.now = func() time.Time { return now }
	if err := service.FinalizeNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.recordings) != 1 || store.recordings[0].State != domain.RecordingQueued || store.events[0].NativeRecordingName != "person.mp4" {
		t.Fatalf("unexpected event finalization: %#v %#v", store.recordings, store.events[0])
	}
	if store.run.Kind != "EventFinalization" || searcher.connection.Password != testconfig.Password() {
		t.Fatalf("unexpected finalization run: %#v connection=%#v", store.run, searcher.connection)
	}
}

func TestEventServiceFinalizesEventWithoutUnavailableNativeMedia(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 1, 0, 0, time.UTC)
	ended := now.Add(-30 * time.Second)
	store := &eventStore{
		camera: domain.Camera{
			ID: "camera-one", Host: testconfig.Host(2), Port: 9000, Timezone: "UTC",
			Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
			RecordingSource: domain.RecordingSourceCameraStorage, RecordingMode: domain.RecordingModeEvents,
		},
		credential: domain.EncryptedCredential{Username: testconfig.Username(), KeyVersion: 1},
		events: []domain.Event{{
			ID: "event-one", CameraID: "camera-one", EventType: domain.EventPerson,
			StartedAtUtc: now.Add(-time.Minute), EndedAtUtc: &ended, CreatedAtUtc: now.Add(-time.Minute),
		}},
	}
	service := NewEventService(store, &eventStreamer{}, serviceCipher{})
	service.SetSearcher(&serviceSearcher{})
	service.now = func() time.Time { return now }
	if err := service.FinalizeNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.missed {
		t.Fatal("event without native media was left pending")
	}
}

func TestErrorsIsMatchesWrappedCancellation(t *testing.T) {
	if !errorsIs(errors.Join(context.Canceled), context.Canceled) {
		t.Fatal("wrapped cancellation did not match its cause")
	}
	if errorsIs(errors.New("unrelated failure"), context.Canceled) {
		t.Fatal("unrelated error matched cancellation")
	}
}

func errorsIs(err, target error) bool {
	return errors.Is(err, target)
}
