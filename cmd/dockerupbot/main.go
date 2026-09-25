package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dockerupbot/dockerupbot/internal/applog"
	"github.com/dockerupbot/dockerupbot/internal/config"
	"github.com/dockerupbot/dockerupbot/internal/diun"
	"github.com/dockerupbot/dockerupbot/internal/dockerapi"
	dock "github.com/dockerupbot/dockerupbot/internal/dockerupdater"
	"github.com/dockerupbot/dockerupbot/internal/emptyscan"
	"github.com/dockerupbot/dockerupbot/internal/store"
	"github.com/dockerupbot/dockerupbot/internal/telegram"
	"github.com/dockerupbot/dockerupbot/internal/version"
)

func main() {
	applog.Init()

	// Debug CLI: `dubot health|telegram|ping|diun` (also `dockerupbot <cmd>`).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "health", "telegram", "ping", "diun", "help", "-h", "--help":
			runCLI(os.Args[1:])
			return
		case "serve":
			// fall through to server
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q — try: dubot help\n", os.Args[1])
			os.Exit(2)
		}
	}

	runServer()
}

func runServer() {
	slog.Info("starting", "app", version.String())

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = os.Getenv("DOCKERUPBOT_CONFIG")
	}
	if cfgPath == "" {
		cfgPath = "/data/config.yml"
	}
	slog.Info("loading config", "path", cfgPath)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err)
		log.Fatal(err)
	}
	slog.Info("config loaded",
		"http_addr", cfg.HTTPAddr,
		"data_path", cfg.DataPath,
		"compose_root", cfg.Update.ComposeRoot,
		"allowed_users", len(cfg.Telegram.AllowedUsers),
		"chat_id", cfg.Telegram.ChatID,
		"webhook_secret_set", cfg.WebhookSecret != "",
		"health_timeout_sec", cfg.Update.HealthTimeoutSeconds,
		"notify_empty_scan", cfg.NotifyEmptyScan,
		"diun_container", cfg.DiunContainer,
	)

	dbPath := cfg.DataPath
	if dbPath == "" {
		dbPath = "/data/dockerupbot.json"
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0750); err != nil {
		log.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		slog.Error("store open failed", "path", dbPath, "err", err)
		log.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			slog.Error("store close failed", "err", err)
		}
	}()
	slog.Info("store ready", "path", dbPath)

	docker, err := dockerapi.New()
	if err != nil {
		slog.Error("docker client failed", "err", err)
		log.Fatal(err)
	}
	defer docker.Close()

	bot := telegram.New(cfg.Telegram.Token, cfg.Telegram.AllowedUsers, cfg.Telegram.RefreshSeconds)

	ctxInit, cancelInit := context.WithTimeout(context.Background(), 15*time.Second)
	botInfo, err := bot.GetMe(ctxInit)
	cancelInit()
	if err != nil {
		slog.Error("telegram connection failed (check TELEGRAM_TOKEN)", "err", err)
		log.Fatal(err)
	}
	if cfg.SanitizeNotifyTargets(botInfo.ID) {
		slog.Warn("TELEGRAM_CHAT_ID (or ALLOWED_USERS) pointed at the bot itself — ignored. Use your personal user ID or a group/channel ID, not the bot id.",
			"bot_id", botInfo.ID,
			"allowed_users", cfg.Telegram.AllowedUsers,
			"notify_targets", cfg.NotifyTargets(),
		)
		// Rebuild allowlist after dropping the bot id.
		bot = telegram.New(cfg.Telegram.Token, cfg.Telegram.AllowedUsers, cfg.Telegram.RefreshSeconds)
	}
	if len(cfg.NotifyTargets()) == 0 {
		slog.Error("no valid notify targets after sanitizing; set ALLOWED_USERS to your Telegram user ID")
		log.Fatal("no notify targets")
	}
	slog.Info("telegram connected",
		"bot_id", botInfo.ID,
		"username", "@"+botInfo.Username,
		"name", botInfo.Name,
		"allowed_users", cfg.Telegram.AllowedUsers,
		"notify_targets", cfg.NotifyTargets(),
	)

	// Work context is independent of SIGTERM so in-flight updates can finish (spec §55).
	workCtx, workCancel := context.WithCancel(context.Background())
	defer workCancel()

	engine := dock.New(cfg, bot, db, docker, workCtx)
	engine.RecoverInFlight(workCtx)
	engine.NotifyRecovery(context.Background())

	handler := diun.NewHandler(engine, cfg.WebhookSecret)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/debug/telegram", func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(cfg.WebhookSecret)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		info, err := bot.GetMe(ctx)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			slog.Error("debug telegram check failed", "err", err)
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "telegram check failed"})
			return
		}
		resp := map[string]any{
			"ok":       true,
			"username": "@" + info.Username,
		}
		if r.URL.Query().Get("ping") == "1" {
			targets := cfg.NotifyTargets()
			if len(targets) == 0 {
				resp["ping"] = map[string]any{"ok": false, "error": "no notify targets (set ALLOWED_USERS or TELEGRAM_CHAT_ID)"}
			} else {
				msg := "🐳 <b>DockerUpBot</b>\n\n✅ Telegram connection OK\nBot: @" + htmlEscapeDebug(info.Username) + "\nVersion: <code>" + htmlEscapeDebug(version.Short()) + "</code>"
				sent := 0
				for _, chat := range targets {
					if _, e := bot.Send(ctx, chat, msg, nil); e != nil {
						slog.Error("debug telegram ping failed", "chat", chat, "err", e)
						continue
					}
					sent++
					slog.Info("debug telegram ping sent", "chat", chat)
				}
				if sent == 0 {
					resp["ping"] = map[string]any{"ok": false, "error": "telegram send failed"}
				} else {
					resp["ping"] = map[string]any{"ok": true, "sent": sent}
				}
			}
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/webhook/diun", handler.ServeHTTP)

	addr := cfg.HTTPAddr
	if addr == "" {
		addr = ":9467"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		slog.Info("http server listening",
			"addr", addr,
			"webhook", "/webhook/diun",
			"health", "/health",
			"debug_telegram", "/debug/telegram",
		)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server failed", "err", err)
			log.Fatal(err)
		}
	}()

	pollCtx, pollStop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer pollStop()
	go bot.Run(pollCtx, engine)
	slog.Info("telegram polling started")
	if err := bot.SetMyCommands(pollCtx); err != nil {
		slog.Warn("setMyCommands failed", "err", err)
	} else {
		slog.Info("telegram command menu registered")
	}
	go engine.RunQuietHoursFlusher(pollCtx)

	if cfg.NotifyEmptyScan {
		w := &emptyscan.Watcher{
			Docker:    docker,
			Bot:       bot,
			Targets:   cfg.NotifyTargets(),
			Container: cfg.DiunContainer,
		}
		go w.Run(pollCtx)
	} else {
		slog.Info("empty-scan notifications disabled (set NOTIFY_EMPTY_SCAN=true to enable)")
	}

	<-pollCtx.Done()
	slog.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http shutdown", "err", err)
	} else {
		slog.Info("http server stopped")
	}

	select {
	case <-time.After(30 * time.Second):
		slog.Warn("grace period ended; cancelling in-flight work")
	case <-func() <-chan struct{} {
		ch := make(chan struct{})
		go func() {
			time.Sleep(2 * time.Second)
			close(ch)
		}()
		return ch
	}():
		slog.Info("grace period complete")
	}
	workCancel()
	_ = db.Close()
	slog.Info("DockerUpBot stopped")
}

func htmlEscapeDebug(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;")
	return replacer.Replace(s)
}
