package server

import (
	"strings"
)

// uploadsPrefix is the path prefix for all tus endpoints.
const uploadsPrefix = "/v1/uploads/"

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
	if len(parts) == 1 {
		return parts[0], "", true
	}
	if parts[1] == "" {
		return parts[0], "", true
	}
	// Disallow further nesting: <namespace>/<id> is the only allowed shape.
	if strings.Contains(parts[1], "/") {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// uploadLocation builds the public Location URL path for a given upload.
func uploadLocation(namespace, id string) string {
	return uploadsPrefix + namespace + "/" + id
}
