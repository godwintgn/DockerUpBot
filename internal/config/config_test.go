package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dockerupbot/dockerupbot/internal/config"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"TELEGRAM_TOKEN", "TELEGRAM_BOT_TOKEN",
		"ALLOWED_USERS", "DOCKERUPBOT_ALLOWED_USERS", "TELEGRAM_ALLOWED_USERS",
		"TELEGRAM_CHAT_ID",
		"WEBHOOK_SECRET", "DOCKERUPBOT_WEBHOOK_SECRET",
		"COMPOSE_ROOT", "DOCKERUPBOT_COMPOSE_ROOT",
		"HTTP_ADDR", "DOCKERUPBOT_HTTP_ADDR",
		"NOTIFY_EMPTY_SCAN", "DOCKERUPBOT_NOTIFY_EMPTY_SCAN",
		"DIUN_CONTAINER", "DOCKERUPBOT_DIUN_CONTAINER",
		"SKIP_IMAGES", "SKIP_CONTAINERS", "QUIET_HOURS",
		"PRUNE_AFTER_SUCCESS", "REQUIRE_HEALTHY",
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

func TestLoadNotifyTargetsChatID(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	content := "http_addr: :9467\ntelegram_token: tok\nwebhook_secret: sec\ntelegram_allowed_users: 1,2\ntelegram_chat_id: -100\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	targets := cfg.NotifyTargets()
	if len(targets) != 1 || targets[0] != -100 {
		t.Fatalf("%v", targets)
	}
}

func TestLoadRequiresWebhookSecret(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("http_addr: :9467\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TELEGRAM_TOKEN", "tok")
	t.Setenv("ALLOWED_USERS", "1")
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected WEBHOOK_SECRET required")
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("http_addr: :9467\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TELEGRAM_TOKEN", "tok")
	t.Setenv("ALLOWED_USERS", "11,22")
	t.Setenv("WEBHOOK_SECRET", "sec")
	t.Setenv("COMPOSE_ROOT", "/apps")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.Token != "tok" || cfg.WebhookSecret != "sec" || cfg.Update.ComposeRoot != "/apps" {
		t.Fatalf("%+v", cfg)
	}
	targets := cfg.NotifyTargets()
	if len(targets) != 2 {
		t.Fatalf("%v", targets)
	}
}

func TestEnsureDefaultCreatesFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := config.EnsureDefault(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "data_path:") {
		t.Fatalf("%s", b)
	}
	if err := config.EnsureDefault(path); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureDefaultRejectsNonEmptyDirectory(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.Mkdir(path, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "junk"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := config.EnsureDefault(path); err == nil {
		t.Fatal("expected error for non-empty directory path")
	}
}

func TestEnsureDefaultReplacesEmptyDirectory(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.Mkdir(path, 0750); err != nil {
		t.Fatal(err)
	}
	if err := config.EnsureDefault(path); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		t.Fatalf("expected file, got dir=%v err=%v", st != nil && st.IsDir(), err)
	}
}

func TestLoadCreatesMissingFile(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("TELEGRAM_TOKEN", "tok")
	t.Setenv("ALLOWED_USERS", "1")
	t.Setenv("WEBHOOK_SECRET", "sec")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataPath != "/data/dockerupbot.json" {
		t.Fatalf("%+v", cfg)
	}
}

func TestSanitizeDropsBotAsChatID(t *testing.T) {
	clearEnv(t)
	c := config.Config{}
	c.Telegram.ChatID = 8209257815
	c.Telegram.AllowedUsers = []int64{86186566}
	if !c.SanitizeNotifyTargets(8209257815) {
		t.Fatal("expected fix")
	}
	targets := c.NotifyTargets()
	if len(targets) != 1 || targets[0] != 86186566 {
		t.Fatalf("%v", targets)
	}
}

func TestNotifyEmptyScanEnv(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("http_addr: :9467\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TELEGRAM_TOKEN", "tok")
	t.Setenv("ALLOWED_USERS", "1")
	t.Setenv("WEBHOOK_SECRET", "sec")
	t.Setenv("NOTIFY_EMPTY_SCAN", "true")
	t.Setenv("DIUN_CONTAINER", "my-diun")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.NotifyEmptyScan || cfg.DiunContainer != "my-diun" {
		t.Fatalf("%+v", cfg)
	}
}

func TestShouldSkip(t *testing.T) {
	c := config.Config{
		SkipImages:     []string{"diun/testnotif", "/postgres/"},
		SkipContainers: []string{"diun"},
	}
	if !c.ShouldSkip("docker.io/diun/testnotif:latest", "x") {
		t.Fatal("image skip")
	}
	if !c.ShouldSkip("ghcr.io/immich-app/postgres:14", "db") {
		t.Fatal("regex skip")
	}
	if !c.ShouldSkip("nginx:latest", "diun") {
		t.Fatal("container skip")
	}
	if c.ShouldSkip("nginx:latest", "web") {
		t.Fatal("should not skip")
	}
}

func TestQuietHoursOvernight(t *testing.T) {
	c := config.Config{QuietHours: "23:00-07:00"}
	quiet := time.Date(2026, 9, 15, 1, 0, 0, 0, time.Local)
	active := time.Date(2026, 9, 15, 12, 0, 0, 0, time.Local)
	if !c.InQuietHours(quiet) {
		t.Fatal("expected quiet")
	}
	if c.InQuietHours(active) {
		t.Fatal("expected active")
	}
}
