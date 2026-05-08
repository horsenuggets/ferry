package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

const tusVersion = "1.0.0"

// Handler implements the tus subset over Store + Authenticator + Locker.
type Handler struct {
	store         *Store
	auth          *Authenticator
	locker        *Locker
	maxPatchBytes int64
	safetyMargin  int64
	completedTTL  time.Duration
	incompleteTTL time.Duration
	version       string // server version for /health
	logger        *slog.Logger
}

// HandlerConfig is the construction-time bundle for Handler.
type HandlerConfig struct {
	Store         *Store
	Auth          *Authenticator
	Locker        *Locker
	MaxPatchBytes int64
	SafetyMargin  int64
	CompletedTTL  time.Duration
	IncompleteTTL time.Duration
	Version       string
	Logger        *slog.Logger
}

// NewHandler wires a HandlerConfig into a Handler. Logger defaults to
// slog.Default if nil.
func NewHandler(cfg HandlerConfig) *Handler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		store:         cfg.Store,
		auth:          cfg.Auth,
		locker:        cfg.Locker,
		maxPatchBytes: cfg.MaxPatchBytes,
		safetyMargin:  cfg.SafetyMargin,
		completedTTL:  cfg.CompletedTTL,
		incompleteTTL: cfg.IncompleteTTL,
		version:       cfg.Version,
		logger:        logger,
	}
}

// Routes returns an http.Handler wired with /health and the tus endpoints.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", h.handleHealth)
	mux.HandleFunc(uploadsPrefix, h.handleUploads)
	return mux
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"version": h.version,
	})
}

// handleUploads is the tus dispatcher. It enforces Tus-Resumable on every
// request, then routes by method + presence of an id.
func (h *Handler) handleUploads(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Tus-Resumable", tusVersion)

	// Strict version check.
	if r.Header.Get("Tus-Resumable") != tusVersion {
		writeError(w, ErrUnsupportedVersion, "")
		return
	}

	namespace, id, ok := parseUploadPath(r.URL.Path)
	if !ok {
		writeError(w, ErrNotFound, "")
		return
	}

	if err := h.auth.Authorize(r, namespace); err != nil {
		writeError(w, err, "")
		return
	}

	switch {
	case r.Method == http.MethodPost && id == "":
		h.postUpload(w, r, namespace)
	case r.Method == http.MethodHead && id != "":
		h.headUpload(w, r, namespace, id)
	case r.Method == http.MethodPatch && id != "":
		h.patchUpload(w, r, namespace, id)
	case r.Method == http.MethodDelete && id != "":
		h.deleteUpload(w, r, namespace, id)
	default:
		w.Header().Set("Allow", "POST, HEAD, PATCH, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) postUpload(w http.ResponseWriter, r *http.Request, namespace string) {
	sizeStr := r.Header.Get("Upload-Length")
	if sizeStr == "" {
		writeError(w, ErrInvalidUploadLength, "")
		return
	}
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil || size < 0 {
		writeError(w, ErrInvalidUploadLength, "")
		return
	}

	// Idempotency-Key: if we've seen this key in this namespace, return the
	// existing upload's URL with 200 OK instead of creating a new one.
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey != "" {
		if existingID, err := h.store.LookupIdem(namespace, idemKey); err != nil {
			writeError(w, err, "")
			return
		} else if existingID != "" {
			w.Header().Set("Location", uploadLocation(namespace, existingID))
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	// Disk preflight. Doesn't catch concurrent overflow but rejects the
	// trivial "one upload bigger than free disk" case.
	available, err := h.store.AvailableBytes()
	if err != nil {
		h.logger.Error("statfs failed", "err", err)
		writeError(w, ErrInternal, "")
		return
	}
	if size > available-h.safetyMargin {
		writeError(w, ErrInsufficientStorage, "")
		return
	}

	now := time.Now().UTC()
	info := Info{
		ID:             newID(),
		Namespace:      namespace,
		Size:           size,
		Metadata:       parseMetadata(r.Header.Get("Upload-Metadata")),
		CreatedAt:      now,
		ExpiresAt:      now.Add(h.incompleteTTL),
		IdempotencyKey: idemKey,
	}
	if err := h.store.Create(info); err != nil {
		h.logger.Error("create upload failed", "err", err, "id", info.ID)
		writeError(w, ErrInternal, "")
		return
	}

	w.Header().Set("Location", uploadLocation(namespace, info.ID))
	w.Header().Set("Upload-Expires", info.ExpiresAt.Format(time.RFC1123))
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) headUpload(w http.ResponseWriter, _ *http.Request, namespace, id string) {
	info, err := h.store.LoadInfo(namespace, id)
	if err != nil {
		writeError(w, err, "")
		return
	}
	offset, err := h.store.CurrentOffset(namespace, id)
	if err != nil {
		writeError(w, err, "")
		return
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(offset, 10))
	w.Header().Set("Upload-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) patchUpload(w http.ResponseWriter, r *http.Request, namespace, id string) {
	if r.Header.Get("Content-Type") != "application/offset+octet-stream" {
		writeError(w, ErrInvalidContentType, "")
		return
	}
	offsetStr := r.Header.Get("Upload-Offset")
	if offsetStr == "" {
		writeError(w, ErrInvalidOffset, "")
		return
	}
	clientOffset, err := strconv.ParseInt(offsetStr, 10, 64)
	if err != nil || clientOffset < 0 {
		writeError(w, ErrInvalidOffset, "")
		return
	}

	info, err := h.store.LoadInfo(namespace, id)
	if err != nil {
		writeError(w, err, "")
		return
	}
	if info.CompletedAt != nil {
		// Upload is already done; cannot extend.
		writeError(w, ErrMismatchOffset, "")
		return
	}

	// Body cap: we never accept more bytes than max_patch_bytes per PATCH.
	// A declared Content-Length over the cap is rejected up front.
	if r.ContentLength > h.maxPatchBytes {
		writeError(w, ErrSizeExceeded, "")
		return
	}

	// Cooperative-cancel lock. requestRelease cancels our request context
	// so the in-flight io.Copy unblocks; whatever made it to disk stays.
	ctx, cancel := contextWithCause(r.Context())
	defer cancel(nil)
	key := namespace + "/" + id
	release, err := h.locker.Acquire(ctx, key, func() {
		cancel(errors.New("upload interrupted by another request"))
	})
	if err != nil {
		writeError(w, err, "")
		return
	}
	defer release()

	// Re-stat under the lock to get the canonical offset.
	currentOffset, err := h.store.CurrentOffset(namespace, id)
	if err != nil {
		writeError(w, err, "")
		return
	}
	if clientOffset != currentOffset {
		writeError(w, ErrMismatchOffset, "")
		return
	}

	// remaining is how many bytes we'll actually accept on this request.
	remaining := info.Size - currentOffset
	limit := h.maxPatchBytes
	if remaining < limit {
		limit = remaining
	}
	// We also cap by content-length+1 to detect overruns. Cap by limit+1
	// gives io.Copy room to read one extra byte and trigger an error if
	// the body is over-long.
	body := http.MaxBytesReader(w, r.Body, limit+1)

	// Wrap body in a context-aware reader so cooperative cancel actually
	// stops the io.Copy.
	cr := &ctxReader{ctx: ctx, r: body}

	n, copyErr := h.store.AppendChunk(namespace, id, cr, limit+1)
	newOffset := currentOffset + n

	// If the body was trimmed/canceled mid-flight (peer hung up, we got
	// ousted by another acquirer), persist what we have and return - the
	// next request resumes from on-disk size. We do NOT 500 in that case.
	if copyErr != nil && !errors.Is(copyErr, io.EOF) && !errors.Is(copyErr, context.Canceled) {
		// Body too long?
		if n > limit {
			writeError(w, ErrSizeExceeded, "")
			return
		}
		// Real error - the partial bytes are still on disk, but we tell
		// the client the request didn't complete. They'll HEAD to recover.
		h.logger.Warn("patch interrupted",
			"namespace", namespace, "id", id, "wrote", n, "err", copyErr)
		// If the underlying error was a max-bytes overrun, that's 413.
		if isMaxBytesError(copyErr) {
			writeError(w, ErrSizeExceeded, "")
			return
		}
		// Otherwise, it's a connection/cancel - close without status. The
		// client retries via HEAD.
		w.Header().Set("Upload-Offset", strconv.FormatInt(newOffset, 10))
		// We still send 204 with the new offset so well-behaved clients
		// keep going. (tusd does similar.)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if n > limit {
		writeError(w, ErrSizeExceeded, "")
		return
	}

	// On full completion, atomically rename + mark sidecar.
	if newOffset == info.Size {
		if err := h.store.Complete(namespace, id); err != nil {
			h.logger.Error("complete failed",
				"namespace", namespace, "id", id, "err", err)
			writeError(w, ErrInternal, "")
			return
		}
	}

	w.Header().Set("Upload-Offset", strconv.FormatInt(newOffset, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteUpload(w http.ResponseWriter, _ *http.Request, namespace, id string) {
	if _, err := h.store.LoadInfo(namespace, id); err != nil {
		writeError(w, err, "")
		return
	}
	if err := h.store.Delete(namespace, id); err != nil {
		h.logger.Error("delete failed",
			"namespace", namespace, "id", id, "err", err)
		writeError(w, ErrInternal, "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeError sets the appropriate status code for the protocol error and
// writes a tiny text body for human debugging.
func writeError(w http.ResponseWriter, err error, msg string) {
	code := statusFor(err)
	if msg == "" {
		msg = err.Error()
	}
	http.Error(w, msg, code)
}
