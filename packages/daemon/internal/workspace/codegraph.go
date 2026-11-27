package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/memoryguard"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// codegraphTimeout bounds each codegraph invocation independently. The warm-up runs only in the
// background, so nothing waits on it; a first index of a large repository (tens of thousands of
// files) takes longer than Provision's own command budget. It is a variable so that a test can
// shorten it.
var codegraphTimeout = 30 * time.Minute

// codegraphStopGrace is how long a codegraph child whose context ended has to leave on its SIGTERM
// before it is killed and the invocation returns: a shim's warm-up ends with the shim's Oh My Pi
// (WarmCodegraphIndex), and the shim waits on it inside the role's stop grace (internal/shim's
// Config.StopGrace, the pod's terminationGracePeriodSeconds, `worker_stop_timeout_seconds`, 10 s
// by default), so this grace is well inside what is left of that once the agent's own grace is
// spent; CodeGraph 1.6.4, SIGTERMed mid-build, exits in 0.03 s (exit 143). It is also how long Run
// waits, once the child has exited, for a grandchild that still holds its pipes (os/exec's
// WaitDelay). It is a variable so that a test can lengthen it.
var codegraphStopGrace = 2 * time.Second

// codegraphStopSettle is how long a warm-up whose build a signal or a non-zero exit ended, while
// its own context still ran, waits for that context to end before it reads the end as the build's
// own and records it (warmBuild). A role's stop is role-init's SIGTERM to every process of the role
// at once (WarmCodegraphIndex): the build, which CodeGraph 1.6.4 leaves 0.03 s after it, and the
// shim, whose own SIGTERM ends this warm-up's context only after a few of its goroutines have run.
// Read before that, the stop's SIGTERM would be recorded as a build that died, and every later
// warm-up on that disk would leave the partial index unrepaired, where R2 repairs an orderly stop
// at the next launch (INFRA-5717). The shim's SIGTERM comes in the same kill as the build's, so
// its context ends well inside codegraphStopGrace; this is that grace. A cap's kill and the
// timeout are the warm-up's own ends and wait for nothing.
var codegraphStopSettle = codegraphStopGrace

// codegraphEmptyLockGrace is how long an empty `.codegraph/codegraph.lock` still counts as held,
// either side of now. CodeGraph 1.6.4's FileLock (FileLock.create) writes the builder's PID to a
// temporary file and hard-links that to the lock, so the lock normally appears with its PID
// already in it. Only when the link fails does it fall back to an exclusive create followed by the
// PID write, two syscalls back to back, and on that path the file exists and is empty for a
// moment. Timed on a 32-core host at load average 130–145, that create-then-write took about 11 µs
// at the median and 107 ms at worst (measured on 1.5.0, whose FileLock took every lock that way,
// in one `fs.writeFileSync(lockPath, String(process.pid), { flag: 'wx' })`), so five seconds is
// about 50 times the worst stall. An empty lock older than that is what a builder on the fallback
// leaves when it dies between the two calls, or when its write fails (ENOSPC); one dated further
// than that into the future is no starting build's. Both are stale. Nothing reruns a warm-up that
// skipped its repair for a held lock: the repair waits for the next claim's provisioning of the
// workspace.
const codegraphEmptyLockGrace = 5 * time.Second

// warmLeaseHeartbeat is how often a warm-up holding `.codegraph/legion-warm.lock` sets the lease's
// mtime to now, and warmLeaseStale is how old that mtime may be before another process reads the
// holder as dead and takes the lease over: six missed heartbeats, so a holder stalled through one
// slow tick or a filesystem hiccup keeps its lease, while a dead one leaves a lease that a warm-up
// within the minute still reads as live and skips, and the first one after that takes over. The
// heartbeat is token-guarded (refreshWarmLease): each tick refreshes the lease only while it still
// carries this holder's token, so a holder stalled past warmLeaseStale and taken over refreshes
// nothing of its successor's lease — its heartbeat stops at the first tick that reads the
// successor's token, and a dead holder's lease is never kept alive by a stalled one's ticks. A
// tick that cannot read the lease at all (a takeover's remove-and-create caught between its two
// steps, a hiccup of the volume) refreshes nothing and the heartbeat goes on: a missed tick is
// what the six-tick budget is for, and a heartbeat ended there would let a live build's lease go
// stale under it, a sibling take it over and a second build run on the same workspace. A
// holder releases its lease on every exit it can see: the warm-up's deferred release runs once its
// codegraph child has ended, and the one process that runs a warm-up and may exit before it, a
// role's shim, ends the warm-up and waits for that release before it exits (WarmCodegraphIndex),
// so a stopped generation leaves no lease. Only a holder killed outright (SIGKILL, a pod that went
// away, a node lost) leaves one, and the minute is what it costs. The lease is the one
// cross-process guard here; everything else is this process's. `warming` is a map in this process.
// nextCodegraphStep lets a live CodeGraph lock veto a repair but not an `init`: an uninitialized
// workspace has no `codegraph.lock` to read, so two processes that both find it uninitialized would
// both run `init`. codegraphLockHeldByLiveProcess judges liveness by PID, which another
// role's PID namespace makes meaningless; it stays as it is for the one-process case. The
// contenders are the six roles of one issue pod, which share the issue's workspace and
// each warm it once their Oh My Pi starts (`legion worker-shim --warm-codegraph`); no other pod
// mounts the issue's volume. The lease is a file judged by its existence and mtime on that
// volume: an exclusive create its holder keeps fresh,
// and the only exclusive step on this filesystem is that create: a stale lease is removed under a
// second exclusive create, the takeover marker (takeOverWarmLease), so two contenders that both
// judged one lease stale cannot both remove it and both build — a rename would not do, since a
// rename moves whatever is at the path, the winner's fresh lease included. Nothing here judges a
// lease by its inode: the tree volume (v9fs) gives a file created at a path the inode the removed
// one had, so os.SameFile reads a successor's fresh lease as the file it replaced; a lease is
// judged by its mtime (stale or live) and known as this holder's by the token written into it.
const (
	warmLeaseHeartbeat = 10 * time.Second
	warmLeaseStale     = 60 * time.Second
)

// warmLeaseName is the lease file a warm-up holds under `.codegraph/`, beside the index itself, so
// every process that shares the index shares the lease; CodeGraph's own `.gitignore` there (`*`)
// keeps it out of the repository.
const warmLeaseName = "legion-warm.lock"

// warming holds the workspace directories with a background warm-up in flight in this process.
var warming sync.Map

// indexed holds the workspace directories where this process has already attempted a
// `codegraph index` repair, so a build that never leaves the index current (codegraphStatus: a
// `"partial"` or `"failed"` result that recurs, or a status this daemon can't parse) gets
// re-indexed at most once per workspace per process rather than on every later spawn.
var indexed sync.Map

// WarmCodegraphIndexInBackground starts WarmCodegraphIndex for dir in its own goroutine and
// returns at once, so no launch ever waits on an index build; a second call for a directory whose
// warm-up is still running in this process does nothing, and one running in another process is
// what the lease settles (warmLeaseHeartbeat). The daemon's host provisioning for tmux panes
// (internal/daemon/outbox.go) calls it: the daemon outlives any build. The pod init container
// builds no index: it runs on the pod's registration path and its goroutines die with it. The
// daemon's tmux path warms a workspace right after provisioning it, so now is that launch, the
// instant the success line reports its build against. start starts each codegraph
// (WarmCodegraphIndex): the daemon's is procgroup.NoReaper. It is the caller's to name, not this
// package's, so the start that reaps nothing never flows into the warm-up's code in a program
// that is not the daemon's (internal/startwalk, whose walk of a shim or a launcher is not told one
// caller of a function from another).
func WarmCodegraphIndexInBackground(start procgroup.Starter, dir string) {
	if _, busy := warming.LoadOrStore(dir, struct{}{}); busy {
		return
	}
	go func() {
		defer warming.Delete(dir)
		WarmCodegraphIndex(context.Background(), start, dir, time.Now())
	}()
}

// WarmCodegraphIndex is the warm-up a caller waits on: it returns once the build it started has
// ended and the workspace's lease is released, and ctx ending ends the build (its codegraph child
// is SIGTERMed, then killed after codegraphStopGrace) rather than the other way round. A role's
// `legion worker-shim --warm-codegraph` runs it under the shim's own lifetime (internal/shim's
// Config.WarmCodegraph): once its Oh My Pi has exited the shim ends the warm-up and waits for it,
// so a stop mid-build — role-init's SIGTERM to every process of the role (its kill(-1) as the
// role's PID 1, internal/launcher's signalGeneration), which ends Oh My Pi and the build, in its own
// process group, together — never leaves the lease behind for the role's relaunch to read as a
// live build. A launcher that is not PID 1 signals the shim's group alone, and the shim's end of
// the warm-up then reaches the build (runCodegraph). Since the build can end on that SIGTERM
// before the shim's own has ended ctx, a build a signal or a non-zero exit ended waits up to
// codegraphStopSettle for ctx before its end is read as the build's own (warmCodegraphIndexOn).
// The index such a stop leaves partial is repaired by the next warm-up, as is
// one another CodeGraph CLI version built (nextCodegraphStep: initialized, not current, no live
// CodeGraph lock, no record of a build that died → `index`). launched is the instant the caller's
// process started — a role shim's Run — which the one line a built index logs reports the build
// against, beside the build's own duration, so a pod's log says how long after the role's launch
// its index was ready. start starts each codegraph the warm-up runs (procgroup.Starter, never
// nil): the daemon's is procgroup.NoReaper, and a shim's is its own guard's (internal/shim's
// Config.WarmCodegraph), which holds the child out of the reaper of the shim's orphans.
func WarmCodegraphIndex(ctx context.Context, start procgroup.Starter, dir string, launched time.Time) {
	warmCodegraphIndexOn(ctx, clock.Real(), start, dir, launched)
}

// warmCodegraphIndexOn builds a workspace's codegraph index before any worker needs it, each
// codegraph run bounded by codegraphTimeout on c. Its one caller outside the tests is
// WarmCodegraphIndex, which passes clock.Real; a test hands it a fake (boundRow). Each role's
// `impact`, `callers`, `affected` and `explore` queries (the role prompts) need an index already
// built, not one built on first use. Callers run it through WarmCodegraphIndexInBackground,
// outside any provisioning serialization: indexing reads only the issue's own workspace directory.
// codegraph is deliberately not one of the tools Runner requires — the daemon must still boot
// where it is absent (the tmux runtime's host provisioning never installs it) — so this resolves
// it from PATH on its own, directly with os/exec, and never fails provisioning: a missing CLI, a
// non-zero exit, or a timeout is logged loudly to stderr. Warming simply does not happen when the
// CLI is missing
// or fails; the worker reads the source instead, per its role prompt. An index that is initialized
// but not current (codegraphStatus) is repaired with `codegraph index` rather than accepted as
// built: one an earlier warm-up left partial (`index.state` not `"complete"`), or one another
// CodeGraph CLI version built (`index.builtWithVersion` set to another version than the running
// CLI's `version`, as a kept volume's 1.5.0 index reads to 1.6.4). `codegraph init` on an
// already-initialized directory only prints "Already initialized" and exits 0, so it cannot do
// this repair itself. Neither repair runs while a build holds `.codegraph/codegraph.lock`:
// nextCodegraphStep skips it entirely then.
// A second `codegraph index` started against a live build damages the shared SQLite database even
// when CodeGraph's own lock refuses it the write (observed: the second process exits on "Could not
// acquire file lock", and the live build still fails with "database disk image is malformed") — so
// this never lets a second process even attempt it, whatever CodeGraph's own lock would decide
// (1.6.4 judges a holder stale only once its PID is not alive, and takes a stale lock over through
// a second lock, `codegraph.lock.break`). The whole warm-up, `status` included, runs under the
// workspace's cross-process lease (acquireWarmLease, warmLeaseHeartbeat): a second process that
// finds the lease held leaves the build to its holder. Beside that lease it keeps the record of
// its last build (warmBuild): a build that died, rather than exiting 0 or being stopped by its own
// warm-up, holds every later warm-up of that workspace on that disk, since the same build would
// die the same way and a build that kills its pod runs again at every role's launch
// (INFRA-5717). Each build is bounded by codegraphMemoryCap (runCodegraph), well under the pod's
// limit, and runs CodeGraph's parse pool at codegraphParseWorkers.
// Every codegraph invocation gets a minimal, explicit environment — PATH, HOME, TMPDIR when set,
// and DO_NOT_TRACK=1 — never the process's full environment: a provisioning caller may hold a
// one-shot GitHub token or other secrets in its own environment, and codegraph gets none of them.
// DO_NOT_TRACK=1 disables both CodeGraph's telemetry and its update check (its bundled docs rank
// DO_NOT_TRACK above CODEGRAPH_TELEMETRY above stored config above default-on), so an automatic,
// non-opt-in warm-up never phones home. start starts each codegraph it runs (WarmCodegraphIndex).
func warmCodegraphIndexOn(ctx context.Context, c clock.Clock, start procgroup.Starter, dir string, launched time.Time) {
	codegraphPath, err := exec.LookPath("codegraph")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up skipped for %s: %s\n", dir, err)
		return
	}
	// dirExists is read before the lease's directory is made: nextCodegraphStep's fallback (a
	// status that failed or didn't parse) reads an existing `.codegraph/` as an index to repair,
	// and a workspace never initialized must still read as directory-less there. `codegraph init`
	// tolerates the directory — a committed `.codegraph/.gitignore` leaves it present anyway.
	codegraphDir := filepath.Join(dir, ".codegraph")
	dirExists, err := pathExists(codegraphDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up could not run for %s: %s\n", dir, err)
		return
	}
	if !dirExists {
		if err := os.Mkdir(codegraphDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up could not run for %s: %s\n", dir, err)
			return
		}
	}
	release, held := acquireWarmLease(dir, filepath.Join(codegraphDir, warmLeaseName))
	if !held {
		return
	}
	defer release()
	status, err := runCodegraph(ctx, c, start, codegraphPath, dir, "status", "--json")
	if ctx.Err() != nil {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s ended with its process before `codegraph status` finished\n", dir)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up could not run for %s: %s\n", dir, err)
		return
	}
	// The lock read and the kill(pid, 0) syscall below are worth paying for only when a repair
	// might actually run: a current index already needs neither, and logging a "skipped" line
	// for a lock some unrelated codegraph run holds at that moment, on an index that needed no
	// repair, would be misleading.
	current := status.exitCode == 0 && json.Valid([]byte(status.stdout))
	if current {
		initialized, indexCurrent := codegraphStatus(status.stdout)
		current = initialized && indexCurrent
	}
	lockLive := dirExists && !current && codegraphLockHeldByLiveProcess(dir)
	// The record of the last build is read under the lease, so a record still `running` is no
	// live build's: it is one that died with the process that wrote it.
	recordFile := filepath.Join(codegraphDir, warmBuildName)
	version := codegraphVersion(codegraphPath)
	record, recorded, err := readWarmBuild(recordFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s reads its build record %s as none: %s\n", dir, recordFile, err)
	}
	holding := false
	if recorded {
		created, createdOK, err := Created(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s could not read when the workspace was created: %s\n", dir, err)
		}
		holding = record.holds(version, codegraphMemoryCap, created, createdOK)
	}
	step := nextCodegraphStep(dirExists, lockLive, holding, status.exitCode, status.stdout)
	if step == codegraphStepNone {
		switch {
		case current && recorded:
			// Whoever left the index current — a later warm-up, an agent's own `codegraph index` —
			// the record says nothing of this workspace any longer.
			removeWarmBuild(dir, recordFile)
		case holding:
			fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s skipped: its last build, %s, %s\n", dir, record.describe(), warmBuildReason(record))
		case lockLive:
			fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s skipped: a build in progress holds the index lock\n", dir)
		}
		return
	}
	subcommand := "init"
	if step == codegraphStepIndex {
		if _, already := indexed.LoadOrStore(dir, struct{}{}); already {
			return
		}
		subcommand = "index"
	}
	buildStarted := time.Now()
	// The record is written, and synced, before the build starts: a build that takes the pod with
	// it leaves no line and runs no code of this warm-up's, and the disk is all the next one has.
	running := warmBuild{Subcommand: subcommand, Started: buildStarted, Outcome: warmOutcomeRunning, CodeGraph: version, Cap: codegraphMemoryCap}
	if err := writeWarmBuild(recordFile, running); err != nil {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s could not record its build: %s\n", dir, err)
		return
	}
	result, err := runCodegraph(ctx, c, start, codegraphPath, dir, subcommand)
	built := time.Since(buildStarted)
	if err == nil && ctx.Err() == nil && result.cappedAt == 0 && !result.timedOut && (result.signal != "" || result.exitCode != 0) {
		// The stop's SIGTERM may have reached the build before the shim's has ended ctx
		// (codegraphStopSettle). Sleep answers at once when ctx ends, and its own error is no news.
		_ = c.Sleep(ctx, codegraphStopSettle)
	}
	if ctx.Err() != nil {
		// The warm-up's own stop ended this build (a role's stop, the shim on its way out), so
		// the next launch builds again: an orderly stop is no reason to hold a workspace.
		removeWarmBuild(dir, recordFile)
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s ended with its process before `codegraph %s` finished; the next launch's warm-up repairs the index\n", dir, subcommand)
		return
	}
	ended := func(outcome string, peak uint64) {
		done := running
		done.Outcome, done.Peak = outcome, peak
		if err := writeWarmBuild(recordFile, done); err != nil {
			fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s could not record how its build ended: %s\n", dir, err)
		}
	}
	if err != nil {
		// The build never ran: nothing of this workspace's is held by a command that failed to
		// start.
		removeWarmBuild(dir, recordFile)
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up could not run for %s: %s\n", dir, err)
		return
	}
	switch {
	case result.cappedAt > 0:
		ended(warmOutcomeMemoryCap, result.cappedAt)
		fmt.Fprintf(os.Stderr, "[legion] codegraph %s for %s ended at its memory cap: its process group read %d MiB, over the cap of %d MiB, after %s: %s\n",
			subcommand, dir, result.cappedAt>>20, codegraphMemoryCap>>20, built.Round(time.Millisecond), strings.TrimSpace(result.stderr))
		return
	case result.timedOut:
		ended(warmOutcomeTimedOut, 0)
		fmt.Fprintf(os.Stderr, "[legion] codegraph %s for %s timed out after %s: %s\n", subcommand, dir, codegraphTimeout, strings.TrimSpace(result.stderr))
		return
	case result.signal != "":
		ended(warmOutcomeKilled, 0)
		fmt.Fprintf(os.Stderr, "[legion] codegraph %s for %s was ended by %s: %s\n", subcommand, dir, result.signal, strings.TrimSpace(result.stderr))
		return
	case result.exitCode != 0:
		ended(exitOutcome(result.exitCode), 0)
		fmt.Fprintf(os.Stderr, "[legion] codegraph %s failed for %s (exit %d): %s\n", subcommand, dir, result.exitCode, strings.TrimSpace(result.stderr))
		return
	}
	removeWarmBuild(dir, recordFile)
	fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s built the index: codegraph %s took %s, done %s after launch\n", dir, subcommand, built.Round(time.Millisecond), time.Since(launched).Round(time.Millisecond))
}

// warmBuildReason is what the skipped line says of a record that holds: a build still `running`
// died with the process that wrote it, since this warm-up holds the lease, and any other outcome
// is one that build reported.
func warmBuildReason(rec warmBuild) string {
	if rec.Outcome == warmOutcomeRunning {
		return "which died unseen (a pod end, an out-of-memory kill, a shim killed at its stop grace); it runs again on a new workspace, or under another CodeGraph or cap"
	}
	return "and it would end the same way again; it runs again on a new workspace, or under another CodeGraph or cap"
}

// warmLeaseStep is a seam for the lease's own tests, nil otherwise: it is called with the name of
// the step acquireWarmLease is about to take, so a test can run a second contender at exactly that
// point and drive an interleaving the scheduler would rarely produce.
var warmLeaseStep func(step string)

func leaseStep(step string) {
	if warmLeaseStep != nil {
		warmLeaseStep(step)
	}
}

// acquireWarmLease takes dir's warm-up lease, the file at path, or reports that another process
// holds it. An exclusive create either wins the lease or finds a holder's file. One whose mtime is
// within warmLeaseStale is a live warm-up elsewhere, and the caller leaves the build to it. An
// older one is a dead holder's, which takeOverWarmLease removes under its own exclusive marker, so
// of two contenders that both judged one lease stale exactly one removes it; the create is then
// tried once more, an EEXIST being a contender that got there first. A lease gone between the
// failed create and its stat was just released, and gets the same one retry.
func acquireWarmLease(dir, path string) (release func(), held bool) {
	skipped := func(what string) (func(), bool) {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up for %s skipped: another process %s %s\n", dir, what, path)
		return nil, false
	}
	failed := func(err error) (func(), bool) {
		fmt.Fprintf(os.Stderr, "[legion] codegraph warm-up could not run for %s: %s\n", dir, err)
		return nil, false
	}
	for range 2 {
		// Two tries: the create, and one more after a released or taken-over lease.
		lease, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			token := rand.Text()
			_, err := lease.WriteString(token)
			_ = lease.Close()
			if err != nil {
				_ = os.Remove(path)
				return failed(err)
			}
			return holdWarmLease(path, token, warmLeaseHeartbeat), true
		}
		if !errors.Is(err, os.ErrExist) {
			return failed(err)
		}
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return failed(err)
		}
		if time.Since(info.ModTime()) < warmLeaseStale {
			return skipped("holds")
		}
		leaseStep("judged stale")
		switch took, err := takeOverWarmLease(path); {
		case err != nil:
			return failed(err)
		case !took:
			return skipped("is taking over")
		}
	}
	return skipped("holds")
}

// takeOverWarmLease removes the lease at path a contender judged stale, under the takeover marker
// beside it, an exclusive create of its own: of two contenders that both judged the same lease
// stale, exactly one creates the marker, and the other finds it and leaves the takeover to the
// holder (false). Under the marker the file at the path is judged again, by its mtime, since the
// first contender may already have replaced it: one still older than warmLeaseStale is the dead
// holder's and is removed; a fresh one is a new holder's and is left for the caller's create to
// find; one already gone was released. Either way the caller's create decides (true). The judgment
// is the mtime and never the file's identity: the tree volume a pod's workspace lives on (v9fs)
// hands a file created at a path the inode the removed one had, so os.SameFile would read the new
// holder's fresh lease as the stale file it was asked about and remove it from under its build —
// the tester's finding on this issue. A marker older than warmLeaseStale is a contender's killed
// inside these few syscalls, removed before the one retry.
func takeOverWarmLease(path string) (bool, error) {
	marker := path + ".takeover"
	for range 2 {
		held, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_ = held.Close()
			leaseStep("marker held")
			current, err := os.Stat(path)
			if err == nil && time.Since(current.ModTime()) >= warmLeaseStale {
				_ = os.Remove(path)
			}
			_ = os.Remove(marker)
			return true, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return false, err
		}
		info, err := os.Stat(marker)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if time.Since(info.ModTime()) < warmLeaseStale {
			return false, nil
		}
		_ = os.Remove(marker)
	}
	return false, nil
}

// holdWarmLease keeps the lease at path fresh, its mtime set to now every heartbeat so a live
// holder's lease is never read as stale, until release. The heartbeat, like the release, knows the
// lease by its token: each tick refreshes the file only while it still carries this holder's token
// (refreshWarmLease). The first tick that reads another token ends the heartbeat for good, the
// build itself running on; one that cannot read the lease refreshes nothing and tries again at the
// next tick. The lease of a holder stalled past warmLeaseStale and taken over belongs to its
// successor by then: a stalled holder's ticks refreshing it would keep the successor's lease fresh
// past the successor's own death, a dead holder's lease that nothing ever takes over. release
// stops the heartbeat and removes the file, but only while it still holds this holder's token: the
// stalled holder must not remove it from under the successor's build. The token, written at the
// create, is the lease's identity — never its inode, which the tree volume (v9fs) hands a
// successor's file created at the same path, so os.SameFile would call the successor's lease this
// holder's.
func holdWarmLease(path, token string, heartbeat time.Duration) (release func()) {
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if !refreshWarmLease(path, token) {
					return
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-stopped
		if warmLeaseHasToken(path, token) {
			_ = os.Remove(path)
		}
	}
}

// refreshWarmLease is one heartbeat tick, and reports whether the heartbeat goes on. It reads the
// lease at path. One carrying token, this holder's, has its mtime set to now (true). One carrying
// another token is a successor's, taken over once this holder stalled past warmLeaseStale: this
// holder's heartbeat is over for good (false), the build itself running on, and nothing of the
// successor's lease is touched. One that cannot be read gets no refresh and is read again at the
// next tick (true): a read error is no proof the lease is gone, since a takeover's remove-and-create
// is two steps and a tick between them finds no file at the path, and the tree volume's 9p can fail
// a read outright (EIO) while the lease is still this holder's. The stale budget is six ticks
// (warmLeaseStale): a heartbeat ended at the first unreadable tick would let a live build's lease
// age past that, a sibling take it over and a second `codegraph init` run on the same workspace.
// The time written is taken here, at the call, never the tick's own value: a tick delivered late
// would otherwise set an mtime already old, and a lease kept fresh must say when it was last known
// live.
func refreshWarmLease(path, token string) bool {
	current, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	if string(current) != token {
		return false
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return true
}

// warmLeaseHasToken reports whether the lease at path still carries token, this holder's: the one
// question the release asks before removing the file. A lease that cannot be read is nobody's to
// remove.
func warmLeaseHasToken(path, token string) bool {
	current, err := os.ReadFile(path)
	return err == nil && string(current) == token
}

// codegraphStep is the single next codegraph subcommand nextCodegraphStep decides, or none.
type codegraphStep int

const (
	codegraphStepNone codegraphStep = iota
	codegraphStepInit
	codegraphStepIndex
)

// nextCodegraphStep decides warmCodegraphIndex's one next subcommand, pure and table-tested
// (codegraph_test.go). When `status` exits 0 and parses as JSON, its own fields decide outright
// (codegraphStatus): `initialized: false` always means `init`, even when `.codegraph/` already
// exists — CodeGraph's own `.gitignore` template (`*` then `!.gitignore`) leaves one tracked file
// behind in every `.codegraph/` a repository commits, so a fresh workspace of such a repository
// starts with the directory present and no database, and `index` on that refuses ("CodeGraph not
// initialized"). An initialized index that is current (`index.state` `"complete"`, built by the
// running CLI's own version or with no version stamp) needs nothing; one that is not — left
// partial, or built by another CodeGraph CLI version — gets an `index` repair. dirExists — whether
// `<workspace>/.codegraph` exists on disk — is consulted only as a fallback, when `status` failed
// to run or returned something that is not even JSON (a corrupted database mid-build, an old
// CLI): there, an existing directory still means `index` over `init`, since `init` on one only
// prints "Already initialized" and exits 0, repairing nothing. lockLive — whether a build holds
// `.codegraph/codegraph.lock` (codegraphLockHeldByLiveProcess) — always wins over an index
// repair, in either case: a build that is still running reports `index.state: "indexing"` and a
// null `index.builtWithVersion`, indistinguishable from one left partial by a dead process, so the
// lock is what tells the two apart, read here rather than left to CodeGraph's own judgment of it;
// and no repair of another version's index starts a second writer beside a live one. held —
// whether the record of the last warm-up build of this workspace holds (warmBuild.holds: that
// build died, under the image's CodeGraph and this cap, in this workspace) — wins over every
// build, an `init` as much as either repair: the same build on the same disk dies the same way,
// and at every role's launch (INFRA-5717 R2).
func nextCodegraphStep(dirExists, lockLive, held bool, statusExitCode int, statusStdout string) codegraphStep {
	if statusExitCode == 0 && json.Valid([]byte(statusStdout)) {
		initialized, current := codegraphStatus(statusStdout)
		switch {
		case held:
			return codegraphStepNone
		case !initialized:
			return codegraphStepInit
		case current:
			return codegraphStepNone
		case lockLive:
			return codegraphStepNone
		default:
			// Left partial, or built by another CLI version. warmCodegraphIndexOn runs this
			// `index` at most once per directory per process (indexed), and once it succeeds the
			// index's builtWithVersion is the running CLI's version, or null when the build indexed
			// no file, and codegraphStatus reads either as current, so every later start does
			// nothing: a kept volume's index is rebuilt once, never at each start.
			return codegraphStepIndex
		}
	}
	if held {
		return codegraphStepNone
	}
	if !dirExists {
		return codegraphStepInit
	}
	if lockLive {
		return codegraphStepNone
	}
	return codegraphStepIndex
}

// codegraphStatus reads `codegraph status --json`'s `initialized` field, and whether the index is
// current: `index.state` is `"complete"` and `index.builtWithVersion` is the running CLI's own
// top-level `version` or empty. `codegraph status` exits 0 whether or not the project has ever been
// indexed, so only the parsed body tells the cases apart, and anything that fails to parse as that
// shape is treated as not initialized. `index.state` is `"complete"` only once a build has
// finished; a build still running, or one an earlier warm-up left partial, reports it `"indexing"`
// with `initialized` already true and `builtWithVersion` null. A complete index is not enough: the
// worker image's CLI moved from 1.5.0 to 1.6.4, and 1.5.0 reads only the root go.mod, so its index
// misses every Go call from another package of a nested module. 1.6.4 opening an index 1.5.0 built
// reports `"version":"1.6.4"` and `"index":{"state":"complete","builtWithVersion":"1.5.0",...}`
// (measured 2026-11-26), so the state alone would never have a kept volume's blind index rebuilt.
// CodeGraph's own `reindexRecommended` is no substitute: it read false on one such index and true
// on another, and this warm-up does not read it. 1.6.4's status of an index it built itself
// reports `"version":"1.6.4"` and `"builtWithVersion":"1.6.4"`. A missing or null field decodes as
// "". A complete index with no version stamp is current: both CLIs write the stamp only when a
// build indexed at least one file (1.5.0 lib/dist/index.js:580, `result.success &&
// result.filesIndexed > 0`), so a null stamp is a build that indexed no file, not an older build.
// Only a set stamp that differs from the CLI's version is another version's index.
func codegraphStatus(stdout string) (initialized, current bool) {
	var parsed struct {
		Initialized bool   `json:"initialized"`
		Version     string `json:"version"`
		Index       struct {
			State            string `json:"state"`
			BuiltWithVersion string `json:"builtWithVersion"`
		} `json:"index"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		return false, false
	}
	return parsed.Initialized, parsed.Index.State == "complete" && (parsed.Index.BuiltWithVersion == "" || parsed.Index.BuiltWithVersion == parsed.Version)
}

// codegraphLockHeldByLiveProcess reports whether dir's .codegraph/codegraph.lock is held by a
// build: it names a process that is still alive, or it is empty and its mtime is within
// codegraphEmptyLockGrace of now. CodeGraph 1.6.4's FileLock (its dist utils.js, FileLock.create)
// writes the lock's entire content as the builder's decimal PID with no other metadata. It writes
// that PID to a temporary file, `codegraph.lock.<pid>.<threadId>.tmp`, and hard-links it to
// `.codegraph/codegraph.lock`, so the lock appears with its PID already in it; only when the link
// fails does it fall back to an exclusive create followed by the PID write, and on that path an
// empty lock is a build that has just taken it, until the grace runs out. The content and the
// mtime come from one open file, so they describe the same lock. CodeGraph's own judgment of a
// lock (readHolder) has no fixed timeout: it calls the lock stale only when the PID in it is not
// alive, or the content is empty past a grace of its own, or is not a PID, and it takes a stale
// lock over through a second lock, `codegraph.lock.break`. Any other lock this cannot read, or
// whose content isn't a PID, is treated as not live: this never blocks a repair on a lock it
// cannot make sense of. Liveness alone is not enough on a host where PIDs recycle: a dead
// builder's PID reused by an unrelated process would read as live forever, so where
// `/proc/<pid>/cmdline` exists, the process also has to look like codegraph — a dead builder whose
// PID is unused, or whose slot now holds something else, is correctly stale. `/proc` absent
// (non-Linux) falls back to the liveness check alone.
func codegraphLockHeldByLiveProcess(dir string) bool {
	lock, err := os.Open(filepath.Join(dir, ".codegraph", "codegraph.lock"))
	if err != nil {
		return false
	}
	defer lock.Close()
	content, err := io.ReadAll(lock)
	if err != nil {
		return false
	}
	text := strings.TrimSpace(string(content))
	if text == "" {
		info, err := lock.Stat()
		if err != nil {
			return false
		}
		return time.Since(info.ModTime()).Abs() < codegraphEmptyLockGrace
	}
	pid, err := strconv.Atoi(text)
	if err != nil || pid <= 0 {
		return false
	}
	// Signal 0 sends nothing; a nil error means the process exists and is signalable, and EPERM
	// means it exists but belongs to another user — both are live. Any other error (typically
	// ESRCH) means it is gone.
	sigErr := syscall.Kill(pid, 0)
	if sigErr != nil && !errors.Is(sigErr, syscall.EPERM) {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		// No /proc entry at all (process gone between the signal and this read, or a non-Linux
		// host where /proc never exists): the signal result is all there is to go on.
		return true
	}
	return strings.Contains(string(cmdline), "codegraph")
}

// codegraphResult is one codegraph run that ran: its output, and its exit status, or the signal
// that ended it, and whether that signal was the run's own codegraphTimeout or its memory cap. A
// codegraph a signal ended answered no status, as one that exited non-zero did, and the warm-up
// reads the two alike: a `status` so ended is one that answered nothing (nextCodegraphStep's
// fallback), and a build so ended leaves its index "indexing" (measured on CodeGraph 1.6.4:
// `codegraph init` killed mid-build exits in 0.03 s on SIGTERM, exit 143, and in 0.02 s on
// SIGKILL, exit 137, and either way `status --json` then reads `"initialized":true` and
// `"state":"indexing"`), which a later warm-up repairs with `codegraph index`, at most once per
// directory per process (indexed) and never where the record of that build holds (warmBuild).
// exitCode is -1 where signal is set. cappedAt is the size the run's process group was read at
// when the cap ended it, and 0 for every other end.
type codegraphResult struct {
	stdout   string
	stderr   string
	exitCode int
	signal   string
	timedOut bool
	cappedAt uint64
}

// codegraphEnvironment is the minimal environment every codegraph invocation gets: its own PATH
// lookup (so a self-exec or any PATH-relative helper it spawns still resolves), HOME (codegraph
// reads its config/cache under it), TMPDIR when the caller's own environment sets one, the size
// of CodeGraph's parse pool (codegraphParseWorkers, the count the memory cap was measured at),
// and the telemetry/update-check opt-out. Nothing else — never GH_*, LEGION_*, provider keys, or
// any other token-shaped variable a provisioning caller's own process environment may hold.
func codegraphEnvironment() []string {
	env := []string{"DO_NOT_TRACK=1", "CODEGRAPH_PARSE_WORKERS=" + codegraphParseWorkers}
	if path, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+path)
	}
	if home, ok := os.LookupEnv("HOME"); ok {
		env = append(env, "HOME="+home)
	}
	if tmpdir, ok := os.LookupEnv("TMPDIR"); ok {
		env = append(env, "TMPDIR="+tmpdir)
	}
	return env
}

// runCodegraph runs one codegraph subcommand in dir, bounded by codegraphTimeout on c and by
// codegraphMemoryCap, with codegraphEnvironment's minimal, explicit environment — never the
// caller's full environment. ctx ending, or the timeout, SIGTERMs the child (CodeGraph 1.6.4,
// SIGTERMed mid-build, exits in 0.03 s) and kills it after codegraphStopGrace; either way Run
// returns once the child is reaped, so a caller waiting on the warm-up is never held longer than
// that grace past the end of its context. start starts the child (WarmCodegraphIndex).
//
// The child leads a process group of its own (Setpgid, as procgroup.Configure sets it), which is
// what makes the cap possible: the group holds the child and everything it starts — CodeGraph's
// node, its parse workers — and nothing of the caller's, so the warm-up may end the whole group
// outright without touching the shim that runs it or that shim's agent. The group's size is read
// every codegraphMemoryPoll on c as the launcher's memory guard reads a process
// (watchCodegraphMemory, memoryguard.GroupReader), and above the cap the group gets SIGKILL: a V8
// flag would bound neither WASM nor native memory, and an address-space limit would break WASM
// outright.
func runCodegraph(ctx context.Context, c clock.Clock, start procgroup.Starter, codegraphPath, dir string, args ...string) (codegraphResult, error) {
	bounded, cancel := c.WithTimeout(ctx, codegraphTimeout)
	defer cancel()
	cmd := exec.CommandContext(bounded, codegraphPath, args...)
	cmd.Dir = dir
	cmd.Env = codegraphEnvironment()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The context's stop signals the group, as the cap's kill does: the child's own children are
	// CodeGraph's, and a stop that reached the child alone would leave them holding its pipes.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = codegraphStopGrace
	killed := procgroup.KilledByContext(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// The watch's first read is armed before the build starts, so it is armed before anything the
	// build does: a test that moves a fake clock one poll on, once its stand-in build holds its
	// memory, moves it past an armed read. This is procgroup.Run with the watch between the start
	// and the wait: the child starts through start, and done follows the wait that read its exit.
	poll := c.NewTimer(codegraphMemoryPoll)
	done, err := start(cmd)
	if err != nil {
		poll.Stop()
		return codegraphResult{stdout: stdout.String(), stderr: stderr.String()}, err
	}
	capped := watchCodegraphMemory(poll, cmd.Process.Pid)
	err = cmd.Wait()
	peak := capped()
	done()
	result := codegraphResult{stdout: stdout.String(), stderr: stderr.String()}
	switch end := procgroup.Ended(err); {
	case err == nil:
	case end.Exited != nil:
		result.exitCode = end.Exited.ExitCode()
	case end.Signal != "":
		result.exitCode, result.signal = -1, end.Signal
		// The bound's SIGTERM is the run's own timeout only while the caller's ctx still runs,
		// and the cap's own SIGKILL is neither.
		result.timedOut = peak == 0 && killed() && ctx.Err() == nil
		result.cappedAt = peak
	default:
		return result, err
	}
	return result, nil
}

// watchCodegraphMemory reads the process group led by pid each time poll fires, as the launcher's
// memory guard reads a process, resetting poll by codegraphMemoryPoll after each read, and sends
// the group SIGKILL at the first read above codegraphMemoryCap. It answers stopped: a function the
// caller calls once the command's wait has returned, which ends the watch and answers the size
// that ended the group, or 0 where the cap never did. A read that fails ends nothing: a group the
// reader cannot see holds nothing this watch can judge, and the next read follows one poll later.
//
// The watch is this warm-up's own, not the launcher's: the guard ends one process near the pod's
// limit, where a role's agent may be the process that grew most, and this ends one build well
// below it (INFRA-5717 R1).
func watchCodegraphMemory(poll clock.Timer, pid int) (stopped func() uint64) {
	stop := make(chan struct{})
	peak := make(chan uint64, 1)
	go func() {
		reader := memoryguard.NewGroupReader("/proc")
		defer reader.Close()
		defer poll.Stop()
		for {
			select {
			case <-stop:
				peak <- 0
				return
			case <-poll.C():
			}
			size, _, err := reader.Read(pid)
			if err != nil || size <= codegraphMemoryCap {
				poll.Reset(codegraphMemoryPoll)
				continue
			}
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			peak <- size
			<-stop
			return
		}
	}()
	return func() uint64 {
		close(stop)
		return <-peak
	}
}
