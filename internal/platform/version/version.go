// Package version stores the build version.
package version

// Version is set at build time:
// go build -ldflags "-X babki.my/babki/internal/platform/version.Version=v0.1.0"
var Version = "dev"
