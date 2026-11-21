package ghtool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// LeakCheckSource reads the shared public leak check at a tag, packed as the daemon serves it to a
// pod's launcher: the Kubernetes runtime's reader (runtime/sandbox's LeakCheckReader, a
// *leakguard.Source, which keeps one copy per tag), whose every error is one cause sentence
// (leakguard.Cause) wrapping what is underneath it (leakguard.Source.Get).
type LeakCheckSource interface {
	Get(ctx context.Context, tag string) (leakguard.Served, error)
}

// leakCheckDir is the directory under the daemon's state directory installLeakCheck installs the
// check into, beside the answers' own directories (Host.StateDir).
const leakCheckDir = "leak-check"

// installLeakCheck installs the shared public leak check source serves at tag into
// <stateDir>/leak-check, for the guard of a Kubernetes daemon's own gh tool (LeakCheck.Install),
// and answers the path of the installed check. Such a daemon's environment holds none of the three
// places the guard resolves a check from (leakguard's Resolve): the worker image carries the pin,
// LEGION_LEAK_CHECK_TAG, and not the check, which every pod's launcher installs for its own pod
// from what the daemon serves it. So the daemon installs its own copy the same way, with the
// launcher's own installer (leakguard.InstallServed: the unpack into a staging directory, the tag
// recorded beside the check, one `--help` run, and the rename over what the directory held), and
// reads it back as a role's shim reads its pod's install (leakguard.Installed), so the path it
// answers is a check of tag. The daemon reaps no child it did not start, so that run starts with
// procgroup.NoReaper. log takes the one line of an install that replaced what the directory held:
// the install of an earlier boot, at its tag.
//
// manifest is the deployment's manifest digest (leakguard.Setting.ManifestSHA256), which the
// install is held to as the install of a pod built under the setting is. What source served must
// be of it (leakguard.Served's ManifestSHA256), as the daemon pushes a pod built under the setting
// no check of another digest (runtime/sandbox's serveLeakCheck), or nothing is installed; and the
// install reads back under it as leakguard.CheckerVar, as a role's shim reads its pod's install, so
// its four files on disk hash to it too.
//
// An error is the read's own (source's), as it came; or the cause sentence a pod's start is
// refused with (leakguard.Cause), which names the failure: a served check of another digest, or
// an install of what source served that failed (leakguard.InstallServed); or the setup failure a
// role's shim would refuse to start with where the install does not read back as tag's and
// manifest's (leakguard.Installed).
func installLeakCheck(ctx context.Context, source LeakCheckSource, tag, manifest, stateDir string, log *slog.Logger) (string, error) {
	served, err := source.Get(ctx, tag)
	if err != nil {
		return "", err
	}
	if served.ManifestSHA256 != manifest {
		return "", errors.New(leakguard.Cause(fmt.Sprintf("the check the daemon read at %s has the manifest digest %s, not the deployment's %s (%s or leak_check.manifest_sha256)",
			tag, served.ManifestSHA256, manifest, leakguard.ManifestVar), nil))
	}
	dir := filepath.Join(stateDir, leakCheckDir)
	if err := leakguard.InstallServed(ctx, leakguard.ServedInstall{
		Dir: dir, Tag: tag, Body: served.Body, Start: procgroup.NoReaper,
		Log: func(format string, args ...any) { log.Info("answers: " + fmt.Sprintf(format, args...)) },
	}); err != nil {
		return "", errors.New(leakguard.Cause(err.Error(), nil))
	}
	check, err := leakguard.Installed([]string{leakguard.TagVariable + "=" + tag, leakguard.CheckerVar + "=" + manifest}, dir)
	if err != nil {
		return "", err
	}
	return check, nil
}

// LeakChecker is the shared public leak check a call's guard runs (Host.LeakCheck), asked at every
// call: Check answers the path of the check the guard is handed (leakguard.Options.Check), so the
// guard never resolves one of its own, or the *leakguard.SetupFailure that refuses the call, gh
// never started. Each runtime has one (internal/daemon's answersTools): a Kubernetes daemon's
// *LeakCheck, the copy it installs at its pin, and a tmux daemon's MachineLeakCheck, the
// machine's own.
type LeakChecker interface {
	Check() (string, error)
}

// LeakCheck is the shared public leak check a Kubernetes daemon's gh tool runs before every call
// (Host.LeakCheck): the copy the daemon installs at its pin and holds to the deployment's manifest
// digest (Install), which an answer's call may come before. Until a try of the install lands,
// Check refuses every call with the setup failure naming the pin and why the last try failed;
// from then on it answers the installed check, to the next call of an answer already open too.
// Once the daemon stops trying (Stop), the refusal says so. The daemon installs into it while the
// answers' calls read it, so it is safe for concurrent use.
type LeakCheck struct {
	tag, manifest string

	mu        sync.Mutex
	installed string
	failure   error
	stopped   bool
}

// NewLeakCheck is the check at tag, of the deployment's manifest digest manifest, not yet
// installed.
func NewLeakCheck(tag, manifest string) *LeakCheck { return &LeakCheck{tag: tag, manifest: manifest} }

// Install makes one try of the install at the pin: the check source serves at it, held to the
// deployment's manifest digest and installed under stateDir (installLeakCheck). A try that lands
// makes the installed check the one every call runs, and logs the tag and the path to log; one
// that fails makes its error why every call is refused until a try lands, and returns it.
func (c *LeakCheck) Install(ctx context.Context, source LeakCheckSource, stateDir string, log *slog.Logger) error {
	check, err := installLeakCheck(ctx, source, c.tag, c.manifest, stateDir, log)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.failure = err
		return err
	}
	c.installed, c.failure = check, nil
	log.Info("answers: installed the shared public leak check the gh tool's leak guard runs", "tag", c.tag, "check", check)
	return nil
}

// Stop records that the daemon tries the install no more until its next boot, over cause, a
// failure no wait changes, which every refusal names from then on.
func (c *LeakCheck) Stop(cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failure, c.stopped = cause, true
}

// Check is the installed check a call runs, or, before an install lands, the
// *leakguard.SetupFailure that refuses the call: the check at the pin is not installed yet, or
// could not be installed once the daemon stopped trying (Stop), with the last failure underneath.
func (c *LeakCheck) Check() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.installed != "" {
		return c.installed, nil
	}
	state := "is not installed yet, and the daemon tries the install again until it is"
	if c.stopped {
		state = "could not be installed, and the daemon tries the install again only at its next boot"
	}
	return "", &leakguard.SetupFailure{
		What: leakguard.WhatGhCall,
		Why:  fmt.Sprintf("the shared public leak check at %s, the pin %s names, %s", c.tag, leakguard.TagVariable, state),
		Err:  c.failure,
	}
}

// MachineLeakCheck is the shared public leak check of the machine a tmux daemon runs on
// (Host.LeakCheck), resolved at each call in Environ, the daemon's own environment
// (Host.Environ), the way the tmux runtime resolves a pane's for each launch (runtime/tmux's
// resolveLeakCheck): leakguard's Resolve, PUBLIC_LEAK_CHECK, then public-leak-check.sh on PATH,
// then the pointer file under the configuration home. It resolves at each call because an
// operator's machine installs, moves and republishes the check while the daemon runs, and in the
// daemon's environment rather than the guard children's (Answer.guardEnv), whose PATH has gh's
// directory in front and which carries no PUBLIC_LEAK_CHECK. Nil Environ is this process's own,
// read at each call.
//
// Resolve answers a check, a broken setup (leakguard.BrokenAt: a check named and not there or not
// an executable file, a file of the check's name on PATH that is no executable, a pointer file
// that cannot be read or names nothing), or no place naming a check at all (leakguard.NoneNamed).
// The tmux runtime tells a pane the path of a broken setup, which its guard refuses every write
// for, and tells it nothing where none is named, which its guard refuses a write for only under a
// deployment that names a check. The gh tool refuses a call for either, under every deployment:
// it runs no call without a check (internal/daemon's answersLeakCheck). Under a deployment that
// names a check, the guard holds the check this answers to the deployment's manifest digest
// (Host.LeakCheckManifestSHA256), as a pane's guard holds the pane's.
type MachineLeakCheck struct {
	Environ []string
}

// Check is the machine's check, or, on a machine whose setup is broken or names none, the setup
// failure the guard itself refuses such a call with (leakguard.WhyNoCheck), naming what Resolve
// read.
func (m MachineLeakCheck) Check() (string, error) {
	check, err := leakguard.New(leakguard.Options{Env: m.Environ}).Resolve()
	if err != nil {
		return "", &leakguard.SetupFailure{What: leakguard.WhatGhCall, Why: leakguard.WhyNoCheck, Err: err}
	}
	return check, nil
}
