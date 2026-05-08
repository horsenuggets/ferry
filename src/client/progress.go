package client

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/term"
)

// ProgressMode controls how a Progress reporter renders its updates.
type ProgressMode int

const (
	// ProgressTTY draws an ANSI redraw bar on stderr, throttled to ~30fps.
	ProgressTTY ProgressMode = iota
	// ProgressLine prints one summary line per second to stderr (no ANSI).
	// Used when stderr is not a TTY so logs stay readable.
	ProgressLine
	// ProgressJSON emits one JSON event per second to stdout, plus a final
	// "done" event. Used by --json mode.
	ProgressJSON
	// ProgressSilent disables all output. Useful in tests.
	ProgressSilent
)

// AutoProgressMode picks ProgressTTY when stderr is a terminal, otherwise
// ProgressLine. JSON mode is opt-in via ProgressJSON, never auto.
func AutoProgressMode() ProgressMode {
	if term.IsTerminal(int(os.Stderr.Fd())) {
		return ProgressTTY
	}
	return ProgressLine
}

// Progress tracks bytes transferred and renders progress to a writer. All
// methods are safe to call from a single goroutine; not goroutine-safe.
type Progress struct {
	mode     ProgressMode
	total    int64
	bytes    int64
	start    time.Time
	lastDraw time.Time
	out      io.Writer // stderr for TTY/Line; stdout for JSON
	stdout   io.Writer // for JSON final "done" event regardless of mode
	now      func() time.Time

	// drawInterval is the minimum time between renders. 33ms gives ~30fps.
	drawInterval time.Duration
	// lineInterval is the minimum time between Line/JSON ticks.
	lineInterval time.Duration

	finished bool
}

// NewProgress builds a Progress for the given total byte count and mode.
// stderrW receives TTY/Line output; stdoutW receives JSON events.
func NewProgress(total int64, mode ProgressMode, stderrW, stdoutW io.Writer) *Progress {
	if stderrW == nil {
		stderrW = io.Discard
	}
	if stdoutW == nil {
		stdoutW = io.Discard
	}
	now := time.Now
	out := stderrW
	if mode == ProgressJSON {
		out = stdoutW
	}
	return &Progress{
		mode:         mode,
		total:        total,
		out:          out,
		stdout:       stdoutW,
		now:          now,
		start:        now(),
		drawInterval: 33 * time.Millisecond,
		lineInterval: 1 * time.Second,
	}
}

// SetClock overrides the clock for tests.
func (p *Progress) SetClock(now func() time.Time) {
	p.now = now
	p.start = now()
}

// Add records that n more bytes have been transferred and renders if the
// throttle gate allows it.
func (p *Progress) Add(n int64) {
	p.bytes += n
	p.maybeRender(false)
}

// SetBytes overrides the byte counter (used after a HEAD-based resume).
func (p *Progress) SetBytes(n int64) {
	p.bytes = n
	p.maybeRender(false)
}

func (p *Progress) maybeRender(force bool) {
	now := p.now()
	switch p.mode {
	case ProgressTTY:
		if !force && now.Sub(p.lastDraw) < p.drawInterval {
			return
		}
		p.lastDraw = now
		p.drawTTY(now)
	case ProgressLine:
		if !force && now.Sub(p.lastDraw) < p.lineInterval {
			return
		}
		p.lastDraw = now
		p.drawLine(now)
	case ProgressJSON:
		if !force && now.Sub(p.lastDraw) < p.lineInterval {
			return
		}
		p.lastDraw = now
		p.drawJSON(now)
	case ProgressSilent:
		// no-op
	}
}

func (p *Progress) drawTTY(now time.Time) {
	pct, rate, eta := p.metrics(now)
	const barWidth = 14
	filled := int(float64(barWidth) * pct / 100.0)
	if filled > barWidth {
		filled = barWidth
	}
	bar := make([]byte, barWidth)
	for i := 0; i < barWidth; i++ {
		if i < filled {
			bar[i] = '#'
		} else {
			bar[i] = '-'
		}
	}
	fmt.Fprintf(p.out, "\ruploading: [%s] %5.1f%% | %s / %s | %s/s | ETA %s   ",
		string(bar), pct,
		humanBytes(p.bytes), humanBytes(p.total),
		humanBytes(rate), humanDuration(eta))
}

func (p *Progress) drawLine(now time.Time) {
	pct, rate, eta := p.metrics(now)
	fmt.Fprintf(p.out, "uploading: %.1f%% (%s / %s, %s/s, ETA %s)\n",
		pct, humanBytes(p.bytes), humanBytes(p.total),
		humanBytes(rate), humanDuration(eta))
}

func (p *Progress) drawJSON(now time.Time) {
	_, rate, eta := p.metrics(now)
	_ = json.NewEncoder(p.out).Encode(map[string]any{
		"event":       "progress",
		"bytes":       p.bytes,
		"total":       p.total,
		"rate_bps":    rate,
		"eta_seconds": int64(eta.Seconds() + 0.5),
	})
}

// Done renders a final "completed" line/event. Should be called exactly once
// after the upload succeeds.
func (p *Progress) Done(uploadID, locationURL string) {
	if p.finished {
		return
	}
	p.finished = true
	now := p.now()
	dur := now.Sub(p.start)
	switch p.mode {
	case ProgressTTY:
		// Force a final draw at 100%, then a newline.
		p.bytes = p.total
		p.maybeRender(true)
		fmt.Fprintf(p.out, "\ndone in %.1fs\n", dur.Seconds())
	case ProgressLine:
		fmt.Fprintf(p.out, "done in %.1fs\n", dur.Seconds())
	case ProgressJSON:
		_ = json.NewEncoder(p.stdout).Encode(map[string]any{
			"event":            "done",
			"duration_seconds": round1(dur.Seconds()),
			"upload_id":        uploadID,
			"url":              locationURL,
		})
	case ProgressSilent:
	}
}

// metrics returns (percent, bytes/sec, eta).
func (p *Progress) metrics(now time.Time) (pct float64, rate int64, eta time.Duration) {
	if p.total > 0 {
		pct = float64(p.bytes) / float64(p.total) * 100.0
		if pct > 100.0 {
			pct = 100.0
		}
	}
	elapsed := now.Sub(p.start).Seconds()
	if elapsed > 0 {
		rate = int64(float64(p.bytes) / elapsed)
	}
	if rate > 0 && p.bytes < p.total {
		eta = time.Duration(float64(p.total-p.bytes)/float64(rate)) * time.Second
	}
	return pct, rate, eta
}

// humanBytes renders a byte count in IEC units to one decimal place.
func humanBytes(n int64) string {
	const unit = 1024.0
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := unit, 0
	for v := float64(n) / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	if exp >= len(suffixes) {
		exp = len(suffixes) - 1
	}
	return fmt.Sprintf("%.1f %s", float64(n)/div, suffixes[exp])
}

// humanDuration renders a Duration as a short human-readable string. For
// short ETAs (<60s) we just print seconds; for longer, mm:ss.
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "--"
	}
	secs := int64(d.Seconds() + 0.5)
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	return fmt.Sprintf("%dm%02ds", secs/60, secs%60)
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}
