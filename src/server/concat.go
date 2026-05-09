package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Concat extension constants. We implement the tus 1.0.0 "concatenation" and
// "concatenation-unfinished" extensions: clients POST several partials,
// upload bytes into each independently, then POST a "final" that names the
// partials by Location URL. The server stitches the partials into a single
// completed upload.
//
// Wire format:
//
//	Upload-Concat: partial
//	Upload-Concat: final;<url1> <url2> <url3>
//
// Whitespace between the URLs is one or more space characters (tus spec).
// We accept tabs and multiple spaces too for robustness.
const (
	concatPartial = "partial"
	concatFinal   = "final"
)

// ParsedConcat is the result of parsing an Upload-Concat header.
type ParsedConcat struct {
	// Kind is "" (no header), "partial", or "final".
	Kind string
	// Sources are the partial location URL paths (final only). Each entry is
	// the path component, e.g. "/v1/uploads/alpha/01H..." (we strip
	// scheme://host if the client sent absolute URLs).
	Sources []string
}

// errInvalidConcatHeader is returned for malformed Upload-Concat values.
// Surfaced to the handler as 400.
var errInvalidConcatHeader = errors.New("invalid Upload-Concat header")

// errConcatSourceNotCompleted means a referenced partial has no completed_at
// timestamp yet. Surfaced to the handler as 400 with a hint body.
var errConcatSourceNotCompleted = errors.New("concat source not completed")

// errConcatSourceWrongNamespace means a referenced partial lives in a
// different namespace from the final's POST. We refuse cross-namespace
// stitching to keep auth scopes meaningful.
var errConcatSourceWrongNamespace = errors.New("concat source in wrong namespace")

// errConcatSourceNotPartial means a referenced upload exists but isn't marked
// as Concat=="partial". Either it's a regular upload or another final - we
// don't allow either as input to a new final.
var errConcatSourceNotPartial = errors.New("concat source is not a partial upload")

// parseUploadConcat parses an Upload-Concat header value. Returns an empty
// ParsedConcat (Kind=="") for missing/empty input. Returns
// errInvalidConcatHeader for any syntactic problem.
func parseUploadConcat(v string) (ParsedConcat, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return ParsedConcat{}, nil
	}
	// Split on the first ';' so we can handle "final;url1 url2".
	semi := strings.IndexByte(v, ';')
	kind := v
	rest := ""
	if semi >= 0 {
		kind = strings.TrimSpace(v[:semi])
		rest = strings.TrimSpace(v[semi+1:])
	}
	switch kind {
	case concatPartial:
		if rest != "" {
			return ParsedConcat{}, errInvalidConcatHeader
		}
		return ParsedConcat{Kind: concatPartial}, nil
	case concatFinal:
		if rest == "" {
			return ParsedConcat{}, errInvalidConcatHeader
		}
		// Accept any whitespace as separator.
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return ParsedConcat{}, errInvalidConcatHeader
		}
		paths := make([]string, 0, len(fields))
		for _, f := range fields {
			p, ok := concatSourcePath(f)
			if !ok {
				return ParsedConcat{}, errInvalidConcatHeader
			}
			paths = append(paths, p)
		}
		return ParsedConcat{Kind: concatFinal, Sources: paths}, nil
	default:
		return ParsedConcat{}, errInvalidConcatHeader
	}
}

// concatSourcePath normalizes a single Upload-Concat source URL to its path
// component and validates the shape. Accepts absolute URLs ("http://h/v1/...")
// and bare paths ("/v1/uploads/ns/id"); rejects empty or path-traversing
// values. Returns (path, true) on success.
func concatSourcePath(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	// Strip scheme://host if present. We don't validate the scheme - any
	// scheme is fine as long as the path component looks like an upload.
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		j := strings.IndexByte(rest, '/')
		if j < 0 {
			return "", false
		}
		s = rest[j:]
	}
	if !strings.HasPrefix(s, uploadsPrefix) {
		return "", false
	}
	// Strip query/fragment if any.
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	return s, true
}

// resolveConcatSources turns the parsed source paths into (namespace, id)
// pairs and validates them against the server's expectations:
//
//   - the path must parse as /v1/uploads/<namespace>/<id>
//   - every source must share the same namespace as the final's POST
//   - every source must exist, be marked Concat=="partial", and be completed
//
// Returns the per-source Info structs in the same order as input, plus the
// total stitched size.
func resolveConcatSources(store *Store, finalNS string, sources []string) ([]Info, int64, error) {
	out := make([]Info, 0, len(sources))
	var total int64
	for _, p := range sources {
		ns, id, ok := parseUploadPath(p)
		if !ok || id == "" {
			return nil, 0, errInvalidConcatHeader
		}
		if ns != finalNS {
			return nil, 0, errConcatSourceWrongNamespace
		}
		info, err := store.LoadInfo(ns, id)
		if err != nil {
			return nil, 0, err
		}
		if info.Concat != concatPartial {
			return nil, 0, errConcatSourceNotPartial
		}
		if info.CompletedAt == nil {
			return nil, 0, errConcatSourceNotCompleted
		}
		// The completed binary lives at completedPath; refuse if it's gone.
		st, err := os.Stat(store.CompletedPath(ns, id))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, 0, ErrNotFound
			}
			return nil, 0, fmt.Errorf("stat partial source: %w", err)
		}
		// Trust on-disk size, not the declared Info.Size. They should be
		// equal, but stat is the canonical record.
		total += st.Size()
		// Update Info.Size to reflect the on-disk size we'll actually copy.
		info.Size = st.Size()
		out = append(out, info)
	}
	return out, total, nil
}

// stitchPartials opens each source, copies it into a freshly-created
// .partial for the final, fsyncs, atomic-renames to the completed path, and
// fsyncs the parent directory. Returns the number of bytes written. The
// final's sidecar is written by the caller, since it owns the Info struct.
func stitchPartials(ctx context.Context, store *Store, finalNS, finalID string, sources []Info) (int64, error) {
	partial := store.PartialPath(finalNS, finalID)
	final := store.CompletedPath(finalNS, finalID)
	dir := store.NamespaceDir(finalNS)

	// O_EXCL so a colliding ULID can't silently clobber an existing file.
	out, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return 0, fmt.Errorf("create final partial: %w", err)
	}
	cleanup := func() {
		_ = out.Close()
		_ = os.Remove(partial)
	}

	var total int64
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			cleanup()
			return total, err
		}
		in, err := os.Open(store.CompletedPath(src.Namespace, src.ID))
		if err != nil {
			cleanup()
			return total, fmt.Errorf("open source %s: %w", src.ID, err)
		}
		n, copyErr := io.Copy(out, in)
		_ = in.Close()
		total += n
		if copyErr != nil {
			cleanup()
			return total, fmt.Errorf("copy source %s: %w", src.ID, copyErr)
		}
	}

	if err := out.Sync(); err != nil {
		cleanup()
		return total, fmt.Errorf("fsync final partial: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(partial)
		return total, fmt.Errorf("close final partial: %w", err)
	}
	if err := os.Rename(partial, final); err != nil {
		_ = os.Remove(partial)
		return total, fmt.Errorf("rename final: %w", err)
	}
	if err := store.FsyncDir(dir); err != nil {
		return total, err
	}
	return total, nil
}

// markConcatConsumed stamps ConcatConsumedAt on each source's sidecar so the
// GC sweeper can reap them after retention. Best-effort: a failure here does
// not invalidate the final; we log and continue.
func markConcatConsumed(store *Store, sources []Info, now time.Time) error {
	var firstErr error
	for _, src := range sources {
		// Reload fresh in case another writer touched it between resolve
		// and now (e.g. a duplicate concurrent final referencing the same
		// partial).
		fresh, err := store.LoadInfo(src.Namespace, src.ID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if fresh.ConcatConsumedAt != nil {
			continue
		}
		t := now
		fresh.ConcatConsumedAt = &t
		if err := store.WriteInfo(fresh); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
