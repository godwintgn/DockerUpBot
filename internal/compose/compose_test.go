package compose_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dockerupbot/dockerupbot/internal/compose"
)

func TestValidateRootAllowsInside(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "immich")
	if err := compose.ValidateRoot(dir, root); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRootRejectsOutside(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	if err := compose.ValidateRoot(other, root); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateRootEmptyAllowsAll(t *testing.T) {
	if err := compose.ValidateRoot("/anywhere", ""); err != nil {
		t.Fatal(err)
	}
}

func TestValidateFilesInsideRoot(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "stack")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := compose.ValidateFiles([]string{"docker-compose.yml", filepath.Join(dir, "docker-compose.yml")}, dir, root); err != nil {
		t.Fatal(err)
	}
}

func TestValidateFilesRejectsOutside(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "stack")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "evil.yml")
	if err := compose.ValidateFiles([]string{outside}, dir, root); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateRootRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := compose.ValidateRoot(link, root); err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}
}
func TestParseConfigFiles(t *testing.T) {
	got := compose.ParseConfigFiles("docker-compose.yml, docker-compose.override.yml")
	if len(got) != 2 || got[0] != "docker-compose.yml" || got[1] != "docker-compose.override.yml" {
		t.Fatalf("%v", got)
	}
}
