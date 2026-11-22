package testenvoy_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testenvoy"
)

// The listener the rig of this change's checks runs is the one production runs: built from the
// commit the pinned image's revision label names, with nothing in its tree modified. A binary of
// any other tree is refused naming the revision it was built from and the pinned one, so a proof
// cannot pass against this checkout's listener and say it ran production's.
func TestAListenerOfAnotherTreeIsRefusedAsThePinnedOne(t *testing.T) {
	binary := testbuild.EnvoyBinary(t, "./cmd/listener")
	err := testenvoy.CheckPinned(binary)
	if err == nil {
		t.Fatalf("this checkout's listener %s was taken for the one built from %s", binary, testenvoy.PinnedRevision)
	}
	if !strings.Contains(err.Error(), testenvoy.PinnedRevision) {
		t.Errorf("the refusal does not name the pinned revision %s: %v", testenvoy.PinnedRevision, err)
	}
}

// A file that carries no Go build information says nothing about the commit it came from, so it
// is refused too rather than read as a binary of unknown origin that may pass.
func TestAFileWithNoBuildInformationIsRefusedAsThePinnedListener(t *testing.T) {
	script := filepath.Join(t.TempDir(), "envoy-listener")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := testenvoy.CheckPinned(script); err == nil {
		t.Fatalf("a shell script was taken for the listener built from %s", testenvoy.PinnedRevision)
	}
}

// The binary scripts/e2e/lib/live-rig.sh builds and names is taken. Without it set there is nothing to
// read, and the test says so rather than passing.
func TestTheListenerTheRigBuiltIsThePinnedOne(t *testing.T) {
	path := os.Getenv(testenvoy.PinnedListenerEnv)
	if path == "" {
		t.Skipf("%s is unset: scripts/e2e/lib/live-rig.sh builds the pinned listener and names it there", testenvoy.PinnedListenerEnv)
	}
	if err := testenvoy.CheckPinned(path); err != nil {
		t.Fatal(err)
	}
}
