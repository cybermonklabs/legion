package daemon

// The running copies (R1 of dispatch://INFRA-1117) of the pause rig's daemon (pause_rig_test.go),
// kept apart from its idle release's bundles. Both ask a bundler for an issue workspace's unpushed
// work, and the rig's runtime answered both into one list, so a pause's check could not tell a
// release that saved a workspace, which a pause forbids, from a running copy, which a pause leaves
// running. The running copy now asks a recorder of its own (overrides.copyBundler), and the
// release's save still asks the runtime (outbox.bundleIssueWorkspace).

import (
	"context"
	"slices"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// pauseCopies is the bundler the rig daemon's running copy asks in place of its runtime: it
// records every issue a running copy asked to bundle, in order, and answers a bundle that holds
// nothing, as pauseRuntime answers a release's save, so each copy drops the issue's running copy
// and owes no retry.
type pauseCopies struct {
	mu     sync.Mutex
	issues []string
}

func (c *pauseCopies) BundleWorkspace(_ context.Context, issue string, _ claim.Role) (runtime.WorkspaceBundle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.issues = append(c.issues, issue)
	return runtime.WorkspaceBundle{}, nil
}

// pauseOverrides is the rig's daemon over rt (fakeRuntime), its running copy asking a pauseCopies
// of its own, so rt's bundled list (pauseRuntime.podReads) holds the release bundles alone.
func pauseOverrides(rt *pauseRuntime, record *built) overrides {
	o := fakeRuntime(rt, record)
	o.copyBundler = &pauseCopies{}
	return o
}

// copiedIssues is every issue the daemon's running copy has asked to bundle so far, in order,
// through every boot of the rig.
func (r *pauseDaemonRig) copiedIssues() []string {
	copies := r.o.copyBundler.(*pauseCopies)
	copies.mu.Lock()
	defer copies.mu.Unlock()
	return slices.Clone(copies.issues)
}
