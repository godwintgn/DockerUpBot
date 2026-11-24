package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dockerupbot/dockerupbot/internal/diun"
	"github.com/dockerupbot/dockerupbot/internal/store"
)

func TestAddSetStatusAndPendingRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.Add("nginx:latest", "sha256:1", store.StatusPending)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetStatus(id, store.StatusPulling, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetStatus(id, store.StatusSuccess, ""); err != nil {
		t.Fatal(err)
	}
	r, ok := db.Get(id)
	if !ok || r.Status != store.StatusSuccess || r.CompletedAt == nil {
		t.Fatalf("record=%+v ok=%v", r, ok)
	}
	items := []store.PendingItem{{
		RecordID: id,
		Event:    diun.Event{Image: "nginx:latest", Digest: "sha256:1"},
		Selected: true,
	}}
	if err := db.SetPending(items); err != nil {
		t.Fatal(err)
	}
	if err := db.SetLatest(42, 99); err != nil {
		t.Fatal(err)
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	db2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	pending := db2.GetPending()
	if len(pending) != 1 || pending[0].Event.Image != "nginx:latest" {
		t.Fatalf("pending=%v", pending)
	}
	latest := db2.LatestMessages()
	if latest[42] != 99 {
		t.Fatalf("latest=%v", latest)
	}
}

func TestDedupKeyLogic(t *testing.T) {
	// Mirrors Engine.DiunEvent dedup: same image+digest is ignored.
	seen := map[string]bool{}
	key := func(image, digest string) string { return image + "|" + digest }
	events := []diun.Event{
		{Image: "a", Digest: "1"},
		{Image: "a", Digest: "1"},
		{Image: "a", Digest: "2"},
	}
	var kept []diun.Event
	for _, e := range events {
		k := key(e.Image, e.Digest)
		if seen[k] {
			continue
		}
		seen[k] = true
		kept = append(kept, e)
	}
	if len(kept) != 2 {
		t.Fatalf("kept=%d", len(kept))
	}
}

func TestRollbackMeta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := db.Add("img:new", "sha256:new", store.StatusPending)
	if err := db.SetRollbackMeta(id, "proj", "svc", "img:old", "sha256:old", "img:new", "sha256:new", "ctr"); err != nil {
		t.Fatal(err)
	}
	r, ok := db.Get(id)
	if !ok || r.PreviousDigest != "sha256:old" || r.Service != "svc" {
		t.Fatalf("%+v", r)
	}
}

func TestSaveEventFailureLeavesPendingStateIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := diun.Event{Image: "app:latest", Digest: "sha256:first"}
	items, err := db.SaveEvent(first, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".previous"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveEvent(diun.Event{Image: first.Image, Digest: "sha256:later"}, items[0].RecordID); err == nil {
		t.Fatal("SaveEvent accepted a failed write")
	}
	pending := db.GetPending()
	if len(pending) != 1 || pending[0].Event.Digest != first.Digest || len(db.ListHistory(0)) != 1 {
		t.Fatalf("pending=%+v history=%+v", pending, db.ListHistory(0))
	}
}
