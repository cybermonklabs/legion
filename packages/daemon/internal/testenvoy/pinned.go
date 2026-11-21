package testenvoy

import (
	"debug/buildinfo"
	"fmt"
	"os"
	"testing"
)

// The listener release production runs, which the rig of this change's checks runs in place of this
// checkout's (the verification's preamble: "built from the commit of the listener release that
// production pins", because R4's fixes go in the plugin and must work with that listener).
//
// Read on 2026-11-16 from the deployment repository's origin/main (a0ad7844), envoy-cluster/
// Pulumi.prod.yaml's `envoyImage` setting, the GKE listener every issue pod's
// session registers with. `crane config` of that digest reads the label
// org.opencontainers.image.revision = PinnedRevision, and the file's own comment names the
// release. A move of that pin is a move of these three constants, read again the same way.
const (
	PinnedImage    = "ghcr.io/cybermonklabs/legion/envoy@sha256:f50121daab35c11668ff9a5db73eab57a865369ab71e339d384f9b86d195c009"
	PinnedRevision = "8b8fac664c42637405e3b2d55b4b717a4cc91393"
	PinnedRelease  = "legion-envoy-v29.51.0"
)

// PinnedBuild is how scripts/e2e/lib/live-rig.sh builds the binary, as the image's own Dockerfile
// (packages/envoy/docker/Dockerfile at PinnedRevision) builds it: in a detached worktree of
// PinnedRevision, the Go contracts regenerated from their schemas first (`bun run gen:go` in
// packages/contracts, which the image runs and which must leave the tree unmodified, since it
// writes the committed file again), then this command in packages/envoy.
const PinnedBuild = "CGO_ENABLED=0 go build -o <binary> ./cmd/listener"

// PinnedListenerEnv names the binary that build wrote, which the script hands the driver.
const PinnedListenerEnv = "LEGION_E2E_ENVOY_LISTENER"

// listenerPackage is the main package a listener binary is built from.
const listenerPackage = "github.com/sjawhar/envoy/cmd/listener"

// CheckPinned refuses a binary that is not the listener built from PinnedRevision with an
// unmodified tree, read from the build information Go stamps into it (vcs.revision and
// vcs.modified, which a build in a git worktree carries). A binary with no such information says
// nothing about where it came from, and is refused as well.
func CheckPinned(path string) error {
	return checkBuiltFrom(path, PinnedRevision, "the commit production's listener image "+PinnedImage+" carries")
}

// checkBuiltFrom refuses a binary that is not the listener built from revision, which what names,
// with an unmodified tree.
func checkBuiltFrom(path, revision, what string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s carries no Go build information, so nothing says it was built from %s: %w", path, revision, err)
	}
	if info.Path != listenerPackage {
		return fmt.Errorf("%s is %s, not the listener (%s) built from %s", path, info.Path, listenerPackage, revision)
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	switch built := settings["vcs.revision"]; {
	case built == "":
		return fmt.Errorf("%s names no vcs.revision, so nothing says it was built from %s", path, revision)
	case built != revision:
		return fmt.Errorf("%s was built from %s, not from %s, %s", path, built, revision, what)
	case settings["vcs.modified"] != "false":
		return fmt.Errorf("%s was built from %s with its tree modified (vcs.modified=%q), so it is not that commit's listener", path, revision, settings["vcs.modified"])
	}
	return nil
}

// The rig's one seam for a check that tests this checkout's own listener rather than production's.
// With ListenerFromCheckoutEnv set to 1, scripts/e2e/lib/live-rig.sh builds the listener from the
// checkout's own commit, as it builds the pinned one, and names that commit in
// ListenerRevisionEnv; RigBinary then takes a listener of that commit and no other. Unset, the rig
// takes the pinned build alone. Check w2 sets it (scripts/e2e/cut-turn-note.sh): it tests this
// change's own plugin and listener together, and the pinned listener predates the one that lets a
// controller register its notice lane. Every other check runs the pinned listener.
const (
	ListenerFromCheckoutEnv = "LEGION_E2E_LISTENER_FROM_CHECKOUT"
	ListenerRevisionEnv     = "LEGION_E2E_LISTENER_REVISION"
)

// RigListener is the listener binary a rig runs, the commit it was built from, and what that
// commit is, for the rig's printed header.
type RigListener struct {
	Path, Revision, Source string
	FromCheckout           bool
}

// RigBinary is the listener the script built for the rig: the pinned one (PinnedBinary), or, with
// ListenerFromCheckoutEnv set to 1, the one built from the checkout's own commit, checked against
// the commit ListenerRevisionEnv names. Any other value of ListenerFromCheckoutEnv fails the test,
// so a misspelt seam is never read as the pinned build.
func RigBinary(t testing.TB) RigListener {
	t.Helper()
	switch seam := os.Getenv(ListenerFromCheckoutEnv); seam {
	case "":
		return RigListener{Path: PinnedBinary(t), Revision: PinnedRevision, Source: "the listener production pins, " + PinnedRelease + " (" + PinnedImage + ")"}
	case "1":
	default:
		t.Fatalf("%s is %q: it is 1, for the listener built from the checkout's own commit, or unset, for the pinned one", ListenerFromCheckoutEnv, seam)
	}
	path, revision := os.Getenv(PinnedListenerEnv), os.Getenv(ListenerRevisionEnv)
	if path == "" || revision == "" {
		t.Skipf("%s=1, and %s or %s is unset: scripts/e2e/lib/live-rig.sh builds the checkout's listener and names both", ListenerFromCheckoutEnv, PinnedListenerEnv, ListenerRevisionEnv)
	}
	if err := checkBuiltFrom(path, revision, "the checkout's own commit, which "+ListenerRevisionEnv+" names"); err != nil {
		t.Fatalf("%s: %v", PinnedListenerEnv, err)
	}
	return RigListener{Path: path, Revision: revision, FromCheckout: true,
		Source: "the listener of the checkout's own commit (" + ListenerFromCheckoutEnv + "=1), not production's pin " + PinnedRelease}
}

// PinnedBinary is the pinned listener the script built, checked (CheckPinned). With none named
// the test skips, naming the script that builds it: a proof that says it runs production's
// listener must not run another one.
func PinnedBinary(t testing.TB) string {
	t.Helper()
	path := os.Getenv(PinnedListenerEnv)
	if path == "" {
		t.Skipf("%s is unset: scripts/e2e/lib/live-rig.sh builds the listener of %s (%s) and names it there", PinnedListenerEnv, PinnedRelease, PinnedRevision)
	}
	if err := CheckPinned(path); err != nil {
		t.Fatalf("%s: %v", PinnedListenerEnv, err)
	}
	return path
}
