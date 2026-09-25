package compose

import (
	"bytes"
	"context"
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

// Up recreates a single service: docker compose [-f ...] up -d -- <service>
func Up(ctx context.Context, t Target) (string, error) {
	args := []string{"compose"}
	for _, f := range t.ConfigFiles {
		args = append(args, "-f", f)
	}
	args = append(args, "up", "-d", "--", t.Service)
	slog.Info("exec docker compose", "dir", t.ProjectDir, "args", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = t.ProjectDir
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
