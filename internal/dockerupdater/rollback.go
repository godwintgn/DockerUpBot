package dockerupdater

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/dockerupbot/dockerupbot/internal/compose"
	"github.com/dockerupbot/dockerupbot/internal/diun"
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
	buttons := [][]telegram.Button{{
		{Text: "▶️ CONTINUE", CallbackData: "queue_continue"},
		{Text: "⏹️ STOP", CallbackData: "queue_stop"},
		{Text: "📋 LOGS", CallbackData: "show_logs"},
	}}
	var lastChat, lastMsg int64
	for _, chat := range targets {
		id, err := e.bot.Send(ctx, chat, text, buttons)
		if err != nil {
			slog.Error("recovery notify failed", "chat", chat, "err", err)
			continue
		}
		lastChat, lastMsg = chat, id
	}
	if lastChat != 0 {
		e.mu.Lock()
		e.latestChat, e.latestMsg = lastChat, lastMsg
		e.mu.Unlock()
		_ = e.db.SetLatest(lastChat, lastMsg)
		slog.Info("recovery notification sent", "jobs", n, "chat", lastChat, "message", lastMsg)
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
	_ = e.db.SetStatus(recordID, store.StatusRollingBack, "")
	_ = api.Edit(ctx, chat, msg, msgHeader("↩️ <b>Rolling back</b>")+
		fmt.Sprintf("<code>%s</code>\n↳ <code>%s</code>\n\n", htmlEscape(rec.Image), htmlEscape(rec.PreviousImage))+
		msgFooter("Live"), [][]telegram.Button{})

	tagAs := rec.PreviousImage
	if tagAs == "" {
		tagAs = rec.NewImage
	}
	if err := e.docker.TagImage(ctx, rec.PreviousDigest, tagAs); err != nil {
		slog.Error("rollback tag failed", "err", err)
		_ = e.db.SetStatus(recordID, store.StatusFailed, "rollback tag: "+err.Error())
		_ = api.Edit(ctx, chat, msg, msgHeader("❌ <b>Rollback failed</b>")+htmlEscape(err.Error())+"\n\n"+msgFooter(""), [][]telegram.Button{})
		return
	}

	ev := diun.Event{
		Image:  firstNonEmpty(rec.NewImage, rec.Image),
		Digest: rec.NewDigest,
		Metadata: map[string]string{
			"ctn_names": rec.ContainerName,
		},
	}
	target, _, _, err := e.resolveComposeTarget(ctx, ev)
	if err != nil {
		_ = e.db.SetStatus(recordID, store.StatusFailed, "rollback resolve: "+err.Error())
		_ = api.Edit(ctx, chat, msg, msgHeader("❌ <b>Rollback failed</b>")+htmlEscape(err.Error())+"\n\n"+msgFooter(""), [][]telegram.Button{})
		return
	}
	out, err := compose.Up(ctx, target)
	if out != "" {
		_ = e.db.AppendLog(recordID, out)
	}
	if err != nil {
		_ = e.db.SetStatus(recordID, store.StatusFailed, "rollback compose: "+err.Error())
		_ = api.Edit(ctx, chat, msg, msgHeader("❌ <b>Rollback failed</b>")+htmlEscape(err.Error())+"\n\n"+msgFooter(""), [][]telegram.Button{})
		return
	}
	if err := e.verifyHealth(ctx, target.Container); err != nil {
		_ = e.db.SetStatus(recordID, store.StatusFailed, "rollback health: "+err.Error())
		_ = api.Edit(ctx, chat, msg, msgHeader("❌ <b>Rollback health failed</b>")+htmlEscape(err.Error())+"\n\n"+msgFooter(""), [][]telegram.Button{})
		return
	}
	_ = e.db.SetStatus(recordID, store.StatusRolledBack, "")
	_ = api.Edit(ctx, chat, msg, formatRolledBack(rec.Image, rec.PreviousImage), [][]telegram.Button{})
	slog.Info("rollback complete", "id", recordID, "image", rec.Image, "previous", rec.PreviousImage)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
