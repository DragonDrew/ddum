// Package buildinfo carries the version stamped in at build time:
//
//	go build -ldflags "-X github.com/DragonDrew/ddum/internal/buildinfo.Version=0.1.0"
package buildinfo

// Version is the software version; "dev" for unstamped builds.
var Version = "dev"
