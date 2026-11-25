// packages/envoy/cmd/cipher/signin_renewer.go
//
// The renewal of a person's sign-in enrollment on this machine, as `agent-secrets renew` renews a
// box's or a pod's. A sign-in's enrollment is leased for BROKER_LEASE_SECONDS like any other, and
// its grants and its requests still waiting for a person end with it: the broker's sweep revokes
// the one and cancels the other ("the requesting enrollment ended: its lease lapsed"). So each
// session form that signs as a sign-in keeps it (keepSignIn): it records that it used the sign-in
// now, and starts the sign-in's one renewer where none runs, `agent-secrets renew --sign-in`, in a
// session of its own beside no caller. A request it leaves waiting for a person is recorded too
// (keepPending), and a use command that waits for a person looks each second that a renewer runs
// (watchRenewer).
//
// The renewer renews the enrollment the sign-in's record names, each time a third of the lease
// has passed, while the machine needs it: a command used the sign-in less than one lease ago, a
// recorded request still waits for a person, or a grant of the enrollment that has an end is still
// live. It asks the broker the last two only once the first no longer holds. A grant with no end,
// one that lasts until its session ends, keeps it no longer than use does: a sign-in's session is
// its one enrollment, which every process of the person on the machine signs as. It stops, and the
// lease then lapses within one lease, once none holds; once the record names no enrollment or is
// gone (the person took the sign-in off the machine); once the broker refuses the enrollment for
// good (a person revoked the sign-in or its machine login, or its sign-in credential lapsed),
// exit 76, recording the refused enrollment so that no later renewer sends it again; and once for
// the blind limit no renewal got through, or no answer said whether anything needs the
// enrollment, exit 1. A new sign-in writes a record naming a new enrollment, which the renewer
// renews from its next renewal on. The renewer never registers an enrollment: a command that
// finds the enrollment ended registers the next one, as before, and keeps that one in turn. A
// renewer killed with its machine or its process tree renews nothing until a command starts one.
//
// While it renews, the renewer also keeps the sign-in's launcher credential live (keepCredential):
// the broker renews a sign-in's session only while that credential is live (enroll.Service.Renew,
// ErrSignInCredentialEnded), and its expiry is the person's last traded Google identity token's,
// about an hour. So the renewer trades the person's identity again (tradeSignIn) after its first
// renewal and each time a third of the credential's remaining life has passed, which moves the
// expiry of the same credential row. A trade that fails is tried again once the smaller of a
// third of the lease and a third of the credential's remaining life has passed, never sooner than
// renew's longest backoff; a trade is made only between renewals, so a renewal the broker keeps
// failing holds the next trade back. On a machine told of no sign-in client nothing is traded at
// all, and a trade whose answer names no credential expiry still to come ends the trading too,
// said once. A trade that cannot be made, as for a person whose Google account is suspended,
// therefore lets the credential lapse within its life, the broker refuses the next renewal, and
// the enrollment ends one lease later: access ends without anybody's act.
//
// One renewer runs for each sign-in: it holds the exclusive flock of the renewal directory's
// lock file for its whole life, and a command starts one only where it can take that lock
// itself. Every stop goes through stop: the renewer lets the lock go first and then reads the
// state again, taking the lock back where a command wrote what it did not see when it decided
// (for a refusal, only a record naming another enrollment): a command writes its use before it
// tries the lock, so either the command finds the lock free and starts a renewer, or the renewer
// that is stopping sees the use.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/leasetime"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/googleidentity"
	"github.com/sjawhar/envoy/internal/redact"
)

// The renewal directory's files (cipherclient.SignInStore.RenewalDir), mode 0600 in a 0700
// directory beside the sign-in's record.
const (
	// renewalLockFile is the file whose flock the one renewer holds. It holds the enrollment the
	// broker last refused for good, or nothing (keptSignIn.refused).
	renewalLockFile = "lock"
	// renewalUsedFile's modification time is when a command last used the sign-in.
	renewalUsedFile = "used"
	// renewalPendingDir holds one empty file for each request a command left waiting for a
	// person, named by the request's id.
	renewalPendingDir = "pending"
	// renewalLogFile is the renewer's standard output and standard error.
	renewalLogFile = "renewer.log"
)

// renewalLogLimit bounds the renewer's log: a command that starts a renewer empties a log that has
// grown past it first, so a machine that runs for months keeps a log of the recent renewers.
const renewalLogLimit = 256 << 10

// renewerPoll is the longest a renewer sleeps before it looks again whether its sign-in is still on
// this machine, so a sign-in taken off the machine stops its renewer within that time rather than
// a third of a lease.
const renewerPoll = 30 * time.Second

// renewerStageVar marks the first of the two processes a command starts to run a renewer: that
// process starts the renewer in a session of its own and exits at once, so the renewer is no child
// of the command, nor of what the use command then becomes, and nothing waits on it or reaps it.
const renewerStageVar = "AGENT_SECRETS_SIGNIN_RENEWER_STAGE"

// renewerStageDetach is renewerStageVar's one value.
const renewerStageDetach = "detach"

// renewalName is the renewal directory as a refusal names it: beside a record that holds a box key
// and a refresh token, so by where it is and never by its path (cipherclient.SignInStoreName).
const renewalName = "the sign-in's renewal directory in " + cipherclient.SignInStoreName

// signInRenewFlag is `renew --sign-in`'s usage line.
const signInRenewFlag = "renew this machine's sign-in enrollment while it is in use; the commands that sign as the sign-in start this themselves"

// renewerEnvironment is the environment a renewer keeps of the command that starts it, beside
// every AGENT_SECRETS_ variable: where the sign-in, the settings file and the state are, what
// reaching the broker over TLS and through a proxy needs, and where this machine's own identity
// is found for the trade that keeps the sign-in's credential live (machineSource: a credentials
// file, or the metadata server's host). It holds nothing the renewer does not read, so a renewer
// that runs for hours keeps no copy of the rest of a caller's environment.
var renewerEnvironment = map[string]bool{
	"HOME": true, "USER": true, "LOGNAME": true, "PATH": true, "TMPDIR": true, "TZ": true,
	"XDG_CONFIG_HOME": true, "XDG_STATE_HOME": true, "XDG_RUNTIME_DIR": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"HTTPS_PROXY": true, "https_proxy": true, "HTTP_PROXY": true, "http_proxy": true,
	"NO_PROXY": true, "no_proxy": true, "ALL_PROXY": true, "all_proxy": true,
	"GOOGLE_APPLICATION_CREDENTIALS": true, googleidentity.MetadataHostVar: true,
}

// keptSignIn is the renewal of one person's sign-in on this machine: the record's path and the
// renewal directory beside it.
type keptSignIn struct {
	person, record, dir string
}

func renewalOf(person string) keptSignIn {
	store := cipherclient.SignIns(homeGetenv)
	return keptSignIn{person: person, record: store.Path(person), dir: store.RenewalDir(person)}
}

func (r keptSignIn) path(name string) string { return filepath.Join(r.dir, name) }

// signInPerson is the person whose sign-in signer is, and whether it is a sign-in's: the one test
// of which signers a command keeps renewed.
func signInPerson(signer Signer) (string, bool) {
	signIn, ok := signer.(*fileSigner)
	if !ok || signIn.person == "" || !renewalSupported {
		return "", false
	}
	return signIn.person, true
}

// keepSignIn records that this command uses person's sign-in now, and starts the sign-in's renewer
// where none runs. It answers why it could not; the command goes on either way (notKept).
func keepSignIn(person string) error {
	if !renewalSupported {
		return nil
	}
	r := renewalOf(person)
	if err := r.prepare(); err != nil {
		return err
	}
	now := time.Now()
	if err := os.Chtimes(r.path(renewalUsedFile), now, now); err != nil {
		return fmt.Errorf("record this use in %s: %w", renewalName, redact.File(err))
	}
	return r.ensureRenewer()
}

// keepPending records that signer's request requestID waits for a person, where signer is a
// person's sign-in, so its renewer keeps the enrollment until the request is decided, cancelled or
// expired. A request it could not record is said on stderr: the command goes on.
func keepPending(signer Signer, requestID string, stderr io.Writer) {
	person, ok := signInPerson(signer)
	if !ok {
		return
	}
	if err := renewalOf(person).notePending(requestID); err != nil {
		fmt.Fprintf(stderr, "agent-secrets: %s\n", notKept(person, err))
	}
}

// watchRenewer starts signer's sign-in renewer again, each renewerWatch, where none runs, for a
// command that waits on the enrollment: the use command's wait for a person. A renewer killed
// meanwhile renews nothing until a command starts one, and the waiting command may be the only
// one. It stops watching once the sign-in is gone from the machine, and writes nothing on stderr:
// the command said at its start whether it could keep the sign-in renewed. It answers the stop,
// which returns once the watch has ended; for any other signer than a sign-in's it watches
// nothing.
func watchRenewer(signer Signer) (stop func()) {
	person, ok := signInPerson(signer)
	if !ok {
		return func() {}
	}
	renewal := renewalOf(person)
	done, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		tick := time.NewTicker(renewerWatch)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if _, err := os.Stat(renewal.record); errors.Is(err, fs.ErrNotExist) {
					return
				}
				_ = renewal.ensureRenewer()
			}
		}
	}()
	return func() {
		close(done)
		<-ended
	}
}

// renewerWatch is how often a waiting command looks for its sign-in's renewer: one flock attempt
// on the renewal directory's lock, and a process start only where no renewer holds it. A second
// is far inside the shortest lease a broker takes (91 s, leasetime.Production().Shortest).
const renewerWatch = time.Second

// notKept is the line a command writes when it cannot keep person's sign-in renewed.
func notKept(person string, err error) string {
	return fmt.Sprintf("%s's sign-in enrollment is not kept renewed (%v), so its grants and its requests waiting for a person end when its lease lapses", person, err)
}

// notePending records requestID, which the renewer reads back with GET /v1/requests/{id}: an id
// that call would refuse (pathID) is refused here.
func (r keptSignIn) notePending(requestID string) error {
	if err := pathID("a request id", requestID); err != nil {
		return err
	}
	if requestID == "" || requestID == "." || requestID == ".." || filepath.Base(requestID) != requestID {
		return errors.New("the broker's request id is not a single path element (not printed)")
	}
	if err := r.prepare(); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(r.dir, renewalPendingDir, requestID), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("record the pending request in %s: %w", renewalName, redact.File(err))
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("record the pending request in %s: %w", renewalName, redact.File(err))
	}
	return r.ensureRenewer()
}

// prepare makes the renewal directory, its pending directory and its used file where they are not
// there, each its owner's alone. Only a command makes them: the renewer makes nothing, so a
// directory taken away with its sign-in, or a test's home, stays gone.
func (r keptSignIn) prepare() error {
	for _, dir := range []string{r.dir, r.path(renewalPendingDir)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("make %s: %w", renewalName, redact.File(err))
		}
	}
	f, err := os.OpenFile(r.path(renewalUsedFile), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("make %s: %w", renewalName, redact.File(err))
	}
	return f.Close()
}

// refused is the enrollment the broker last refused for good, as the renewer that met the refusal
// wrote it into the lock file, or "".
func (r keptSignIn) refused() string {
	raw, err := os.ReadFile(r.path(renewalLockFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// recordRefused writes enrollmentID into the lock file, which the renewer holds and which exists
// whenever a renewer runs, so nothing new is made in the directory.
func (r keptSignIn) recordRefused(enrollmentID string) {
	f, err := os.OpenFile(r.path(renewalLockFile), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return
	}
	_, _ = f.WriteString(enrollmentID + "\n")
	_ = f.Close()
}

// ensureRenewer starts the sign-in's renewer unless one holds its lock, the sign-in is gone, or
// its record names the enrollment the broker refused for good, which no renewer would renew.
func (r keptSignIn) ensureRenewer() error {
	record, err := cipherclient.SignIns(homeGetenv).Read(r.record)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && record.EnrollmentID != "" && record.EnrollmentID == r.refused()) {
		return nil
	}
	release, held, err := tryRenewerLock(r.path(renewalLockFile), true)
	if err != nil {
		return fmt.Errorf("lock %s: %w", renewalName, redact.File(err))
	}
	if held {
		return nil
	}
	release()
	return launchRenewer(r)
}

// launchRenewer starts a renewer for a sign-in (keptSignIn.startRenewer). Nothing in this program
// replaces it: this package's TestMain does, since a test that runs a command in its own process
// (runIn) is the test binary, which this program's own path would start again as a renewer.
var launchRenewer = keptSignIn.startRenewer

// startRenewer starts `agent-secrets renew --sign-in` for r's person, through a first process that
// starts it in a session of its own and exits (renewerStageVar), writing to the renewal log. It
// waits for that first process alone, for renewerStartBound at most: it does nothing but start a
// process, and a command must not hang on it.
func (r keptSignIn) startRenewer() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find this program to start the renewer: %w", redact.File(err))
	}
	logPath := r.path(renewalLogFile)
	if info, err := os.Stat(logPath); err == nil && info.Size() > renewalLogLimit {
		_ = os.Truncate(logPath, 0)
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open the renewer's log in %s: %w", renewalName, redact.File(err))
	}
	defer log.Close()
	ctx, cancel := context.WithTimeout(context.Background(), renewerStartBound)
	defer cancel()
	// The renewer runs in /, so the paths the command's environment names relative to where it
	// runs are made absolute here (renewerEnv); a working directory this process cannot read
	// leaves them as they came.
	dir, _ := os.Getwd()
	cmd := exec.CommandContext(ctx, self, "renew", "--sign-in")
	cmd.Env = append(renewerEnv(os.Environ(), r.person, dir), renewerStageVar+"="+renewerStageDetach)
	cmd.Stdout, cmd.Stderr, cmd.Dir = log, log, "/"
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("start the renewer: %w", err)
	}
	return nil
}

// renewerStartBound is how long a command waits for the first process that starts a renewer, which
// takes milliseconds; one that takes longer is killed, and the command goes on without a renewer.
const renewerStartBound = 10 * time.Second

// renewerEnv is environ as a renewer of person's sign-in keeps it (renewerEnvironment), naming
// person in cipherclient.PersonVar so the renewer renews that sign-in whoever else signed in here,
// and with each path a kept variable names made absolute against dir, the starting command's
// working directory (absoluteIn): the renewer runs in /, and must read the files the command
// read. dir "" leaves every value as it came.
func renewerEnv(environ []string, person, dir string) []string {
	kept := make([]string, 0, len(renewerEnvironment)+8)
	for _, variable := range environ {
		name, value, _ := strings.Cut(variable, "=")
		if name == cipherclient.PersonVar || name == renewerStageVar {
			continue
		}
		if renewerEnvironment[name] || strings.HasPrefix(name, "AGENT_SECRETS_") {
			kept = append(kept, name+"="+absoluteIn(dir, name, value))
		}
	}
	return append(kept, cipherclient.PersonVar+"="+person)
}

// renewerPaths are the kept variables that name one file or directory: the credentials file and
// the token file the trade proves this machine's identity with, the sign-in client's secret file,
// the sign-in store, the TLS roots, and the home, state, configuration, runtime and temporary
// directories every other path is found under.
var renewerPaths = map[string]bool{
	"HOME": true, "TMPDIR": true, "XDG_CONFIG_HOME": true, "XDG_STATE_HOME": true, "XDG_RUNTIME_DIR": true,
	"SSL_CERT_FILE": true, "GOOGLE_APPLICATION_CREDENTIALS": true,
	cipherclient.SignInDirVar: true, cipherclient.SignInClientSecretFileVar: true, cipherclient.MachineTokenFileVar: true,
	"AGENT_SECRETS_KEY_DIR": true, "AGENT_SECRETS_HELPER_SOCK": true,
}

// absoluteIn is value, the value of the variable name, with each relative path it names joined to
// dir: one path for a variable of renewerPaths, each entry of the list SSL_CERT_DIR holds, and
// nothing of any other variable. An empty value, or an empty dir, is left as it came.
func absoluteIn(dir, name, value string) string {
	switch {
	case dir == "" || value == "":
		return value
	case name == "SSL_CERT_DIR":
		entries := filepath.SplitList(value)
		for i, entry := range entries {
			if entry != "" && !filepath.IsAbs(entry) {
				entries[i] = filepath.Join(dir, entry)
			}
		}
		return strings.Join(entries, string(filepath.ListSeparator))
	case renewerPaths[name] && !filepath.IsAbs(value):
		return filepath.Join(dir, value)
	}
	return value
}

// cmdRenewSignIn is `agent-secrets renew --sign-in`. Started as the first stage, it starts the
// renewer and exits; started as the renewer, it takes the sign-in's lock or exits 0 where another
// renewer holds it, and renews until nothing needs the enrollment (runSignInRenewer).
func cmdRenewSignIn(stderr io.Writer) int {
	const prefix = "agent-secrets renew --sign-in"
	if !renewalSupported {
		fmt.Fprintf(stderr, "%s: this platform has no flock, which keeps one renewer for each sign-in\n", prefix)
		return 1
	}
	if os.Getenv(renewerStageVar) == renewerStageDetach {
		return startDetachedRenewer(stderr)
	}
	store := cipherclient.SignIns(homeGetenv)
	record, err := store.Choose(os.Getenv(cipherclient.PersonVar))
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prefix, err)
		return 1
	}
	renewal := renewalOf(record.Person)
	release, held, err := tryRenewerLock(renewal.path(renewalLockFile), false)
	switch {
	case errors.Is(err, fs.ErrNotExist), held:
		// No command ever kept this sign-in, or one renewer already renews it.
		return 0
	case err != nil:
		fmt.Fprintf(stderr, "%s: lock %s: %v\n", prefix, renewalName, redact.File(err))
		return 1
	}
	base := brokerBaseURL()
	if base.Empty() {
		release()
		fmt.Fprintf(stderr, "%s: %s is required: it names the secrets broker the sign-in's enrollment is renewed at\n", prefix, brokerURLVar)
		return exitUsageError
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	renewer := &signInRenewer{renewal: renewal, store: store, broker: newClient(base), trade: tradeForRenewal(base),
		stderr: stderr, after: time.After, now: time.Now, release: release}
	return runSignInRenewer(ctx, renewer)
}

// tradeForRenewal is a renewer's trade of a sign-in at the broker base names (tradeSignIn): it
// answers when the credential the trade moved now expires, the zero time where the broker named no
// expiry, which keepCredential takes as an answer no retry mends (errNoLiveExpiry).
func tradeForRenewal(base cipheraddr.Address) func(context.Context, cipherclient.SignInRecord, *ecdsa.PrivateKey) (time.Time, error) {
	return func(ctx context.Context, record cipherclient.SignInRecord, key *ecdsa.PrivateKey) (time.Time, error) {
		token, err := tradeSignIn(ctx, base, record, key)
		if err != nil {
			return time.Time{}, err
		}
		return token.CredentialExpiresAt, nil
	}
}

// startDetachedRenewer is the first stage: it starts this program again as the renewer, in a
// session of its own (detach) with the same standard output and error, and exits. The command
// that started it made every path its environment names absolute already (startRenewer).
func startDetachedRenewer(stderr io.Writer) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets renew --sign-in: find this program: %v\n", redact.File(err))
		return 1
	}
	cmd := exec.Command(self, "renew", "--sign-in")
	cmd.Env = renewerEnv(os.Environ(), os.Getenv(cipherclient.PersonVar), "")
	cmd.Stdout, cmd.Stderr, cmd.Dir = os.Stdout, os.Stderr, "/"
	detach(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "agent-secrets renew --sign-in: start the renewer: %v\n", err)
		return 1
	}
	_ = cmd.Process.Release()
	return 0
}

// signInRenewer is one renewer of one sign-in's enrollment, holding its lock (release lets it go).
// trade trades the person's identity again (tradeForRenewal) and answers the credential's new
// expiry. after and now are its clock, time.After and time.Now outside this package's tests.
type signInRenewer struct {
	renewal keptSignIn
	store   cipherclient.SignInStore
	broker  *client
	trade   func(context.Context, cipherclient.SignInRecord, *ecdsa.PrivateKey) (time.Time, error)
	stderr  io.Writer
	after   func(time.Duration) <-chan time.Time
	now     func() time.Time
	release func()
}

// renewerBlindLimit is the shortest a renewer goes on without knowing: with every renewal failing,
// or with every broker answer to whether anything needs the enrollment unreadable. It is twice the
// production lease (BROKER_LEASE_SECONDS, 900 s), and twice the lease the broker gave where that
// is longer (blindLimit): an enrollment nothing could renew for that long has lapsed unless no
// broker was serving, and the next command starts a renewer again. Without it, a renewer of a
// machine whose broker went away would retry forever.
const renewerBlindLimit = 30 * time.Minute

// blindLimit is renewerBlindLimit, or twice lease where that is longer.
func blindLimit(lease time.Duration) time.Duration { return max(renewerBlindLimit, 2*lease) }

// renewalState is what a renewer decides by, read in one go: the sign-in's record (or why it
// could not be read), when a command last used the sign-in, and the requests recorded as waiting
// for a person.
type renewalState struct {
	record    cipherclient.SignInRecord
	recordErr error
	used      time.Time
	pending   []string
}

// enrollment is the enrollment the state's record names, "" where it names none or was not read.
func (s renewalState) enrollment() string {
	if s.recordErr != nil {
		return ""
	}
	return s.record.EnrollmentID
}

// state reads r's renewal state now.
func (r *signInRenewer) state() renewalState {
	var s renewalState
	s.record, s.recordErr = r.store.Read(r.renewal.record)
	if info, err := os.Stat(r.renewal.path(renewalUsedFile)); err == nil {
		s.used = info.ModTime()
	}
	if entries, err := os.ReadDir(r.renewal.path(renewalPendingDir)); err == nil {
		for _, entry := range entries {
			s.pending = append(s.pending, entry.Name())
		}
	}
	return s
}

// movedOn reports whether next holds something a command wrote after s was read: another
// enrollment, a later use, or a request s did not hold. The renewer itself only removes requests.
func (s renewalState) movedOn(next renewalState) bool {
	if next.enrollment() != s.enrollment() || !next.used.Equal(s.used) {
		return true
	}
	for _, id := range next.pending {
		if !slices.Contains(s.pending, id) {
			return true
		}
	}
	return false
}

// logf writes one line of the renewer's log, naming the person whose sign-in it renews.
func (r *signInRenewer) logf(format string, args ...any) {
	fmt.Fprintf(r.stderr, "%s agent-secrets renew --sign-in: %s's sign-in: %s\n", r.now().UTC().Format(time.RFC3339), r.renewal.person, fmt.Sprintf(format, args...))
}

// stop lets the lock go and answers whether the renewer goes on: where goOn holds of the state read
// again, it takes the lock back, and goes on where it got it.
func (r *signInRenewer) stop(goOn func(next renewalState) bool) bool {
	r.release()
	r.release = func() {}
	if !goOn(r.state()) {
		return false
	}
	release, held, err := tryRenewerLock(r.renewal.path(renewalLockFile), false)
	if err != nil || held {
		// Gone with its directory, or a renewer a command started holds it now.
		return false
	}
	r.release = release
	return true
}

// end is every stop of the renewer but a signal's: it goes on, answering true, where a command
// wrote to the state after decided was read (stop), and otherwise logs why and answers false. The
// caller returns its own exit code, so each is a literal the error page's generator reads.
func (r *signInRenewer) end(decided renewalState, format string, args ...any) bool {
	if r.stop(decided.movedOn) {
		return true
	}
	r.logf(format, args...)
	return false
}

// renewalNeed is what a renewer knows of whether the machine still needs the enrollment.
type renewalNeed int

const (
	// stillNeeded: a recent use, a pending request or a grant with an end keeps it.
	stillNeeded renewalNeed = iota
	// notNeeded: none of the three holds.
	notNeeded
	// cannotTell: an answer of the broker could not be read; the enrollment is kept for now.
	cannotTell
)

// runSignInRenewer renews r's sign-in enrollment until nothing needs it, and answers the exit
// status. It is a function rather than a method so the error page's generator reads its exits
// (cmd/cipher-refgen, checkExitCodes).
func runSignInRenewer(ctx context.Context, r *signInRenewer) int {
	defer func() { r.release() }()
	// lease is the lease the broker's last renewal granted, 0 before the first: the first renewal
	// is never in doubt, since a command used the sign-in just now. renewed is when a renewal last
	// got through, and sure when the renewer last knew whether the machine needs the enrollment;
	// the start counts as both, since a command signed with the enrollment just now. cred is what
	// it knows of the sign-in's credential, which it keeps live (keepCredential).
	var lease time.Duration
	var cred credentialKeep
	renewed, sure := r.now(), r.now()
cycle:
	for {
		decided := r.state()
		id := decided.enrollment()
		switch {
		case errors.Is(decided.recordErr, fs.ErrNotExist):
			if !r.end(decided, "stopped: the sign-in is no longer on this machine") {
				return 0
			}
			continue
		case decided.recordErr != nil:
			if !r.end(decided, "stopped: %v", decided.recordErr) {
				return 1
			}
			continue
		case id == "":
			if !r.end(decided, "stopped: the sign-in holds no enrollment") {
				return 0
			}
			continue
		case id == r.renewal.refused():
			if !r.endRefused(id) {
				return exitRenewRefused
			}
			lease = 0
			continue
		}
		key, err := decided.record.Key()
		if err != nil {
			if !r.end(decided, "stopped: %v", err) {
				return 1
			}
			continue
		}
		signer := &fileSigner{key: key, enrollmentID: id}
		if lease > 0 {
			// Decided once a cycle: a renewal that fails is retried below without asking again,
			// so no retry spends its time on these reads.
			switch r.need(ctx, signer, decided, lease) {
			case stillNeeded:
				sure = r.now()
			case notNeeded:
				if !r.end(decided, "stopped: nothing has used it for %s, no request of enrollment %s waits for a person, and no grant of it that has an end is live", lease.Round(time.Second), id) {
					return 0
				}
				continue
			case cannotTell:
				if limit := blindLimit(lease); r.now().Sub(sure) >= limit {
					if !r.end(decided, "stopped: for %s no answer of the broker said whether anything needs enrollment %s", limit, id) {
						return 1
					}
					continue
				}
			}
		}
		backoff := leasetime.RenewBackoffInitial
		for {
			expires, err := r.broker.RenewEnrollment(ctx, signer, id, "")
			if err == nil {
				renewed = r.now()
				lease = max(expires.Sub(renewed), time.Second)
				break
			}
			if ctx.Err() != nil {
				return 0
			}
			var refused *apiError
			if errors.As(err, &refused) && refused.Permanent() {
				r.logf("enrollment %s permanently refused by %s: %s", id, brokerSource(r.broker.baseURL), refusalWords(refused))
				r.renewal.recordRefused(id)
				if !r.endRefused(id) {
					return exitRenewRefused
				}
				lease = 0
				continue cycle
			}
			if failing, limit := r.now().Sub(renewed), blindLimit(lease); failing >= limit {
				if !r.end(decided, "stopped: no renewal of enrollment %s got through for %s; the last failed: %v", id, failing.Round(time.Second), err) {
					return 1
				}
				continue cycle
			}
			r.logf("enrollment %s: %v; retrying in %s", id, err, backoff)
			if !r.sleep(ctx, backoff) {
				return 0
			}
			backoff = leasetime.NextBackoff(backoff)
			if r.state().enrollment() != id {
				// A command registered the next enrollment meanwhile: renew that one.
				continue cycle
			}
		}
		// The trade comes after the renewal, so a slow one never holds the lease the renewal
		// keeps: a command may start a renewer with little of its lease left.
		if cred.box != decided.record.Thumbprint {
			// A new sign-in is a new box key, and so a credential of its own.
			cred = credentialKeep{box: decided.record.Thumbprint}
		}
		if !cred.off && !r.now().Before(cred.next) {
			cred = r.keepCredential(ctx, decided.record, key, lease, cred)
		}
		if !r.sleep(ctx, cred.wait(r.now(), leasetime.RenewAfter(lease))) {
			return 0
		}
	}
}

// credentialKeep is what a renewer knows of its sign-in's launcher credential: when it expires,
// zero before a trade answered that; when to trade next, zero for now; and whether this machine
// trades the sign-in at all. box is the box key thumbprint it is about: a new sign-in is a new
// credential.
type credentialKeep struct {
	box           string
	expires, next time.Time
	off           bool
}

// wait is how long a renewer sleeps after a renewal: renewAfter, a third of the lease, or less
// where the next trade comes first, a second at least.
func (c credentialKeep) wait(now time.Time, renewAfter time.Duration) time.Duration {
	if c.off || c.next.IsZero() {
		return renewAfter
	}
	return max(min(renewAfter, c.next.Sub(now)), time.Second)
}

// keepCredential trades record's sign-in again (signInRenewer.trade) and answers what the renewer
// knows then. A trade that moved the credential's expiry is due again once a third of its
// remaining life has passed, the rule the lease's own renewal keeps (leasetime.RenewAfter). One
// that failed is tried again once the smaller of a third of the lease and a third of the
// credential's remaining life has passed (a third of the lease where nothing is known of the
// expiry), never sooner than renew's longest backoff (leasetime.RenewBackoffMax), so a person the
// issuer refuses is not asked for every ten seconds of an hour: at a 15-minute lease, 17 more
// tries before an hour's credential lapses, every five minutes and then closer together.
//
// Two answers end the trading instead, each said once: a machine told of no sign-in client
// cannot trade at all (errSignInUnrenewable), and a trade whose answer names no credential
// expiry, or one already past, is one no retry mends (errNoLiveExpiry): the broker names the
// expiry beside every credential it answers, so the broker that left it out leaves it out of
// every retry too, as agent-secrets-helper's own trade loop takes such an answer
// (errAnswerLacksCredential). After either, the renewer trades no more for this sign-in, and the
// broker ends the enrollment once its credential expires (exit 76), within the hour the last
// trade gave it; a new sign-in, a box key of its own, trades again.
func (r *signInRenewer) keepCredential(ctx context.Context, record cipherclient.SignInRecord, key *ecdsa.PrivateKey, lease time.Duration, c credentialKeep) credentialKeep {
	expires, err := r.trade(ctx, record, key)
	now := r.now()
	if err == nil && !expires.After(now) {
		err = noLiveExpiry(expires)
	}
	switch {
	case ctx.Err() != nil:
		return c
	case err == nil:
		c.expires, c.next = expires, now.Add(leasetime.RenewAfter(expires.Sub(now)))
		return c
	case errors.Is(err, errSignInUnrenewable):
		r.logf("this machine cannot trade the person's identity (%v), so enrollment %s ends once its sign-in credential expires", err, record.EnrollmentID)
		c.off = true
		return c
	case errors.Is(err, errNoLiveExpiry):
		r.logf("the trade of the person's identity got an answer no retry mends (%v), so this renewer trades no more and enrollment %s ends once its sign-in credential expires", err, record.EnrollmentID)
		c.off = true
		return c
	}
	retry := leasetime.RenewAfter(lease)
	if false && c.expires.After(now) {
		retry = min(retry, leasetime.RenewAfter(c.expires.Sub(now)))
	}
	c.next = now.Add(max(retry, leasetime.RenewBackoffMax))
	if c.expires.IsZero() {
		r.logf("the trade of the person's identity failed (%v); trying again in %s", err, c.next.Sub(now).Round(time.Second))
	} else {
		r.logf("the trade of the person's identity failed (%v); trying again in %s, before its sign-in credential expires at %s",
			err, c.next.Sub(now).Round(time.Second), c.expires.UTC().Format(time.RFC3339))
	}
	return c
}

// errNoLiveExpiry is a trade the broker answered with a credential but no expiry for it still to
// come (noLiveExpiry): no credential_expires_at, or one already past.
var errNoLiveExpiry = errors.New("the broker's answer names no expiry of the sign-in credential still to come")

// noLiveExpiry is errNoLiveExpiry for expires, the expiry the answer named (zero for none).
func noLiveExpiry(expires time.Time) error {
	if expires.IsZero() {
		return fmt.Errorf("%w: it named no %s", errNoLiveExpiry, oauthwire.ExtraCredentialExpiresAt)
	}
	return fmt.Errorf("%w: it named %s, already past", errNoLiveExpiry, expires.UTC().Format(time.RFC3339))
}

// endRefused is the stop after the broker refused enrollment id for good: the renewer goes on,
// answering true, only where the record names another enrollment by then, one a command
// registered; a use alone is a command that will find the enrollment ended and register the next
// one itself.
func (r *signInRenewer) endRefused(id string) bool {
	return r.stop(func(next renewalState) bool { return next.enrollment() != "" && next.enrollment() != id })
}

// need reports whether the machine still needs the sign-in's enrollment: by state, and once a
// lease has passed since a command last used the sign-in, by what the broker answers: a recorded
// request that still waits for a person, or a live grant of the enrollment that has an end. A
// recorded request the broker no longer holds pending, or answers is not this session's (403 or
// 404), is removed. An answer the renewer cannot read is cannotTell; it asks again next cycle, for
// the blind limit at most (runSignInRenewer). All the reads of one call share a third of the lease
// (leasetime.RenewAfter), so however slowly the broker answers them, the renewal that follows has
// a third of the lease left before the lease lapses.
func (r *signInRenewer) need(ctx context.Context, signer *fileSigner, s renewalState, lease time.Duration) renewalNeed {
	if r.now().Sub(s.used) < lease {
		return stillNeeded
	}
	ctx, cancel := context.WithTimeout(ctx, leasetime.RenewAfter(lease))
	defer cancel()
	unreadable := false
	for _, id := range s.pending {
		status, _, err := r.broker.GetRequest(ctx, signer, id, 0)
		var refused *apiError
		switch {
		case err == nil && status.State == "pending":
			return stillNeeded
		case err == nil, errors.As(err, &refused) && (refused.Status == 403 || refused.Status == 404):
			_ = os.Remove(filepath.Join(r.renewal.path(renewalPendingDir), id))
		case enrollmentNotLive(err):
			// The renewal that follows says so.
			return stillNeeded
		default:
			unreadable = true
		}
	}
	self, _, err := r.broker.Self(ctx, signer)
	switch {
	case enrollmentNotLive(err):
		return stillNeeded
	case err != nil:
		return cannotTell
	}
	now := r.now()
	for _, grant := range self.Grants {
		if grant.ExpiresAt != nil && grant.ExpiresAt.After(now) {
			return stillNeeded
		}
	}
	if unreadable {
		return cannotTell
	}
	return notNeeded
}

// sleep waits d, looking every renewerPoll whether the sign-in's record is still there, and answers
// false once ctx ends. It wakes early when the record is gone: the loop's next read stops it.
func (r *signInRenewer) sleep(ctx context.Context, d time.Duration) bool {
	for d > 0 {
		step := min(d, renewerPoll)
		select {
		case <-ctx.Done():
			return false
		case <-r.after(step):
		}
		d -= step
		if _, err := os.Stat(r.renewal.record); errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return true
}
