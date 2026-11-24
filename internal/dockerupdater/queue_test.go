package dockerupdater

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dockerupbot/dockerupbot/internal/config"
	"github.com/dockerupbot/dockerupbot/internal/diun"
	"github.com/dockerupbot/dockerupbot/internal/store"
)

func TestApprovalUsesSnapshotAndRecoveryPreservesNewArrival(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := diun.Event{Image: "alpine:latest", Digest: "sha256:" + strings.Repeat("a", 64)}
	second := diun.Event{Image: "busybox:latest", Digest: "sha256:" + strings.Repeat("b", 64)}
	items, err := db.SaveEvent(first, 0)
	if err != nil {
		t.Fatal(err)
	}
	e := New(config.Config{}, nil, db, nil, context.Background())
	e.setLatestLocked(1, 99)
	e.setLatestLocked(2, 99)
	e.confirmations[1] = confirmation{token: "first", jobs: items}
	e.confirmations[2] = confirmation{token: "second", jobs: items}
	e.pending, err = db.SaveEvent(second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.acceptConfirmation(2, 99, "first"); err == nil {
		t.Fatal("another chat accepted the confirmation token")
	}
	jobs, err := e.acceptConfirmation(1, 99, "first")
	if err != nil || len(jobs) != 1 || jobs[0].Event.Digest != first.Digest {
		t.Fatalf("approved jobs = %v, err = %v", jobs, err)
	}

	for _, action := range []string{"queue_stop", "queue_skip_continue"} {
		t.Run(action, func(t *testing.T) {
			if err := db.SaveQueue(e.pending, jobs, 0, false, jobIDs(jobs), store.StatusFailed); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			recovered := New(config.Config{}, nil, reopened, nil, context.Background())
			recovered.RecoverInFlight(context.Background())
			remaining, pending, err := recovered.resumePausedLocked(action)
			if err != nil || len(remaining) != 0 || len(pending) == 0 || pending[0].Event.Image != second.Image {
				t.Fatalf("remaining = %v, pending = %v, err = %v", remaining, pending, err)
			}
			want := 1
			if action == "queue_stop" {
				want = 2
			}
			if stored := reopened.GetPending(); len(stored) != want {
				t.Fatalf("persisted %d pending jobs, want %d", len(stored), want)
			}
		})
	}
}

func TestDrainWaitsForActiveWorkAndRejectsNewStarts(t *testing.T) {
	e := &Engine{running: true}
	done := e.Drain()
	select {
	case <-done:
		t.Fatal("drain finished while work was still active")
	default:
	}
	e.mu.Lock()
	e.finishWorkLocked()
	e.mu.Unlock()

	select {
	case <-e.Drain():
	default:
		t.Fatal("drain did not finish when work completed")
	}
	if _, err := e.acceptConfirmation(1, 1, "old"); err == nil || err.Error() != "bot is shutting down" {
		t.Fatalf("confirmation during shutdown returned %v", err)
	}
}
