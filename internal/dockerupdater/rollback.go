package dockerupdater

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/dockerupbot/dockerupbot/internal/compose"
	"github.com/dockerupbot/dockerupbot/internal/diun"
	"github.com/dockerupbot/dockerupbot/internal/dockerapi"
	"github.com/dockerupbot/dockerupbot/internal/store"
	"github.com/dockerupbot/dockerupbot/internal/telegram"
)

// NotifyRecovery sends a Telegram alert when startup recovery paused jobs.
func (e *Engine) NotifyRecovery(ctx context.Context) {
	e.mu.Lock()
	n := e.recovered
	paused := e.paused
	e.mu.Unlock()
	if n == 0 {
		pausedFlag, jobs, _ := e.db.GetQueuePaused()
		if !pausedFlag || len(jobs) == 0 {
			if !paused {
				return
			}
		} else {
			n = len(jobs)
		}
	}
	if n == 0 {
		return
	}
	targets := e.cfg.NotifyTargets()
	if len(targets) == 0 {
		return
	}
	text := formatRecovery(n)
	buttons := pausedButtons()
	for _, chat := range targets {
		id, err := e.bot.Send(ctx, chat, text, buttons)
		if err != nil {
			slog.Error("recovery notify failed", "chat", chat, "err", err)
			continue
		}
		e.mu.Lock()
		e.setLatestLocked(chat, id)
		e.mu.Unlock()
	}
}

// RunQuietHoursFlusher wakes when quiet hours end and sends a digest of pending updates.
func (e *Engine) RunQuietHoursFlusher(ctx context.Context) {
	if e.cfg.QuietHours == "" {
		return
	}
	slog.Info("quiet-hours flusher started", "quiet_hours", e.cfg.QuietHours)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	wasQuiet := e.cfg.InQuietHours(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			nowQuiet := e.cfg.InQuietHours(time.Now())
			if wasQuiet && !nowQuiet {
				e.mu.Lock()
				pending := len(e.pending)
				buffered := e.quietBuffered
				e.quietBuffered = false
				e.mu.Unlock()
				if buffered && pending > 0 {
					slog.Info("quiet hours ended; sending digest", "pending", pending)
					e.notifyPending()
				}
			}
			wasQuiet = nowQuiet
		}
	}
}

func (e *Engine) rollbackByID(ctx context.Context, api *telegram.API, chat, msg, recordID int64) {
	rec, ok := e.db.Get(recordID)
	if !ok {
		_, _ = api.Send(ctx, chat, msgHeader("❌ <b>Rollback failed</b>")+"Record not found.\n\n"+msgFooter(""), nil)
		return
	}
	if rec.PreviousImage == "" || rec.PreviousDigest == "" || rec.ContainerName == "" {
		_, _ = api.Send(ctx, chat, msgHeader("❌ <b>Rollback unavailable</b>")+
			"No previous image metadata for this job.\n\n"+msgFooter(""), nil)
		return
	}
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		_, _ = api.Send(ctx, chat, "An update is already running.", nil)
		return
	}
	e.running = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
		e.armCoalesce()
	}()

	_ = api.Edit(ctx, chat, msg, msgHeader("↩️ <b>Rolling back</b>")+
		fmt.Sprintf("<code>%s</code>\n", htmlEscape(rec.Image)), [][]telegram.Button{})
	if err := e.restoreImage(ctx, rec); err != nil {
		slog.Error("rollback failed", "id", recordID, "err", err)
		_ = e.db.SetStatus(recordID, store.StatusFailed, "rollback: "+err.Error())
		_ = api.Edit(ctx, chat, msg, msgHeader("❌ <b>Rollback failed</b>")+htmlEscape(err.Error()), [][]telegram.Button{})
		return
	}
	if err := e.db.SetStatus(recordID, store.StatusRolledBack, ""); err != nil {
		slog.Error("save rollback result", "id", recordID, "err", err)
		_ = api.Edit(ctx, chat, msg, "Previous image restored. Could not save the rollback result.", [][]telegram.Button{})
		return
	}
	_ = api.Edit(ctx, chat, msg, formatRolledBack(rec.Image, rec.PreviousImage), [][]telegram.Button{})
}

func rollbackTag(imageID string) string {
	return "dockerupbot-rollback:" + strings.TrimPrefix(imageID, "sha256:")
}

func (e *Engine) restoreImage(ctx context.Context, rec store.Record) error {
	ev := diun.Event{
		Image:    firstNonEmpty(rec.NewImage, rec.Image),
		Metadata: map[string]string{"ctn_names": rec.ContainerName},
	}
	target, _, _, err := e.resolveComposeTarget(ctx, ev)
	if err != nil {
		return err
	}
	if target.Project != rec.Project || target.Service != rec.Service {
		return fmt.Errorf("rollback target is now %s/%s", target.Project, target.Service)
	}
	configuredImage, err := compose.Image(ctx, target)
	if err != nil {
		return err
	}
	if !dockerapi.SameImageRef(configuredImage, rec.PreviousImage) || strings.Contains(configuredImage, "@") {
		return fmt.Errorf("Compose image changed to %q since the update", configuredImage)
	}
	if err := e.db.SetStatus(rec.ID, store.StatusRollingBack, ""); err != nil {
		return err
	}
	if err := e.docker.TagImage(ctx, rec.PreviousDigest, configuredImage); err != nil {
		return err
	}
	out, err := compose.Up(ctx, target)
	if out != "" {
		_ = e.db.AppendLog(rec.ID, out)
	}
	if err != nil {
		return fmt.Errorf("rollback compose: %w", err)
	}
	return e.verifyHealth(ctx, target, rec.PreviousDigest)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
