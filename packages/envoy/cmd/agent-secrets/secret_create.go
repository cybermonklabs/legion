// packages/envoy/cmd/agent-secrets/secret_create.go
//
// The stretch of `secret create` between the store making a secret and that secret holding its
// value, or being gone. On GCP the secret is made, empty, before its value is read
// (newSecret.begin), and from then until store succeeds or discard has run, neither a failure
// nor a signal may leave the empty secret behind, and nothing may delete a secret that may hold
// the value. Each store call of the stretch has a deadline (storeCallDeadline), and a second
// signal ends the form at once rather than waiting on the call the first one's cleanup waits on.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// promptBusy is held by the value prompt for as long as it reads (readHiddenAtTerminal locks it
// first and unlocks it last, after any end by a signal). A signal the prompt catches, it ends the
// form by itself once it has restored the terminal, so watchCreateSignals waits on promptBusy
// rather than ending the form under the prompt.
var promptBusy sync.Mutex

// createSignals are the signals that end a form while it holds a secret it has not seen through:
// a hang-up, an interrupt, a quit and a termination.
var createSignals = []os.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM}

// storeCallDeadline bounds each store call `secret create` makes from CreateSecret until the value
// is stored or the discard is done, so that a call the store never answers holds neither the form
// nor a signal waiting on it (pendingCreate) for longer. It is 60 s, Secret Manager's Go client's
// own default on each of those calls (CreateSecret, AddSecretVersion, ListSecretVersions and
// DeleteSecret: defaultCallOptions in cloud.google.com/go/secretmanager/apiv1), which that client
// applies only to a call whose context carries no deadline of its own. The AWS SDK puts none on a
// call - its defaults bound the dial, at 30 s, and the TLS handshake, at 10 s, not the wait for an
// answer - so AWS's one CreateSecret takes the same 60 s. Tests shorten it.
var storeCallDeadline = 60 * time.Second

// withStoreDeadline is ctx bounded by storeCallDeadline, for one store call.
func withStoreDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, storeCallDeadline)
}

// storeCallDeadlineText is storeCallDeadline as a line says it: whole seconds as such ("60s"),
// any other length as Go writes it.
func storeCallDeadlineText() string {
	if storeCallDeadline >= time.Second && storeCallDeadline%time.Second == 0 {
		return strconv.FormatInt(int64(storeCallDeadline/time.Second), 10) + "s"
	}
	return storeCallDeadline.String()
}

// onSecondSignal, while set, says what a second signal ends the form on (endAtOnce): the state the
// store may be left in and the command that clears it. Create sets it beside onSignalDeath
// (pendingCreate.endingAtOnce); nil otherwise.
var onSecondSignal func(sig os.Signal)

// cleanupBegun is closed once a signal's end of the form has begun its cleanup (endBySignal), which
// is after the prompt, where it was reading, has restored the terminal: a second signal ends the
// form at once from then on (endAtOnce), and never under the prompt.
var (
	cleanupBegun = make(chan struct{})
	beginCleanup = sync.OnceFunc(func() { close(cleanupBegun) })
)

// watchCreateSignals catches each of createSignals that this process did not inherit as ignored,
// from now until stop is called, and ends the form by the first that comes (endBySignal), which
// runs onSignalDeath first. While the value prompt reads, a signal is the prompt's to act on, so
// that end waits for the prompt to finish first (promptBusy). It keeps watching once it has a
// signal: a second one, once the first one's cleanup has begun, ends the form at once by that
// second signal (endAtOnce), rather than waiting on a store call the cleanup waits on. stop never
// returns once a signal was taken, since the form is ending by it; a signal that came with the
// stop still ends the form. It works through os/signal alone, on every platform this command
// builds for; how the form then ends by the signal is the platform's (raiseSignal).
func watchCreateSignals() (stop func()) {
	var watched []os.Signal
	for _, sig := range createSignals {
		if !signal.Ignored(sig) {
			watched = append(watched, sig)
		}
	}
	if len(watched) == 0 {
		// signal.Notify naming no signal relays every signal.
		return func() {}
	}
	signals := make(chan os.Signal, len(watched))
	signal.Notify(signals, watched...)
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		select {
		case sig := <-signals:
			go endByCaughtSignal(sig)
		case <-done:
			close(exited)
			return
		}
		second := <-signals
		<-cleanupBegun
		endAtOnce(second)
	}()
	return sync.OnceFunc(func() {
		close(done)
		<-exited
		signal.Stop(signals)
		select {
		case sig := <-signals:
			endByCaughtSignal(sig)
		default:
		}
	})
}

// endByCaughtSignal ends the form by sig, which watchCreateSignals caught, once the prompt, where
// it is reading, has finished.
func endByCaughtSignal(sig os.Signal) {
	promptBusy.Lock()
	endBySignal(sig)
}

// endBySignal ends the form by sig, a hang-up, an interrupt, a quit or a termination that came
// while it held a secret it had not seen through, or while the prompt read a value: from the
// prompt's reader, once it has restored the terminal, or from watchCreateSignals. It runs
// onSignalDeath first, when that is set, during which a second signal ends the form at once
// (watchCreateSignals, endAtOnce). Then it ends the process by sig (exitBySignal).
func endBySignal(sig os.Signal) {
	if cleanup := onSignalDeath; cleanup != nil {
		beginCleanup()
		cleanup()
	}
	exitBySignal(sig)
}

// endAtOnce ends the form by sig, a second signal that came while the first one's cleanup
// (onSignalDeath) may still wait on a store call, without waiting on it: onSecondSignal first says
// what the store may be left holding and the command that clears it.
func endAtOnce(sig os.Signal) {
	if say := onSecondSignal; say != nil {
		say(sig)
	}
	exitBySignal(sig)
}

// exitBySignal ends the process by sig: back at its default action, the platform raises it where
// it can (raiseSignal), and the exit status a shell gives for sig is the fallback.
func exitBySignal(sig os.Signal) {
	signal.Reset(sig)
	if raiseSignal(sig) {
		time.Sleep(time.Second)
	}
	os.Exit(signalExitCode(sig))
}

// signalExitCode is the status a shell reports for a process sig ended.
func signalExitCode(sig os.Signal) int {
	switch sig {
	case syscall.SIGHUP:
		return exitHangup
	case syscall.SIGTERM:
		return exitTerminated
	case syscall.SIGQUIT:
		return exitQuit
	default:
		return exitInterrupted
	}
}

// signalNoun names sig as a line says it.
func signalNoun(sig os.Signal) string {
	switch sig {
	case syscall.SIGHUP:
		return "hang-up"
	case syscall.SIGINT:
		return "interrupt"
	case syscall.SIGQUIT:
		return "quit"
	case syscall.SIGTERM:
		return "termination"
	}
	return sig.String()
}

// discardOutcome is what a failed newSecret.begin or a newSecret.discard left in the store. A
// discard answers an error exactly when its outcome is one of the kept ones, the error naming what
// stays and the command that clears or checks it; begin's own failure is an error whatever its
// outcome. pendingCreate.rereadOutcome reads it to decide whether the broker rereads the name.
type discardOutcome int

const (
	// madeNothing: nothing was made, so there was nothing to remove (AWS, or a create the store
	// refused before acting).
	madeNothing discardOutcome = iota
	// deletedEmpty: the empty secret this create made is deleted, or was already gone.
	deletedEmpty
	// keptEmpty: the store refused the delete; this create's empty secret stays.
	keptEmpty
	// keptMayHoldValue: the secret holds a version, whether this create or another writer added
	// it, or this create's add may still land, so it stays.
	keptMayHoldValue
	// keptOthers: the name is held by a secret this create did not make, or one that changed before
	// the delete, so it is left as it is.
	keptOthers
	// keptUnknown: what the name holds could not be read, or the secret's versions could not be
	// listed, so it stays.
	keptUnknown
)

// pendingCreate owns a create from before its first store call until its value is stored or its
// discard has run (runCreate holds that order). mu is held across begin, store and discard, so a
// signal waits for the call in flight before cleaning up. The backend settles a failed begin
// itself; discard runs only after begin succeeds. A second signal waits for none of it
// (endingAtOnce). Whatever the stretch ends in is reported once: by the form, which reports only
// once the stretch is over (runCreate), or by a signal's cleanup (interrupted), after which the
// stretch never ends, since the form ends by the signal.
type pendingCreate struct {
	s              *secretSession
	form, name     string
	secretType     string // the type tag the create writes, "" for none, which its reread is held to (rereadValue)
	owner, tier    string // the owner and tier tags the created line names
	stdout, stderr io.Writer
	created        newSecret

	mu        sync.Mutex
	begun     bool
	stored    bool
	discarded bool
	// failure is what the form reports should it end now: begin's failure, the store's, or, once
	// the discard has run, that failure beside what the discard left (abandonLocked); nil for none.
	failure error
}

// runCreate is p's stretch, in its order: the watch on the signals, begin, the read of the value
// (readValue, handed the deadline its read ends at: valueReadWindow after the store is asked to
// make the secret, where begin makes one that waits for its value, and the zero time, no bound,
// where it makes nothing), store, and abandon after any failure. It ends the stretch before it
// reports anything, the failure, or the value stored and the broker's reread, held to the type the
// create wrote (rereadValue), so a signal that comes as it reports finds no cleanup to run, and one
// that came before keeps the stretch from ending: interrupted reports instead. It answers the
// form's exit code, and is a function rather than a method of pendingCreate because broker-refgen
// reads every exit code this package returns, and it follows a plain call, never a method's.
func runCreate(p *pendingCreate, readValue func(deadline time.Time) (string, error)) int {
	end := p.watch()
	defer end()
	// The broker holds a secret waiting for its value for valueReadWindow from its making, so the
	// window counts from before the store is asked to make it: the create gives up, and discards
	// it, no later than the broker stops waiting.
	asked := time.Now()
	if err := p.begin(); err != nil {
		end()
		return secretFailIn(p.stderr, p.form, p.s, err)
	}
	var deadline time.Time
	if p.created.madeBeforeValue() {
		deadline = asked.Add(valueReadWindow)
	}
	value, err := readValue(deadline)
	if err == nil {
		err = p.store(value)
	}
	if err != nil {
		err = p.abandon(err)
	}
	end()
	if err != nil {
		return secretFailIn(p.stderr, p.form, p.s, err)
	}
	p.sayStored()
	noteEdgeSpaces(p.stderr, p.form, p.name, value)
	return rereadValue(p.s, p.stdout, p.stderr, p.form, p.name, p.secretType)
}

// sayStored prints the line that says the create stored the secret with its value.
func (p *pendingCreate) sayStored() {
	fmt.Fprintf(p.stdout, "created %s in %s (owner=%s, tier=%s)\n", p.name, p.s.storeName(), p.owner, p.tier)
}

// watch makes p what a signal ends the form on, onSignalDeath and onSecondSignal, and starts
// watchCreateSignals. The func it answers ends the stretch: it stops the watch and clears both,
// once, however often it is called, so a form calls it as soon as the stretch is over and defers
// it as well.
func (p *pendingCreate) watch() (end func()) {
	onSignalDeath, onSecondSignal = p.interrupted, p.endingAtOnce
	stop := watchCreateSignals()
	return sync.OnceFunc(func() {
		stop()
		onSignalDeath, onSecondSignal = nil, nil
	})
}

// begin makes any value-free store call while signal cleanup waits for its answer. Its failure,
// which says what its own recovery left, is what the form reports (p.failure), by run or by a
// signal's cleanup, once.
func (p *pendingCreate) begin() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.discarded {
		return fmt.Errorf("the create of %s was interrupted before it began", p.name)
	}
	p.begun = true
	outcome, err := p.created.begin(p.s.ctx)
	if err != nil {
		p.discarded, p.failure = true, err
		p.rereadOutcome(outcome)
	}
	return err
}

// store writes value as the secret's first value. Its failure, which says what the store may hold
// (AWS: whether it may have made the secret), is what the form reports (p.failure) until the
// discard has run.
func (p *pendingCreate) store(value string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.discarded {
		return fmt.Errorf("the secret %s was discarded before its value was stored", p.name)
	}
	err := p.created.store(p.s.ctx, value)
	p.stored, p.failure = err == nil, err
	return err
}

// abandon discards what create made after err, the failure that stopped the value being stored,
// and answers the error the form reports (abandonLocked).
func (p *pendingCreate) abandon(err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.abandonLocked(err)
}

// abandonLocked runs the discard once, under mu, after err, the failure that stopped the value
// being stored (nil where a signal ended the read), and answers, and keeps in p.failure, the error
// the form reports: err itself where the store held nothing to discard, so a usage error still
// exits 2 having written nothing; err beside the line that the empty secret the create made is
// deleted and nothing stays, its exit code kept; and otherwise err beside what stays, which exits
// 1, the store holding a secret it did not hold before. After deleting the empty secret it asks the
// broker to reread the name, so a refusal or a wait for a value the broker logged for the secret
// meanwhile ends at once; the create has failed already, so that reread's own failure changes
// nothing it reports. A secret that may hold the value is reread with its answer printed, which
// says whether the broker serves it.
func (p *pendingCreate) abandonLocked(err error) error {
	if p.discarded {
		if p.failure != nil {
			return p.failure
		}
		return err
	}
	p.discarded = true
	outcome, left := p.created.discard(p.s.ctx)
	p.rereadOutcome(outcome)
	switch {
	case left != nil && err != nil:
		p.failure = fmt.Errorf("%v; %v", p.s.refusedAs(err), p.s.refusedAs(left))
	case left != nil:
		p.failure = left
	case outcome == deletedEmpty && err != nil:
		stated := fmt.Errorf("%v; %s", p.s.refusedAs(err), p.created.unsettled())
		if errors.As(err, new(usageErr)) {
			stated = usageErr{stated}
		}
		p.failure = stated
	case outcome == deletedEmpty:
		p.failure = errors.New(p.created.unsettled())
	default:
		p.failure = err
	}
	return p.failure
}

// interrupted is onSignalDeath while p is pending: the cleanup of a signal that ends the form, run
// once the call in flight has answered, which reports what the stretch ended in, since the form,
// ending by the signal, never will. A value stored is said stored, as the form says it, and the
// broker asked to reread the name; otherwise it discards what create made, unless that has run,
// and reports the failure beside what the discard left (abandonLocked). The signal then decides
// the exit status.
func (p *pendingCreate) interrupted() {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case !p.begun:
		p.discarded = true
		return
	case p.stored:
		p.sayStored()
		reread(p.s, p.stdout, p.stderr, p.form, p.name, true, nil)
		return
	}
	if failure := p.abandonLocked(p.failure); failure != nil {
		secretFailIn(p.stderr, p.form, p.s, failure)
	}
}

// endingAtOnce is onSecondSignal while p is pending: it says what the store may be left holding,
// should the form end now without waiting on the store call in flight, and the command that clears
// it (newSecret.unsettled). It takes no lock, since the first signal's cleanup may hold mu across
// that very call.
func (p *pendingCreate) endingAtOnce(sig os.Signal) {
	fmt.Fprintf(p.stderr, "agent-secrets %s: a second signal (%s) ends the create before the first one's cleanup is done: %s\n", p.form, signalNoun(sig), p.created.unsettled())
}

// rereadOutcome updates the broker after either recovery from begin or the later discard.
func (p *pendingCreate) rereadOutcome(outcome discardOutcome) {
	switch outcome {
	case deletedEmpty:
		_, _ = p.s.broker.RereadSecret(context.WithoutCancel(p.s.ctx), p.name)
	case keptMayHoldValue:
		reread(p.s, p.stdout, p.stderr, p.form, p.name, true, nil)
	}
}
