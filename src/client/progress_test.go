package client

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fakeClock advances by an explicit step on each call.
type fakeClock struct {
	t    time.Time
	step time.Duration
}

func (f *fakeClock) now() time.Time {
	cur := f.t
	f.t = f.t.Add(f.step)
	return cur
}

func TestProgress_TTYThrottle(t *testing.T) {
	var stderr bytes.Buffer
	p := NewProgress(1<<20, ProgressTTY, &stderr, nil)

	// Pin the clock so 100 Add calls all happen "at the same instant".
	frozen := time.Unix(0, 0)
	p.now = func() time.Time { return frozen }
	p.start = frozen
	p.lastDraw = time.Time{} // ensure first draw goes through

	// First call draws; next 99 within the same instant should be throttled.
	for i := 0; i < 100; i++ {
		p.Add(1024)
	}
	// Count how many times "uploading:" appears in the buffer. Allow 0..2
	// since the throttle only updates lastDraw on a successful render and
	// the first one may or may not happen depending on how the gate trips
	// at the zero-value Time. But it must be << 100.
	got := strings.Count(stderr.String(), "uploading:")
	if got > 2 {
		t.Fatalf("expected throttled redraws (<=2), got %d in output: %q", got, stderr.String())
	}
}

func TestProgress_JSONShape(t *testing.T) {
	var stdout bytes.Buffer
	p := NewProgress(1000, ProgressJSON, nil, &stdout)
	// Inject a clock that ticks 1.1s on each call so the throttle gate (1s)
	// fires every render.
	clk := &fakeClock{t: time.Unix(0, 0), step: 1100 * time.Millisecond}
	p.now = clk.now
	// Re-stamp start so elapsed math is sensible.
	p.start = time.Unix(0, 0)

	for i := 0; i < 3; i++ {
		p.Add(100)
	}

	dec := json.NewDecoder(&stdout)
	for i := 0; i < 1; i++ { // at least one event must have been written
		var ev map[string]any
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("decode JSON event: %v (raw=%q)", err, stdout.String())
		}
		if ev["event"] != "progress" {
			t.Fatalf("expected event=progress, got %v", ev["event"])
		}
		if _, ok := ev["bytes"]; !ok {
			t.Fatal("missing bytes field")
		}
		if _, ok := ev["total"]; !ok {
			t.Fatal("missing total field")
		}
	}
}

func TestProgress_DoneJSONEmitsCompletionEvent(t *testing.T) {
	var stdout bytes.Buffer
	p := NewProgress(100, ProgressJSON, nil, &stdout)
	p.now = func() time.Time { return time.Unix(0, 0) }
	p.start = time.Unix(0, 0)

	p.Done("ulid-x", "http://h/v1/uploads/ns/ulid-x")
	if !strings.Contains(stdout.String(), `"event":"done"`) {
		t.Fatalf("expected done event, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"upload_id":"ulid-x"`) {
		t.Fatalf("expected upload_id in done event, got %q", stdout.String())
	}
}

func TestProgress_LineModeDoesNotUseANSI(t *testing.T) {
	var stderr bytes.Buffer
	p := NewProgress(1000, ProgressLine, &stderr, nil)
	clk := &fakeClock{t: time.Unix(0, 0), step: 1100 * time.Millisecond}
	p.now = clk.now
	p.start = time.Unix(0, 0)

	p.Add(500)
	p.Done("uid", "http://h")

	out := stderr.String()
	if strings.Contains(out, "\r") {
		t.Fatalf("ProgressLine must not emit \\r, got %q", out)
	}
	if !strings.Contains(out, "done in") {
		t.Fatalf("expected 'done in' line, got %q", out)
	}
}

func TestProgress_SilentWritesNothing(t *testing.T) {
	var stderr, stdout bytes.Buffer
	p := NewProgress(100, ProgressSilent, &stderr, &stdout)
	for i := 0; i < 10; i++ {
		p.Add(10)
	}
	p.Done("x", "y")
	if stderr.Len() != 0 || stdout.Len() != 0 {
		t.Fatalf("silent mode wrote: stderr=%q stdout=%q", stderr.String(), stdout.String())
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
	}
	for _, c := range cases {
		got := humanBytes(c.in)
		if got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
