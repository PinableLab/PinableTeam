package repo

import (
	"errors"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestDesktopProbeRevocationWinsOverConcurrentHeartbeatAndEnqueue(t *testing.T) {
	s := newTestRunner(t)
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	runner := domain.Runner{ID: domain.RunnerDesktopProbeIDPrefix + "race", Status: domain.RunnerStatusOnline, SessionTokenHash: "test-hash", SessionExpiresAt: &future, Runtimes: []domain.RunnerRuntime{{Provider: domain.RunnerProviderDesktop, Status: domain.RuntimeStatusOnline, Capabilities: []string{domain.RunnerCapabilityDesktopProbe}}}}
	if err := s.PutRunner(runner); err != nil {
		t.Fatal(err)
	}
	task := domain.RunnerTask{ID: "live", RunnerID: runner.ID, Provider: domain.RunnerProviderDesktop, Action: domain.RunnerTaskActionDesktopProbe, Status: domain.RunnerTaskQueued, MaxAttempts: 1}
	if err := s.PutRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimRunnerTask(runner.ID, []string{task.Provider}, now, time.Minute); err != nil || !ok {
		t.Fatalf("claim: %v, %v", ok, err)
	}
	if err := s.StartRunnerTask(task.ID, runner.ID, now); err != nil {
		t.Fatal(err)
	}
	task.ID = "new"
	start := make(chan struct{})
	beat := make(chan error, 1)
	create := make(chan error, 1)
	go func() { <-start; beat <- s.RunnerHeartbeat(runner.ID, runner.Runtimes, "", now, future) }()
	go func() { <-start; create <- s.PutRunnerTask(task) }()
	close(start)
	if err := s.DeregisterRunner(runner.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := <-beat; err != nil && !errors.Is(err, ErrRunnerNotFound) {
		t.Fatalf("racing heartbeat: %v", err)
	}
	if err := <-create; err != nil && !errors.Is(err, ErrRunnerNotFound) {
		t.Fatalf("racing enqueue: %v", err)
	}
	if _, ok := s.RunnerSessionByHash(runner.SessionTokenHash); ok {
		t.Fatal("revoked registration came back")
	}
	if err := s.RunnerHeartbeat(runner.ID, runner.Runtimes, "", now, future); !errors.Is(err, ErrRunnerNotFound) {
		t.Fatalf("stale heartbeat: %v", err)
	}
	for _, saved := range s.RunnerTasks() {
		if saved.Status != domain.RunnerTaskCanceled || saved.LeaseOwner != "" {
			t.Fatalf("probe outlived its binding: %+v", saved)
		}
	}
}

func TestDesktopProbeExpiredSessionCannotRenew(t *testing.T) {
	s := newTestRunner(t)
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	for _, expiry := range []*time.Time{nil, &past} {
		runner := domain.Runner{ID: domain.RunnerDesktopProbeIDPrefix + "expired", SessionTokenHash: "expired-hash", SessionExpiresAt: expiry}
		if err := s.PutRunner(runner); err != nil {
			t.Fatal(err)
		}
		if _, ok := s.RunnerSessionByHash(runner.SessionTokenHash); ok {
			t.Fatalf("accepted invalid expiry %v", expiry)
		}
		if err := s.RunnerHeartbeat(runner.ID, nil, "", now, now.Add(time.Hour)); !errors.Is(err, ErrRunnerNotFound) {
			t.Fatalf("renewed invalid expiry %v: %v", expiry, err)
		}
	}
}
