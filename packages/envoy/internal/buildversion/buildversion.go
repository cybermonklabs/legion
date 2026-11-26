// packages/envoy/internal/buildversion/buildversion.go

// Package buildversion names the build a binary was made from: the release a host binary
// (agent-secrets, agent-secrets-helper) was built as, for `--version` and the helper's startup
// line, and the commit every program that connects to NATS names in its connection name
// (ConnectionBuild), which legacy-subjects-gate.sh reads from the hub's census of connections.
// cmd/dispatch also stores it in the Legion facts consumer's record of the build that last read
// each project whole (processBuild), where it is the build of any binary no release stamped a
// commit into.
package buildversion

import (
	"regexp"
	"runtime/debug"
)

// tag is the release tag, stamped only by the release_build job of
// .github/workflows/release-envoy-listener.yaml:
//
//	-ldflags "-X github.com/sjawhar/envoy/internal/buildversion.tag=legion-envoy-vX.Y.Z"
//
// Every other build leaves it empty.
var tag string

// commit is the legion commit the binary was built from, stamped by the image build
// (packages/envoy/docker/Dockerfile passes its LEGION_COMMIT build argument):
//
//	-ldflags "-X github.com/sjawhar/envoy/internal/buildversion.commit=<commit>"
//
// Every other build leaves it empty.
var commit string

// fullCommit is a whole commit id, the only form a connection name carries.
var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ConnectionBuild is the build a NATS connection of this binary names after its name:
// legion@<commit>, the stamped commit, or else the commit Go recorded when it built from a checkout
// with no change (vcs.revision, vcs.modified false); "" for any other build, a test binary and a
// build from a changed checkout among them, which names no build rather than a commit it does not
// hold.
func ConnectionBuild() string {
	if fullCommit.MatchString(commit) {
		return "legion@" + commit
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
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
	if modified != "false" || !fullCommit.MatchString(revision) {
		return ""
	}
	return "legion@" + revision
}

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
