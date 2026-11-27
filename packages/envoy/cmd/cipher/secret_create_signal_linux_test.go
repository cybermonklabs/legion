//go:build linux && (amd64 || arm64)

// packages/envoy/cmd/cipher/secret_create_signal_linux_test.go
//
// A signal between the store making a secret and that secret holding its value, in a real process:
// TestCreateSignalHelper runs `secret create NEW_TEST` against its own fake store and fake broker,
// printing each store call and each reread as it is made, and the tests signal it while it reads a
// value piped in, or while a store call is in flight, once or twice.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"

	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// TestCreateSignalHelper is the process the tests below signal. It creates on GCP's fake Secret
// Manager, or, with AGENT_SECRETS_CREATE_STORE=aws, on secrets.Local through the AWS backend.
// AGENT_SECRETS_CREATE_ADD makes the GCP store answer AddSecretVersion "unavailable" without
// storing the value, or "lost" (Unavailable) once it has stored it. AGENT_SECRETS_CREATE_HOLD,
// <method>:<duration>, holds each call of that method that long before the store answers it, so a
// signal can arrive while it is in flight; <method>:signal holds it until this process has received
// a signal, so the signal arrives while it is in flight whatever the load; <method>:self-<SIGNAL>
// (self-INT, self-HUP) sends this process that signal as the call begins and holds the call until
// the signal is received and from then on, never answering it, so the call ends only at its own
// deadline, which the signal is sure to come before. AGENT_SECRETS_CREATE_DEADLINE
// is the deadline on each store call (storeCallDeadline). It prints READING at its first read of
// the value, CALL <method> at each store call (on AWS, CreateSecret alone), CREATED once a held GCP
// CreateSecret has made the secret, SIGNALLED <signal> each time it receives a signal it does not
// ignore, REREAD <name> at each broker reread, and RETURNED <exit code> when the create returns.
func TestCreateSignalHelper(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (a signal to this process (unix.Kill) at secret_create_signal_linux_test.go:70); it sets a package variable the package's other tests read (storeCallDeadline set at secret_create_signal_linux_test.go:56).
	if os.Getenv("AGENT_SECRETS_CREATE_HELPER") == "" {
		t.Skip("subprocess helper")
	}
	if d := os.Getenv("AGENT_SECRETS_CREATE_DEADLINE"); d != "" {
		deadline, err := time.ParseDuration(d)
		if err != nil {
			t.Fatal(err)
		}
		storeCallDeadline = deadline
	}
	signalled := acknowledgeSignals()
	held, holdFor, _ := strings.Cut(os.Getenv("AGENT_SECRETS_CREATE_HOLD"), ":")
	var hold func()
	switch name, self := strings.CutPrefix(holdFor, "self-"); {
	case holdFor == "signal":
		hold = func() { <-signalled }
	case self:
		sig := unix.SignalNum("SIG" + name)
		if sig == 0 {
			t.Fatalf("AGENT_SECRETS_CREATE_HOLD names no signal %q", name)
		}
		hold = func() {
			if err := unix.Kill(os.Getpid(), sig); err != nil {
				t.Error(err)
			}
			<-signalled
			select {}
		}
	default:
		d, err := time.ParseDuration(holdFor)
		if held != "" && err != nil {
			t.Fatal(err)
		}
		hold = func() { time.Sleep(d) }
	}
	if os.Getenv("AGENT_SECRETS_CREATE_STORE") == "aws" {
		local := secrets.NewLocal()
		served := servedBy(local)
		startSecretBroker(t, func(name string) Reread {
			fmt.Println("REREAD", name)
			return served(name)
		})
		useAWS(t, announcedCreate{Local: local, held: held == "CreateSecret", hold: hold}, testAccount, adaSignIn)
	} else {
		fake := newFakeSecretManager()
		switch os.Getenv("AGENT_SECRETS_CREATE_ADD") {
		case "unavailable":
			fake.refuseMethods = map[string]codes.Code{"AddSecretVersion": codes.Unavailable}
		case "lost":
			fake.answerAfter = map[string]codes.Code{"AddSecretVersion": codes.Unavailable}
		}
		if os.Getenv("AGENT_SECRETS_CREATE_LOST_INITIAL_ANSWER") != "" {
			if fake.answerAfter == nil {
				fake.answerAfter = map[string]codes.Code{}
			}
			fake.answerAfter["CreateSecret"] = codes.Unavailable
		}
		fake.beforeCall = func(method string) {
			fmt.Println("CALL", method)
			if method == held && method != "CreateSecret" {
				hold()
			}
		}
		fake.afterCall = func(method string) {
			if method == "CreateSecret" && method == held {
				fmt.Println("CREATED")
				hold()
			}
		}
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "authorized_user", "ada@example.com")
		served := gcpServedBy(fake, gcpTestSettings())
		startSecretBrokerWith(t, gcpTestSettings(), func(name string) Reread {
			fmt.Println("REREAD", name)
			return served(name)
		})
	}
	secretStdin = &onFirstRead{r: os.Stdin, first: func() { fmt.Println("READING") }}
	code := cmdSecret([]string{"create", "NEW_TEST", "--owner", "me", "--tier", "agent"}, os.Stdout, os.Stderr)
	fmt.Println("RETURNED", code)
}

// acknowledgeSignals prints SIGNALLED <signal> each time this process receives one of
// createSignals, and answers a channel closed at the first. It watches only the signals this
// process did not inherit as ignored, as watchCreateSignals does, so an ignored one stays ignored;
// os/signal hands each signal to every channel watching it, so the create's own watch gets it too,
// and exitBySignal's signal.Reset ends both.
func acknowledgeSignals() <-chan struct{} {
	signalled := make(chan struct{})
	var watched []os.Signal
	for _, sig := range createSignals {
		if !signal.Ignored(sig) {
			watched = append(watched, sig)
		}
	}
	if len(watched) == 0 {
		return signalled
	}
	seen := make(chan os.Signal, 8)
	signal.Notify(seen, watched...)
	go func() {
		first := sync.OnceFunc(func() { close(signalled) })
		for sig := range seen {
			fmt.Println("SIGNALLED", signalNoun(sig))
			first()
		}
	}()
	return signalled
}

// announcedCreate is secrets.Local printing CALL CreateSecret at each CreateSecret, and, where held,
// answering it only once hold returns, or with its context's error once that ends first, as the AWS
// SDK does.
type announcedCreate struct {
	*secrets.Local
	held bool
	hold func()
}

func (a announcedCreate) CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	fmt.Println("CALL CreateSecret")
	if a.held {
		released := make(chan struct{})
		go func() {
			a.hold()
			close(released)
		}()
		select {
		case <-released:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return a.Local.CreateSecret(ctx, in, o...)
}

// createProcess is a TestCreateSignalHelper process: its standard input, a pipe held open until
// the test writes or closes it, everything it printed, and a channel closed once it has been waited
// for, with what the wait answered.
type createProcess struct {
	t       *testing.T
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	out     *syncWriter
	waited  chan struct{}
	waitErr error
}

// startCreate starts cmd, a command that runs TestCreateSignalHelper, with env added to its
// environment, killing it if the test ends first. Where this test process is itself killed and
// runs no cleanup, the helper is killed with it (its parent-death signal), so it does not outlive
// the test binary holding a store call for up to an hour (heldDeadline). The signal follows the
// thread that started the helper, so one goroutine holds that thread from Start until the helper
// has been waited for, and it is the one that waits: the rows read the end from waited.
func startCreate(t *testing.T, cmd *exec.Cmd, env ...string) *createProcess {
	t.Helper()
	cmd.Env = append(append(os.Environ(), "AGENT_SECRETS_CREATE_HELPER=1"), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	p := &createProcess{t: t, cmd: cmd, stdin: stdin, out: &syncWriter{}, waited: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p.out, p.out
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		p.waitErr = cmd.Wait()
		close(p.waited)
	}()
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-p.waited
	})
	return p
}

// helperCommand runs TestCreateSignalHelper with each of createSignals at its default action
// (`env --default-signal`), so it does not inherit one this test process ignores (a job run in the
// background of a script ignores SIGINT and SIGQUIT, nohup SIGHUP), at which the create rightly
// goes on.
func helperCommand() *exec.Cmd {
	return exec.Command("env", "--default-signal=HUP,INT,QUIT,TERM", os.Args[0], "-test.run=^TestCreateSignalHelper$")
}

// waitFor returns once the process has printed want, failing the test once the moment the waits of
// these tests give up has passed (waitsGaveUp).
func (p *createProcess) waitFor(want string) {
	p.t.Helper()
	if !heldBeforeGiveUp(p.t, func() bool { return strings.Contains(p.out.String(), want) }) {
		p.t.Fatalf("the create never printed %q, %s before the test binary's deadline: %q", want, waitMargin, p.out.String())
	}
}

// signal sends sig to the process.
func (p *createProcess) signal(sig syscall.Signal) {
	p.t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		p.t.Fatal(err)
	}
}

// endedBy waits for the process and fails the test unless sig ended it, the create never having
// returned. The wait's one bound is the moment the waits of these tests give up (waitsGaveUp): a
// process sig did not end fails the test then, and is killed. That bound decides only whether the
// process ended; whether the signal, not the store call's own deadline, ended it is the row's to
// show (endsAtOnceBy).
func (p *createProcess) endedBy(sig syscall.Signal) string {
	p.t.Helper()
	if _, ok := received(p.t, p.waited); !ok {
		_ = p.cmd.Process.Kill()
		<-p.waited
		p.t.Fatalf("the create had not ended %s before the test binary's deadline, output %q; want it ended by %s by then", waitMargin, p.out.String(), sig)
	}
	out := p.out.String()
	if ws := p.cmd.ProcessState.Sys().(syscall.WaitStatus); !ws.Signaled() || ws.Signal() != sig || strings.Contains(out, "RETURNED") {
		p.t.Fatalf("the create ended %v, output %q; want it ended by %s before it returned", p.cmd.ProcessState, out, sig)
	}
	return out
}

// heldDeadline is the deadline on each store call (AGENT_SECRETS_CREATE_DEADLINE) of the rows whose
// second signal ends the process at once: longer than any run, so with the store holding its
// answer for an hour, nothing but that signal can end the process while the test waits.
const heldDeadline = "AGENT_SECRETS_CREATE_DEADLINE=1h"

// endsAtOnceBy sends sig, which must end the process without waiting on the store call in flight,
// and answers its output once sig has ended it (endedBy). What decides that the signal ended it,
// not the call it interrupts, is the cause: the row's store call can end no other way while the
// test waits (heldDeadline), so a signal path that waits behind that call never ends and fails
// endedBy, and endedBy reads that sig, and not an exit of the create's own, ended it. The end is
// not timed: the create runs in a process of its own, which a machine can starve alone.
func (p *createProcess) endsAtOnceBy(sig syscall.Signal) string {
	p.t.Helper()
	p.signal(sig)
	return p.endedBy(sig)
}

// A signal after the store has created the container but before its answer arrives must wait
// for that answer and delete the container, just as a signal while reading the value does. The
// store holds its answer until the process has received the signal.
func TestCreateSignalBeforeTheCreateAnswerDeletesTheEmptySecret(t *testing.T) {
	t.Parallel()
	p := startCreate(t, helperCommand(), "AGENT_SECRETS_CREATE_HOLD=CreateSecret:signal")
	p.waitFor("CREATED")
	p.signal(syscall.SIGTERM)
	out := p.endedBy(syscall.SIGTERM)
	if strings.Count(out, "CALL DeleteSecret") != 1 || !strings.Contains(out, "REREAD NEW_TEST") || strings.Contains(out, "CALL AddSecretVersion") {
		t.Fatalf("output %q; want the empty secret deleted once and reread, with no value added", out)
	}
}

// A second signal while the first waits on a CreateSecret the store never answers ends the
// process at once, naming the empty secret that may stay and the command that clears it. The
// second is sent only once the process has received the first, so the two cannot merge.
func TestCreateSecondSignalBeforeTheCreateAnswerNamesWhatMayStay(t *testing.T) {
	// Not parallel: it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past ((*createProcess).endsAtOnceBy, main.requireCommandOnTime, onTimeWithin at waits_test.go:245).
	p := startCreate(t, helperCommand(), "AGENT_SECRETS_CREATE_HOLD=CreateSecret:1h", heldDeadline)
	p.waitFor("CREATED")
	p.signal(syscall.SIGINT)
	p.waitFor("SIGNALLED interrupt")
	out := p.endsAtOnceBy(syscall.SIGTERM)
	if !strings.Contains(out, "a second signal") || !strings.Contains(out, clearEmptySecret) {
		t.Fatalf("output %q; want the interrupted create's empty secret and its cleanup command", out)
	}
}

// A signal while the create settles a CreateSecret whose answer was lost waits for that settling,
// which deletes the empty secret it made and rereads once. The read back is held until the process
// has received the signal.
func TestCreateSignalDuringInitialRecoveryDeletesTheEmptySecret(t *testing.T) {
	t.Parallel()
	p := startCreate(t, helperCommand(), "AGENT_SECRETS_CREATE_LOST_INITIAL_ANSWER=1", "AGENT_SECRETS_CREATE_HOLD=GetSecret:signal")
	p.waitFor("CALL GetSecret")
	p.signal(syscall.SIGHUP)
	out := p.endedBy(syscall.SIGHUP)
	if strings.Count(out, "CALL DeleteSecret") != 1 || strings.Count(out, "REREAD NEW_TEST") != 1 || strings.Contains(out, "CALL AddSecretVersion") {
		t.Fatalf("output %q; want initial recovery to delete and reread once, without adding a value", out)
	}
}

// TestCreateSignalWhileReadingDeletesTheEmptySecret: a hang-up, an interrupt, a quit or a
// termination while the create reads a value piped in deletes the empty secret it made, asks the
// broker to reread the name, and then ends the process by that signal, the value never added.
func TestCreateSignalWhileReadingDeletesTheEmptySecret(t *testing.T) {
	t.Parallel()
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM} {
		t.Run(unix.SignalName(sig), func(t *testing.T) {
			p := startCreate(t, helperCommand())
			p.waitFor("READING")
			p.signal(sig)
			out := p.endedBy(sig)
			if strings.Count(out, "CALL DeleteSecret") != 1 || !strings.Contains(out, "REREAD NEW_TEST") || strings.Contains(out, "CALL AddSecretVersion") {
				t.Fatalf("output %q; want the empty secret deleted once and reread, and no value added", out)
			}
		})
	}
}

// TestCreateSignalWhileTheAddIsInFlightWaitsForItsAnswer: a signal that arrives while the add of
// the value is in flight waits for the store's answer before it decides anything; the store holds
// that answer until the process has received the signal. An add the store answered with no
// definite refusal never leads to a delete, since the add may have stored the value, or may still:
// one refused as unavailable is listed, found empty, and kept, the create saying a late add may
// still land; one the store carried out and whose answer was lost is listed, found holding a
// version, and kept, the create saying the value may have been stored; both reread the name. One
// the store answered as done has stored the value: the create says so, on the line a create that
// ends as it should prints, and asks the broker to reread the name. The process then ends by that
// signal.
func TestCreateSignalWhileTheAddIsInFlightWaitsForItsAnswer(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, add string
		sig       syscall.Signal
		want      []string
		not       []string
	}{
		{"the add refused as unavailable", "unavailable", syscall.SIGINT,
			[]string{"CALL ListSecretVersions", "a late add may still land", "REREAD NEW_TEST"}, []string{"CALL DeleteSecret", "may have been stored"}},
		{"the add carried out and its answer lost", "lost", syscall.SIGTERM,
			[]string{"CALL ListSecretVersions", "may have been stored", "REREAD NEW_TEST"}, []string{"CALL DeleteSecret"}},
		{"the add answered as done", "", syscall.SIGINT,
			[]string{"created NEW_TEST in GCP Secret Manager project", "REREAD NEW_TEST"}, []string{"CALL ListSecretVersions", "CALL DeleteSecret"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			p := startCreate(t, helperCommand(), "AGENT_SECRETS_CREATE_HOLD=AddSecretVersion:signal", "AGENT_SECRETS_CREATE_ADD="+row.add)
			p.waitFor("READING")
			_, _ = io.WriteString(p.stdin, "shh")
			_ = p.stdin.Close()
			p.waitFor("CALL AddSecretVersion")
			p.signal(row.sig)
			out := p.endedBy(row.sig)
			for _, want := range row.want {
				if !strings.Contains(out, want) {
					t.Fatalf("output %q; want %q", out, want)
				}
			}
			for _, not := range row.not {
				if strings.Contains(out, not) {
					t.Fatalf("output %q; want no %q", out, not)
				}
			}
		})
	}
}

// TestCreateSignalDuringAnAWSCreateThatSucceedsSaysItWasStored: on AWS, a signal that arrives while
// the one CreateSecret is in flight waits for its answer; one the store answers as done has made
// the secret with its value, so the create says it was stored, on the line a create that ends as
// it should prints, asks the broker to reread the name, and only then ends by the signal.
func TestCreateSignalDuringAnAWSCreateThatSucceedsSaysItWasStored(t *testing.T) {
	t.Parallel()
	p := startCreate(t, helperCommand(), "AGENT_SECRETS_CREATE_STORE=aws", "AGENT_SECRETS_CREATE_HOLD=CreateSecret:signal")
	p.waitFor("READING")
	_, _ = io.WriteString(p.stdin, "shh")
	_ = p.stdin.Close()
	p.waitFor("CALL CreateSecret")
	p.signal(syscall.SIGTERM)
	out := p.endedBy(syscall.SIGTERM)
	if n := strings.Count(out, "created NEW_TEST in AWS Secrets Manager account"); n != 1 || !strings.Contains(out, "REREAD NEW_TEST") {
		t.Fatalf("output %q says %d times that NEW_TEST was created; want once, and the name reread", out, n)
	}
}

// TestCreateFirstSignalDuringAnAWSCreateSaysWhatTheStoreMayHold: a first signal that arrives while
// AWS's one CreateSecret is in flight waits for it; a CreateSecret that then runs out of its
// deadline may have made the secret with its value, and the signal's cleanup says so, once, naming
// the command that tells which, before the process ends by the signal. The helper sends itself the
// signal as the call begins (self-HUP), so the signal comes inside the deadline however loaded the
// machine.
func TestCreateFirstSignalDuringAnAWSCreateSaysWhatTheStoreMayHold(t *testing.T) {
	t.Parallel()
	p := startCreate(t, helperCommand(), "AGENT_SECRETS_CREATE_STORE=aws", "AGENT_SECRETS_CREATE_HOLD=CreateSecret:self-HUP", "AGENT_SECRETS_CREATE_DEADLINE=5s")
	p.waitFor("READING")
	_, _ = io.WriteString(p.stdin, "shh")
	_ = p.stdin.Close()
	p.waitFor("SIGNALLED hang-up")
	out := p.endedBy(syscall.SIGHUP)
	for _, want := range []string{"did not answer within 5s", "`agent-secrets secret show NEW_TEST`"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q; want %q", out, want)
		}
	}
	if n := strings.Count(out, "the store may hold NEW_TEST"); n != 1 {
		t.Fatalf("output %q says %d times what the store may hold; want once", out, n)
	}
}

// TestCreateSignalWaitsOnAnAddTheStoreNeverAnswersNoLongerThanItsDeadline: a signal that comes
// while the store holds the add of the value without answering waits for that add no longer than
// the deadline on each store call (storeCallDeadline, here five seconds): the add then ends as one
// that ran out of time, which may have stored the value or may still, so the create lists the
// secret's versions, finds none, keeps the secret, says a late add may still land, asks the broker
// to reread the name, and ends by the signal, though the store never answers the add. The helper
// sends itself the signal as the add begins (self-INT), so the signal comes inside the deadline
// however loaded the machine.
func TestCreateSignalWaitsOnAnAddTheStoreNeverAnswersNoLongerThanItsDeadline(t *testing.T) {
	t.Parallel()
	p := startCreate(t, helperCommand(), "AGENT_SECRETS_CREATE_HOLD=AddSecretVersion:self-INT", "AGENT_SECRETS_CREATE_DEADLINE=5s")
	p.waitFor("READING")
	_, _ = io.WriteString(p.stdin, "shh")
	_ = p.stdin.Close()
	p.waitFor("SIGNALLED interrupt")
	out := p.endedBy(syscall.SIGINT)
	for _, want := range []string{"CALL ListSecretVersions", "a late add may still land", "REREAD NEW_TEST"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q; want %q", out, want)
		}
	}
	if strings.Contains(out, "CALL DeleteSecret") {
		t.Fatalf("output %q; want no delete after an add whose answer was lost", out)
	}
}

// TestCreateSecondSignalEndsAtOnceNamingWhatMayStay: a second signal that comes while the first one
// waits on a store call in flight ends the process at once, by that second signal, without waiting
// on the call, and says on one line the state the store may be left in and the command that clears
// or settles it: on GCP, while the add of the value is in flight, the secret may stay empty or
// holding the value, which `secret show` tells apart and gcloud clears; while the delete of the
// empty secret is in flight, that empty secret may stay, which gcloud clears; on AWS, while the
// one CreateSecret is in flight, the store may hold the secret with its value or nothing at all,
// which `secret show` tells apart. The same signal twice and two different ones both do.
func TestCreateSecondSignalEndsAtOnceNamingWhatMayStay(t *testing.T) {
	// Not parallel: it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past ((*createProcess).endsAtOnceBy, main.requireCommandOnTime, onTimeWithin at waits_test.go:245).
	for _, row := range []struct {
		name          string
		env           []string
		piped         bool   // the value is piped in before the first signal, which then comes while the add is in flight
		hung          string // the call the first signal's cleanup waits on
		first, second syscall.Signal
		want, not     []string
	}{
		{"GCP, the add in flight", []string{"AGENT_SECRETS_CREATE_HOLD=AddSecretVersion:1h", heldDeadline}, true, "CALL AddSecretVersion", syscall.SIGINT, syscall.SIGINT,
			[]string{"a second signal (interrupt)", newTestID, "empty or holding the value", "`agent-secrets secret show NEW_TEST`", "`" + clearEmptySecret + "`"},
			[]string{"CALL ListSecretVersions", "CALL DeleteSecret"}},
		{"GCP, the delete in flight", []string{"AGENT_SECRETS_CREATE_HOLD=DeleteSecret:1h", heldDeadline}, false, "CALL DeleteSecret", syscall.SIGTERM, syscall.SIGTERM,
			[]string{"a second signal (termination)", "the empty secret " + newTestID, "another writer may now hold that name", "agent-secrets-create label is", "`" + clearEmptySecret + "`"},
			[]string{"holding the value", "REREAD"}},
		{"AWS, the create in flight", []string{"AGENT_SECRETS_CREATE_STORE=aws", "AGENT_SECRETS_CREATE_HOLD=CreateSecret:1h", heldDeadline}, true, "CALL CreateSecret", syscall.SIGHUP, syscall.SIGTERM,
			[]string{"a second signal (termination)", "the store may hold NEW_TEST", "or no NEW_TEST at all", "`agent-secrets secret show NEW_TEST`"},
			[]string{"REREAD"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			p := startCreate(t, helperCommand(), row.env...)
			p.waitFor("READING")
			if row.piped {
				_, _ = io.WriteString(p.stdin, "shh")
				_ = p.stdin.Close()
				p.waitFor(row.hung)
				p.signal(row.first)
				// The second goes only once the process has the first, so the two cannot merge.
				p.waitFor("SIGNALLED " + signalNoun(row.first))
			} else {
				p.signal(row.first)
				p.waitFor(row.hung)
			}
			out := p.endsAtOnceBy(row.second)
			for _, want := range row.want {
				if !strings.Contains(out, want) {
					t.Fatalf("output %q; want %q", out, want)
				}
			}
			for _, not := range row.not {
				if strings.Contains(out, not) {
					t.Fatalf("output %q; want no %q", out, not)
				}
			}
		})
	}
}

// TestCreateSignalInheritedIgnoredStaysIgnored: a hang-up or an interrupt the process inherited as
// ignored, as under nohup or a shell trap that ignores it, stays ignored while the create reads a
// value piped in: the read goes on, nothing is deleted, and the whole value is stored once the
// pipe closes.
func TestCreateSignalInheritedIgnoredStaysIgnored(t *testing.T) {
	t.Parallel()
	requireSignalDispositions(t)
	p := startCreate(t, exec.Command("bash", "-c", `trap '' HUP INT; exec "$0" -test.run='^TestCreateSignalHelper$'`, os.Args[0]))
	p.waitFor("READING")
	// The create's watch on the signals is in place by the read: a signal it watched would have a
	// handler of its own and be ignored no longer.
	pid := p.cmd.Process.Pid
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGINT} {
		if !ignores(t, pid, sig) || catches(t, pid, sig) {
			t.Fatalf("while the create reads, %s is ignored %t and caught %t; want it ignored, as inherited, and not caught", unix.SignalName(sig), ignores(t, pid, sig), catches(t, pid, sig))
		}
	}
	p.signal(syscall.SIGHUP)
	p.signal(syscall.SIGINT)
	_, _ = io.WriteString(p.stdin, "piped-value")
	_ = p.stdin.Close()
	if _, ok := received(t, p.waited); !ok {
		t.Fatalf("the create had not ended after its value came, %s before the test binary's deadline; output %q", waitMargin, p.out.String())
	}
	if out := p.out.String(); p.waitErr != nil || !strings.Contains(out, "RETURNED 0") || !strings.Contains(out, "created NEW_TEST") || strings.Contains(out, "CALL DeleteSecret") {
		t.Fatalf("the create ended %v, output %q; want the value stored and nothing deleted", p.waitErr, out)
	}
}
