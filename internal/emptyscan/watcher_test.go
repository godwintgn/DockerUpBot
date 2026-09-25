package emptyscan_test

import (
	"strings"
	"testing"

	"github.com/dockerupbot/dockerupbot/internal/emptyscan"
)

func TestParseJobsCompleted(t *testing.T) {
	line := `Tue, 15 Sep 2026 12:00:21 +04 INF Jobs completed added=1 failed=1 skipped=0 unchanged=15 updated=0`
	s, ok := emptyscan.ParseJobsCompleted(line)
	if !ok {
		t.Fatal("expected parse")
	}
	if s.Added != 1 || s.Failed != 1 || s.Unchanged != 15 || s.Updated != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestParseJobsCompletedUpdated(t *testing.T) {
	line := `Jobs completed added=0 failed=0 skipped=0 unchanged=10 updated=2`
	s, ok := emptyscan.ParseJobsCompleted(line)
	if !ok || s.Updated != 2 {
		t.Fatalf("%v %+v", ok, s)
	}
}

func TestFormatMessage(t *testing.T) {
	msg := emptyscan.FormatMessage(emptyscan.JobStats{Unchanged: 15, Failed: 1})
	for _, want := range []string{"0 updates", "Unchanged", "15", "NOTIFY_EMPTY_SCAN"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in %s", want, msg)
		}
	}
}
