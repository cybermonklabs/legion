// packages/envoy/internal/buildversion/buildversion.go

// Package buildversion names the release a binary was built as. The host binaries
// (agent-secrets, agent-secrets-helper) report it for `--version` and the helper's startup
// line. cmd/dispatch stores it in the Legion facts consumer's record of the build that last
// read each project whole (processBuild), where it is the build of any binary no release
// stamped a commit into.
package buildversion

import "runtime/debug"

// tag is the release tag, stamped only by the release_build job of
// .github/workflows/release-envoy-listener.yaml:
//
//	-ldflags "-X github.com/sjawhar/envoy/internal/buildversion.tag=legion-envoy-vX.Y.Z"
//
// Every other build leaves it empty.
var tag string

// String is the stamped release tag, or "devel" for any other build, followed by the commit and
// dirty flag Go recorded when it built from a checkout (a release builds with -buildvcs=false, so
// it records none).
func String() string {
	if tag != "" {
		return tag
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision == "" {
		return "devel"
	}
	if modified == "true" {
		return "devel (" + revision + ", modified)"
	}
	return "devel (" + revision + ")"
}
