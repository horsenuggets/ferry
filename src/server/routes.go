package server

import (
	"strings"
)

// uploadsPrefix is the path prefix for all tus endpoints.
const uploadsPrefix = "/v1/uploads/"

// maxNamespaceLen / maxIDLen bound the path components to prevent
// pathological values from blowing up filesystem entry sizes or logs.
const (
	maxNamespaceLen = 64
	maxIDLen        = 128
)

// validNamespace reports whether s is a safe namespace identifier: ASCII
// alphanumerics, dash, and underscore, length 1..maxNamespaceLen. This
// rules out path traversal (".."), separators, NUL bytes, and any other
// filesystem-meaningful values flowing into filepath.Join.
func validNamespace(s string) bool {
	if s == "" || len(s) > maxNamespaceLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// validID reports whether s is a safe upload id. Same allow-list as
// namespace, but with the longer limit to fit a 26-char ULID with room for
// future schemes.
func validID(s string) bool {
	if s == "" || len(s) > maxIDLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// validIdemKey is the same allow-list, but allows '.' too since some clients
// use dotted namespaces (e.g., "post.user.123") in their keys. Bounds the
// length so a malicious header can't blow up directory entries.
func validIdemKey(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// parseUploadPath extracts (namespace, id) from /v1/uploads/<namespace>[/<id>].
// id is "" when the path is the namespace-only collection path.
// ok is false if the path is not under uploadsPrefix or the namespace is empty.
func parseUploadPath(path string) (namespace, id string, ok bool) {
	if !strings.HasPrefix(path, uploadsPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, uploadsPrefix)
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return "", "", false
	}
	parts := strings.SplitN(rest, "/", 2)
	if parts[0] == "" {
		return "", "", false
	}
	if !validNamespace(parts[0]) {
		return "", "", false
	}
	if len(parts) == 1 || parts[1] == "" {
		return parts[0], "", true
	}
	// Disallow further nesting: <namespace>/<id> is the only allowed shape.
	if strings.Contains(parts[1], "/") {
		return "", "", false
	}
	if !validID(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// uploadLocation builds the public Location URL path for a given upload.
func uploadLocation(namespace, id string) string {
	return uploadsPrefix + namespace + "/" + id
}
