package dockerupdater

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dockerupbot/dockerupbot/internal/compose"
	"github.com/dockerupbot/dockerupbot/internal/config"
	"github.com/dockerupbot/dockerupbot/internal/diun"
	"github.com/dockerupbot/dockerupbot/internal/dockerapi"
	"github.com/dockerupbot/dockerupbot/internal/store"
	"github.com/dockerupbot/dockerupbot/internal/telegram"
	"github.com/dockerupbot/dockerupbot/internal/version"
)

type Engine struct {
	cfg           config.Config
	bot           *telegram.API
	db            store.Store
	docker        *dockerapi.Client
	mu            sync.Mutex
	pending       []store.PendingItem
	running       bool
	paused        bool
	selectMode    bool
	latestChat    int64
	latestMsg     int64
	workCtx       context.Context
	lastFailID    int64
	recovered     int
	quietBuffered bool
	lastSuccessID int64
	coalesceArmed bool
	pauseAfter    bool
	cancelRun     bool
	runCancel     context.CancelFunc
}

const coalesceDelay = 8 * time.Second

// errSkipJob means this image cannot be updated and the queue should continue.
var errSkipJob = errors.New("skip job")

// errUserCancel means the user pressed CANCEL during a running update.
var errUserCancel = errors.New("update cancelled")

func New(c config.Config, b *telegram.API, d store.Store, docker *dockerapi.Client, workCtx context.Context) *Engine {
	e := &Engine{cfg: c, bot: b, db: d, docker: docker, workCtx: workCtx}
	e.pending = d.GetPending()
	if latest := d.GetLatest(); latest != nil {
		e.latestChat = latest.ChatID
		e.latestMsg = latest.MessageID
	}
	e.selectMode = d.GetSelectMode()
	paused, jobs, _ := d.GetQueuePaused()
	e.paused = paused
	slog.Info("engine ready",
		"pending", len(e.pending),
		"paused", e.paused,
		"paused_jobs", len(jobs),
		"notify_targets", len(c.NotifyTargets()),
		"compose_root", c.Update.ComposeRoot,
	)
	return e
}

// RecoverInFlight reconciles jobs left in pulling/updating after a crash (spec §56).
func (e *Engine) RecoverInFlight(ctx context.Context) {
	hist := e.db.ListHistory(50)
	recovered := 0
	for _, r := range hist {
		switch r.Status {
		case store.StatusPulling, store.StatusUpdating, store.StatusVerifying, store.StatusQueued, store.StatusApproved:
			slog.Warn("recovering interrupted job", "id", r.ID, "image", r.Image, "status", r.Status)
			_ = e.db.SetStatus(r.ID, store.StatusPaused, "interrupted by restart; manual continue required")
			item := store.PendingItem{
				RecordID: r.ID,
				Event: diun.Event{
					Image:  r.Image,
					Digest: r.Digest,
					Status: "update",
					Metadata: map[string]string{
						"ctn_names": r.ContainerName,
					},
				},
			}
			e.mu.Lock()
			found := false
			for _, p := range e.pending {
				if p.RecordID == r.ID || (p.Event.Image == r.Image && p.Event.Digest == r.Digest) {
					found = true
					break
				}
			}
			if !found {
				e.pending = append(e.pending, item)
				_ = e.db.SetPending(e.pending)
			}
			e.paused = true
			_ = e.db.SetQueuePaused(true, e.pending, 0)
			e.mu.Unlock()
			recovered++
		}
	}
	if recovered > 0 {
		e.mu.Lock()
		e.recovered = recovered
		e.mu.Unlock()
		slog.Warn("startup recovery complete", "interrupted_jobs", recovered, "queue", "paused")
	} else {
		slog.Info("startup recovery: no interrupted jobs")
	}
}

func (e *Engine) DiunEvent(ev diun.Event) {
	ctn := ""
	if ev.Metadata != nil {
		ctn = strings.TrimPrefix(strings.Split(ev.Metadata["ctn_names"], ",")[0], "/")
	}
	if e.cfg.ShouldSkip(ev.Image, ctn) {
		slog.Info("update skipped by policy", "image", ev.Image, "container", ctn)
		return
	}
	e.mu.Lock()
	for i, p := range e.pending {
		if p.Event.Image == ev.Image {
			e.pending[i].Event = ev
			_ = e.db.SetPending(e.pending)
			e.mu.Unlock()
			slog.Info("pending image refreshed", "image", ev.Image, "digest", shortDigest(ev.Digest))
			e.armCoalesce()
			return
		}
	}
	id, err := e.db.Add(ev.Image, ev.Digest, store.StatusPending)
	if err != nil {
		e.mu.Unlock()
		slog.Error("failed to persist pending update", "image", ev.Image, "err", err)
		return
	}
	item := store.PendingItem{RecordID: id, Event: ev, Selected: true}
	e.pending = append(e.pending, item)
	_ = e.db.SetPending(e.pending)
	pendingCount := len(e.pending)
	quiet := e.cfg.InQuietHours(time.Now())
	if quiet {
		e.quietBuffered = true
	}
	e.mu.Unlock()

	slog.Info("pending update recorded", "id", id, "image", ev.Image, "digest", shortDigest(ev.Digest), "pending_total", pendingCount, "quiet_hours", quiet)

	if quiet {
		slog.Info("quiet hours active; notification deferred", "quiet_hours", e.cfg.QuietHours)
		return
	}
	e.armCoalesce()
}

func (e *Engine) armCoalesce() {
	e.mu.Lock()
	if e.coalesceArmed || e.running {
		e.mu.Unlock()
		return
	}
	e.coalesceArmed = true
	e.mu.Unlock()
	go func() {
		time.Sleep(coalesceDelay)
		if e.cfg.InQuietHours(time.Now()) {
			e.mu.Lock()
			e.coalesceArmed = false
			e.quietBuffered = true
			e.mu.Unlock()
			return
		}
		e.mu.Lock()
		e.coalesceArmed = false
		running := e.running
		e.mu.Unlock()
		if running {
			return
		}
		e.notifyPending()
	}()
}

func (e *Engine) notifyPending() {
	targets := e.cfg.NotifyTargets()
	if len(targets) == 0 {
		slog.Warn("no telegram notify targets configured; update stored but not sent")
		return
	}
	e.mu.Lock()
	text := e.pendingTextLocked()
	buttons := e.pendingButtonsLocked()
	pendingCount := len(e.pending)
	e.mu.Unlock()

	ctx := context.Background()
	e.mu.Lock()
	prevChat, prevMsg := e.latestChat, e.latestMsg
	e.latestChat, e.latestMsg = 0, 0
	e.mu.Unlock()
	if prevChat != 0 && prevMsg != 0 {
		if err := e.bot.Delete(ctx, prevChat, prevMsg); err != nil {
			slog.Debug("delete previous notification", "chat", prevChat, "message", prevMsg, "err", err)
		}
		_ = e.db.ClearLatest()
	}
	var lastChat, lastMsg int64
	sent := 0
	for _, chat := range targets {
		id, err := e.bot.Send(ctx, chat, text, buttons)
		if err != nil {
			slog.Error("telegram notify failed", "chat", chat, "err", err)
			continue
		}
		slog.Info("telegram notify sent", "chat", chat, "message", id, "pending", pendingCount)
		lastChat, lastMsg = chat, id
		sent++
	}
	if lastChat == 0 {
		slog.Error("telegram notify failed for all targets", "targets", len(targets))
		return
	}
	e.mu.Lock()
	e.latestChat = lastChat
	e.latestMsg = lastMsg
	e.mu.Unlock()
	_ = e.db.SetLatest(lastChat, lastMsg)
	slog.Info("latest actionable notification set", "chat", lastChat, "message", lastMsg, "delivered", sent)
}

func (e *Engine) pendingTextLocked() string {
	return formatPending(e.pending, e.selectMode)
}

func (e *Engine) pendingButtonsLocked() [][]telegram.Button {
	rows := [][]telegram.Button{
		{
			{Text: "🚀 UPDATE ALL", CallbackData: "confirm_all"},
			{Text: "⏭️ SKIP ALL", CallbackData: "skip_all"},
		},
		{
			{Text: "☑️ SELECT", CallbackData: "select_on"},
		},
	}
	if e.selectMode {
		rows = [][]telegram.Button{}
		for i := range e.pending {
			label := fmt.Sprintf("Toggle %d", i+1)
			rows = append(rows, []telegram.Button{{Text: label, CallbackData: fmt.Sprintf("toggle_%d", i)}})
		}
		rows = append(rows, []telegram.Button{
			{Text: "🚀 UPDATE SELECTED", CallbackData: "confirm_selected"},
			{Text: "⏭ SKIP SELECTED", CallbackData: "skip_selected"},
		})
		rows = append(rows, []telegram.Button{
			{Text: "◀️ BACK", CallbackData: "select_off"},
		})
		return rows
	}
	return rows
}

func (e *Engine) HandleTelegram(ctx context.Context, api *telegram.API, chatID int64, messageID int64, fromID int64, text, callback, callbackID string) {
	e.Handle(ctx, api, chatID, messageID, fromID, text, callback, callbackID)
}

func (e *Engine) Handle(ctx context.Context, api *telegram.API, chatID int64, msgID int64, fromID int64, text, callback, callbackID string) {
	if text != "" {
		slog.Info("telegram command", "user", fromID, "chat", chatID, "text", text)
	}
	if callback != "" {
		slog.Info("telegram callback", "user", fromID, "chat", chatID, "message", msgID, "callback", callback)
	}
	if text == "/start" || text == "/help" {
		api.Answer(ctx, callbackID, "")
		_, _ = api.Send(ctx, chatID, msgHeader("👋 <b>Ready</b>")+
			"Diun detects updates.\nDockerUpBot pulls &amp; recreates — with your OK.\n\n"+
			"<b>Commands</b>\n"+
			"/updates — pending updates\n"+
			"/status — bot state\n"+
			"/history — recent jobs\n"+
			"/help — this message\n\n"+
			msgFooter(htmlEscape(version.Short())), nil)
		return
	}
	if text == "/status" {
		e.mu.Lock()
		n := len(e.pending)
		paused := e.paused
		running := e.running
		e.mu.Unlock()
		state := "idle"
		switch {
		case running:
			state = "updating"
		case paused:
			state = "paused (failure)"
		case n > 0:
			state = "waiting for your action"
		}
		msg := msgHeader("📊 <b>Status</b>") +
			fmt.Sprintf("Version <code>%s</code>\nCommit <code>%s</code>\n\nState <b>%s</b>\nPending <b>%d</b>\n\n",
				htmlEscape(version.Short()), htmlEscape(version.Commit), htmlEscape(state), n) +
			msgFooter("")
		_, _ = api.Send(ctx, chatID, msg, nil)
		return
	}
	if text == "/history" {
		hist := e.db.ListHistory(8)
		var b strings.Builder
		b.WriteString(msgHeader("📜 <b>Recent history</b>"))
		buttons := [][]telegram.Button{}
		if len(hist) == 0 {
			b.WriteString("No jobs yet.\n\n")
		}
		shown := 0
		for i := len(hist) - 1; i >= 0 && shown < 5; i-- {
			r := hist[i]
			icon := "🟡"
			switch r.Status {
			case store.StatusSuccess:
				icon = "✅"
			case store.StatusFailed:
				icon = "❌"
			case store.StatusSkipped:
				icon = "⏭"
			case store.StatusRolledBack:
				icon = "↩️"
			}
			fmt.Fprintf(&b, "%s <code>%s</code>\n   %s · %s\n\n", icon, htmlEscape(r.Image), htmlEscape(r.Status), r.DetectedAt.Format("02 Jan 15:04"))
			row := []telegram.Button{}
			if r.Status == store.StatusFailed || r.Status == store.StatusSuccess {
				if r.PreviousDigest != "" && r.ContainerName != "" {
					row = append(row, telegram.Button{Text: fmt.Sprintf("↩️ RB #%d", r.ID), CallbackData: fmt.Sprintf("rollback_%d", r.ID)})
				}
			}
			if r.Status == store.StatusFailed {
				row = append(row, telegram.Button{Text: fmt.Sprintf("🔁 RETRY #%d", r.ID), CallbackData: fmt.Sprintf("retry_%d", r.ID)})
				row = append(row, telegram.Button{Text: fmt.Sprintf("📋 LOG #%d", r.ID), CallbackData: fmt.Sprintf("hist_logs_%d", r.ID)})
			}
			if len(row) > 0 {
				buttons = append(buttons, row)
			}
			shown++
		}
		b.WriteString(msgFooter(""))
		id, err := api.Send(ctx, chatID, b.String(), buttons)
		if err == nil && len(buttons) > 0 {
			e.mu.Lock()
			e.latestChat, e.latestMsg = chatID, id
			e.mu.Unlock()
			_ = e.db.SetLatest(chatID, id)
		}
		return
	}
	if text == "/updates" || text == "/update" {
		e.mu.Lock()
		if len(e.pending) == 0 {
			e.mu.Unlock()
			_, _ = api.Send(ctx, chatID, msgHeader("✅ <b>No pending updates</b>")+
				"Nothing waiting right now.\nDiun will notify when an image digest changes.\n\n"+
				msgFooter(""), nil)
			return
		}
		textMsg := e.pendingTextLocked()
		buttons := e.pendingButtonsLocked()
		prevC, prevM := e.latestChat, e.latestMsg
		e.mu.Unlock()
		if prevC != 0 && prevM != 0 {
			_ = api.StripButtons(ctx, prevC, prevM)
		}
		id, err := api.Send(ctx, chatID, textMsg, buttons)
		if err == nil {
			e.mu.Lock()
			e.latestChat = chatID
			e.latestMsg = id
			e.mu.Unlock()
			_ = e.db.SetLatest(chatID, id)
		}
		return
	}

	if callback == "" {
		return
	}

	// Latest notification rule: reject obsolete callbacks (spec §9).
	e.mu.Lock()
	latestMsg := e.latestMsg
	e.mu.Unlock()
	if latestMsg != 0 && msgID != 0 && msgID != latestMsg {
		slog.Warn("obsolete notification callback rejected", "user", fromID, "message", msgID, "latest", latestMsg, "callback", callback)
		api.Answer(ctx, callbackID, "Obsolete notification — use the latest message")
		return
	}

	switch {
	case callback == "skip_all":
		api.Answer(ctx, callbackID, "Skipped")
		e.mu.Lock()
		n := len(e.pending)
		images := make([]string, 0, n)
		for _, p := range e.pending {
			images = append(images, p.Event.Image)
			_ = e.db.SetStatus(p.RecordID, store.StatusSkipped, "")
		}
		e.pending = nil
		e.selectMode = false
		_ = e.db.SetPending(nil)
		_ = e.db.SetSelectMode(false)
		e.mu.Unlock()
		slog.Info("updates skipped", "user", fromID, "count", n)
		_ = api.Edit(ctx, chatID, msgID, formatSkipped(images), [][]telegram.Button{})
		return

	case callback == "skip_selected":
		api.Answer(ctx, callbackID, "Skipped selected")
		e.mu.Lock()
		var images []string
		remain := make([]store.PendingItem, 0)
		for _, p := range e.pending {
			if p.Selected {
				images = append(images, p.Event.Image)
				_ = e.db.SetStatus(p.RecordID, store.StatusSkipped, "")
				continue
			}
			remain = append(remain, p)
		}
		e.pending = remain
		e.selectMode = false
		_ = e.db.SetPending(remain)
		_ = e.db.SetSelectMode(false)
		textMsg := e.pendingTextLocked()
		buttons := e.pendingButtonsLocked()
		e.mu.Unlock()
		if len(images) == 0 {
			_ = api.Edit(ctx, chatID, msgID, textMsg, buttons)
			return
		}
		if len(remain) == 0 {
			_ = api.Edit(ctx, chatID, msgID, formatSkipped(images), [][]telegram.Button{})
			return
		}
		_ = api.Edit(ctx, chatID, msgID, textMsg, buttons)
		return

	case callback == "select_on":
		api.Answer(ctx, callbackID, "Select mode")
		e.mu.Lock()
		e.selectMode = true
		for i := range e.pending {
			e.pending[i].Selected = false
		}
		_ = e.db.SetSelectMode(true)
		_ = e.db.SetPending(e.pending)
		textMsg := e.pendingTextLocked()
		buttons := e.pendingButtonsLocked()
		e.mu.Unlock()
		_ = api.Edit(ctx, chatID, msgID, textMsg, buttons)
		return

	case callback == "select_off":
		api.Answer(ctx, callbackID, "")
		e.mu.Lock()
		e.selectMode = false
		for i := range e.pending {
			e.pending[i].Selected = true
		}
		_ = e.db.SetSelectMode(false)
		_ = e.db.SetPending(e.pending)
		textMsg := e.pendingTextLocked()
		buttons := e.pendingButtonsLocked()
		e.mu.Unlock()
		_ = api.Edit(ctx, chatID, msgID, textMsg, buttons)
		return

	case strings.HasPrefix(callback, "toggle_"):
		api.Answer(ctx, callbackID, "")
		idx, _ := strconv.Atoi(strings.TrimPrefix(callback, "toggle_"))
		e.mu.Lock()
		if idx >= 0 && idx < len(e.pending) {
			e.pending[idx].Selected = !e.pending[idx].Selected
			_ = e.db.SetPending(e.pending)
		}
		textMsg := e.pendingTextLocked()
		buttons := e.pendingButtonsLocked()
		e.mu.Unlock()
		_ = api.Edit(ctx, chatID, msgID, textMsg, buttons)
		return

	case callback == "confirm_all" || callback == "confirm_selected" || callback == "confirm_next":
		e.showConfirmation(ctx, api, chatID, msgID, callbackID, callback)
		return

	case callback == "start_all" || callback == "start_selected" || callback == "start_next":
		e.startQueue(ctx, api, chatID, msgID, callbackID, callback)
		return

	case callback == "queue_pause":
		e.mu.Lock()
		if !e.running {
			e.mu.Unlock()
			api.Answer(ctx, callbackID, "Not running")
			return
		}
		e.pauseAfter = true
		e.mu.Unlock()
		api.Answer(ctx, callbackID, "Pausing after this image")
		return

	case callback == "queue_cancel":
		e.mu.Lock()
		if !e.running {
			e.mu.Unlock()
			api.Answer(ctx, callbackID, "Not running")
			return
		}
		e.cancelRun = true
		cancel := e.runCancel
		e.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		api.Answer(ctx, callbackID, "Cancelling")
		return

	case callback == "cancel_confirm":
		api.Answer(ctx, callbackID, "Cancelled")
		e.mu.Lock()
		textMsg := e.pendingTextLocked()
		buttons := e.pendingButtonsLocked()
		e.mu.Unlock()
		_ = api.Edit(ctx, chatID, msgID, textMsg, buttons)
		return

	case callback == "queue_continue":
		api.Answer(ctx, callbackID, "Continuing")
		e.mu.Lock()
		if e.running {
			e.mu.Unlock()
			slog.Warn("continue ignored: queue already running")
			return
		}
		paused, jobs, idx := e.db.GetQueuePaused()
		if !paused || len(jobs) == 0 {
			e.mu.Unlock()
			slog.Warn("continue ignored: nothing paused")
			return
		}
		if idx < 0 {
			idx = 0
		}
		if idx >= len(jobs) {
			e.paused = false
			_ = e.db.SetQueuePaused(false, nil, 0)
			e.mu.Unlock()
			slog.Info("paused queue already finished")
			return
		}
		remain := jobs[idx:]
		e.running = true
		e.paused = false
		_ = e.db.SetQueuePaused(false, nil, 0)
		e.mu.Unlock()
		slog.Info("queue continued", "remaining", len(remain), "from_index", idx+1)
		go e.runQueue(e.workCtx, api, chatID, msgID, remain, 0)
		return

	case callback == "queue_skip_continue":
		api.Answer(ctx, callbackID, "Skipped failed, continuing")
		e.mu.Lock()
		if e.running {
			e.mu.Unlock()
			return
		}
		paused, jobs, idx := e.db.GetQueuePaused()
		if !paused || len(jobs) == 0 || idx < 0 || idx >= len(jobs) {
			e.mu.Unlock()
			return
		}
		failed := jobs[idx]
		_ = e.db.SetStatus(failed.RecordID, store.StatusSkipped, "skipped after failure")
		next := idx + 1
		if next >= len(jobs) {
			e.paused = false
			e.pending = nil
			_ = e.db.SetQueuePaused(false, nil, 0)
			_ = e.db.SetPending(nil)
			e.mu.Unlock()
			_ = api.Edit(ctx, chatID, msgID, formatSkipped([]string{failed.Event.Image}), [][]telegram.Button{})
			return
		}
		remain := jobs[next:]
		e.running = true
		e.paused = false
		_ = e.db.SetQueuePaused(false, nil, 0)
		e.mu.Unlock()
		slog.Info("skipped failed job; continuing", "skipped", failed.Event.Image, "remaining", len(remain))
		go e.runQueue(e.workCtx, api, chatID, msgID, remain, 0)
		return

	case callback == "queue_stop":
		api.Answer(ctx, callbackID, "Stopped")
		e.mu.Lock()
		paused, jobs, idx := e.db.GetQueuePaused()
		var done, remaining []queueResult
		var keep []store.PendingItem
		if paused && len(jobs) > 0 {
			if idx < 0 {
				idx = 0
			}
			if idx > len(jobs) {
				idx = len(jobs)
			}
			for i, j := range jobs {
				r := queueResult{Image: j.Event.Image}
				if i < idx {
					done = append(done, r)
					continue
				}
				remaining = append(remaining, r)
				j.Selected = true
				keep = append(keep, j)
				_ = e.db.SetStatus(j.RecordID, store.StatusPending, "stopped; still pending")
			}
		} else {
			keep = append(keep, e.pending...)
			for _, p := range keep {
				remaining = append(remaining, queueResult{Image: p.Event.Image})
			}
		}
		e.paused = false
		e.selectMode = false
		e.pending = keep
		_ = e.db.SetQueuePaused(false, nil, 0)
		_ = e.db.SetPending(keep)
		_ = e.db.SetSelectMode(false)
		textMsg := formatQueueStopped(done, remaining)
		var buttons [][]telegram.Button
		if len(keep) > 0 {
			textMsg += "\nThese stay pending. Use the buttons below, or /updates later.\n"
			buttons = e.pendingButtonsLocked()
		}
		e.mu.Unlock()
		slog.Info("queue stopped by user", "user", fromID, "done", len(done), "still_pending", len(keep))
		_ = api.Edit(ctx, chatID, msgID, textMsg, buttons)
		if len(keep) > 0 {
			e.mu.Lock()
			e.latestChat, e.latestMsg = chatID, msgID
			e.mu.Unlock()
			_ = e.db.SetLatest(chatID, msgID)
		}
		return

	case callback == "show_logs":
		api.Answer(ctx, callbackID, "")
		e.mu.Lock()
		id := e.lastFailID
		e.mu.Unlock()
		logText := "(no logs)"
		if id != 0 {
			if r, ok := e.db.Get(id); ok {
				if r.LastLog != "" {
					logText = r.LastLog
				} else if r.Error != "" {
					logText = r.Error
				}
				if r.ContainerName != "" && e.docker != nil {
					if l, err := e.docker.ContainerLogsTail(ctx, r.ContainerName, 30); err == nil && l != "" {
						logText = l
					}
				}
			}
		}
		if len(logText) > 3500 {
			logText = logText[len(logText)-3500:]
		}
		_, _ = api.Send(ctx, chatID, "📋 <b>Logs</b>\n\n<pre>"+htmlEscape(logText)+"</pre>", nil)
		return

	case callback == "keep_update":
		api.Answer(ctx, callbackID, "Kept")
		img := ""
		e.mu.Lock()
		if e.lastSuccessID != 0 {
			if r, ok := e.db.Get(e.lastSuccessID); ok {
				img = r.Image
			}
		}
		e.mu.Unlock()
		_ = api.Edit(ctx, chatID, msgID, formatKept(img), [][]telegram.Button{})
		return

	case strings.HasPrefix(callback, "rollback_"):
		api.Answer(ctx, callbackID, "Rolling back")
		id, _ := strconv.ParseInt(strings.TrimPrefix(callback, "rollback_"), 10, 64)
		e.rollbackByID(ctx, api, chatID, msgID, id)
		return

	case strings.HasPrefix(callback, "retry_"):
		api.Answer(ctx, callbackID, "Retry queued")
		id, _ := strconv.ParseInt(strings.TrimPrefix(callback, "retry_"), 10, 64)
		rec, ok := e.db.Get(id)
		if !ok {
			return
		}
		ev := diun.Event{
			Status: "update",
			Image:  firstNonEmpty(rec.NewImage, rec.Image),
			Digest: firstNonEmpty(rec.NewDigest, rec.Digest+"-retry-"+strconv.FormatInt(time.Now().Unix(), 10)),
			Metadata: map[string]string{
				"ctn_names": rec.ContainerName,
			},
		}
		e.DiunEvent(ev)
		return

	case strings.HasPrefix(callback, "hist_logs_"):
		api.Answer(ctx, callbackID, "")
		id, _ := strconv.ParseInt(strings.TrimPrefix(callback, "hist_logs_"), 10, 64)
		logText := "(no logs)"
		if r, ok := e.db.Get(id); ok {
			if r.LastLog != "" {
				logText = r.LastLog
			} else if r.Error != "" {
				logText = r.Error
			}
		}
		if len(logText) > 3500 {
			logText = logText[len(logText)-3500:]
		}
		_, _ = api.Send(ctx, chatID, "📋 <b>Logs</b>\n\n<pre>"+htmlEscape(logText)+"</pre>", nil)
		return
	}
}

func (e *Engine) jobsForCallback(kind string) []store.PendingItem {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch kind {
	case "confirm_next", "start_next":
		if len(e.pending) == 0 {
			return nil
		}
		return []store.PendingItem{e.pending[0]}
	case "confirm_selected", "start_selected":
		var out []store.PendingItem
		for _, p := range e.pending {
			if p.Selected {
				out = append(out, p)
			}
		}
		return out
	default:
		return append([]store.PendingItem(nil), e.pending...)
	}
}

func (e *Engine) showConfirmation(ctx context.Context, api *telegram.API, chatID, msgID int64, callbackID, kind string) {
	jobs := e.jobsForCallback(kind)
	if len(jobs) == 0 {
		api.Answer(ctx, callbackID, "Nothing to update")
		slog.Info("confirmation skipped: nothing to update", "kind", kind, "user_chat", chatID)
		return
	}
	api.Answer(ctx, callbackID, "")
	slog.Info("showing confirmation screen", "kind", kind, "jobs", len(jobs), "chat", chatID)
	startCB := "start_all"
	switch kind {
	case "confirm_selected":
		startCB = "start_selected"
	case "confirm_next":
		startCB = "start_next"
	}
	buttons := [][]telegram.Button{
		{{Text: "🚀 START UPDATE", CallbackData: startCB}},
		{{Text: "❌ CANCEL", CallbackData: "cancel_confirm"}},
	}
	_ = api.Edit(ctx, chatID, msgID, formatConfirmation(jobs), buttons)
	e.mu.Lock()
	e.latestChat = chatID
	e.latestMsg = msgID
	e.mu.Unlock()
	_ = e.db.SetLatest(chatID, msgID)
}

func (e *Engine) startQueue(ctx context.Context, api *telegram.API, chatID, msgID int64, callbackID, kind string) {
	jobs := e.jobsForCallback(kind)
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		api.Answer(ctx, callbackID, "An update is already running")
		slog.Warn("queue start rejected: already running", "kind", kind)
		return
	}
	if len(jobs) == 0 {
		e.mu.Unlock()
		api.Answer(ctx, callbackID, "Nothing to update")
		slog.Warn("queue start rejected: empty", "kind", kind)
		return
	}
	// Remove selected jobs from pending; keep unselected.
	jobSet := map[int64]bool{}
	for _, j := range jobs {
		jobSet[j.RecordID] = true
		_ = e.db.SetStatus(j.RecordID, store.StatusQueued, "")
	}
	remain := make([]store.PendingItem, 0)
	for _, p := range e.pending {
		if !jobSet[p.RecordID] {
			remain = append(remain, p)
		}
	}
	e.pending = remain
	_ = e.db.SetPending(remain)
	e.running = true
	e.paused = false
	e.selectMode = false
	_ = e.db.SetSelectMode(false)
	_ = e.db.SetQueuePaused(false, nil, 0)
	e.mu.Unlock()
	api.Answer(ctx, callbackID, "Update queue started")
	slog.Info("update queue started", "kind", kind, "jobs", len(jobs), "chat", chatID)
	for i, j := range jobs {
		slog.Info("queue job", "index", i+1, "of", len(jobs), "id", j.RecordID, "image", j.Event.Image)
	}
	go e.runQueue(e.workCtx, api, chatID, msgID, jobs, 0)
}

func (e *Engine) processingButtons() [][]telegram.Button {
	return [][]telegram.Button{{
		{Text: "⏸ PAUSE", CallbackData: "queue_pause"},
		{Text: "❌ CANCEL", CallbackData: "queue_cancel"},
	}}
}

func (e *Engine) userCancelled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cancelRun
}

// holdPending puts unfinished jobs back on the idle pending list.
func (e *Engine) holdPending(jobs []store.PendingItem) {
	keep := make([]store.PendingItem, 0, len(jobs))
	seen := map[int64]bool{}
	for _, j := range jobs {
		j.Selected = true
		keep = append(keep, j)
		seen[j.RecordID] = true
		_ = e.db.SetStatus(j.RecordID, store.StatusPending, "still pending")
	}
	e.mu.Lock()
	for _, p := range e.pending {
		if !seen[p.RecordID] {
			keep = append(keep, p)
		}
	}
	e.paused = false
	e.selectMode = false
	e.pauseAfter = false
	e.pending = keep
	e.mu.Unlock()
	_ = e.db.SetQueuePaused(false, nil, 0)
	_ = e.db.SetPending(keep)
	_ = e.db.SetSelectMode(false)
}

func (e *Engine) runQueue(ctx context.Context, api *telegram.API, chat, msg int64, jobs []store.PendingItem, startIdx int) {
	runCtx, runCancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.runCancel = runCancel
	e.pauseAfter = false
	e.cancelRun = false
	e.mu.Unlock()
	defer func() {
		runCancel()
		e.mu.Lock()
		if e.runCancel != nil {
			e.runCancel = nil
		}
		e.pauseAfter = false
		e.cancelRun = false
		e.running = false
		e.mu.Unlock()
		slog.Info("update queue idle")
	}()
	queueStart := time.Now()
	done := make([]queueResult, 0, len(jobs))
	// Preserve successes from earlier steps when continuing a paused queue.
	for i := 0; i < startIdx && i < len(jobs); i++ {
		done = append(done, queueResult{Image: jobs[i].Event.Image})
	}
	slog.Info("processing queue", "jobs", len(jobs), "start_index", startIdx+1)
	for i := startIdx; i < len(jobs); i++ {
		e.mu.Lock()
		pause := e.pauseAfter
		e.mu.Unlock()
		if pause {
			e.holdPending(jobs[i:])
			e.showHeld(ctx, api, chat, msg, "⏸ <b>Queue paused</b>", "Paused after the current image. These are still pending.")
			return
		}
		j := jobs[i]
		slog.Info("queue step begin", "step", i+1, "of", len(jobs), "image", j.Event.Image, "id", j.RecordID)
		res, err := e.runOne(ctx, runCtx, api, chat, msg, i+1, len(jobs), j)
		if err != nil && errors.Is(err, errUserCancel) {
			e.holdPending(jobs[i:])
			e.showHeld(ctx, api, chat, msg, "❌ <b>Update cancelled</b>", "Stopped in progress. These are still pending.")
			return
		}
		if err != nil && errors.Is(err, errSkipJob) {
			slog.Info("queue step skipped", "image", j.Event.Image, "err", err)
			_ = e.db.SetStatus(j.RecordID, store.StatusSkipped, err.Error())
			e.mu.Lock()
			filtered := e.pending[:0]
			for _, p := range e.pending {
				if p.RecordID != j.RecordID {
					filtered = append(filtered, p)
				}
			}
			e.pending = filtered
			_ = e.db.SetPending(e.pending)
			e.mu.Unlock()
			continue
		}
		if err != nil {
			res.Err = err
			e.lastFailID = j.RecordID
			remain := jobs[i:] // include failed + waiting
			e.mu.Lock()
			e.paused = true
			merged := append([]store.PendingItem(nil), remain...)
			for _, p := range e.pending {
				dup := false
				for _, r := range merged {
					if r.RecordID == p.RecordID {
						dup = true
						break
					}
				}
				if !dup {
					merged = append(merged, p)
				}
			}
			e.pending = merged
			_ = e.db.SetPending(e.pending)
			_ = e.db.SetQueuePaused(true, jobs, i)
			e.mu.Unlock()
			waiting := make([]queueResult, 0, len(jobs)-i-1)
			for k := i + 1; k < len(jobs); k++ {
				waiting = append(waiting, queueResult{Image: jobs[k].Event.Image})
			}
			slog.Error("update failed; queue paused",
				"image", j.Event.Image, "id", j.RecordID, "err", err,
				"completed", len(done), "waiting", len(waiting))
			buttons := [][]telegram.Button{
				{
					{Text: "📋 LOGS", CallbackData: "show_logs"},
					{Text: "🔁 RETRY", CallbackData: "queue_continue"},
				},
				{
					{Text: "⏭ SKIP & CONTINUE", CallbackData: "queue_skip_continue"},
					{Text: "⏹️ STOP", CallbackData: "queue_stop"},
				},
			}
			_ = api.Edit(ctx, chat, msg, formatQueueFailed(res, done, waiting), buttons)
			e.mu.Lock()
			e.latestChat, e.latestMsg = chat, msg
			e.mu.Unlock()
			_ = e.db.SetLatest(chat, msg)
			return
		}
		done = append(done, res)
		e.mu.Lock()
		e.lastSuccessID = res.RecordID
		e.mu.Unlock()
		slog.Info("queue step success", "step", i+1, "of", len(jobs), "image", j.Event.Image)
		e.mu.Lock()
		filtered := e.pending[:0]
		for _, p := range e.pending {
			if p.RecordID != j.RecordID {
				filtered = append(filtered, p)
			}
		}
		e.pending = filtered
		_ = e.db.SetPending(e.pending)
		e.mu.Unlock()
		e.mu.Lock()
		pause = e.pauseAfter
		e.mu.Unlock()
		if pause && i+1 < len(jobs) {
			e.holdPending(jobs[i+1:])
			e.showHeld(ctx, api, chat, msg, "⏸ <b>Queue paused</b>", "Paused after the current image. These are still pending.")
			return
		}
	}
	slog.Info("update queue complete", "jobs", len(jobs), "succeeded", len(done))
	buttons := [][]telegram.Button{}
	if e.lastSuccessID != 0 {
		if r, ok := e.db.Get(e.lastSuccessID); ok && r.PreviousDigest != "" && r.ContainerName != "" {
			buttons = [][]telegram.Button{{
				{Text: "↩️ ROLLBACK", CallbackData: fmt.Sprintf("rollback_%d", e.lastSuccessID)},
				{Text: "✅ KEEP", CallbackData: "keep_update"},
			}}
		}
	}
	_ = api.Edit(ctx, chat, msg, formatQueueComplete(done, time.Since(queueStart)), buttons)
	e.mu.Lock()
	e.latestChat, e.latestMsg = chat, msg
	e.mu.Unlock()
	_ = e.db.SetLatest(chat, msg)

	if e.cfg.PruneAfterSuccess {
		if n, err := e.docker.PruneDanglingImages(ctx); err != nil {
			slog.Warn("prune after success failed", "err", err)
		} else {
			slog.Info("prune after success", "reclaimed_bytes", n)
		}
	}
}

func (e *Engine) showHeld(ctx context.Context, api *telegram.API, chat, msg int64, title, note string) {
	e.mu.Lock()
	var b strings.Builder
	b.WriteString(msgHeader(title))
	b.WriteString(note)
	b.WriteString("\n\n")
	for i, p := range e.pending {
		fmt.Fprintf(&b, "%d. <code>%s</code>\n", i+1, htmlEscape(p.Event.Image))
	}
	b.WriteString("\n")
	b.WriteString(msgFooter("Use UPDATE ALL or SKIP ALL"))
	buttons := [][]telegram.Button{}
	if len(e.pending) > 0 {
		buttons = e.pendingButtonsLocked()
	}
	e.latestChat, e.latestMsg = chat, msg
	e.mu.Unlock()
	_ = e.db.SetLatest(chat, msg)
	_ = api.Edit(ctx, chat, msg, b.String(), buttons)
}

func (e *Engine) runOne(ctx, opCtx context.Context, api *telegram.API, chat, msg int64, n, total int, item store.PendingItem) (queueResult, error) {
	ev := item.Event
	image := ev.Image
	start := time.Now()
	out := queueResult{RecordID: item.RecordID, Image: image}

	target, prevImage, prevDigest, err := e.resolveComposeTarget(opCtx, ev)
	if e.userCancelled() {
		out.Duration = time.Since(start)
		return out, errUserCancel
	}
	if errors.Is(err, errSkipJob) {
		slog.Info("skipping image with no container", "image", image, "err", err)
		out.Duration = time.Since(start)
		return out, err
	}
	if err != nil {
		_ = e.db.SetStatus(item.RecordID, store.StatusFailed, err.Error())
		_ = e.db.AppendLog(item.RecordID, err.Error())
		slog.Error("compose target resolve failed", "image", image, "err", err)
		out.Duration = time.Since(start)
		return out, err
	}
	out.Service = target.Service
	out.Container = target.Container
	slog.Info("compose target resolved",
		"image", image,
		"project", target.Project,
		"service", target.Service,
		"dir", target.ProjectDir,
		"container", target.Container,
		"config_files", len(target.ConfigFiles),
		"previous_image", prevImage,
	)
	_ = e.db.SetRollbackMeta(item.RecordID, target.Project, target.Service, prevImage, prevDigest, image, ev.Digest, target.Container)

	_ = e.db.SetStatus(item.RecordID, store.StatusPulling, "")
	slog.Info("pull starting", "step", n, "of", total, "image", image, "id", item.RecordID)
	_ = api.Edit(ctx, chat, msg, formatStepPull(n, total, image), e.processingButtons())

	last := time.Time{}
	lastPct := -1
	err = e.docker.PullImage(opCtx, image, func(s dockerapi.PullState) {
		if time.Since(last) < e.cfgRefresh() {
			return
		}
		last = time.Now()
		pct := 0
		if s.Total > 0 {
			pct = int(s.Current * 100 / s.Total)
		}
		if pct != lastPct && (pct%10 == 0 || pct == 100) {
			slog.Info("pull progress", "image", image, "percent", pct, "done_layers", s.Done, "active", s.Active)
			lastPct = pct
		}
		_ = api.Edit(ctx, chat, msg, formatPull(n, total, image, s, time.Since(start)), e.processingButtons())
	})
	if err != nil {
		if e.userCancelled() {
			out.Duration = time.Since(start)
			return out, errUserCancel
		}
		_ = e.db.SetStatus(item.RecordID, store.StatusFailed, err.Error())
		_ = e.db.AppendLog(item.RecordID, err.Error())
		slog.Error("pull failed", "image", image, "err", err)
		out.Duration = time.Since(start)
		return out, err
	}
	_ = e.db.SetStatus(item.RecordID, store.StatusPulled, "")
	slog.Info("pull complete", "image", image, "duration", time.Since(start).Round(time.Second).String())
	_ = api.Edit(ctx, chat, msg, formatStepPullDone(n, total, image, time.Since(start)), e.processingButtons())

	_ = e.db.SetStatus(item.RecordID, store.StatusUpdating, "")
	_ = api.Edit(ctx, chat, msg, formatStepRecreate(n, total, target.Service, target.ProjectDir), e.processingButtons())
	slog.Info("compose up starting", "service", target.Service, "dir", target.ProjectDir)
	composeOut, err := compose.Up(opCtx, target)
	if composeOut != "" {
		_ = e.db.AppendLog(item.RecordID, composeOut)
		slog.Debug("compose output", "service", target.Service, "output", strings.TrimSpace(composeOut))
	}
	if err != nil {
		if e.userCancelled() {
			out.Duration = time.Since(start)
			return out, errUserCancel
		}
		_ = e.db.SetStatus(item.RecordID, store.StatusFailed, err.Error()+": "+composeOut)
		slog.Error("compose up failed", "service", target.Service, "err", err, "output", strings.TrimSpace(composeOut))
		out.Duration = time.Since(start)
		return out, fmt.Errorf("compose update failed: %w", err)
	}
	slog.Info("compose up complete", "service", target.Service)
	if name, ferr := e.docker.FindComposeContainer(opCtx, target.Project, target.Service); ferr == nil && name != "" {
		target.Container = name
		out.Container = name
	}

	_ = e.db.SetStatus(item.RecordID, store.StatusVerifying, "")
	_ = api.Edit(ctx, chat, msg, formatStepHealth(n, total, target.Service), e.processingButtons())
	slog.Info("health verification starting", "container", target.Container, "timeout_sec", e.cfg.Update.HealthTimeoutSeconds)
	if err := e.verifyHealth(opCtx, target.Container); err != nil {
		if e.userCancelled() {
			out.Duration = time.Since(start)
			return out, errUserCancel
		}
		_ = e.db.SetStatus(item.RecordID, store.StatusFailed, err.Error())
		_ = e.db.AppendLog(item.RecordID, err.Error())
		slog.Error("health verification failed", "container", target.Container, "err", err)
		out.Duration = time.Since(start)
		return out, err
	}
	slog.Info("health verification ok", "container", target.Container)

	_ = e.db.SetStatus(item.RecordID, store.StatusSuccess, "")
	out.Duration = time.Since(start)
	slog.Info("update success",
		"step", n, "of", total,
		"service", target.Service,
		"image", image,
		"duration", out.Duration.Round(time.Second).String(),
	)
	_ = api.Edit(ctx, chat, msg, formatStepSuccess(n, total, out), e.processingButtons())
	return out, nil
}

func (e *Engine) resolveComposeTarget(ctx context.Context, ev diun.Event) (compose.Target, string, string, error) {
	name := ""
	if ev.Metadata != nil {
		name = ev.Metadata["ctn_names"]
	}
	name = strings.TrimPrefix(strings.Split(name, ",")[0], "/")
	if name == "" {
		found, ferr := e.docker.FindRunningByImage(ctx, ev.Image)
		if ferr != nil || found == "" {
			return compose.Target{}, "", "", fmt.Errorf("%w: no container name for %s", errSkipJob, ev.Image)
		}
		slog.Info("resolved container by image", "image", ev.Image, "container", found)
		name = found
	}
	info, err := e.docker.InspectContainer(ctx, name)
	if err != nil {
		return compose.Target{}, "", "", fmt.Errorf("cannot update %s: %w", ev.Image, err)
	}
	labels := info.Config.Labels
	if labels == nil {
		return compose.Target{}, "", "", fmt.Errorf("container missing labels")
	}
	service := labels["com.docker.compose.service"]
	dir := labels["com.docker.compose.project.working_dir"]
	project := labels["com.docker.compose.project"]
	files := compose.ParseConfigFiles(labels["com.docker.compose.project.config_files"])
	if service == "" || dir == "" {
		return compose.Target{}, "", "", fmt.Errorf("%s is not a Compose-managed container", name)
	}
	dir = filepath.Clean(dir)
	if err := compose.ValidateRoot(dir, e.cfg.Update.ComposeRoot); err != nil {
		return compose.Target{}, "", "", err
	}
	if err := compose.ValidateFiles(files, dir, e.cfg.Update.ComposeRoot); err != nil {
		return compose.Target{}, "", "", err
	}
	prevImage := info.Config.Image
	prevDigest := info.Image
	return compose.Target{
		ProjectDir:  dir,
		Service:     service,
		ConfigFiles: files,
		Project:     project,
		Container:   name,
	}, prevImage, prevDigest, nil
}

func (e *Engine) verifyHealth(ctx context.Context, containerName string) error {
	timeout := time.Duration(e.cfg.Update.HealthTimeoutSeconds) * time.Second
	if timeout < time.Second {
		timeout = 120 * time.Second
	}
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		info, err := e.docker.InspectContainer(ctx, containerName)
		if err != nil {
			last = err.Error()
			slog.Debug("health poll retry", "container", containerName, "err", err)
			time.Sleep(2 * time.Second)
			continue
		}
		hasHC := info.State.Health != nil && info.State.Health.Status != ""
		status := info.State.Status
		if hasHC {
			status = info.State.Health.Status
		} else if info.State.Running {
			status = "running"
		}
		last = status
		slog.Debug("health poll", "container", containerName, "status", status, "has_healthcheck", hasHC)
		switch {
		case status == "healthy":
			return nil
		case status == "running" && !hasHC:
			return nil
		case status == "running" && hasHC && !e.cfg.RequireHealthy:
			return nil
		}
		// starting, unhealthy, or waiting for healthy: keep polling until timeout
		time.Sleep(2 * time.Second)
	}
	if last == "" {
		last = "unknown"
	}
	return fmt.Errorf("health verification timed out after %s (last status %s)", timeout, last)
}

func (e *Engine) cfgRefresh() time.Duration {
	n := e.cfg.Update.RefreshSeconds
	if n < 1 {
		n = 2
	}
	return time.Duration(n) * time.Second
}
