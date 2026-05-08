// Package version exposes the build version, set via ldflags at build time.
package version

// Version is the ferry release version. Overridden via -ldflags at build time.
var Version = "dev"
