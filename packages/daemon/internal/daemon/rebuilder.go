// Package daemon: this file holds the rebuilder, the daemon's one observer of applied settings
// changes — what a holder of the class table's `rebuild` and `launch` rows is (rebuilt,
// credentialHolder) and where it is registered (register), how a change's holders are built
// again (observe, rebuild) and what the rebuilds a caller's change could not do answer that
// caller (noteRebuilds, route, takeFailures), how a credential that moved under the daemon with
// no setting changing is found (credentialDigest, rereadCredentials), and the bounds a
// rebuild's own work is given (drainBound, natsRebuildBound, gatedStepsQueueBound). The holders
// it rebuilds are in holders.go, nats_holder.go and runtime_holder.go.
package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// rebuilt is one thing the daemon built from its configuration and can build again while it runs:
// a holder of the class table's `rebuild` and `launch` rows (config.Row.Holder). Each one knows
// its own name there, builds the new one from the configuration that has just settled, swaps it
// behind the pointer its readers go through, and drains or closes what it replaced. Nothing above
// a holder knows a rebuild happened: a reader holds the holder, never the thing inside it.
//
// A rebuild that fails returns its error and changes nothing: the old one keeps serving, the
// daemon keeps running, and the rebuilder logs what could not be built (rebuilder.rebuild).
type rebuilt interface {
	// holder is the name the class table gives it (config.HolderNATS and its neighbours).
	holder() string
	// rebuild builds this holder again from cfg and swaps it in.
	rebuild(cfg *config.Config) error
}

// rebuilder is the daemon's one observer of applied settings changes: it reads each change's
// holder (config.Change.Holder) and builds that holder again, once per apply however many of its
// paths changed, in the order the holders were registered. A change of a `decision` path with no
// holder rebuilds nothing — its readers read the new value at their next decision.
type rebuilder struct {
	log *slog.Logger
	// live is the settings holder the daemon runs on, which register reads to record what a
	// holder's credentials said when that holder was built from them.
	live *config.Live
	// order is the registration order, and holders the registry by name: a holder the daemon did
	// not build — a test's replaced runtime, a supplied token source — is simply not registered,
	// and a change naming it is logged and does nothing.
	order   []string
	holders map[string]rebuilt
	// credentialDigests is what each credential holder was built from, by holder name: recorded
	// at registration and at every rebuild that succeeded (rebuilder.rebuild), and the guard that
	// keeps a wake of the file watcher from rebuilding a holder whose credentials have not moved
	// since (rereadCredentials).
	credentialDigests map[string]string
	// applyMu serialises a route change with the take of what that change's own rebuilds could
	// not do (route). What an apply records and what a route takes are both by path
	// (noteRebuilds writes and deletes the paths of its own apply, takeFailures reads the
	// caller's own), so two route calls that name no path in common cannot reach each other's
	// answer at all: the keys alone close that. This lock is for the case the keys do not —
	// two calls changing one path, a key two operators set at once or a caller's own retry of
	// a request it never saw answered.
	//
	// The applies themselves are serialised already — internal/settings' Keeper.mu holds one
	// route change at a time and config.Live.mu one apply — but the take that answers the
	// caller runs after the apply has let both of those go, and a second request for that same
	// path arriving in that window used to settle it first: its own rebuild of the path
	// succeeded, which drops what the first apply's rebuilds had recorded under it
	// (noteRebuilds), and the first was answered `rebuilt` while the daemon kept the connection
	// it could not replace. Held across the whole route call, this puts the take inside the
	// window that serialises the applies, so one request can never take or overwrite another's.
	//
	// The file arm's apply takes no applyMu, so a file that takes a path back while a route
	// request for that path sits between its apply and its take leaves that request reading
	// what the file's own apply recorded for it. The two are not the same answer, but the
	// daemon runs on the file's value by then, which is the later of the two: the request is
	// told what the daemon could not do for the path it asked about, as that path was last
	// applied.
	applyMu sync.Mutex
	// failed is what an applied change's rebuilds could not do, by the path of that change, as
	// the settings route answers the caller that asked for it (observe, route). It is by path
	// rather than by holder because that is what the caller is answered by: one holder's
	// failure belongs to every path of the apply that reaches that holder, and the route takes
	// the paths of its own apply alone. An apply whose rebuilds all succeeded drops the entry
	// of every path it carried, so nothing stands for a path the daemon has since built its
	// holders for; what the file's own apply records and no route call takes is dropped by the
	// next apply of that path. It has a lock of its own because the route reads it from the
	// request's goroutine, outside the settings holder's lock every rebuild runs under.
	failedMu sync.Mutex
	failed   map[string]string
}

// newRebuilder is the rebuild observer of the daemon running on live, whose snapshot every
// registration reads the new holder's credentials from.
func newRebuilder(log *slog.Logger, live *config.Live) *rebuilder {
	return &rebuilder{log: log, live: live, holders: map[string]rebuilt{},
		credentialDigests: map[string]string{}, failed: map[string]string{}}
}

// register adds holders, each in the place its name's first registration gives it. That place
// never moves and nor does the holder in it: a name already registered is a second place
// registering what one place owns, which the daemon does not do, so it is logged and left as
// it is.
//
// The order is load-bearing, which is why the rule is here and not left to the registry's
// behaviour: rebuilder.observe builds the holders of one change in registration order. What that
// buys the pair it matters for, the repository mapping and the intake consumers, and what
// registering them the other way round would cost, is said where they are registered
// (workflowRuntime.connect).
//
// A holder built from a credential is registered having just been built from it, so what that
// credential says now is recorded here: without it the first reread after boot would count every
// one of them as moved and rebuild it — reconnecting NATS, migrating every durable consumer and
// dropping every App lease — on the first wake of the file watcher, which the kubelet's refresh
// of any watched directory, `agent_secrets.token_file`'s among them, is.
func (r *rebuilder) register(holders ...rebuilt) {
	for _, h := range holders {
		if h == nil {
			continue
		}
		if _, known := r.holders[h.holder()]; known {
			r.log.Error("legion settings: a holder is registered twice; the daemon keeps the one registered first",
				"holder", h.holder())
			continue
		}
		r.order = append(r.order, h.holder())
		r.holders[h.holder()] = h
		if credentials, reads := h.(credentialHolder); reads {
			r.credentialDigests[h.holder()] = credentials.credentials(r.live.Snapshot())
		}
	}
}

// observe is what config.Live calls once every apply has settled: it is registered with
// Live.OnChange, and runs under the holder's lock, so it reads the configuration it is handed and
// never applies another change of its own.
//
// A change names the holder the caller is told was rebuilt (config.Change.Holder) and, where
// more than one thing holds the value, the rest on its row (config.Row.Also): `repository_renames`
// is held by the API server, the repository mapping, the intake consumers and the runtime's gh
// bundles, and every one of them is built again here, so no reader is left comparing names by
// the value the daemon booted on.
func (r *rebuilder) observe(cfg *config.Config, applied []config.Change) {
	named := map[string]struct{}{}
	// Every path of this apply with the holders it reaches, its own first and its row's Also
	// after it, which is the order a path with two failures reads in (noteRebuilds).
	holdersOf := make(map[string][]string, len(applied))
	for _, change := range applied {
		if change.Holder == "" {
			continue
		}
		named[change.Holder] = struct{}{}
		holders := []string{change.Holder}
		if row, known := config.ClassOf(change.Path); known {
			for _, also := range row.Also {
				named[also] = struct{}{}
				if !slices.Contains(holders, also) {
					holders = append(holders, also)
				}
			}
		}
		holdersOf[change.Path] = holders
	}
	if len(named) == 0 {
		return
	}
	failures := map[string]string{}
	for _, name := range r.order {
		if _, asked := named[name]; asked {
			if err := r.rebuild(cfg, r.holders[name]); err != nil {
				// The holder's own name is in the text, because a path whose value several
				// things hold is answered for all of them and the caller is owed the one that
				// kept what it had.
				failures[name] = fmt.Sprintf("%s: %v", name, err)
			}
		}
		delete(named, name)
	}
	for name := range named {
		r.log.Warn("legion settings: a changed setting names a holder this daemon did not build; nothing was rebuilt",
			"holder", name)
	}
	r.noteRebuilds(holdersOf, failures)
}

// rebuild builds one holder again, records what its credentials said when it was built, logs
// what it did, and answers what stopped it. A failure is logged with the holder's name and
// leaves the old one serving: a daemon that cannot reach NATS with a new seed keeps reading
// events on the connection it has, and the operator sees why in the log. What the caller that
// asked for the change is told is the apply's business (observe, noteRebuilds); a reread of the
// credential files is no caller's change, so the error it is answered here is logged and
// recorded nowhere (rereadCredentials).
//
// The digest is recorded here and at registration, and nowhere else: a holder is recorded as
// what it was built from, whichever route built it. It is read before the rebuild, so a
// credential that moved while the holder was being built is read again at the next reread
// rather than taken for the one it was built from, and a rebuild that failed records nothing,
// so the credentials it could not build on are tried again at the next reread. Recorded where a
// rebuild is decided instead — in the reread's own loop — the two drifted apart: one
// `dispatch_token_file` change rebuilt the client twice (the change's own rebuild, then the
// reread the watcher's settle runs), a route change of `github_apps.*` was rebuilt over again
// at the next unrelated wake, closing the manager that had served since — every lease dropped,
// every `token_command` waiting on a person killed — and a reread rebuild that failed was never
// retried.
func (r *rebuilder) rebuild(cfg *config.Config, holder rebuilt) error {
	name := holder.holder()
	credentials, reads := holder.(credentialHolder)
	var built string
	if reads {
		built = credentials.credentials(cfg)
	}
	if err := holder.rebuild(cfg); err != nil {
		r.log.Error("legion settings: rebuilding a holder failed; the daemon keeps the one it has",
			"holder", name, "error", err)
		return err
	}
	if reads {
		r.credentialDigests[name] = built
	}
	r.log.Info("legion settings: holder rebuilt", "holder", name)
	return nil
}

// noteRebuilds records what this apply's rebuilds could not do, by the path of each change that
// asked for them: holdersOf is every path of the apply with the holders it reaches, and
// failures is why each holder that could not be built kept what it had, by holder name. A path
// whose holders were all built again drops whatever stood for it, so nothing is answered for a
// path the daemon has since carried the change into.
//
// Two failures of one path read in the row's own order, the named holder first (holdersOf),
// since a `repository_renames` whose intake consumers would not reopen is as unfinished as one
// whose NATS connection would not, and the row names only one of the holders a change reaches.
//
// It runs inside the apply, under the settings holder's lock (config.Live.Apply,
// rebuilder.observe), which is what makes the route's take of these paths this caller's own.
func (r *rebuilder) noteRebuilds(holdersOf map[string][]string, failures map[string]string) {
	r.failedMu.Lock()
	defer r.failedMu.Unlock()
	for path, holders := range holdersOf {
		var why []string
		for _, holder := range holders {
			if reason, could := failures[holder]; could {
				why = append(why, reason)
			}
		}
		if len(why) == 0 {
			delete(r.failed, path)
			continue
		}
		r.failed[path] = strings.Join(why, "; ")
	}
}

// route is how the settings route reaches the daemon's settings holder (api.ApplySettings):
// apply, the holder's own route arm (internal/settings' Keeper.Route), and then what the
// rebuilds that change caused could not do. Without it a caller that asked for a change the
// daemon could not make — a seed the new NATS refuses, a `repository_renames` whose intake
// consumers would not reopen — was answered `rebuilt` and told nothing, while the failure sat
// in the daemon's log alone.
//
// What is answered is this apply's own: the paths it applied, taken from what its own rebuilds
// recorded under the settings holder's lock (observe, noteRebuilds), so a failure of an earlier
// change, of another caller's change or of the file's own apply is never read as this caller's.
//
// The whole call runs under applyMu, which is what makes the take this apply's own where
// another request names a path this one named too: the keys answer every other case (applyMu).
func (r *rebuilder) route(apply func(context.Context, config.Caller, string, map[string]any) ([]config.Change, []config.Refusal, error)) api.ApplySettings {
	return func(ctx context.Context, caller config.Caller, by string, changes map[string]any) ([]config.Change, []config.Refusal, api.SettingsRebuildFailures, error) {
		r.applyMu.Lock()
		defer r.applyMu.Unlock()
		applied, refused, err := apply(ctx, caller, by, changes)
		return applied, refused, r.takeFailures(applied), err
	}
}

// takeFailures empties what this apply's rebuilds recorded for the paths it applied and answers
// it, nil for none. A path no change of applied carries belongs to another apply and is left
// where it is.
func (r *rebuilder) takeFailures(applied []config.Change) api.SettingsRebuildFailures {
	r.failedMu.Lock()
	defer r.failedMu.Unlock()
	if len(r.failed) == 0 {
		return nil
	}
	var taken api.SettingsRebuildFailures
	for _, change := range applied {
		why, failed := r.failed[change.Path]
		if !failed {
			continue
		}
		if taken == nil {
			taken = api.SettingsRebuildFailures{}
		}
		taken[change.Path] = why
		delete(r.failed, change.Path)
	}
	return taken
}

// credentialHolder is a holder built from a credential that can move under the daemon with no
// setting changing: a file of this host's, read through internal/config's readers
// (config.ReadSecretPointer and its neighbours), or a value of the configuration itself. It
// says what its own rebuild would be built from as one string, beside that reader, so the
// daemon holds one list of what a holder reads rather than one here and one where it is read.
//
// What that string is, a reread compares with what the holder was built from
// (rereadCredentials), and the rebuilder records at every rebuild (rebuilder.rebuild). A
// holder's files are digested with credentialDigest; the App manager, whose private key is the
// key itself and whose command and secret are no file, digests the credentials it was built
// from instead, and that is why the comparison is a string rather than a list of files. It has
// to be compared at all because its rebuild closes the manager it replaces — every lease
// dropped, every `token_command` still waiting on a person's approval killed
// (appauth.Manager.Close) — and the file watcher wakes on the state of a whole directory, the
// one the kubelet rewrites a projected token in among them.
//
// A holder that is no credentialHolder is left alone by a reread: nothing it holds was read
// from a credential that can move under it.
type credentialHolder interface {
	rebuilt
	credentials(cfg *config.Config) string
}

// credentialDigest is what files say, as one string: each file's name beside the digest of its
// bytes, with a read failure folded in as its own text so a file that comes and goes is a change
// like any other. A file the configuration does not name is its own distinct mark, so a path
// appearing or being cleared moves the digest.
func credentialDigest(files ...string) string {
	sum := sha256.New()
	for _, file := range files {
		if file == "" {
			fmt.Fprint(sum, "\x00unnamed")
			continue
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(sum, "\x00error %s: %v", file, err)
			continue
		}
		fmt.Fprintf(sum, "\x00file %s %x", file, sha256.Sum256(contents))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// drainBound is how long a rebuild waits for what it replaced to let go of the work inside it
// before it ends that thing anyway: the whole of the NATS rebuild's wait for the intake it
// ended to settle its messages (natsHolder.rebuild), and the margin the runtime rebuild adds to
// the bound of the runtime it replaced (runtimeDrainBound). It is longer than the ack wait a
// consumer is built with (`worker_rpc_timeout_seconds`, 5 s by default), so a message that is
// going to settle has its turn, and short enough that an intake that will never settle does not
// hold the settings change for ever: the daemon then closes the old connection and logs that it
// did, where the messages still in flight are redelivered by the durable consumers the new
// connection holds.
//
// It is no bound for a launch and the runtime rebuild does not take it for one. One Spawn or
// Resume try may rightfully run for the whole of the runtime's LaunchBound — the Sandbox
// runtime's is `worker_boot_timeout_seconds` several times over plus its provision bound, which
// is settled when that runtime is built and no caller can raise under it, since that timeout is
// class `rebuild` held by the runtime (config.Classes), so a change of it builds another runtime
// rather than lengthening the one a launch is inside — so that rebuild waits the replaced
// runtime's own bound with this as the margin beyond it (runtimeDrainBound), and only past that
// is the old runtime ended and the call inside it left with a cancelled context.
//
// That wait is taken under the settings holder's lock, which every other settings change takes:
// a rebuild runs either from an apply's observers (config.Live.Apply holds l.mu for the whole
// apply) or from the watcher's reread (config.Live.WhileSettled), so for as long as a rebuild
// drains, another route call and the file watcher's own settle wait behind it. That is the
// trade this daemon takes, and it is taken deliberately: the alternative — closing what was
// replaced from a goroutine of its own — would answer the operator `rebuilt` while the daemon
// still held the connection or the runtime it says it replaced, and would let the next rebuild
// of that holder start while the one before it was still letting go, with two drains of one
// holder in flight and no order between their ends. A drain is rare (nothing is in flight in
// the ordinary case, and the wait returns at once) and bounded, by the replaced runtime's own
// launch bound where that is the longer, which is the one wait that may hold a settings change
// for as long as a launch; a rebuild answering for work it has not finished would be neither.
const drainBound = 2 * time.Minute

// natsRebuildBound bounds the work of one NATS rebuild: the dial, the four consumer opens and
// the handover's one read-only transaction (natsHolder.rebuild). A rebuild is neither a boot
// attempt nor a request, so it takes neither's context — boot's is cancelled the moment
// workflowRuntime.connect returns (readinessAttempt), and a route's when the answer is written
// — and makes its own, at the rebuild and for that rebuild alone. It is what boot's own
// attempt gives the identical work (bootTimeout), and a rebuild that runs this long holds a
// NATS or a Postgres that stopped answering, which must not hold every other settings change
// behind the settings holder's lock for ever.
const natsRebuildBound = bootTimeout

// gatedStepsQueueBound bounds the one transaction a live `gates.design` change spends queueing
// the publish of the new list (gatedStepsHolder, workflowRuntime.queueGatedStepsPublish): a
// single upsert of one outbox row. It is a context of the rebuild's own for the reason
// natsRebuildBound is, and short because the row is sent by the outbox and not here.
const gatedStepsQueueBound = 30 * time.Second

// rereadCredentials builds every holder whose credentials have moved since that holder was
// built again, whatever the configuration says: the answer to a rotation of a file whose path
// did not change, which no settings change names, since the path is what the file holds and the
// path has not moved. It runs in the order the holders were registered, and over the holders
// that were built from a credential at all (credentialHolder).
//
// The file watcher calls it, once the file arm has settled the configuration, and calls it
// under the settings holder's own lock (config.Live.WhileSettled), where an applied change's
// rebuilds run: a reread and a route change never build one holder at once, and the
// configuration it reads is the one the daemon is running on.
//
// The digest guard is what keeps an unrelated wake cheap. The watcher wakes on the state of a
// whole directory, so a projected token the kubelet refreshed beside a seed that did not move
// settles the configuration again; without the guard that wake would reconnect NATS and migrate
// every durable consumer, drop every App lease and rewrite the deployment instructions. A holder
// whose credentials read the same holds what they say already, so rebuilding it is work with no
// answer of its own. What each holder was built from is recorded as it is registered (register)
// and at every rebuild that succeeded (rebuilder.rebuild), so the first reread after boot
// rebuilds nothing, a holder a route change has just rebuilt is left alone at the next wake, and
// a rebuild that failed is tried again at the next reread.
func (r *rebuilder) rereadCredentials(cfg *config.Config) {
	for _, name := range r.order {
		holder, reads := r.holders[name].(credentialHolder)
		if !reads || r.credentialDigests[name] == holder.credentials(cfg) {
			continue
		}
		// A reread is no caller's change: a rebuild that failed is logged by rebuild itself and
		// tried again at the next reread, and nothing of it is answered to a settings route
		// call that asked for no such rebuild.
		_ = r.rebuild(cfg, holder)
	}
}
