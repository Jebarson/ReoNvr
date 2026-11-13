package control

import (
	"context"
	"errors"
	"testing"
	"time"

	"reonavr/internal/domain"
)

type recordingStore struct {
	state domain.RecordingControl
	err   error
}

func (store *recordingStore) GetRecordingControl(context.Context) (domain.RecordingControl, error) {
	return store.state, store.err
}

func (store *recordingStore) SetRecordingControl(_ context.Context, enabled bool, updatedAt time.Time) error {
	if store.err != nil {
		return store.err
	}
	store.state = domain.RecordingControl{Enabled: enabled, UpdatedAtUtc: updatedAt}
	return nil
}

func TestRecordingControlPersistsBeforeLifecycleChange(t *testing.T) {
	store := &recordingStore{state: domain.RecordingControl{Enabled: true}}
	service := NewRecordingService(store)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	called := false
	service.SetLifecycleHook(func(enabled bool) {
		called = true
		if enabled || store.state.Enabled {
			t.Fatal("recording state was not persisted before the lifecycle hook")
		}
	})
	state, err := service.Update(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if state.Enabled || !state.UpdatedAtUtc.Equal(now) || !called {
		t.Fatalf("unexpected recording control update: %#v", state)
	}
}

func TestRecordingControlDoesNotNotifyWhenPersistenceFails(t *testing.T) {
	store := &recordingStore{err: errors.New("database unavailable")}
	service := NewRecordingService(store)
	called := false
	service.SetLifecycleHook(func(bool) { called = true })
	if _, err := service.Update(context.Background(), false); err == nil {
		t.Fatal("expected persistence error")
	}
	if called {
		t.Fatal("lifecycle hook ran after a failed update")
	}
}

type blockingRecordingStore struct {
	recordingStore
	resumePersisted chan struct{}
	releaseResume   chan struct{}
	pausePersisted  chan struct{}
}

func (store *blockingRecordingStore) SetRecordingControl(ctx context.Context, enabled bool, now time.Time) error {
	if err := store.recordingStore.SetRecordingControl(ctx, enabled, now); err != nil {
		return err
	}
	if enabled {
		close(store.resumePersisted)
		<-store.releaseResume
	} else {
		close(store.pausePersisted)
	}
	return nil
}

func TestCameraLifecycleCannotRegisterWorkersAfterPause(t *testing.T) {
	for _, cameraFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "registration first", false: "pause first"}[cameraFirst], func(t *testing.T) {
			store := &recordingStore{state: domain.RecordingControl{Enabled: true}}
			service := NewRecordingService(store)
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			workerActive := true
			service.SetLifecycleHook(func(enabled bool) {
				if !cameraFirst {
					close(entered)
					<-release
				}
				workerActive = enabled
			})
			register := func() error {
				service.ApplyCameraChange(func() {
					enabled := store.state.Enabled
					if cameraFirst {
						close(entered)
						<-release
					}
					workerActive = enabled
				})
				return nil
			}
			pause := func() error { _, err := service.Update(context.Background(), false); return err }
			first, second := register, pause
			if !cameraFirst {
				first, second = pause, register
			}
			results := make(chan error, 2)
			go func() { results <- first() }()
			<-entered
			started := make(chan struct{})
			go func() { close(started); results <- second() }()
			<-started
			select {
			case <-results:
				t.Fatal("camera registration and global pause were not serialized")
			case <-time.After(50 * time.Millisecond):
			}
			close(release)
			for range 2 {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			if workerActive || store.state.Enabled {
				t.Fatal("camera lifecycle restarted a worker after pause")
			}
		})
	}
}

func TestRecordingControlSerializesPersistenceAndLifecycle(t *testing.T) {
	store := &blockingRecordingStore{
		resumePersisted: make(chan struct{}), releaseResume: make(chan struct{}), pausePersisted: make(chan struct{}),
	}
	service := NewRecordingService(store)
	lifecycle := make(chan bool, 2)
	service.SetLifecycleHook(func(enabled bool) { lifecycle <- enabled })
	results := make(chan error, 2)
	go func() { _, err := service.Update(context.Background(), true); results <- err }()
	<-store.resumePersisted
	go func() { _, err := service.Update(context.Background(), false); results <- err }()
	premature := false
	select {
	case <-store.pausePersisted:
		premature = true
	case <-time.After(50 * time.Millisecond):
	}
	close(store.releaseResume)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if premature || !<-lifecycle || <-lifecycle || store.state.Enabled {
		t.Fatal("concurrent pause overtook an unfinished resume or left lifecycle enabled")
	}
}
