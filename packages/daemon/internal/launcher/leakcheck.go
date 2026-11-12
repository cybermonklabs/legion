package launcher

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// checkInstall is the pod's one install of the shared public leak check, shared by every role
// manager of the launcher's container as bundleRestore is, the controller's single role included.
// The daemon reads the check at the tag the launcher's hello names (its own environment's pin,
// leakguard.TagVariable) and pushes it in launcher_leak_check slices before it starts a role; this
// holds the slices of one sequence in memory, checks the whole body's digest, and installs it once
// for the process (leakguard.InstallServed), so no role of the pod reads GitHub for it and each
// role's shim only reads the install (leakguard.Installed).
//
// The install runs under mu, which every role's connection takes for every slice, so two roles
// the daemon pushes the same check to install it once: the second finds the tag and digest
// installed and is answered Installed at once.
type checkInstall struct {
	// pin is the launcher's own pin, "" when its environment names none; dir is where the check
	// is installed, never "" (leakguard.InstallDir unless Config.LeakCheckDir names another).
	pin, dir string
	// done is whether this process has installed its pin, which every hello says
	// (LauncherHello.LeakCheckInstalled). It is read apart from mu, so a role's reconnect is not
	// held up by another role's install in progress.
	done atomic.Bool

	mu sync.Mutex
	// sequence is the id of the slice sequence being assembled, with the count and the digest its
	// slices agree on, and slices what has arrived of it.
	sequence string
	count    int
	digest   string
	slices   [][]byte
	// installedTag and installedDigest are the check this process installed, "" until it has.
	installedTag, installedDigest string
}

// newCheckInstall is the install of a launcher whose environment pins tag pin ("" for none), into
// dir ("" for leakguard.InstallDir).
func newCheckInstall(pin, dir string) *checkInstall {
	if dir == "" {
		dir = leakguard.InstallDir
	}
	return &checkInstall{pin: pin, dir: dir}
}

// hello is what a hello says of the check: the launcher's pin, and whether this process has
// installed it. A launcher with no pin says nothing of either.
func (c *checkInstall) hello() (tag string, installed bool) {
	if c == nil || c.pin == "" {
		return "", false
	}
	return c.pin, c.done.Load()
}

// receive takes one slice of the check. A slice whose tag is not the launcher's own pin is
// refused, and one of the tag and digest already installed is answered Installed at once. The
// sequence is rebuilt in memory and nothing is written anywhere until its digest matches the one
// every slice declares; the last slice of a whole sequence runs the install and is answered
// Installed, or refused with the pod's cause sentence. Either way the slices are dropped, so a
// sequence pushed again after a refusal starts fresh. ctx is the container's: the install ends
// with it, and leakguard.InstallServed bounds it besides. logf writes the role's own lines.
func (c *checkInstall) receive(ctx context.Context, frame shimwire.LauncherLeakCheck, logf func(string, ...any)) shimwire.LauncherLeakCheckResult {
	refuse := func(cause string) shimwire.LauncherLeakCheckResult {
		return shimwire.LauncherLeakCheckResult{ID: frame.ID, Index: frame.Index, Error: cause}
	}
	if err := frame.Validate(); err != nil {
		return refuse(leakguard.Cause("the pod's launcher could not take a slice of the check the daemon served", err))
	}
	switch {
	case c.pin == "":
		return refuse(leakguard.Cause(fmt.Sprintf("the pod's launcher was served %s, and its environment names no pin (%s)",
			frame.Tag, leakguard.TagVariable), nil))
	case frame.Tag != c.pin:
		return refuse(leakguard.Cause(fmt.Sprintf("the pod's launcher was served %s, not its own pin %s", frame.Tag, c.pin), nil))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if frame.Tag == c.installedTag && frame.SHA256 == c.installedDigest {
		return shimwire.LauncherLeakCheckResult{ID: frame.ID, Index: frame.Index, OK: true, Installed: true}
	}
	if frame.ID != c.sequence {
		c.sequence, c.count, c.digest = frame.ID, frame.Count, frame.SHA256
		c.slices = make([][]byte, frame.Count)
	}
	if frame.Count != c.count || frame.SHA256 != c.digest {
		return refuse(leakguard.Cause(fmt.Sprintf("the pod's launcher was served slice %d of %d of %s, which does not match its sequence's %d slices of %s",
			frame.Index, frame.Count, frame.SHA256, c.count, c.digest), nil))
	}
	c.slices[frame.Index] = frame.Payload
	body := joined(c.slices)
	if body == nil {
		return shimwire.LauncherLeakCheckResult{ID: frame.ID, Index: frame.Index, OK: true}
	}
	c.sequence, c.count, c.digest, c.slices = "", 0, "", nil
	if got := shimwire.BundleDigest(body); got != frame.SHA256 {
		return refuse(leakguard.Cause(fmt.Sprintf("the check the daemon served rebuilt to sha256 %s, not the %s its slices declared",
			got, frame.SHA256), nil))
	}
	if _, err := leakguard.InstallServed(ctx, leakguard.ServedInstall{
		Dir: c.dir, Tag: frame.Tag, Body: body,
		Log: func(format string, args ...any) { logf("launcher: "+format, args...) },
	}); err != nil {
		return refuse(leakguard.Cause(err.Error(), nil))
	}
	c.installedTag, c.installedDigest = frame.Tag, frame.SHA256
	c.done.Store(true)
	logf("launcher: installed the shared public leak check %s at %s, served by the daemon (sha256 %s)",
		frame.Tag, c.dir, frame.SHA256[:12])
	return shimwire.LauncherLeakCheckResult{ID: frame.ID, Index: frame.Index, OK: true, Installed: true}
}
