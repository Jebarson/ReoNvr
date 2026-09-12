package workers

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"reonavr/internal/domain"
)

const applicationScope = "Application"

type Persistence interface {
	SaveWorkerState(context.Context, domain.WorkerState) error
	ReportIssue(context.Context, domain.OperationalIssue) error
	ResolveIssue(context.Context, string, string, string, time.Time) error
	ListOperationalIssues(context.Context) ([]domain.OperationalIssue, error)
}

type Spec struct {
	Name     string
	ScopeKey string
	Interval time.Duration
	Run      func(context.Context) error
}

type workerRegistration struct {
	cancel context.CancelFunc
	id     uint64
}

type Supervisor struct {
	store   Persistence
	logger  *slog.Logger
	mu      sync.RWMutex
	states  map[string]domain.WorkerState
	workers map[string]workerRegistration
	nextID  uint64
	root    context.Context
}

func NewSupervisor(store Persistence, logger *slog.Logger) *Supervisor {
	return &Supervisor{
		store: store, logger: logger,
		states: make(map[string]domain.WorkerState), workers: make(map[string]workerRegistration),
	}
}

func (supervisor *Supervisor) Start(ctx context.Context) {
	supervisor.mu.Lock()
	supervisor.root = ctx
	supervisor.mu.Unlock()
	now := time.Now().UTC()
	supervisor.setState(ctx, domain.WorkerState{
		WorkerName: "WorkerSupervisor", ScopeKey: applicationScope, Stage: "Running",
		LastHeartbeatAtUtc: &now, LastSuccessAtUtc: &now, UpdatedAtUtc: now,
	})
	go func() {
		<-ctx.Done()
		stopped := time.Now().UTC()
		supervisor.setState(context.Background(), domain.WorkerState{
			WorkerName: "WorkerSupervisor", ScopeKey: applicationScope, Stage: "Stopped",
			LastHeartbeatAtUtc: &stopped, LastSuccessAtUtc: &now, UpdatedAtUtc: stopped,
		})
	}()
}

func (supervisor *Supervisor) Add(spec Spec) {
	if spec.ScopeKey == "" {
		spec.ScopeKey = applicationScope
	}
	if spec.Interval <= 0 {
		spec.Interval = time.Minute
	}
	key := workerKey(spec.Name, spec.ScopeKey)
	supervisor.mu.Lock()
	if _, exists := supervisor.workers[key]; exists {
		supervisor.mu.Unlock()
		return
	}
	root := supervisor.root
	if root == nil {
		root = context.Background()
	}
	ctx, cancel := context.WithCancel(root)
	supervisor.nextID++
	id := supervisor.nextID
	supervisor.workers[key] = workerRegistration{cancel: cancel, id: id}
	supervisor.mu.Unlock()
	go supervisor.run(ctx, spec, id)
}

func (supervisor *Supervisor) Remove(name, scopeKey string) {
	if scopeKey == "" {
		scopeKey = applicationScope
	}
	key := workerKey(name, scopeKey)
	supervisor.mu.Lock()
	registration, exists := supervisor.workers[key]
	delete(supervisor.workers, key)
	supervisor.mu.Unlock()
	if exists {
		registration.cancel()
	}
}

func (supervisor *Supervisor) RemoveAll(names ...string) {
	selected := make(map[string]bool, len(names))
	for _, name := range names {
		selected[name] = true
	}
	supervisor.mu.Lock()
	cancellations := make([]context.CancelFunc, 0)
	for key, registration := range supervisor.workers {
		name, _, _ := strings.Cut(key, ":")
		if selected[name] {
			delete(supervisor.workers, key)
			cancellations = append(cancellations, registration.cancel)
		}
	}
	supervisor.mu.Unlock()
	for _, cancel := range cancellations {
		cancel()
	}
}

func (supervisor *Supervisor) Ready() bool {
	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	state, ok := supervisor.states[workerKey("GatewayHealthWorker", applicationScope)]
	return ok && state.LastSuccessAtUtc != nil && state.Stage != "Degraded" && state.Stage != "Backoff" && state.Stage != "Stopped"
}

func (supervisor *Supervisor) Workers() any {
	supervisor.mu.RLock()
	states := make([]domain.WorkerState, 0, len(supervisor.states))
	for _, state := range supervisor.states {
		states = append(states, state)
	}
	supervisor.mu.RUnlock()
	sort.Slice(states, func(left, right int) bool {
		if states[left].WorkerName == states[right].WorkerName {
			return states[left].ScopeKey < states[right].ScopeKey
		}
		return states[left].WorkerName < states[right].WorkerName
	})
	return states
}

func (supervisor *Supervisor) Issues() any {
	issues, err := supervisor.store.ListOperationalIssues(context.Background())
	if err != nil {
		supervisor.logger.Error("list operational issues", "error", err)
		return []domain.OperationalIssue{}
	}
	return issues
}

func (supervisor *Supervisor) run(ctx context.Context, spec Spec, id uint64) {
	failures := 0
	var lastSuccess *time.Time
	for {
		now := time.Now().UTC()
		supervisor.setState(ctx, domain.WorkerState{
			WorkerName: spec.Name, ScopeKey: spec.ScopeKey, Stage: "Running",
			LastHeartbeatAtUtc: &now, LastSuccessAtUtc: lastSuccess,
			ConsecutiveFailures: failures, UpdatedAtUtc: now,
		})
		err := runSafely(ctx, spec.Run)
		now = time.Now().UTC()
		if ctx.Err() != nil {
			if supervisor.workerWasReplaced(workerKey(spec.Name, spec.ScopeKey), id) {
				return
			}
			supervisor.setState(context.Background(), domain.WorkerState{
				WorkerName: spec.Name, ScopeKey: spec.ScopeKey, Stage: "Stopped",
				LastHeartbeatAtUtc: &now, LastSuccessAtUtc: lastSuccess, UpdatedAtUtc: now,
			})
			return
		}
		wait := spec.Interval
		if err == nil {
			failures = 0
			lastSuccess = &now
			_ = supervisor.store.ResolveIssue(ctx, spec.Name, cameraScope(spec), "WorkerCycleFailed", now)
			supervisor.setState(ctx, domain.WorkerState{
				WorkerName: spec.Name, ScopeKey: spec.ScopeKey, Stage: "Waiting",
				LastHeartbeatAtUtc: &now, LastSuccessAtUtc: lastSuccess, UpdatedAtUtc: now,
			})
		} else {
			failures++
			wait = retryDelay(failures)
			next := now.Add(wait)
			message := boundedMessage(err.Error())
			supervisor.setState(ctx, domain.WorkerState{
				WorkerName: spec.Name, ScopeKey: spec.ScopeKey, Stage: "Backoff",
				LastHeartbeatAtUtc: &now, LastSuccessAtUtc: lastSuccess,
				ConsecutiveFailures: failures, NextAttemptAtUtc: &next,
				LastErrorCode: "WorkerCycleFailed", UpdatedAtUtc: now,
			})
			id := issueID(spec.Name, spec.ScopeKey, "WorkerCycleFailed")
			issue := domain.OperationalIssue{
				ID: id, Severity: "Error",
				Source: spec.Name, CameraID: cameraScope(spec), ErrorCode: "WorkerCycleFailed",
				Message: message, FirstSeenAtUtc: now, LastSeenAtUtc: now,
			}
			supervisor.logger.Error("worker cycle failed", "issueId", id, "worker", spec.Name, "scope", spec.ScopeKey, "errorCode", issue.ErrorCode, "error", message)
			if saveErr := supervisor.store.ReportIssue(ctx, issue); saveErr != nil {
				supervisor.logger.Error("report worker issue", "worker", spec.Name, "scope", spec.ScopeKey, "error", saveErr)
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

func (supervisor *Supervisor) workerWasReplaced(key string, id uint64) bool {
	supervisor.mu.RLock()
	registration, exists := supervisor.workers[key]
	supervisor.mu.RUnlock()
	return exists && registration.id != id
}

func (supervisor *Supervisor) setState(ctx context.Context, state domain.WorkerState) {
	supervisor.mu.Lock()
	supervisor.states[workerKey(state.WorkerName, state.ScopeKey)] = state
	supervisor.mu.Unlock()
	if err := supervisor.store.SaveWorkerState(ctx, state); err != nil && !errors.Is(err, context.Canceled) {
		supervisor.logger.Error("save worker state", "worker", state.WorkerName, "scope", state.ScopeKey, "error", err)
	}
}

func runSafely(ctx context.Context, action func(context.Context) error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("worker panic: %v", recovered)
		}
	}()
	return action(ctx)
}

func retryDelay(failures int) time.Duration {
	if failures > 6 {
		failures = 6
	}
	base := time.Second * time.Duration(1<<failures)
	return base + time.Duration(time.Now().UnixNano()%int64(base/4+1))
}

func issueID(name, scope, code string) string {
	sum := sha256.Sum256([]byte(name + ":" + scope + ":" + code))
	return fmt.Sprintf("%x", sum[:16])
}

func workerKey(name, scope string) string {
	return name + ":" + scope
}

func cameraScope(spec Spec) string {
	if spec.ScopeKey == applicationScope {
		return ""
	}
	if spec.Name == "CameraLifecycleWorker" || spec.Name == "ReconciliationWorker" ||
		spec.Name == "DownloadWorker" || spec.Name == "RtspRecorderWorker" ||
		spec.Name == "LiveEventWorker" || spec.Name == "EventFinalizationWorker" ||
		spec.Name == "VerificationWorker" || spec.Name == "RetentionWorker" {
		return spec.ScopeKey
	}
	return ""
}

func boundedMessage(message string) string {
	if len(message) > 500 {
		return message[:500]
	}
	return message
}

func GatewayProbe(address string) func(context.Context) error {
	return func(ctx context.Context) error {
		connection, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", address)
		if err != nil {
			return fmt.Errorf("gateway unavailable: %w", err)
		}
		return connection.Close()
	}
}
