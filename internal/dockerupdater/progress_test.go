package dockerupdater

import (
	"strings"
	"testing"
	"time"

	"github.com/dockerupbot/dockerupbot/internal/diun"
	"github.com/dockerupbot/dockerupbot/internal/dockerapi"
	"github.com/dockerupbot/dockerupbot/internal/store"
)

func TestHtmlEscape(t *testing.T) {
	got := htmlEscape(`a<b>&"c`)
	if !strings.Contains(got, "&lt;") || !strings.Contains(got, "&amp;") {
		t.Fatalf("%q", got)
	}
}

func TestFormatPullAggregates(t *testing.T) {
	s := dockerapi.PullState{
		Status: "Downloading", ID: "abc", Current: 500, Total: 1000,
		Done: 2, Active: 1, Waiting: 3, SpeedBps: 1024, ETA: 5 * time.Second,
	}
	msg := formatPull(1, 2, "ghcr.io/x/y:latest", s, time.Second)
	for _, want := range []string{"50%", "Layers", "ETA", "ghcr.io/x/y:latest", "Pulling"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in %s", want, msg)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	if formatBytes(0) != "0 B" {
		t.Fatal(formatBytes(0))
	}
	if formatBytes(1024) != "1.00 KB" {
		t.Fatal(formatBytes(1024))
	}
}

func TestJobsForCallbackOrdering(t *testing.T) {
	e := &Engine{
		pending: []store.PendingItem{
			{RecordID: 1, Event: diun.Event{Image: "a", Digest: "1"}, Selected: true},
			{RecordID: 2, Event: diun.Event{Image: "b", Digest: "2"}, Selected: false},
			{RecordID: 3, Event: diun.Event{Image: "c", Digest: "3"}, Selected: true},
		},
	}
	next := e.jobsForCallback("confirm_next")
	if len(next) != 1 || next[0].RecordID != 1 {
		t.Fatalf("%v", next)
	}
	sel := e.jobsForCallback("confirm_selected")
	if len(sel) != 2 || sel[0].RecordID != 1 || sel[1].RecordID != 3 {
		t.Fatalf("%v", sel)
	}
	all := e.jobsForCallback("confirm_all")
	if len(all) != 3 {
		t.Fatalf("%v", all)
	}
}

func TestConcurrencyGuardFlag(t *testing.T) {
	e := &Engine{running: true}
	if !e.running {
		t.Fatal("expected running guard")
	}
}
