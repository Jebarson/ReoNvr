package workers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"reonavr/internal/domain"
)

type testPersistence struct {
	issues chan domain.OperationalIssue
}

func (persistence *testPersistence) SaveWorkerState(context.Context, domain.WorkerState) error {
	return nil
}

func (persistence *testPersistence) ReportIssue(_ context.Context, issue domain.OperationalIssue) error {
	persistence.issues <- issue
	return nil
}

func (persistence *testPersistence) ResolveIssue(context.Context, string, string, string, time.Time) error {
	return nil
}

func (persistence *testPersistence) ListOperationalIssues(context.Context) ([]domain.OperationalIssue, error) {
	return nil, nil
}

func TestRunSafelyConvertsPanicToError(t *testing.T) {
	err := runSafely(context.Background(), func(context.Context) error {
		panic("isolated failure")
	})
	if err == nil || err.Error() != "worker panic: isolated failure" {
		t.Fatalf("unexpected panic result: %v", err)
	}
}

func TestRunSafelyReturnsWorkerError(t *testing.T) {
	expected := errors.New("probe failed")
	if err := runSafely(context.Background(), func(context.Context) error { return expected }); !errors.Is(err, expected) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRetryDelayIsCapped(t *testing.T) {
	delay := retryDelay(100)
	if delay < 64*time.Second || delay > 80*time.Second {
		t.Fatalf("unexpected capped retry delay: %v", delay)
	}
}

func TestApplicationWorkerIssueHasNoCameraScope(t *testing.T) {
	if scope := cameraScope(Spec{Name: "EventFinalizationWorker", ScopeKey: applicationScope}); scope != "" {
		t.Fatalf("application worker was assigned camera scope %q", scope)
	}
}

func TestSupervisorContainsPanicWithoutStoppingSibling(t *testing.T) {
	persistence := &testPersistence{issues: make(chan domain.OperationalIssue, 1)}
	supervisor := NewSupervisor(persistence, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor.Start(ctx)
	healthyRan := make(chan struct{}, 1)
	supervisor.Add(Spec{Name: "PanickingWorker", Interval: time.Hour, Run: func(context.Context) error {
		panic("isolated failure")
	}})
	supervisor.Add(Spec{Name: "HealthyWorker", Interval: time.Hour, Run: func(context.Context) error {
		healthyRan <- struct{}{}
		return nil
	}})
	select {
	case <-healthyRan:
	case <-time.After(time.Second):
		t.Fatal("sibling worker did not run")
	}
	select {
	case issue := <-persistence.issues:
		if issue.Source != "PanickingWorker" || issue.ErrorCode != "WorkerCycleFailed" {
			t.Fatalf("unexpected panic issue: %#v", issue)
		}
	case <-time.After(time.Second):
		t.Fatal("panicking worker did not report an issue")
	}
}

func TestRemoveAllCancelsEveryMatchingWorker(t *testing.T) {
	persistence := &testPersistence{issues: make(chan domain.OperationalIssue, 1)}
	supervisor := NewSupervisor(persistence, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor.Start(ctx)
	started := make(chan struct{}, 2)
	stopped := make(chan struct{}, 2)
	for _, scope := range []string{"camera-one", "camera-two"} {
		supervisor.Add(Spec{Name: "RtspRecorderWorker", ScopeKey: scope, Run: func(workerContext context.Context) error {
			started <- struct{}{}
			<-workerContext.Done()
			stopped <- struct{}{}
			return workerContext.Err()
		}})
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("worker did not start")
		}
	}
	supervisor.RemoveAll("RtspRecorderWorker")
	for range 2 {
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("worker was not canceled")
		}
	}
}

func TestReplacementWorkerRejectsStaleStoppedState(t *testing.T) {
	supervisor := NewSupervisor(&testPersistence{issues: make(chan domain.OperationalIssue, 1)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	key := workerKey("RtspRecorderWorker", "camera-one")
	supervisor.workers[key] = workerRegistration{id: 2}
	if !supervisor.workerWasReplaced(key, 1) {
		t.Fatal("replaced worker was allowed to publish stale state")
	}
	if supervisor.workerWasReplaced(key, 2) {
		t.Fatal("current worker was treated as replaced")
	}
	delete(supervisor.workers, key)
	if supervisor.workerWasReplaced(key, 1) {
		t.Fatal("removed worker should be allowed to publish its stopped state")
	}
}

func TestReadyRequiresSuccessfulGatewayProbe(t *testing.T) {
	supervisor := NewSupervisor(&testPersistence{issues: make(chan domain.OperationalIssue, 1)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if supervisor.Ready() {
		t.Fatal("supervisor was ready before a gateway probe")
	}
	now := time.Now().UTC()
	supervisor.setState(context.Background(), domain.WorkerState{
		WorkerName: "GatewayHealthWorker", ScopeKey: applicationScope,
		Stage: "Waiting", LastSuccessAtUtc: &now, UpdatedAtUtc: now,
	})
	if !supervisor.Ready() {
		t.Fatal("supervisor was not ready after a successful gateway probe")
	}
}
