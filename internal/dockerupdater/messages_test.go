package dockerupdater

import (
	"strings"
	"testing"
	"time"

	"github.com/dockerupbot/dockerupbot/internal/diun"
	"github.com/dockerupbot/dockerupbot/internal/store"
)

func TestFormatQueueCompleteListsJobs(t *testing.T) {
	msg := formatQueueComplete([]queueResult{
		{Image: "kopia/kopia:latest", Service: "kopia", Container: "kopia-home", Duration: 12 * time.Second},
	}, 12*time.Second)
	for _, want := range []string{"All updates succeeded", "kopia/kopia:latest", "kopia-home", "12s"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in %s", want, msg)
		}
	}
}

func TestFormatQueueFailedSections(t *testing.T) {
	msg := formatQueueFailed(
		queueResult{Image: "bad:latest", Err: errString("boom")},
		[]queueResult{{Image: "ok:latest"}},
		[]queueResult{{Image: "wait:latest"}},
	)
	for _, want := range []string{"Update failed", "bad:latest", "boom", "Already succeeded", "ok:latest", "Still waiting", "wait:latest"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in %s", want, msg)
		}
	}
}

func TestFormatSkippedListsImages(t *testing.T) {
	msg := formatSkipped([]string{"a:latest", "b:latest"})
	if !strings.Contains(msg, "Skipped 2") || !strings.Contains(msg, "a:latest") {
		t.Fatal(msg)
	}
}

func TestFormatPendingSelectMarks(t *testing.T) {
	items := []store.PendingItem{
		{Event: diun.Event{Image: "x:1", Digest: "sha256:abcdef1234567890"}, Selected: true},
		{Event: diun.Event{Image: "y:1"}, Selected: false},
	}
	msg := formatPending(items, true)
	if !strings.Contains(msg, "☑") || !strings.Contains(msg, "☐") || !strings.Contains(msg, "abcdef123456") {
		t.Fatal(msg)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
