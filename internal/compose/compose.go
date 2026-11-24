package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
)

// Target identifies a Compose service to update.
type Target struct {
	ProjectDir  string
	Service     string
	ConfigFiles []string
	Project     string
	Container   string
}

// ValidateRoot ensures dir is under composeRoot when composeRoot is set.
// Symlinks are resolved when the path exists, so a link inside the root cannot point outside it.
func ValidateRoot(dir, composeRoot string) error {
	if composeRoot == "" {
		return nil
	}
	absDir, err := resolvePath(dir)
	if err != nil {
		return err
	}
	absRoot, err := resolvePath(composeRoot)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absDir)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("compose project %s is outside compose_root %s", absDir, absRoot)
	}
	return nil
}

// ValidateFiles ensures each Compose -f path stays under composeRoot.
// Relative paths are joined to projectDir. An empty composeRoot allows them, matching ValidateRoot.
func ValidateFiles(files []string, projectDir, composeRoot string) error {
	if composeRoot == "" {
		return nil
	}
	for _, f := range files {
		p := f
		if !filepath.IsAbs(p) {
			p = filepath.Join(projectDir, p)
		}
		if err := ValidateRoot(p, composeRoot); err != nil {
			return fmt.Errorf("compose file %s is outside compose_root", f)
		}
	}
	return nil
}

func resolvePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

func command(ctx context.Context, t Target, args ...string) *exec.Cmd {
	base := []string{"compose", "--project-name", t.Project, "--project-directory", t.ProjectDir}
	for _, f := range t.ConfigFiles {
		base = append(base, "-f", f)
	}
	cmd := exec.CommandContext(ctx, "docker", append(base, args...)...)
	cmd.Dir = t.ProjectDir
	return cmd
}

// Image returns the resolved image reference for the target service.
func Image(ctx context.Context, t Target) (string, error) {
	out, err := command(ctx, t, "config", "--no-env-resolution", "--format", "json", "--", t.Service).Output()
	if err != nil {
		return "", fmt.Errorf("resolve Compose image: %w", err)
	}
	var config struct {
		Services map[string]struct{ Image string }
	}
	if err := json.Unmarshal(out, &config); err != nil {
		return "", fmt.Errorf("decode Compose config: %w", err)
	}
	image := config.Services[t.Service].Image
	if image == "" {
		return "", fmt.Errorf("no configured image for service %q", t.Service)
	}
	return image, nil
}

// Up recreates one service using its locally prepared image.
func Up(ctx context.Context, t Target) (string, error) {
	cmd := command(ctx, t, "up", "-d", "--no-deps", "--pull", "never", "--no-build", "--", t.Service)
	slog.Info("exec docker compose", "dir", t.ProjectDir, "args", cmd.Args)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err != nil {
		slog.Error("docker compose failed", "service", t.Service, "err", err, "output", strings.TrimSpace(buf.String()))
	}
	return buf.String(), err
}

// ParseConfigFiles splits the Compose label com.docker.compose.project.config_files.
func ParseConfigFiles(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
