package dockerupdater

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dockerupbot/dockerupbot/internal/dockerapi"
	"github.com/dockerupbot/dockerupbot/internal/store"
)

const msgRule = "────────────────"

type queueResult struct {
	RecordID  int64
	Image     string
	Service   string
	Container string
	Duration  time.Duration
	Err       error
}

func msgHeader(title string) string {
	return fmt.Sprintf("🐳 <b>DockerUpBot</b>\n%s\n\n%s\n\n", msgRule, title)
}

func msgFooter(extra string) string {
	if extra == "" {
		return fmt.Sprintf("%s\n%s", msgRule, time.Now().Format("15:04"))
	}
	return fmt.Sprintf("%s\n%s · %s", msgRule, extra, time.Now().Format("15:04"))
}

func formatPending(items []store.PendingItem, selectMode bool) string {
	var b strings.Builder
	b.WriteString(msgHeader(fmt.Sprintf("🔔 <b>%d update(s) available</b>", len(items))))
	for i, p := range items {
		mark := "•"
		if selectMode {
			if p.Selected {
				mark = "☑"
			} else {
				mark = "☐"
			}
		}
		fmt.Fprintf(&b, "%s <code>%s</code>\n", mark, htmlEscape(p.Event.Image))
		ctn := ""
		if p.Event.Metadata != nil {
			ctn = strings.TrimPrefix(strings.Split(p.Event.Metadata["ctn_names"], ",")[0], "/")
		}
		if ctn != "" {
			risky := ""
			low := strings.ToLower(ctn + " " + p.Event.Image)
			if strings.Contains(low, "postgres") || strings.Contains(low, "mysql") || strings.Contains(low, "mariadb") || strings.Contains(low, "kopia") || strings.Contains(low, "redis") || strings.Contains(low, "valkey") {
				risky = " ⚠ data"
			}
			fmt.Fprintf(&b, "   container <code>%s</code>%s\n", htmlEscape(ctn), risky)
		}
		if d := shortDigest(p.Event.Digest); d != "" {
			fmt.Fprintf(&b, "   <i>%s</i>\n", htmlEscape(d))
		}
		if i < len(items)-1 {
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
	b.WriteString(msgFooter("Only latest message is actionable"))
	return b.String()
}

func formatRecovery(n int) string {
	return msgHeader("⚠️ <b>Update interrupted</b>") +
		fmt.Sprintf("%d job(s) were in progress when the bot restarted.\n\n"+
			"Queue is <b>paused</b>. CONTINUE resumes it. STOP leaves unfinished images pending.\n\n", n) +
		msgFooter("Startup recovery")
}

func formatRolledBack(image, prev string) string {
	return msgHeader("↩️ <b>Rolled back</b>") +
		fmt.Sprintf("Service restored to previous image.\n\n<code>%s</code>\n↳ <code>%s</code>\n\n", htmlEscape(image), htmlEscape(prev)) +
		msgFooter("")
}

func formatKept(image string) string {
	return msgHeader("✅ <b>Update kept</b>") +
		fmt.Sprintf("<code>%s</code>\n\nNo rollback.\n\n", htmlEscape(image)) +
		msgFooter("")
}

func formatConfirmation(jobs []store.PendingItem) string {
	var b strings.Builder
	b.WriteString(msgHeader(fmt.Sprintf("✅ <b>Confirm %d update(s)</b>", len(jobs))))
	b.WriteString("Will run <b>one at a time</b>:\n\n")
	for i, j := range jobs {
		fmt.Fprintf(&b, "%d. <code>%s</code>\n", i+1, htmlEscape(j.Event.Image))
	}
	b.WriteByte('\n')
	b.WriteString(msgFooter("Review then START or CANCEL"))
	return b.String()
}

func formatSkipped(images []string) string {
	var b strings.Builder
	b.WriteString(msgHeader(fmt.Sprintf("⏭ <b>Skipped %d update(s)</b>", len(images))))
	if len(images) == 0 {
		b.WriteString("Nothing was pending.\n\n")
	} else {
		b.WriteString("No images were pulled or recreated:\n\n")
		for i, img := range images {
			fmt.Fprintf(&b, "%d. <code>%s</code>\n", i+1, htmlEscape(img))
		}
		b.WriteByte('\n')
	}
	b.WriteString(msgFooter("Monitoring continues"))
	return b.String()
}

func formatQueueStopped(done []queueResult, remaining []queueResult) string {
	var b strings.Builder
	b.WriteString(msgHeader("⏹ <b>Queue stopped</b>"))
	if len(done) > 0 {
		b.WriteString("<b>Finished before stop</b>\n")
		for i, r := range done {
			fmt.Fprintf(&b, "%d. ✅ <code>%s</code>", i+1, htmlEscape(displayName(r)))
			if r.Duration > 0 {
				fmt.Fprintf(&b, " · %s", r.Duration.Round(time.Second))
			}
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	if len(remaining) > 0 {
		b.WriteString("<b>Not applied</b>\n")
		for i, r := range remaining {
			fmt.Fprintf(&b, "%d. ⏸ <code>%s</code>\n", i+1, htmlEscape(displayName(r)))
		}
		b.WriteByte('\n')
	}
	if len(done) == 0 && len(remaining) == 0 {
		b.WriteString("No further updates will run.\n\n")
	}
	b.WriteString(msgFooter(fmt.Sprintf("%d done · %d still pending", len(done), len(remaining))))
	return b.String()
}

func formatQueueFailed(failed queueResult, done []queueResult, waiting []queueResult) string {
	var b strings.Builder
	b.WriteString(msgHeader("⚠️ <b>Update failed</b> — queue paused"))
	b.WriteString("<b>Failed</b>\n")
	fmt.Fprintf(&b, "❌ <code>%s</code>\n", htmlEscape(displayName(failed)))
	if failed.Err != nil {
		fmt.Fprintf(&b, "<i>%s</i>\n", htmlEscape(failed.Err.Error()))
	}
	b.WriteByte('\n')
	if len(done) > 0 {
		b.WriteString("<b>Already succeeded</b>\n")
		for i, r := range done {
			fmt.Fprintf(&b, "%d. ✅ <code>%s</code>\n", i+1, htmlEscape(displayName(r)))
		}
		b.WriteByte('\n')
	}
	if len(waiting) > 0 {
		b.WriteString("<b>Still waiting</b>\n")
		for i, r := range waiting {
			fmt.Fprintf(&b, "%d. ⏳ <code>%s</code>\n", i+1, htmlEscape(displayName(r)))
		}
		b.WriteByte('\n')
	}
	b.WriteString(msgFooter(fmt.Sprintf("%d ok · 1 failed · %d waiting", len(done), len(waiting))))
	return b.String()
}

func formatQueueComplete(results []queueResult, total time.Duration) string {
	var b strings.Builder
	ok := 0
	for _, r := range results {
		if r.Err == nil {
			ok++
		}
	}
	title := fmt.Sprintf("✅ <b>All updates succeeded</b> (%d/%d)", ok, len(results))
	if ok != len(results) {
		title = fmt.Sprintf("☑️ <b>Queue finished</b> (%d/%d ok)", ok, len(results))
	}
	b.WriteString(msgHeader(title))
	for i, r := range results {
		icon := "✅"
		if r.Err != nil {
			icon = "❌"
		}
		fmt.Fprintf(&b, "%d. %s <code>%s</code>", i+1, icon, htmlEscape(displayName(r)))
		if r.Service != "" && r.Service != r.Image {
			fmt.Fprintf(&b, "\n    service <code>%s</code>", htmlEscape(r.Service))
		}
		if r.Container != "" {
			fmt.Fprintf(&b, "\n    container <code>%s</code>", htmlEscape(r.Container))
		}
		if r.Duration > 0 {
			fmt.Fprintf(&b, "\n    ⏱ %s", r.Duration.Round(time.Second))
		}
		if r.Err != nil {
			fmt.Fprintf(&b, "\n    <i>%s</i>", htmlEscape(r.Err.Error()))
		}
		b.WriteByte('\n')
		if i < len(results)-1 {
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
	b.WriteString(msgFooter(fmt.Sprintf("Total %s", total.Round(time.Second))))
	return b.String()
}

func formatStepPull(n, total int, image string) string {
	return msgHeader(fmt.Sprintf("🚀 <b>Updating %d/%d</b>", n, total)) +
		fmt.Sprintf("<code>%s</code>\n\n⏳ Pulling image…\n\n", htmlEscape(image)) +
		msgFooter("Live")
}

func formatStepPullDone(n, total int, image string, d time.Duration) string {
	return msgHeader(fmt.Sprintf("🚀 <b>Updating %d/%d</b>", n, total)) +
		fmt.Sprintf("<code>%s</code>\n\n📥 Pull complete · %s\n\n🔄 Recreating service…\n\n", htmlEscape(image), d.Round(time.Second)) +
		msgFooter("Live")
}

func formatStepRecreate(n, total int, service, dir string) string {
	return msgHeader(fmt.Sprintf("🚀 <b>Updating %d/%d</b>", n, total)) +
		fmt.Sprintf("🔄 Recreating <code>%s</code>\n📁 <code>%s</code>\n\n", htmlEscape(service), htmlEscape(dir)) +
		msgFooter("Live")
}

func formatStepHealth(n, total int, service string) string {
	return msgHeader(fmt.Sprintf("🚀 <b>Updating %d/%d</b>", n, total)) +
		fmt.Sprintf("🔍 Health check\n<code>%s</code>\n\n⏳ Waiting for healthy/running…\n\n", htmlEscape(service)) +
		msgFooter("Live")
}

func formatStepSuccess(n, total int, r queueResult) string {
	var b strings.Builder
	b.WriteString(msgHeader(fmt.Sprintf("✅ <b>Update %d/%d done</b>", n, total)))
	fmt.Fprintf(&b, "<code>%s</code>\n", htmlEscape(displayName(r)))
	if r.Service != "" {
		fmt.Fprintf(&b, "service <code>%s</code>\n", htmlEscape(r.Service))
	}
	if r.Container != "" {
		fmt.Fprintf(&b, "container <code>%s</code>\n", htmlEscape(r.Container))
	}
	b.WriteString("\nPull ✅ · Recreate ✅ · Health ✅\n\n")
	b.WriteString(msgFooter(fmt.Sprintf("%s", r.Duration.Round(time.Second))))
	return b.String()
}

func formatPull(n, total int, image string, s dockerapi.PullState, d time.Duration) string {
	pct := "--"
	bar := "░░░░░░░░░░"
	if s.Total > 0 {
		p := int(s.Current * 100 / s.Total)
		if p > 100 {
			p = 100
		}
		pct = strconv.Itoa(p) + "%"
		filled := p / 10
		if filled > 10 {
			filled = 10
		}
		bar = strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)
	}
	speed := "--"
	if s.SpeedBps > 0 {
		speed = formatBytes(int64(s.SpeedBps)) + "/s"
	}
	eta := "--"
	if s.ETA > 0 {
		eta = s.ETA.Round(time.Second).String()
	}
	var b strings.Builder
	b.WriteString(msgHeader(fmt.Sprintf("🚀 <b>Updating %d/%d</b>", n, total)))
	fmt.Fprintf(&b, "<code>%s</code>\n\n", htmlEscape(image))
	fmt.Fprintf(&b, "📥 Pulling\n<code>%s</code>  %s\n", bar, pct)
	fmt.Fprintf(&b, "%s / %s · %s · ETA %s\n", formatBytes(s.Current), formatBytes(s.Total), speed, eta)
	fmt.Fprintf(&b, "Layers %d done · %d active · %d waiting\n", s.Done, s.Active, s.Waiting)
	if s.Status != "" {
		fmt.Fprintf(&b, "<i>%s</i>", htmlEscape(s.Status))
		if s.ID != "" {
			fmt.Fprintf(&b, " <code>%s</code>", htmlEscape(s.ID))
		}
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(msgFooter(fmt.Sprintf("Live · %s", d.Round(time.Second))))
	return b.String()
}

func displayName(r queueResult) string {
	if r.Image != "" {
		return r.Image
	}
	if r.Container != "" {
		return r.Container
	}
	return r.Service
}

func formatBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", n, units[i])
	}
	return fmt.Sprintf("%.2f %s", f, units[i])
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;").Replace(s)
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
