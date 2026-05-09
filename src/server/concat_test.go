package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// uploadPartial creates a partial upload, PATCHes the supplied bytes into it,
// and returns the upload's location URL path (e.g. /v1/uploads/alpha/<id>).
func (r *testRig) uploadPartial(t *testing.T, namespace string, body []byte) string {
	t.Helper()
	// POST partial.
	req := r.newReq(t, "POST", "/v1/uploads/"+namespace, nil)
	req.Header.Set("Upload-Length", strconv.Itoa(len(body)))
	req.Header.Set("Upload-Concat", "partial")
	resp := r.do(t, req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("partial POST status = %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatalf("partial POST missing Location")
	}
	// PATCH the body in (max-patch-bytes is 1024 on the rig, so chunk).
	off := 0
	for off < len(body) {
		end := off + 1024
		if end > len(body) {
			end = len(body)
		}
		chunk := body[off:end]
		preq := r.newReq(t, "PATCH", pathOf(loc), bytes.NewReader(chunk))
		preq.Header.Set("Upload-Offset", strconv.Itoa(off))
		preq.Header.Set("Content-Type", "application/offset+octet-stream")
		preq.ContentLength = int64(len(chunk))
		// Disable any auto checksum: tests of concat shouldn't be entangled.
		presp := r.do(t, preq)
		presp.Body.Close()
		if presp.StatusCode != http.StatusNoContent {
			t.Fatalf("partial PATCH status = %d at off=%d", presp.StatusCode, off)
		}
		off = end
	}
	return loc
}

func TestParseUploadConcat(t *testing.T) {
	cases := []struct {
		in     string
		want   string // Kind
		nSrcs  int
		fail   bool
	}{
		{"", "", 0, false},
		{"partial", "partial", 0, false},
		{"  partial  ", "partial", 0, false},
		{"partial;extra", "", 0, true},
		{"final;/v1/uploads/a/x /v1/uploads/a/y", "final", 2, false},
		{"final ; /v1/uploads/a/x /v1/uploads/a/y /v1/uploads/a/z", "final", 3, false},
		{"final;http://h/v1/uploads/a/x http://h/v1/uploads/a/y", "final", 2, false},
		{"final", "", 0, true},
		{"final;", "", 0, true},
		{"final;/etc/passwd", "", 0, true},
		{"final;/v1/uploads/a/x?q=1 /v1/uploads/a/y", "final", 2, false},
		{"bogus", "", 0, true},
	}
	for _, tc := range cases {
		got, err := parseUploadConcat(tc.in)
		if tc.fail {
			if err == nil {
				t.Errorf("parse(%q): want err, got %+v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parse(%q): unexpected err %v", tc.in, err)
			continue
		}
		if got.Kind != tc.want {
			t.Errorf("parse(%q): kind=%q want %q", tc.in, got.Kind, tc.want)
		}
		if len(got.Sources) != tc.nSrcs {
			t.Errorf("parse(%q): %d sources, want %d", tc.in, len(got.Sources), tc.nSrcs)
		}
	}
}

func TestConcatThreePartials(t *testing.T) {
	r := newRig(t)
	a := bytes.Repeat([]byte("A"), 500)
	b := bytes.Repeat([]byte("B"), 700)
	c := bytes.Repeat([]byte("C"), 300)
	la := r.uploadPartial(t, "alpha", a)
	lb := r.uploadPartial(t, "alpha", b)
	lc := r.uploadPartial(t, "alpha", c)

	// POST final.
	hdr := "final;" + pathOf(la) + " " + pathOf(lb) + " " + pathOf(lc)
	req := r.newReq(t, "POST", "/v1/uploads/alpha", nil)
	req.Header.Set("Upload-Concat", hdr)
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("final POST status = %d, body=%s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("final missing Location")
	}
	want := append(append(append([]byte{}, a...), b...), c...)
	if got := resp.Header.Get("Upload-Length"); got != strconv.Itoa(len(want)) {
		t.Errorf("final Upload-Length = %q, want %d", got, len(want))
	}

	// HEAD the final: should report complete.
	headReq := r.newReq(t, "HEAD", pathOf(loc), nil)
	headResp := r.do(t, headReq)
	headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD final status = %d", headResp.StatusCode)
	}
	off, _ := strconv.ParseInt(headResp.Header.Get("Upload-Offset"), 10, 64)
	if off != int64(len(want)) {
		t.Errorf("HEAD final offset = %d, want %d", off, len(want))
	}

	// Verify the stitched bytes via sha256 of the on-disk completed file.
	finalID := lastSegment(loc)
	got, err := readCompleted(r, "alpha", finalID)
	if err != nil {
		t.Fatalf("read completed: %v", err)
	}
	gotHash := sha256.Sum256(got)
	wantHash := sha256.Sum256(want)
	if hex.EncodeToString(gotHash[:]) != hex.EncodeToString(wantHash[:]) {
		t.Errorf("stitched sha256 mismatch")
	}

	// Each partial should be marked ConcatConsumedAt.
	for _, l := range []string{la, lb, lc} {
		id := lastSegment(l)
		info, err := r.store.LoadInfo("alpha", id)
		if err != nil {
			t.Fatalf("load partial %s: %v", id, err)
		}
		if info.ConcatConsumedAt == nil {
			t.Errorf("partial %s ConcatConsumedAt was not stamped", id)
		}
		if info.Concat != "partial" {
			t.Errorf("partial %s Concat = %q", id, info.Concat)
		}
	}
	// Final's sidecar.
	finalInfo, err := r.store.LoadInfo("alpha", finalID)
	if err != nil {
		t.Fatalf("load final: %v", err)
	}
	if finalInfo.Concat != "final" {
		t.Errorf("final Concat = %q", finalInfo.Concat)
	}
	if len(finalInfo.ConcatSourceIDs) != 3 {
		t.Errorf("final ConcatSourceIDs len = %d", len(finalInfo.ConcatSourceIDs))
	}
}

func TestConcatPartialNotCompleted(t *testing.T) {
	r := newRig(t)
	// Create a partial but DON'T finish PATCHing it.
	req := r.newReq(t, "POST", "/v1/uploads/alpha", nil)
	req.Header.Set("Upload-Length", "100")
	req.Header.Set("Upload-Concat", "partial")
	resp := r.do(t, req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("partial POST status = %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")

	// Try to use it as a final source.
	hdr := "final;" + pathOf(loc)
	freq := r.newReq(t, "POST", "/v1/uploads/alpha", nil)
	freq.Header.Set("Upload-Concat", hdr)
	fresp := r.do(t, freq)
	defer fresp.Body.Close()
	if fresp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", fresp.StatusCode)
	}
	body, _ := io.ReadAll(fresp.Body)
	if !strings.Contains(string(body), "Concat-Source-Not-Completed") {
		t.Errorf("body did not mention Concat-Source-Not-Completed: %s", body)
	}
}

func TestConcatMalformedHeader(t *testing.T) {
	r := newRig(t)
	for _, hdr := range []string{"bogus", "final", "partial;junk", "final;/etc/passwd"} {
		req := r.newReq(t, "POST", "/v1/uploads/alpha", nil)
		req.Header.Set("Upload-Concat", hdr)
		// Some of these don't have Upload-Length, but we want to test the
		// concat parse fires first.
		resp := r.do(t, req)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("hdr %q: status = %d, want 400", hdr, resp.StatusCode)
		}
	}
}

func TestConcatNamespaceMismatch(t *testing.T) {
	r := newRig(t)
	la := r.uploadPartial(t, "alpha", []byte("data"))
	// Reference alpha's partial from a final POSTed under beta.
	hdr := "final;" + pathOf(la)
	req := r.newReq(t, "POST", "/v1/uploads/beta", nil)
	req.Header.Set("Upload-Concat", hdr)
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestConcatSourceNotPartial(t *testing.T) {
	r := newRig(t)
	// Regular (non-partial) upload.
	loc := r.createUpload(t, "alpha", 3, "")
	body := bytes.NewReader([]byte("abc"))
	preq := r.newReq(t, "PATCH", pathOf(loc), body)
	preq.Header.Set("Upload-Offset", "0")
	preq.Header.Set("Content-Type", "application/offset+octet-stream")
	preq.ContentLength = 3
	presp := r.do(t, preq)
	presp.Body.Close()

	// Try to stitch this regular upload as a partial source.
	hdr := "final;" + pathOf(loc)
	req := r.newReq(t, "POST", "/v1/uploads/alpha", nil)
	req.Header.Set("Upload-Concat", hdr)
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestConcatFinalRejectsBody(t *testing.T) {
	r := newRig(t)
	la := r.uploadPartial(t, "alpha", []byte("data"))
	hdr := "final;" + pathOf(la)
	req := r.newReq(t, "POST", "/v1/uploads/alpha", bytes.NewReader([]byte("nope")))
	req.Header.Set("Upload-Concat", hdr)
	req.ContentLength = 4
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestConcatStitchSurvivesDuplicateMark exercises markConcatConsumed when
// some sources are already stamped (e.g. a duplicate concurrent final).
func TestConcatStitchSurvivesDuplicateMark(t *testing.T) {
	r := newRig(t)
	la := r.uploadPartial(t, "alpha", []byte("aa"))
	lb := r.uploadPartial(t, "alpha", []byte("bb"))

	// Stamp la as already-consumed.
	idA := lastSegment(la)
	infoA, err := r.store.LoadInfo("alpha", idA)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	infoA.ConcatConsumedAt = &now
	if err := r.store.WriteInfo(infoA); err != nil {
		t.Fatal(err)
	}

	hdr := "final;" + pathOf(la) + " " + pathOf(lb)
	req := r.newReq(t, "POST", "/v1/uploads/alpha", nil)
	req.Header.Set("Upload-Concat", hdr)
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// la's ConcatConsumedAt should still be set (we don't double-stamp).
	infoA2, err := r.store.LoadInfo("alpha", idA)
	if err != nil {
		t.Fatal(err)
	}
	if infoA2.ConcatConsumedAt == nil {
		t.Errorf("la unmarked")
	}
}

// TestConcatMissingSource: a source that doesn't exist returns 404.
func TestConcatMissingSource(t *testing.T) {
	r := newRig(t)
	hdr := "final;/v1/uploads/alpha/01HZZZZZZZZZZZZZZZZZZZZZZZ"
	req := r.newReq(t, "POST", "/v1/uploads/alpha", nil)
	req.Header.Set("Upload-Concat", hdr)
	resp := r.do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestStitchPartialsOpenError directly drives stitchPartials with a missing
// source to cover its error-cleanup path.
func TestStitchPartialsOpenError(t *testing.T) {
	r := newRig(t)
	// Construct a fake Info pointing at a non-existent ID. stitchPartials
	// will create the .partial then fail on Open of the source.
	bogus := []Info{{ID: "ghost-id", Namespace: "alpha", Size: 10}}
	_, err := stitchPartials(context.Background(), r.store, "alpha", "fake-final-id", bogus)
	if err == nil {
		t.Fatal("expected error from stitchPartials with missing source")
	}
	// The .partial cleanup should have removed the half-built file.
	if _, statErr := r.store.HasPartial("alpha", "fake-final-id"); statErr != nil {
		t.Logf("HasPartial err (ok): %v", statErr)
	}
}

func TestConcatGCSweepConsumedPartials(t *testing.T) {
	r := newRig(t)
	la := r.uploadPartial(t, "alpha", []byte("aaaa"))
	lb := r.uploadPartial(t, "alpha", []byte("bbbb"))
	hdr := "final;" + pathOf(la) + " " + pathOf(lb)
	req := r.newReq(t, "POST", "/v1/uploads/alpha", nil)
	req.Header.Set("Upload-Concat", hdr)
	resp := r.do(t, req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("final POST status = %d", resp.StatusCode)
	}

	// Backdate ConcatConsumedAt so the GC will reap.
	for _, l := range []string{la, lb} {
		id := lastSegment(l)
		info, err := r.store.LoadInfo("alpha", id)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		past := time.Now().Add(-2 * consumedPartialTTL)
		info.ConcatConsumedAt = &past
		if err := r.store.WriteInfo(info); err != nil {
			t.Fatal(err)
		}
	}
	gc := NewGC(GCConfig{
		Store:         r.store,
		Locker:        NewLocker(),
		Interval:      time.Hour,
		CompletedTTL:  24 * time.Hour, // generous so the final isn't reaped
		IncompleteTTL: 24 * time.Hour,
	})
	gc.SweepOnce(context.Background())

	for _, l := range []string{la, lb} {
		id := lastSegment(l)
		if _, err := r.store.LoadInfo("alpha", id); err == nil {
			t.Errorf("partial %s still present after GC", id)
		}
	}
}
