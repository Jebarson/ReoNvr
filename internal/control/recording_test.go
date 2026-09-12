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
