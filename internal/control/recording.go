package control

import (
	"context"
	"sync"
	"time"

	"reonavr/internal/domain"
)

type Store interface {
	GetRecordingControl(context.Context) (domain.RecordingControl, error)
	SetRecordingControl(context.Context, bool, time.Time) error
}

type RecordingService struct {
	store Store
	now   func() time.Time
	mu    sync.RWMutex
	hook  func(bool)
}

func NewRecordingService(store Store) *RecordingService {
	return &RecordingService{store: store, now: time.Now}
}

func (service *RecordingService) SetLifecycleHook(hook func(bool)) {
	service.mu.Lock()
	service.hook = hook
	service.mu.Unlock()
}

func (service *RecordingService) Get(ctx context.Context) (domain.RecordingControl, error) {
	return service.store.GetRecordingControl(ctx)
}

func (service *RecordingService) Update(ctx context.Context, enabled bool) (domain.RecordingControl, error) {
	state := domain.RecordingControl{Enabled: enabled, UpdatedAtUtc: service.now().UTC()}
	if err := service.store.SetRecordingControl(ctx, state.Enabled, state.UpdatedAtUtc); err != nil {
		return domain.RecordingControl{}, err
	}
	service.mu.RLock()
	hook := service.hook
	service.mu.RUnlock()
	if hook != nil {
		hook(enabled)
	}
	return state, nil
}
