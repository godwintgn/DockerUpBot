package dockerupdater

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dockerupbot/dockerupbot/internal/compose"
	"github.com/dockerupbot/dockerupbot/internal/config"
	"github.com/dockerupbot/dockerupbot/internal/diun"
	"github.com/dockerupbot/dockerupbot/internal/dockerapi"
	"github.com/dockerupbot/dockerupbot/internal/store"
	"github.com/dockerupbot/dockerupbot/internal/telegram"
)

func TestComposeUpdateAndRollback(t *testing.T) {
	if os.Getenv("DOCKERUPBOT_TEST_DOCKER") != "1" {
		t.Skip("set DOCKERUPBOT_TEST_DOCKER=1 to use a disposable Compose project")
	}
	dir := t.TempDir()
	project := fmt.Sprintf("dockerupbot-test-%d", time.Now().UnixNano())
	image := "alpine:" + project
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	docker := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	docker("pull", "alpine:3.22")
	docker("pull", "alpine:3.23")
	oldID := docker("image", "inspect", "--format", "{{.Id}}", "alpine:3.22")
	expectedID := docker("image", "inspect", "--format", "{{.Id}}", "alpine:3.23")
	reference := docker("image", "inspect", "--format", "{{index .RepoDigests 0}}", "alpine:3.23")
	_, digest, ok := strings.Cut(reference, "@")
	if !ok || oldID == expectedID {
		t.Fatalf("expected two image IDs and a registry digest, got %s, %s, %s", oldID, expectedID, reference)
	}
	backup := rollbackTag(oldID)
	backupExists := exec.CommandContext(ctx, "docker", "image", "inspect", backup).Run() == nil
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, args := range [][]string{
			{"compose", "--project-name", project, "down", "--remove-orphans"},
			{"image", "rm", image},
		} {
			cmd := exec.CommandContext(cleanupCtx, "docker", args...)
			cmd.Dir = dir
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("cleanup %v: %v\n%s", args, err, output)
			}
		}
		if !backupExists && exec.CommandContext(cleanupCtx, "docker", "image", "inspect", backup).Run() == nil {
			if output, err := exec.CommandContext(cleanupCtx, "docker", "image", "rm", backup).CombinedOutput(); err != nil {
				t.Errorf("cleanup backup tag: %v\n%s", err, output)
			}
		}
	})
	docker("tag", oldID, image)
	composeFile := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(composeFile, []byte(fmt.Sprintf(`services:
  app:
    image: %s
    pull_policy: always
    command: ["sleep", "600"]
    depends_on:
      - helper
  helper:
    image: alpine:3.22
    command: ["sleep", "600"]
`, image)), 0600); err != nil {
		t.Fatal(err)
	}
	docker("compose", "--project-name", project, "up", "-d", "--no-deps", "--pull", "never", "app")

	client, err := dockerapi.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	container := project + "-app-1"
	event := diun.Event{Image: image, Digest: digest, Metadata: map[string]string{"ctn_names": container}}
	items, err := db.SaveEvent(event, 0)
	if err != nil {
		t.Fatal(err)
	}
	api := telegram.New("unused", nil, 2)
	e := New(config.Config{Update: config.UpdateConfig{ComposeRoot: dir, HealthTimeoutSeconds: 5}}, api, db, client, ctx)
	noTelegram, stopTelegram := context.WithCancel(ctx)
	stopTelegram()
	if _, err := e.runOne(noTelegram, ctx, api, 0, 0, 1, 1, items[0]); err != nil {
		t.Fatal(err)
	}
	assertImage := func(want string) {
		t.Helper()
		info, err := client.InspectContainer(ctx, container)
		if err != nil || info.Image != want || !info.State.Running || info.Config.Image != image {
			t.Fatalf("container=%+v, err=%v, want running %s with tag %s", info, err, want, image)
		}
	}
	assertImage(expectedID)
	target := compose.Target{Project: project, Service: "app", Container: container}
	if err := e.verifyHealth(ctx, target, oldID); err == nil {
		t.Fatal("a healthy container running the wrong image passed verification")
	}
	if got := docker("compose", "--project-name", project, "ps", "--all", "--services"); got != "app" {
		t.Fatalf("Compose touched dependencies: %q", got)
	}
	record, _ := db.Get(items[0].RecordID)
	if err := e.restoreImage(ctx, record); err != nil {
		t.Fatal(err)
	}
	assertImage(oldID)
}
