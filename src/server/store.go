package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Info is the persisted sidecar metadata for an upload (.info file). The
// canonical offset on resume comes from os.Stat of the .partial file; we do
// NOT trust an "offset" field in the sidecar (mirroring tusd's behavior).
type Info struct {
	ID             string            `json:"id"`
	Namespace      string            `json:"namespace"`
	Size           int64             `json:"size"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	ExpiresAt      time.Time         `json:"expires_at"`
	CompletedAt    *time.Time        `json:"completed_at"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
}

// Store wraps the on-disk filesystem layout for ferry uploads.
//
// Layout:
//
//	<root>/<namespace>/<id>.partial   - in-progress binary
//	<root>/<namespace>/<id>           - completed binary (atomic-rename)
//	<root>/<namespace>/<id>.info      - sidecar JSON
//	<root>/<namespace>/.idem/<key>    - idempotency-key -> id mapping
type Store struct {
	root string
}

// NewStore constructs a Store rooted at root, creating the root directory
// (and any missing parents) if needed. Namespace subdirs are created
// lazily on first Create.
func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir data root: %w", err)
	}
	return &Store{root: root}, nil
}

// nsDir returns the namespace directory path.
func (s *Store) nsDir(namespace string) string {
	return filepath.Join(s.root, namespace)
}

func (s *Store) partialPath(namespace, id string) string {
	return filepath.Join(s.nsDir(namespace), id+".partial")
}

func (s *Store) completedPath(namespace, id string) string {
	return filepath.Join(s.nsDir(namespace), id)
}

func (s *Store) infoPath(namespace, id string) string {
	return filepath.Join(s.nsDir(namespace), id+".info")
}

func (s *Store) idemPath(namespace, key string) string {
	return filepath.Join(s.nsDir(namespace), ".idem", key)
}

// Create initializes a new upload: an empty .partial file, a sidecar .info,
// and (if idempotencyKey != "") an idem mapping. On failure of any step
// past the .partial creation, best-effort cleans up partially-written
// files so failed creates don't accumulate orphans.
func (s *Store) Create(info Info) error {
	dir := s.nsDir(info.Namespace)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir namespace: %w", err)
	}
	partial := s.partialPath(info.Namespace, info.ID)
	// Create empty .partial.
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create partial: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("close partial: %w", err)
	}
	if err := s.writeInfo(info); err != nil {
		_ = os.Remove(partial)
		_ = os.Remove(s.infoPath(info.Namespace, info.ID))
		return err
	}
	if info.IdempotencyKey != "" {
		if err := s.writeIdem(info.Namespace, info.IdempotencyKey, info.ID); err != nil {
			_ = os.Remove(partial)
			_ = os.Remove(s.infoPath(info.Namespace, info.ID))
			return err
		}
	}
	return nil
}

// writeInfo writes the sidecar atomically: write tmp, fsync, rename, fsync
// parent dir.
func (s *Store) writeInfo(info Info) error {
	dir := s.nsDir(info.Namespace)
	finalPath := s.infoPath(info.Namespace, info.ID)
	tmpPath := finalPath + ".tmp"

	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal info: %w", err)
	}
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open info tmp: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("write info tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync info tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close info tmp: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("rename info: %w", err)
	}
	return fsyncDir(dir)
}

// writeIdem records an idempotency-key -> upload-id mapping. Uses the
// same write-tmp + fsync + rename + fsync-dir dance as the sidecar so
// the mapping survives a crash if it survives the rename.
func (s *Store) writeIdem(namespace, key, id string) error {
	dir := filepath.Join(s.nsDir(namespace), ".idem")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir idem: %w", err)
	}
	final := s.idemPath(namespace, key)
	tmp := final + ".tmp"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open idem tmp: %w", err)
	}
	if _, err := f.Write([]byte(id)); err != nil {
		_ = f.Close()
		return fmt.Errorf("write idem tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync idem tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close idem tmp: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("rename idem: %w", err)
	}
	return fsyncDir(dir)
}

// LookupIdem returns the upload id previously recorded under key, or "" if
// none. Returns no error for "not found".
func (s *Store) LookupIdem(namespace, key string) (string, error) {
	b, err := os.ReadFile(s.idemPath(namespace, key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read idem: %w", err)
	}
	return string(b), nil
}

// LoadInfo reads the sidecar for an upload. Returns ErrNotFound if the .info
// is missing.
func (s *Store) LoadInfo(namespace, id string) (Info, error) {
	b, err := os.ReadFile(s.infoPath(namespace, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Info{}, ErrNotFound
		}
		return Info{}, fmt.Errorf("read info: %w", err)
	}
	var info Info
	if err := json.Unmarshal(b, &info); err != nil {
		return Info{}, fmt.Errorf("parse info: %w", err)
	}
	return info, nil
}

// CurrentOffset returns the on-disk size of the .partial (or completed) file
// as the canonical byte offset. Returns ErrNotFound if neither exists.
func (s *Store) CurrentOffset(namespace, id string) (int64, error) {
	if st, err := os.Stat(s.partialPath(namespace, id)); err == nil {
		return st.Size(), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("stat partial: %w", err)
	}
	if st, err := os.Stat(s.completedPath(namespace, id)); err == nil {
		return st.Size(), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("stat completed: %w", err)
	}
	return 0, ErrNotFound
}

// AppendChunk opens the .partial in append mode, copies up to limit bytes
// from src, fsyncs, and returns bytes written. err includes early src errors
// (e.g., context cancel mid-read); n reflects what made it to disk before
// the error.
func (s *Store) AppendChunk(namespace, id string, src io.Reader, limit int64) (int64, error) {
	path := s.partialPath(namespace, id)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("open partial: %w", err)
	}
	defer f.Close()

	limited := io.LimitReader(src, limit)
	n, copyErr := io.Copy(f, limited)
	// Always fsync what made it to disk, even on copy error, so the next
	// HEAD reflects reality. Treat fsync EIO as fatal: we surface the
	// error and let the handler return 500. Per fsyncgate, retrying is
	// unsafe.
	syncErr := f.Sync()
	if copyErr != nil {
		// Prefer the copy error since it's more actionable, but log the
		// fsync failure so we don't lose it silently.
		if syncErr != nil {
			// Combine into a single error for visibility upstream.
			return n, fmt.Errorf("io.Copy: %w (also fsync partial: %v)", copyErr, syncErr)
		}
		return n, copyErr
	}
	if syncErr != nil {
		return n, fmt.Errorf("fsync partial: %w", syncErr)
	}
	return n, nil
}

// Complete marks the upload as done: fsync the .partial, atomic-rename
// .partial -> id, fsync parent dir, then update the sidecar with
// completed_at.
func (s *Store) Complete(namespace, id string) error {
	dir := s.nsDir(namespace)
	partial := s.partialPath(namespace, id)
	final := s.completedPath(namespace, id)

	// fsync the data file before rename.
	f, err := os.OpenFile(partial, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open partial for fsync: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync partial: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close partial: %w", err)
	}
	if err := os.Rename(partial, final); err != nil {
		return fmt.Errorf("rename completed: %w", err)
	}
	if err := fsyncDir(dir); err != nil {
		return err
	}
	info, err := s.LoadInfo(namespace, id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	info.CompletedAt = &now
	return s.writeInfo(info)
}

// Delete removes the partial/completed binary, the sidecar, and the idem
// mapping (if any). Missing files are not an error - delete is idempotent.
func (s *Store) Delete(namespace, id string) error {
	info, infoErr := s.LoadInfo(namespace, id)
	if infoErr != nil && !errors.Is(infoErr, ErrNotFound) {
		return infoErr
	}
	for _, p := range []string{
		s.partialPath(namespace, id),
		s.completedPath(namespace, id),
		s.infoPath(namespace, id),
	} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	if infoErr == nil && info.IdempotencyKey != "" {
		if err := os.Remove(s.idemPath(namespace, info.IdempotencyKey)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove idem mapping: %w", err)
		}
	}
	return fsyncDir(s.nsDir(namespace))
}

// AvailableBytes returns the bytes free at the data root, via the
// platform-specific implementation in store_unix.go / store_windows.go.
func (s *Store) AvailableBytes() (int64, error) {
	return availableBytes(s.root)
}

// Truncate trims the .partial file back to size bytes. Used by checksum
// mismatch handling to roll a failed PATCH off disk so the next attempt
// resumes from the previous offset. fsyncs the file after the truncate so
// the rollback survives a crash; otherwise we could respond 460 to the
// client and then come back up still holding the bad bytes.
func (s *Store) Truncate(namespace, id string, size int64) error {
	path := s.partialPath(namespace, id)
	if err := os.Truncate(path, size); err != nil {
		return fmt.Errorf("truncate partial: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open partial after truncate: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync after truncate: %w", err)
	}
	return nil
}

// ListNamespaces enumerates the immediate subdirectories of the data root.
// Used by the GC sweeper to walk every namespace.
func (s *Store) ListNamespaces() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("readdir data root: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// ListUploads enumerates the upload ids in a namespace by scanning for
// *.info sidecars. Returns ids without the .info suffix. Skips the .idem
// directory (it has no .info entries by construction).
func (s *Store) ListUploads(namespace string) ([]string, error) {
	entries, err := os.ReadDir(s.nsDir(namespace))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("readdir namespace: %w", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".info") {
			continue
		}
		out = append(out, strings.TrimSuffix(name, ".info"))
	}
	return out, nil
}

// HasPartial reports whether a .partial exists for the upload. Returns
// (exists, err); a non-nil err means the answer is unknown (typically
// permission/IO error). Callers that can't tolerate "unknown" - e.g. the
// GC sweeper deciding whether to delete a sidecar - should err on the side
// of keeping the upload.
func (s *Store) HasPartial(namespace, id string) (bool, error) {
	_, err := os.Stat(s.partialPath(namespace, id))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("stat partial: %w", err)
}

// HasCompleted reports whether the completed (post-rename) file exists.
// Same (bool, error) semantics as HasPartial.
func (s *Store) HasCompleted(namespace, id string) (bool, error) {
	_, err := os.Stat(s.completedPath(namespace, id))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("stat completed: %w", err)
}

// ListIdemKeys enumerates idempotency-key entries in the namespace's .idem
// directory. Returns the basenames (the keys themselves). Skips .tmp
// half-written entries.
func (s *Store) ListIdemKeys(namespace string) ([]string, error) {
	dir := filepath.Join(s.nsDir(namespace), ".idem")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("readdir idem: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// RemoveIdem deletes the idempotency-key mapping. Missing is not an error.
func (s *Store) RemoveIdem(namespace, key string) error {
	if err := os.Remove(s.idemPath(namespace, key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove idem: %w", err)
	}
	return nil
}

// fsyncDir opens dir, fsyncs it, and closes. Required so that recent
// renames/creates within dir are durable.
func fsyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir for fsync: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync dir: %w", err)
	}
	return nil
}
