package dockerupdater

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	latest        map[int64]int64
	confirmations map[int64]confirmation
	activeChat    int64
	activeMsg     int64
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

type confirmation struct {
	token string
	jobs  []store.PendingItem
}

const coalesceDelay = 8 * time.Second

// errSkipJob means this image cannot be updated and the queue should continue.
var errSkipJob = errors.New("skip job")

// errUserCancel means the user pressed CANCEL during a running update.
var errUserCancel = errors.New("update cancelled")

func New(c config.Config, b *telegram.API, d store.Store, docker *dockerapi.Client, workCtx context.Context) *Engine {
	e := &Engine{cfg: c, bot: b, db: d, docker: docker, workCtx: workCtx}
	e.pending = d.GetPending()
	e.latest = d.LatestMessages()
	e.confirmations = make(map[int64]confirmation)
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
	e.mu.Lock()
	defer e.mu.Unlock()
	paused, jobs, index := e.db.GetQueuePaused()
	var recoveredIDs []int64
	if !paused || index < 0 || index >= len(jobs) {
		// Older state files recorded the running batch only in history.
		jobs, index = nil, 0
		for _, r := range e.db.ListHistory(0) {
			switch r.Status {
			case store.StatusPulling, store.StatusPulled, store.StatusUpdating, store.StatusVerifying, store.StatusQueued, store.StatusApproved:
				jobs = append(jobs, store.PendingItem{
					RecordID: r.ID,
					Selected: true,
					Event: diun.Event{
						Image: r.Image, Digest: r.Digest, Status: "update",
						Metadata: map[string]string{"ctn_names": r.ContainerName},
					},
				})
				recoveredIDs = append(recoveredIDs, r.ID)
			}
		}
	}
	if len(jobs) == 0 {
		return
	}
	pending := mergePending(e.pending, jobs[index:])
	if err := e.db.SaveQueue(pending, jobs, index, e.selectMode, recoveredIDs, store.StatusPaused); err != nil {
		slog.Error("save recovered queue", "err", err)
	}
	e.pending, e.paused = pending, true
	e.recovered = len(jobs) - index
	e.lastFailID = jobs[index].RecordID
	slog.Warn("startup recovery paused interrupted work", "jobs", e.recovered)
}

func (e *Engine) DiunEvent(ev diun.Event) error {
	if ev.DigestRef() == "" {
		return fmt.Errorf("missing immutable image reference")
	}
	name := containerName(ev)
	if e.cfg.ShouldSkip(ev.Image, name) {
		return nil
	}
	e.mu.Lock()
	var replaceID int64
	for _, p := range e.pending {
		if dockerapi.SameImageRef(p.Event.Image, ev.Image) && containerName(p.Event) == name {
			if p.Event.Digest == ev.Digest {
				e.mu.Unlock()
				return nil
			}
			replaceID = p.RecordID
			break
		}
	}
	pending, err := e.db.SaveEvent(ev, replaceID)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	e.pending = pending
	quiet := e.cfg.InQuietHours(time.Now())
	if quiet {
		e.quietBuffered = true
	}
	e.mu.Unlock()
	if !quiet {
		e.armCoalesce()
	}
	return nil
}

func containerName(ev diun.Event) string {
	name, _, _ := strings.Cut(ev.Metadata["ctn_names"], ",")
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}

func (e *Engine) setLatestLocked(chat, msg int64) {
	if e.latest[chat] != msg {
		delete(e.confirmations, chat)
		e.latest[chat] = msg
		if err := e.db.SetLatest(chat, msg); err != nil {
			slog.Error("save latest Telegram message", "err", err)
		}
	}
}

func (e *Engine) armCoalesce() {
	e.mu.Lock()
	if e.coalesceArmed || e.running || e.paused {
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
	e.mu.Lock()
	if e.running || e.paused || len(e.pending) == 0 {
		e.mu.Unlock()
		return
	}
	text := e.pendingTextLocked()
	buttons := e.pendingButtonsLocked()
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(e.workCtx, 30*time.Second)
	defer cancel()
	for _, chat := range e.cfg.NotifyTargets() {
		e.mu.Lock()
		previous := e.latest[chat]
		confirming := e.confirmations[chat].token != ""
		e.mu.Unlock()
		if confirming {
			continue
		}
		id, err := e.bot.Send(ctx, chat, text, buttons)
		if err != nil {
			slog.Error("telegram notify failed", "chat", chat, "err", err)
			continue
		}
		e.mu.Lock()
		if e.running || e.paused || e.latest[chat] != previous || e.confirmations[chat].token != "" {
			e.mu.Unlock()
			_ = e.bot.StripButtons(ctx, chat, id)
			continue
		}
		e.setLatestLocked(chat, id)
		e.mu.Unlock()
		if previous != 0 {
			_ = e.bot.StripButtons(ctx, chat, previous)
		}
	}
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
			busy := e.running
			if !busy {
				e.setLatestLocked(chatID, id)
			}
			e.mu.Unlock()
			if busy {
				_ = api.StripButtons(ctx, chatID, id)
			}
		}
		return
	}
	if text == "/updates" || text == "/update" {
		e.mu.Lock()
		if e.running {
			e.mu.Unlock()
			_, _ = api.Send(ctx, chatID, "An update is running. Use /status or the active update card.", nil)
			return
		}
		if len(e.pending) == 0 && !e.paused {
			e.mu.Unlock()
			_, _ = api.Send(ctx, chatID, msgHeader("✅ <b>No pending updates</b>")+
				"Nothing waiting right now.\nDiun will notify when an image digest changes.\n\n"+
				msgFooter(""), nil)
			return
		}
		textMsg := e.pendingTextLocked()
		buttons := e.pendingButtonsLocked()
		if paused, jobs, index := e.db.GetQueuePaused(); e.paused && paused && index >= 0 && index < len(jobs) {
			textMsg = formatRecovery(len(jobs) - index)
			buttons = pausedButtons()
		}
		prevM := e.latest[chatID]
		e.mu.Unlock()
		if prevM != 0 {
			_ = api.StripButtons(ctx, chatID, prevM)
		}
		id, err := api.Send(ctx, chatID, textMsg, buttons)
		if err == nil {
			e.mu.Lock()
			e.setLatestLocked(chatID, id)
			e.mu.Unlock()
		}
		return
	}

	if callback == "" {
		return
	}

	// Message IDs are scoped to their chat.
	e.mu.Lock()
	latestMsg := e.latest[chatID]
	busy := e.running
	paused := e.paused
	e.mu.Unlock()
	if chatID == 0 || msgID == 0 || msgID != latestMsg {
		slog.Warn("obsolete notification callback rejected", "user", fromID, "message", msgID, "latest", latestMsg, "callback", callback)
		api.Answer(ctx, callbackID, "Obsolete notification — use the latest message")
		return
	}

	if busy && callback != "queue_pause" && callback != "queue_cancel" {
		api.Answer(ctx, callbackID, "An update is already running")
		return
	}
	if paused && (callback == "skip_all" || callback == "skip_selected" || callback == "select_on" || callback == "select_off" || strings.HasPrefix(callback, "toggle_") || strings.HasPrefix(callback, "confirm_")) {
		api.Answer(ctx, callbackID, "Resolve the paused queue first using /updates")
		return
	}

	switch {
	case callback == "skip_all" || callback == "skip_selected":
		e.mu.Lock()
		var images []string
		var ids []int64
		for _, item := range e.pending {
			if callback == "skip_all" || item.Selected {
				images = append(images, item.Event.Image)
				ids = append(ids, item.RecordID)
			}
		}
		remain := withoutJobs(e.pending, ids)
		if e.running || e.paused || e.latest[chatID] != msgID {
			e.mu.Unlock()
			api.Answer(ctx, callbackID, "Queue changed; open /updates")
			return
		}
		if err := e.db.SaveQueue(remain, nil, 0, false, ids, store.StatusSkipped); err != nil {
			e.mu.Unlock()
			slog.Error("save skipped jobs", "err", err)
			api.Answer(ctx, callbackID, "Could not save skipped jobs")
			return
		}
		clear(e.confirmations)
		e.pending, e.selectMode = remain, false
		text, buttons := e.pendingTextLocked(), e.pendingButtonsLocked()
		e.mu.Unlock()
		api.Answer(ctx, callbackID, "Skipped")
		if len(remain) == 0 {
			text, buttons = formatSkipped(images), [][]telegram.Button{}
		}
		_ = api.Edit(ctx, chatID, msgID, text, buttons)
		return

	case callback == "select_on" || callback == "select_off" || strings.HasPrefix(callback, "toggle_"):
		e.mu.Lock()
		pending := append([]store.PendingItem(nil), e.pending...)
		selectMode := e.selectMode
		if strings.HasPrefix(callback, "toggle_") {
			index, err := strconv.Atoi(strings.TrimPrefix(callback, "toggle_"))
			if err != nil || !selectMode || index < 0 || index >= len(pending) {
				e.mu.Unlock()
				api.Answer(ctx, callbackID, "Selection changed; open /updates")
				return
			}
			pending[index].Selected = !pending[index].Selected
		} else {
			selectMode = callback == "select_on"
			for i := range pending {
				pending[i].Selected = !selectMode
			}
		}
		if e.running || e.paused || e.latest[chatID] != msgID {
			e.mu.Unlock()
			api.Answer(ctx, callbackID, "Queue changed; open /updates")
			return
		}
		if err := e.db.SaveQueue(pending, nil, 0, selectMode, nil, ""); err != nil {
			e.mu.Unlock()
			slog.Error("save selection", "err", err)
			api.Answer(ctx, callbackID, "Could not save selection")
			return
		}
		delete(e.confirmations, chatID)
		e.pending, e.selectMode = pending, selectMode
		textMsg, buttons := e.pendingTextLocked(), e.pendingButtonsLocked()
		e.mu.Unlock()
		api.Answer(ctx, callbackID, "")
		_ = api.Edit(ctx, chatID, msgID, textMsg, buttons)
		return

	case callback == "confirm_all" || callback == "confirm_selected" || callback == "confirm_next":
		e.showConfirmation(ctx, api, chatID, msgID, callbackID, callback)
		return

	case strings.HasPrefix(callback, "start_"):
		e.startQueue(ctx, api, chatID, msgID, callbackID, strings.TrimPrefix(callback, "start_"))
		return

	case callback == "queue_pause":
		e.mu.Lock()
		if !e.running || e.activeChat != chatID || e.activeMsg != msgID {
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
		if !e.running || e.activeChat != chatID || e.activeMsg != msgID {
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
		delete(e.confirmations, chatID)
		textMsg := e.pendingTextLocked()
		buttons := e.pendingButtonsLocked()
		e.mu.Unlock()
		_ = api.Edit(ctx, chatID, msgID, textMsg, buttons)
		return

	case callback == "queue_continue" || callback == "queue_skip_continue" || callback == "queue_stop":
		e.pausedQueueAction(ctx, api, chatID, msgID, callbackID, callback)
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
		if err := e.DiunEvent(ev); err != nil {
			_, _ = api.Send(ctx, chatID, "Could not queue retry: "+htmlEscape(err.Error()), nil)
		}
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
		return
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		api.Answer(ctx, callbackID, "Could not create confirmation")
		return
	}
	token := hex.EncodeToString(id[:])
	buttons := [][]telegram.Button{
		{{Text: "🚀 START UPDATE", CallbackData: "start_" + token}},
		{{Text: "❌ CANCEL", CallbackData: "cancel_confirm"}},
	}
	api.Answer(ctx, callbackID, "")
	e.mu.Lock()
	if !e.running && !e.paused && e.latest[chatID] == msgID {
		e.confirmations[chatID] = confirmation{token: token, jobs: jobs}
	}
	e.mu.Unlock()
	if err := api.Edit(ctx, chatID, msgID, formatConfirmation(jobs), buttons); err != nil {
		e.mu.Lock()
		if e.confirmations[chatID].token == token {
			delete(e.confirmations, chatID)
		}
		e.mu.Unlock()
	}
}

func (e *Engine) startQueue(ctx context.Context, api *telegram.API, chatID, msgID int64, callbackID, token string) {
	jobs, err := e.acceptConfirmation(chatID, msgID, token)
	if err != nil {
		api.Answer(ctx, callbackID, err.Error())
		return
	}
	api.Answer(ctx, callbackID, "Update queue started")
	go e.runQueue(e.workCtx, api, chatID, msgID, jobs)
}

func (e *Engine) acceptConfirmation(chat, msg int64, token string) ([]store.PendingItem, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return nil, errors.New("an update is already running")
	}
	if e.paused {
		return nil, errors.New("resolve the paused queue first using /updates")
	}
	approved := e.confirmations[chat]
	if token == "" || approved.token != token || e.latest[chat] != msg {
		return nil, errors.New("confirmation expired; open /updates")
	}
	ids := make([]int64, 0, len(approved.jobs))
	for _, job := range approved.jobs {
		if job.Event.DigestRef() == "" {
			return nil, errors.New("detected digest missing; refresh the update via Diun")
		}
		ids = append(ids, job.RecordID)
	}
	remain := withoutJobs(e.pending, ids)
	if len(e.pending)-len(remain) != len(ids) {
		delete(e.confirmations, chat)
		return nil, errors.New("updates changed; open /updates and confirm again")
	}
	if err := e.db.SaveQueue(remain, approved.jobs, 0, false, ids, store.StatusQueued); err != nil {
		slog.Error("save queue start", "err", err)
		return nil, errors.New("could not save queue")
	}
	clear(e.confirmations)
	e.pending = remain
	e.running, e.paused, e.selectMode = true, false, false
	e.activeChat, e.activeMsg = chat, msg
	return approved.jobs, nil
}

func withoutJobs(pending []store.PendingItem, ids []int64) []store.PendingItem {
	remove := make(map[int64]bool, len(ids))
	for _, id := range ids {
		remove[id] = true
	}
	remain := make([]store.PendingItem, 0, len(pending))
	for _, job := range pending {
		if !remove[job.RecordID] {
			remain = append(remain, job)
		}
	}
	return remain
}

func (e *Engine) pausedQueueAction(ctx context.Context, api *telegram.API, chat, msg int64, callbackID, action string) {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		api.Answer(ctx, callbackID, "An update is already running")
		return
	}
	_, previous, index := e.db.GetQueuePaused()
	jobs, pending, err := e.resumePausedLocked(action)
	if err != nil {
		e.mu.Unlock()
		api.Answer(ctx, callbackID, err.Error())
		return
	}
	if len(jobs) > 0 {
		e.running = true
		e.activeChat, e.activeMsg = chat, msg
	}
	text, buttons := e.pendingTextLocked(), e.pendingButtonsLocked()
	if action == "queue_stop" {
		done := make([]queueResult, 0, index)
		for _, item := range previous[:index] {
			done = append(done, queueResult{Image: item.Event.Image})
		}
		remaining := make([]queueResult, 0, len(pending))
		for _, item := range pending {
			remaining = append(remaining, queueResult{Image: item.Event.Image})
		}
		text = formatQueueStopped(done, remaining)
	}
	if len(pending) == 0 {
		buttons = [][]telegram.Button{}
	}
	e.mu.Unlock()
	api.Answer(ctx, callbackID, "")
	if len(jobs) > 0 {
		go e.runQueue(e.workCtx, api, chat, msg, jobs)
		return
	}
	_ = api.Edit(ctx, chat, msg, text, buttons)
}

// resumePausedLocked updates the paused batch and pending list together.
func (e *Engine) resumePausedLocked(action string) ([]store.PendingItem, []store.PendingItem, error) {
	paused, jobs, idx := e.db.GetQueuePaused()
	if !paused || idx < 0 || idx >= len(jobs) {
		return nil, nil, errors.New("no paused queue")
	}
	jobs = jobs[idx:]
	pending := e.pending
	var skip []int64
	if action == "queue_skip_continue" {
		skip = []int64{jobs[0].RecordID}
		pending = withoutJobs(pending, skip)
		jobs = jobs[1:]
	}
	if action == "queue_stop" {
		pending = mergePending(pending, jobs)
		if err := e.db.SaveQueue(pending, nil, 0, false, jobIDs(jobs), store.StatusPending); err != nil {
			slog.Error("save stopped queue", "err", err)
			return nil, nil, errors.New("could not save stopped queue")
		}
		jobs = nil
	} else {
		if err := e.db.SaveQueue(pending, jobs, 0, false, skip, store.StatusSkipped); err != nil {
			slog.Error("save resumed queue", "err", err)
			return nil, nil, errors.New("could not save resumed queue")
		}
	}
	e.pending, e.paused, e.selectMode = pending, false, false
	return jobs, pending, nil
}

func jobIDs(jobs []store.PendingItem) []int64 {
	ids := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.RecordID)
	}
	return ids
}

func mergePending(pending, jobs []store.PendingItem) []store.PendingItem {
	out := append([]store.PendingItem(nil), pending...)
	for _, job := range jobs {
		found := false
		for _, item := range out {
			if item.RecordID == job.RecordID || (dockerapi.SameImageRef(item.Event.Image, job.Event.Image) && containerName(item.Event) == containerName(job.Event)) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, job)
		}
	}
	return out
}

func pausedButtons() [][]telegram.Button {
	return [][]telegram.Button{
		{{Text: "📋 LOGS", CallbackData: "show_logs"}, {Text: "🔁 RETRY", CallbackData: "queue_continue"}},
		{{Text: "⏭ SKIP & CONTINUE", CallbackData: "queue_skip_continue"}, {Text: "⏹️ STOP", CallbackData: "queue_stop"}},
	}
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
func (e *Engine) holdPending(jobs []store.PendingItem) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	keep := mergePending(e.pending, jobs)
	if err := e.db.SaveQueue(keep, nil, 0, false, jobIDs(jobs), store.StatusPending); err != nil {
		e.paused = true
		return err
	}
	e.paused, e.selectMode, e.pauseAfter = false, false, false
	e.pending = keep
	return nil
}

func (e *Engine) runQueue(ctx context.Context, api *telegram.API, chat, msg int64, jobs []store.PendingItem) {
	runCtx, runCancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.runCancel = runCancel
	if e.cancelRun {
		runCancel()
	}
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
		e.activeChat, e.activeMsg = 0, 0
		e.mu.Unlock()
		slog.Info("update queue idle")
		e.armCoalesce()
	}()
	queueStart := time.Now()
	done := make([]queueResult, 0, len(jobs))
	slog.Info("processing queue", "jobs", len(jobs))
	for i, j := range jobs {
		e.mu.Lock()
		pause := e.pauseAfter
		e.mu.Unlock()
		if pause {
			if err := e.holdPending(jobs[i:]); err != nil {
				slog.Error("could not save unfinished queue", "err", err)
				e.showHeld(ctx, api, chat, msg, "❌ <b>Queue save failed</b>", "Check the bot logs before continuing.")
				return
			}
			e.showHeld(ctx, api, chat, msg, "⏸ <b>Queue paused</b>", "Paused after the current image. These are still pending.")
			return
		}
		slog.Info("queue step begin", "step", i+1, "of", len(jobs), "image", j.Event.Image, "id", j.RecordID)
		res, err := e.runOne(ctx, runCtx, api, chat, msg, i+1, len(jobs), j)
		if err != nil && (errors.Is(err, errUserCancel) || e.userCancelled()) {
			if err := e.holdPending(jobs[i:]); err != nil {
				slog.Error("could not save unfinished queue", "err", err)
				e.showHeld(ctx, api, chat, msg, "❌ <b>Queue save failed</b>", "Check the bot logs before continuing.")
				return
			}
			e.showHeld(ctx, api, chat, msg, "❌ <b>Update cancelled</b>", "Stopped in progress. These are still pending.")
			return
		}
		if errors.Is(err, errSkipJob) {
			if saveErr := e.finishJob(j, jobs, i+1, store.StatusSkipped); saveErr != nil {
				err = saveErr
			} else {
				continue
			}
		}
		if err != nil {
			res.Err = err
			if saveErr := e.db.SetStatus(j.RecordID, store.StatusFailed, err.Error()); saveErr != nil {
				slog.Error("save failed job", "err", saveErr)
			}
			e.mu.Lock()
			e.lastFailID = j.RecordID
			e.paused = true
			merged := mergePending(e.pending, jobs[i:])
			if saveErr := e.db.SaveQueue(merged, jobs, i, false, nil, ""); saveErr != nil {
				slog.Error("save paused queue", "err", saveErr)
			} else {
				e.pending = merged
			}
			e.mu.Unlock()
			waiting := make([]queueResult, 0, len(jobs)-i-1)
			for k := i + 1; k < len(jobs); k++ {
				waiting = append(waiting, queueResult{Image: jobs[k].Event.Image})
			}
			slog.Error("update failed; queue paused",
				"image", j.Event.Image, "id", j.RecordID, "err", err,
				"completed", len(done), "waiting", len(waiting))
			_ = api.Edit(ctx, chat, msg, formatQueueFailed(res, done, waiting), pausedButtons())
			e.mu.Lock()
			e.setLatestLocked(chat, msg)
			e.mu.Unlock()
			return
		}
		if err := e.finishJob(j, jobs, i+1, store.StatusSuccess); err != nil {
			slog.Error("save completed update", "err", err)
			e.showHeld(ctx, api, chat, msg, "❌ <b>Queue save failed</b>", "Check the bot logs before continuing.")
			return
		}
		done = append(done, res)
		_ = api.Edit(ctx, chat, msg, formatStepSuccess(i+1, len(jobs), res), e.processingButtons())
		e.mu.Lock()
		e.lastSuccessID = res.RecordID
		e.mu.Unlock()
		e.mu.Lock()
		pause = e.pauseAfter
		e.mu.Unlock()
		if pause && i+1 < len(jobs) {
			if err := e.holdPending(jobs[i+1:]); err != nil {
				slog.Error("could not save unfinished queue", "err", err)
				e.showHeld(ctx, api, chat, msg, "❌ <b>Queue save failed</b>", "Check the bot logs before continuing.")
				return
			}
			e.showHeld(ctx, api, chat, msg, "⏸ <b>Queue paused</b>", "Paused after the current image. These are still pending.")
			return
		}
	}
	slog.Info("update queue complete", "jobs", len(jobs), "succeeded", len(done))
	buttons := [][]telegram.Button{}
	if len(done) > 0 {
		id := done[len(done)-1].RecordID
		if r, ok := e.db.Get(id); ok && r.PreviousDigest != "" && r.ContainerName != "" {
			buttons = [][]telegram.Button{{
				{Text: "↩️ ROLLBACK", CallbackData: fmt.Sprintf("rollback_%d", id)},
				{Text: "✅ KEEP", CallbackData: "keep_update"},
			}}
		}
	}
	_ = api.Edit(ctx, chat, msg, formatQueueComplete(done, time.Since(queueStart)), buttons)
	e.mu.Lock()
	e.setLatestLocked(chat, msg)
	e.mu.Unlock()

	if e.cfg.PruneAfterSuccess {
		if n, err := e.docker.PruneDanglingImages(ctx); err != nil {
			slog.Warn("prune after success failed", "err", err)
		} else {
			slog.Info("prune after success", "reclaimed_bytes", n)
		}
	}
}

func (e *Engine) finishJob(job store.PendingItem, jobs []store.PendingItem, next int, status string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	pending := withoutJobs(e.pending, []int64{job.RecordID})
	if err := e.db.SaveQueue(pending, jobs[next:], 0, false, []int64{job.RecordID}, status); err != nil {
		e.paused = true
		return err
	}
	e.pending = pending
	return nil
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
	e.setLatestLocked(chat, msg)
	e.mu.Unlock()
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
	immutable := ev.DigestRef()
	if immutable == "" || !dockerapi.SameImageRef(prevImage, image) {
		return out, fmt.Errorf("detected image %q no longer matches %q or has no valid digest", image, prevImage)
	}
	configuredImage, err := compose.Image(opCtx, target)
	if err != nil {
		return out, err
	}
	if !dockerapi.SameImageRef(configuredImage, prevImage) || strings.Contains(configuredImage, "@") {
		return out, fmt.Errorf("Compose image %q changed or is pinned; confirm the service configuration", configuredImage)
	}
	// Retrying after a recreate must retain the original rollback image.
	if record, ok := e.db.Get(item.RecordID); ok && record.PreviousDigest != "" {
		if record.Project != target.Project || record.Service != target.Service {
			return out, fmt.Errorf("Compose target changed since this update was attempted")
		}
		prevDigest = record.PreviousDigest
	}
	if err := e.docker.TagImage(opCtx, prevDigest, rollbackTag(prevDigest)); err != nil {
		return out, fmt.Errorf("retain rollback image: %w", err)
	}
	if err := e.db.SetRollbackMeta(item.RecordID, target.Project, target.Service, configuredImage, prevDigest, image, ev.Digest, target.Container); err != nil {
		return out, fmt.Errorf("save rollback image: %w", err)
	}
	if err := e.db.SetStatus(item.RecordID, store.StatusPulling, ""); err != nil {
		return out, err
	}
	slog.Info("pull starting", "step", n, "of", total, "image", image, "id", item.RecordID)
	_ = api.Edit(ctx, chat, msg, formatStepPull(n, total, image), e.processingButtons())

	last := time.Time{}
	lastPct := -1
	err = e.docker.PullImage(opCtx, immutable, func(s dockerapi.PullState) {
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
		slog.Error("pull failed", "image", image, "err", err)
		out.Duration = time.Since(start)
		return out, err
	}
	expectedID, err := e.docker.ImageID(opCtx, immutable)
	if err != nil {
		return out, err
	}
	slog.Info("pull complete", "image", image, "duration", time.Since(start).Round(time.Second).String())
	_ = api.Edit(ctx, chat, msg, formatStepPullDone(n, total, image, time.Since(start)), e.processingButtons())

	if err := e.db.SetStatus(item.RecordID, store.StatusUpdating, ""); err != nil {
		return out, err
	}
	if err := e.docker.TagImage(opCtx, expectedID, configuredImage); err != nil {
		return out, err
	}
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
		slog.Error("compose up failed", "service", target.Service, "err", err, "output", strings.TrimSpace(composeOut))
		out.Duration = time.Since(start)
		return out, fmt.Errorf("compose update failed: %w", err)
	}
	slog.Info("compose up complete", "service", target.Service)
	_ = e.db.SetStatus(item.RecordID, store.StatusVerifying, "")
	_ = api.Edit(ctx, chat, msg, formatStepHealth(n, total, target.Service), e.processingButtons())
	slog.Info("health verification starting", "container", target.Container, "timeout_sec", e.cfg.Update.HealthTimeoutSeconds)
	if err := e.verifyHealth(opCtx, target, expectedID); err != nil {
		if e.userCancelled() {
			out.Duration = time.Since(start)
			return out, errUserCancel
		}
		slog.Error("health verification failed", "container", target.Container, "err", err)
		out.Duration = time.Since(start)
		return out, err
	}
	slog.Info("health verification ok", "container", target.Container)

	out.Duration = time.Since(start)
	slog.Info("update success",
		"step", n, "of", total,
		"service", target.Service,
		"image", image,
		"duration", out.Duration.Round(time.Second).String(),
	)
	return out, nil
}

func (e *Engine) resolveComposeTarget(ctx context.Context, ev diun.Event) (compose.Target, string, string, error) {
	name := containerName(ev)
	if name == "" {
		found, err := e.docker.FindRunningByImage(ctx, ev.Image)
		if err != nil {
			return compose.Target{}, "", "", err
		}
		if found == "" {
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
	if service == "" || dir == "" || project == "" {
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

func (e *Engine) verifyHealth(ctx context.Context, target compose.Target, expectedID string) error {
	timeout := time.Duration(e.cfg.Update.HealthTimeoutSeconds) * time.Second
	if timeout < time.Second {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	last := "not inspected"
	for {
		info, err := e.docker.InspectContainer(ctx, target.Container)
		switch {
		case err != nil:
			last = err.Error()
		case info.Config.Labels["com.docker.compose.project"] != target.Project || info.Config.Labels["com.docker.compose.service"] != target.Service:
			return fmt.Errorf("container %q no longer belongs to %s/%s", target.Container, target.Project, target.Service)
		case info.Image != expectedID:
			return fmt.Errorf("container %q is using image %s, expected %s", target.Container, info.Image, expectedID)
		default:
			last = info.State.Status
			if info.State.Running && !e.cfg.RequireHealthy {
				return nil
			}
			if info.State.Health != nil {
				last = info.State.Health.Status
				if info.State.Running && last == "healthy" {
					return nil
				}
			} else if e.cfg.RequireHealthy {
				return fmt.Errorf("container %q has no Docker HEALTHCHECK", target.Container)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("health verification for %q: %w (last status %s)", target.Container, ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

func (e *Engine) cfgRefresh() time.Duration {
	n := e.cfg.Update.RefreshSeconds
	if n < 1 {
		n = 2
	}
	return time.Duration(n) * time.Second
}
