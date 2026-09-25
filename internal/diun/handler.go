package diun

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

type Engine interface{ DiunEvent(Event) }
type Event struct {
	DiunVersion string            `json:"diun_version"`
	Hostname    string            `json:"hostname"`
	Status      string            `json:"status"`
	Image       string            `json:"image"`
	Digest      string            `json:"digest"`
	Platform    string            `json:"platform"`
	Metadata    map[string]string `json:"metadata"`
}
type Handler struct {
	engine Engine
	secret string
}

func NewHandler(e Engine, secret string) *Handler { return &Handler{engine: e, secret: secret} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	remote := r.RemoteAddr
	if h.secret == "" {
		slog.Error("webhook rejected: WEBHOOK_SECRET not configured")
		http.Error(w, "unauthorized", 401)
		return
	}
	got := r.Header.Get("Authorization")
	if subtle.ConstantTimeCompare([]byte(got), []byte(h.secret)) != 1 {
		slog.Warn("webhook rejected: bad authorization", "remote", remote)
		http.Error(w, "unauthorized", 401)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		slog.Warn("webhook rejected: content-type must be json", "remote", remote, "content_type", r.Header.Get("Content-Type"))
		http.Error(w, "content-type must be json", 415)
		return
	}
	var e Event
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		slog.Warn("webhook rejected: bad json", "remote", remote, "err", err)
		http.Error(w, "bad json", 400)
		return
	}
	ctn := ""
	if e.Metadata != nil {
		ctn = e.Metadata["ctn_names"]
	}
	if e.Status != "new" && e.Status != "update" {
		slog.Info("webhook ignored: status not actionable",
			"status", e.Status, "image", e.Image, "digest", shortDigest(e.Digest), "remote", remote)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if IsTestOrIncomplete(e) {
		slog.Info("webhook ignored: test payload or missing ctn_names",
			"image", e.Image, "container", ctn, "remote", remote)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	slog.Info("webhook received",
		"status", e.Status,
		"image", e.Image,
		"digest", shortDigest(e.Digest),
		"platform", e.Platform,
		"container", ctn,
		"hostname", e.Hostname,
		"diun", e.DiunVersion,
		"remote", remote,
	)
	h.engine.DiunEvent(e)
	w.WriteHeader(http.StatusAccepted)
}

// IsTestOrIncomplete is true for Diun notif-test payloads and events without a container name.
func IsTestOrIncomplete(e Event) bool {
	img := strings.ToLower(e.Image)
	if strings.Contains(img, "diun/testnotif") {
		return true
	}
	ctn := ""
	if e.Metadata != nil {
		ctn = strings.TrimSpace(strings.TrimPrefix(strings.Split(e.Metadata["ctn_names"], ",")[0], "/"))
	}
	return ctn == ""
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
