package emptyscan

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dockerupbot/dockerupbot/internal/dockerapi"
	"github.com/dockerupbot/dockerupbot/internal/telegram"
)

// JobStats is parsed from a Diun "Jobs completed …" log line.
type JobStats struct {
	Added     int
	Failed    int
	Skipped   int
	Unchanged int
	Updated   int
}

var jobsCompletedRe = regexp.MustCompile(
	`Jobs completed added=(\d+) failed=(\d+) skipped=(\d+) unchanged=(\d+) updated=(\d+)`,
)

// ParseJobsCompleted extracts Diun job counters from a log line.
func ParseJobsCompleted(line string) (JobStats, bool) {
	m := jobsCompletedRe.FindStringSubmatch(line)
	if m == nil {
		return JobStats{}, false
	}
	atoi := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	return JobStats{
		Added:     atoi(m[1]),
		Failed:    atoi(m[2]),
		Skipped:   atoi(m[3]),
		Unchanged: atoi(m[4]),
		Updated:   atoi(m[5]),
	}, true
}

// FormatMessage builds the Telegram body for an empty Diun scan.
func FormatMessage(s JobStats) string {
	var b strings.Builder
	b.WriteString("🐳 <b>DockerUpBot</b>\n────────────────\n\n")
	b.WriteString("💤 <b>Diun scan complete</b> — 0 updates\n\n")
	b.WriteString("Cron finished with no image updates.\n\n")
	fmt.Fprintf(&b, "Unchanged <b>%d</b>\n", s.Unchanged)
	fmt.Fprintf(&b, "Added <b>%d</b> · Failed <b>%d</b> · Skipped <b>%d</b>\n\n", s.Added, s.Failed, s.Skipped)
	b.WriteString("This ping confirms Diun ran.\n")
	b.WriteString("Disable: <code>NOTIFY_EMPTY_SCAN=false</code>\n\n")
	fmt.Fprintf(&b, "────────────────\n%s", time.Now().Format("15:04"))
	return b.String()
}

// Watcher tails the Diun container logs and notifies Telegram when a scan
// finishes with updated=0 (optional heartbeat that Diun cron is alive).
type Watcher struct {
	Docker    *dockerapi.Client
	Bot       *telegram.API
	Targets   []int64
	Container string
}

// Run follows logs until ctx is cancelled. Restarts the stream on transient errors.
func (w *Watcher) Run(ctx context.Context) {
	if w == nil || w.Docker == nil || w.Bot == nil || w.Container == "" {
		return
	}
	if len(w.Targets) == 0 {
		slog.Warn("empty-scan watcher: no notify targets")
		return
	}
	slog.Info("empty-scan watcher started", "container", w.Container, "targets", len(w.Targets))
	since := time.Now()
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := w.Docker.FollowLogs(ctx, w.Container, since, func(line string) {
			stats, ok := ParseJobsCompleted(line)
			if !ok {
				return
			}
			since = time.Now()
			if stats.Updated > 0 {
				slog.Info("diun scan had updates; empty-scan notify skipped",
					"updated", stats.Updated, "unchanged", stats.Unchanged, "added", stats.Added)
				return
			}
			msg := FormatMessage(stats)
			slog.Info("diun empty scan detected; notifying",
				"added", stats.Added, "failed", stats.Failed, "unchanged", stats.Unchanged, "updated", stats.Updated)
			sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			for _, chat := range w.Targets {
				if _, e := w.Bot.Send(sendCtx, chat, msg, nil); e != nil {
					slog.Error("empty-scan notify failed", "chat", chat, "err", e)
				}
			}
			cancel()
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("empty-scan log follow ended", "container", w.Container, "err", err, "retry_in", backoff.String())
		} else {
			slog.Warn("empty-scan log follow ended", "container", w.Container, "retry_in", backoff.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}
