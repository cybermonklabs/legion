package sandbox

// A session store that stops answering a Kubernetes start, at its proof dial, at the resume's
// lookup (a refused dial, or an instance that cannot take a connection now), at a Claude Code
// keeper's own dial or its read of the session's rows (rows held locked, or a connection the store
// ends under the read), or on the pinned Oh My Pi's own connection after the start's checks passed,
// is waited out a minute at a time: the claim keeps its session, is charged nothing, and the next
// minute's try, once the store answers, launches the same session. A store that answers about its
// configuration (a credential that is not there, a password it refuses) is charged until the
// claim's launch budget runs out. The tests drive the whole chain: a claim's machine on a clock of
// the test's own, this runtime over the rig's fake cluster, a real `legion launcher` in each pod the
// rig makes (launcher.Run), its real session proxy to the test's Postgres, and the real Oh My Pi or
// a Claude Code start's real keeper. The one stand-in is the pod's file system: the relay between
// the runtime and the launcher points the start's argv and environment at the pinned binary and the
// launcher's own directory where the runtime names a pod's paths.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/sessionproxy"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/supervise/supervisetest"
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

func TestAKubernetesStartWhoseSessionStoreStopsAnsweringIsWaitedOutAMinuteAndResumesTheSameSession(t *testing.T) {
	omp := testbin.OMP(t)
	for name, tc := range map[string]struct {
		// fault puts the store's fault in place on d before the first start.
		fault func(t *testing.T, d *storeDialer)
		// childExits is whether the fault is the child's own: the start succeeds, and the child
		// exits on the store.
		childExits bool
		// stopGrace is the launcher's, which bounds its proof dial at half of it.
		stopGrace time.Duration
	}{
		"the proof dial is refused":                  {fault: func(_ *testing.T, d *storeDialer) { d.refuseFrom.Store(1) }},
		"the proof dial is not answered":             {fault: func(_ *testing.T, d *storeDialer) { d.stall.Store(true) }, stopGrace: 2 * time.Second},
		"the lookup is refused after the proof dial": {fault: func(_ *testing.T, d *storeDialer) { d.refuseFrom.Store(2) }},
		"the lookup meets an instance starting up (57P03)": {fault: func(t *testing.T, d *storeDialer) {
			d.reach(testdb.Refusing(t, "57P03", "the database system is starting up"))
		}},
		"the child's own connection is refused":             {fault: func(_ *testing.T, d *storeDialer) { d.refuseFrom.Store(3) }, childExits: true},
		"control: the child exits with the store answering": {fault: func(*testing.T, *storeDialer) {}, childExits: true},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStoreFaultRig(t, omp, tc.stopGrace)
			tc.fault(t, s.dialer)
			control := strings.HasPrefix(name, "control:")

			err := s.m.Handle(s.g.ctx, supervise.RequestResume{Claim: s.spec.Claim})
			s.m.Wait()
			if tc.childExits {
				if err != nil {
					t.Fatalf("the resume answered %v, want the child started", err)
				}
				loc := s.m.Claim().Locator
				if loc == nil {
					t.Fatal("the resume recorded no process")
				}
				obs := s.awaitExit(t, *loc)
				if obs.StoreFault == control {
					t.Fatalf("the child's exit was observed %+v, want StoreFault %t", obs, !control)
				}
				err = s.m.Handle(s.g.ctx, supervise.RuntimeObservation{Observation: obs})
				s.m.Wait()
			}
			c := s.m.Claim()
			if control {
				if c.Budgets.LaunchFailures != 1 || len(s.faults()) != 0 {
					t.Fatalf("a child that exited with the store answering left the claim %s with %d launch failures and %d store faults told, want the death charged once and no store fault",
						c.State, c.Budgets.LaunchFailures, len(s.faults()))
				}
				return
			}
			if !errors.Is(err, runtime.ErrSessionStore) && !tc.childExits {
				t.Fatalf("the start over a store that did not answer answered %v, want runtime.ErrSessionStore", err)
			}
			if c.State != supervise.StateLaunching || c.Locator != nil || c.Budgets.LaunchFailures != 0 ||
				c.Session != storeFaultSession || c.SessionFile != storeFaultSessionFile || len(s.faults()) != 1 {
				t.Fatalf("after the store fault the claim is %s, locator %v, %d launch failures, session %q at %q, %d store faults told; want launching with no process, nothing charged, %s at %s kept, the fault told once",
					c.State, c.Locator, c.Budgets.LaunchFailures, c.Session, c.SessionFile, len(s.faults()), storeFaultSession, storeFaultSessionFile)
			}
			starts := s.relay.startCount()

			s.dialer.answer()
			s.clock.Advance(s.m, time.Minute-time.Second)
			if got := s.relay.startCount(); got != starts {
				t.Fatalf("the claim was started again before its minute was out: %d starts, had %d", got, starts)
			}
			s.clock.Advance(s.m, time.Second)
			c = s.m.Claim()
			if c.Locator == nil || c.Budgets.LaunchFailures != 0 || c.Session != storeFaultSession {
				t.Fatalf("after the minute's try the claim is %s, locator %v, %d launch failures, session %q; want it launched on %s with nothing charged",
					c.State, c.Locator, c.Budgets.LaunchFailures, c.Session, storeFaultSession)
			}
			if last := s.relay.lastStart(); last.ResumeFile != storeFaultSessionFile {
				t.Errorf("the minute's start resumes %q, want the same session %s", last.ResumeFile, storeFaultSessionFile)
			}
		})
	}
}

// A Kubernetes start whose session store is configured wrong is charged as the launch failure it
// is, naming the cause, until the claim's launch budget runs out, and no store fault is told: a
// proxy whose credential is not there, and a store that refuses the password (28P01), which
// answered the start about its configuration.
func TestAKubernetesStartWhoseSessionStoreIsConfiguredWrongIsChargedNamingIt(t *testing.T) {
	omp := testbin.OMP(t)
	for name, tc := range map[string]struct {
		edit  func(*launcher.Config)
		fault func(t *testing.T, d *storeDialer)
		names string
	}{
		"a proxy credential that is not there": {names: "could not find default credentials", edit: func(cfg *launcher.Config) {
			cfg.SessionDialer = func(context.Context, string) (sessionproxy.Dialer, error) {
				return nil, errors.New("google: could not find default credentials")
			}
		}},
		"a password the store refuses (28P01)": {names: "password authentication failed", fault: func(t *testing.T, d *storeDialer) {
			d.reach(testdb.Refusing(t, "28P01", `password authentication failed for user "legion"`))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			var edits []func(*launcher.Config)
			if tc.edit != nil {
				edits = append(edits, tc.edit)
			}
			s := newStoreFaultRig(t, omp, 0, edits...)
			if tc.fault != nil {
				tc.fault(t, s.dialer)
			}
			err := s.m.Handle(s.g.ctx, supervise.RequestResume{Claim: s.spec.Claim})
			s.m.Wait()
			c := s.m.Claim()
			if errors.Is(err, runtime.ErrSessionStore) || c.State != supervise.StateFailed || c.Budgets.LaunchFailures != 3 ||
				!strings.Contains(c.LastStartFailure, tc.names) || len(s.faults()) != 0 {
				t.Fatalf("the resume answered %v and left the claim %s with %d launch failures, last start failure %q, %d store faults told; want it failed after 3 charged launch failures naming %q, no store fault",
					err, c.State, c.Budgets.LaunchFailures, c.LastStartFailure, len(s.faults()), tc.names)
			}
		})
	}
}

// A Claude Code start whose keeper cannot reach the session's rows is waited out a minute at a time
// through the claim's machine: a fresh start whose keeper's own dial is refused, and a resume whose
// rows another transaction holds locked past the start's bound, or whose read the store ends by
// resetting or closing the connection under it. The claim keeps its session, is charged nothing,
// and the next minute's try, once the store answers, launches the same session, a resume's
// transcript restored from the table onto the pod's disk.
func TestAKubernetesClaudeStartWhoseKeeperCannotReachTheRowsIsWaitedOutAMinuteAndStartsTheSameSession(t *testing.T) {
	// ending is a store whose connection the restore's read of the session's parts is ended under.
	ending := func(reset bool) func(t *testing.T, s *storeFaultRig) {
		return func(t *testing.T, s *storeFaultRig) {
			parsed, err := url.Parse(testdb.Breaking(t, s.dsn, "omp_session_files_parts WHERE path", reset))
			if err != nil {
				t.Fatal(err)
			}
			s.dialer.reach(parsed.Host)
		}
	}
	for name, tc := range map[string]struct {
		fresh bool
		// fault puts the store's fault in place before the first start; names is what the
		// refusal names of the step that met it.
		fault func(t *testing.T, s *storeFaultRig)
		names func(s *storeFaultRig) string
	}{
		"a fresh start whose keeper's own dial is refused": {fresh: true,
			fault: func(_ *testing.T, s *storeFaultRig) { s.dialer.refuseFrom.Store(2) },
			names: func(*storeFaultRig) string { return "keep Claude Code session " }},
		"a resume whose rows are held locked":  {fault: func(t *testing.T, s *storeFaultRig) { s.lockRows(t) }},
		"a resume whose read the store resets": {fault: ending(true)},
		"a resume whose read the store closes": {fault: ending(false)},
	} {
		t.Run(name, func(t *testing.T) {
			s := newClaudeStoreFaultRig(t, testbin.OMP(t), tc.fresh)
			names := "restore " + s.claude.transcript
			if tc.names != nil {
				names = tc.names(s)
			}
			tc.fault(t, s)

			err := s.m.Handle(s.g.ctx, supervise.RequestResume{Claim: s.spec.Claim})
			s.m.Wait()
			c := s.m.Claim()
			if !errors.Is(err, runtime.ErrSessionStore) || !strings.Contains(err.Error(), names) {
				t.Fatalf("the start whose keeper could not reach the rows answered %v, want runtime.ErrSessionStore naming %q", err, names)
			}
			if c.State != supervise.StateLaunching || c.Locator != nil || c.Budgets.LaunchFailures != 0 ||
				c.Session != storeFaultSession || c.SessionFile != storeFaultSessionFile || len(s.faults()) != 1 {
				t.Fatalf("after the store fault the claim is %s, locator %v, %d launch failures, session %q at %q, %d store faults told; want launching with no process, nothing charged, %s at %s kept, the fault told once",
					c.State, c.Locator, c.Budgets.LaunchFailures, c.Session, c.SessionFile, len(s.faults()), storeFaultSession, storeFaultSessionFile)
			}
			if _, err := os.Stat(s.claude.transcript); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the refused start left the transcript on the pod's disk (%v), want nothing restored", err)
			}
			starts := s.relay.startCount()

			s.dialer.answer()
			s.unlockRows()
			s.clock.Advance(s.m, time.Minute-time.Second)
			if got := s.relay.startCount(); got != starts {
				t.Fatalf("the claim was started again before its minute was out: %d starts, had %d", got, starts)
			}
			s.clock.Advance(s.m, time.Second)
			c = s.m.Claim()
			if c.Locator == nil || c.Budgets.LaunchFailures != 0 || c.Session != storeFaultSession {
				t.Fatalf("after the minute's try the claim is %s, locator %v, %d launch failures, session %q; want it launched on %s with nothing charged",
					c.State, c.Locator, c.Budgets.LaunchFailures, c.Session, storeFaultSession)
			}
			if last := s.relay.lastStart(); last.ResumeFile != storeFaultSessionFile {
				t.Errorf("the minute's start resumes %q, want the same session %s", last.ResumeFile, storeFaultSessionFile)
			}
			if tc.fresh {
				return
			}
			if got, err := os.ReadFile(s.claude.transcript); err != nil || string(got) != storeFaultTranscript {
				t.Errorf("the minute's start restored the transcript %q (%v), want the row's %q", got, err, storeFaultTranscript)
			}
		})
	}
}

// The session the store-fault tests' claim records, and the file it is recorded under: any key
// the table holds, since the relay starts the real Oh My Pi fresh beside the launcher's own
// lookup of it. storeFaultTranscript is what the table holds of it, and of a Claude Code resume's
// transcript (claudeResume).
const (
	storeFaultSession     = "ses-tester"
	storeFaultSessionFile = "/legion/scratch/state/tester/.omp/profiles/legion/agent/sessions/--legion-workspaces-store-fault--/2026-11-19T12-00-00-000Z_ses-tester.jsonl"
	storeFaultTranscript  = "{}\n"
)

// storeFaultRig is one suspended Oh My Pi claim of the rig's worker, its machine on a clock of the
// test's own, and the real launcher its pods run.
type storeFaultRig struct {
	g      *rig
	spec   runtime.SpawnSpec
	m      *supervise.Machine
	clock  *supervisetest.Clock
	dialer *storeDialer
	relay  *realLauncher
	// dsn is the session database's URL; claude, when not nil, is the Claude Code start the relay
	// starts in Oh My Pi's place (newClaudeStoreFaultRig).
	dsn    string
	claude *claudeResume
	// unlock ends lockRows' hold, nil while none is held.
	unlock func()

	mu   sync.Mutex
	told []string
}

// claudeResume is a Claude Code start of the claim's session that the relay starts in place of
// the runtime's Oh My Pi start: the environment names the session's id and a config directory of
// the test's own, the resume file is the session's transcript under it, which the table holds, or
// none for a fresh start, and the child is a shell that waits, since the launcher's keeper, not the
// child, is the subject.
type claudeResume struct {
	configDir, transcript string
	fresh                 bool
}

func (s *storeFaultRig) faults() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.told)
}

// lockRows holds the session's parts locked in a transaction of the test's own until unlockRows,
// so that a read of the session's rows waits past the start's bound, as a table that does not
// answer it does.
func (s *storeFaultRig) lockRows(t *testing.T) {
	t.Helper()
	conn, err := ompsessions.ConnectURL(context.Background(), s.dsn)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), "LOCK TABLE omp_session_files_parts IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	s.unlock = func() {
		once.Do(func() {
			_ = tx.Rollback(context.Background())
			_ = conn.Close(context.Background())
		})
	}
	t.Cleanup(s.unlock)
}

// unlockRows ends lockRows' hold, if one is held.
func (s *storeFaultRig) unlockRows() {
	if s.unlock != nil {
		s.unlock()
	}
}

func newStoreFaultRig(t *testing.T, omp string, stopGrace time.Duration, edits ...func(*launcher.Config)) *storeFaultRig {
	t.Helper()
	return storeFaultRigOf(t, omp, stopGrace, nil, edits...)
}

// newClaudeStoreFaultRig is newStoreFaultRig whose relay starts a Claude Code start of the claim's
// session (claudeResume) in Oh My Pi's place: a resume of the transcript the table holds, or with
// fresh a start of the session that names no transcript yet.
func newClaudeStoreFaultRig(t *testing.T, omp string, fresh bool) *storeFaultRig {
	t.Helper()
	configDir := t.TempDir()
	session := claudelaunch.NewSessionID()
	return storeFaultRigOf(t, omp, 0, &claudeResume{configDir: configDir, fresh: fresh,
		transcript: filepath.Join(configDir, "projects", "-legion-workspaces-store-fault", session+".jsonl")})
}

// storeFaultRigOf is newStoreFaultRig, the relay starting claude in Oh My Pi's place when it is not
// nil.
func storeFaultRigOf(t *testing.T, omp string, stopGrace time.Duration, claude *claudeResume, edits ...func(*launcher.Config)) *storeFaultRig {
	t.Helper()
	dsn := testdb.Database(t, "legion_sandbox_store_fault_test")
	database, address := sessionDatabaseOf(t, dsn)
	seedSession(t, dsn, storeFaultSessionFile)
	if claude != nil {
		seedSession(t, dsn, claude.transcript)
	}
	home := t.TempDir()
	testbin.OMPHome(t, omp, home)
	testbin.WriteOMPProfile(t, home, "providers:\n  operator:\n    baseUrl: http://127.0.0.1:9\n    auth: apiKey\n    api: anthropic-messages\n"+
		"    apiKey: unused\n    models:\n      - id: store-fault-test\n        name: Store fault test\n", "modelRoles:\n  default: operator/store-fault-test\n")

	g := newRig(t, nil, withOptions(func(o *Options) {
		o.SessionStore = ompsessions.Store{Kind: ompsessions.StoreCloudSQLIAM, Database: database}
		// A real launcher's start reaches the real database and the real Oh My Pi, which a
		// loaded machine is slow to: the runtime's wait on it is not this test's subject.
		o.BootTimeout = 2 * time.Minute
	}))
	spec := workerSpec(t)
	dialer := &storeDialer{address: address, refused: testdb.RefusedDial(t)}
	// A start the store does not answer is refused once the lookup's bound is out: three seconds,
	// as the launcher's own tests take, rather than the daemon's thirty, since what these cases
	// test is the minute's wait and the same session after it, not how long a lookup asks.
	cfg := launcher.Config{StopGrace: 10 * time.Second, SessionLookup: 3 * time.Second, SessionDatabase: database, SessionDialer: dialer.build}
	if stopGrace > 0 {
		cfg.StopGrace = stopGrace
	}
	for _, edit := range edits {
		edit(&cfg)
	}
	relay := g.realLauncher(t, spec.Claim, cfg, func(l *realLauncher, start shimwire.LauncherStart) shimwire.LauncherStart {
		dsnFile := filepath.Join(l.privateDir(), shimwire.GenerationDir(start.Generation), ompsessions.DSNFileName)
		if !slices.Contains(start.Env, ompsessions.StorageVariable+"="+ompsessions.SQLStorage) ||
			!slices.ContainsFunc(start.Env, func(pair string) bool {
				return strings.HasPrefix(pair, ompsessions.DSNFileVariable+"=") && strings.HasSuffix(pair, "/"+shimwire.GenerationDir(start.Generation)+"/"+ompsessions.DSNFileName)
			}) {
			t.Errorf("the start of generation %d names no SQL storage at its generation's URL file: %q", start.Generation, start.Env)
		}
		start.Argv = []string{omp, "--mode", "rpc"}
		start.Env = append(testbin.OMPEnviron(home), ompsessions.StorageVariable+"="+ompsessions.SQLStorage,
			ompsessions.DSNFileVariable+"="+dsnFile, "LEGION_WORKSPACE="+filepath.Join(home, "workspace"))
		if claude != nil {
			start.Argv = []string{"/bin/sh", "-c", "exec sleep 600"}
			start.Env = append(start.Env, "CLAUDE_CONFIG_DIR="+claude.configDir,
				claudelaunch.SessionVariable+"="+strings.TrimSuffix(filepath.Base(claude.transcript), ".jsonl"))
			start.ResumeFile = claude.transcript
			if claude.fresh {
				start.ResumeFile = ""
			}
		}
		return start
	})
	s := &storeFaultRig{g: g, spec: spec, clock: supervisetest.NewClock(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)), dialer: dialer, relay: relay,
		dsn: dsn, claude: claude}
	suspended := supervise.Claim{Token: spec.Claim, Project: spec.Project, Tree: spec.Tree, Issue: spec.Issue, Role: spec.Role,
		Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByOperator,
		Session: storeFaultSession, SessionFile: storeFaultSessionFile}
	s.m, _, _ = clockedSandboxMachine(t, g, spec, suspended, io.Discard, s.clock, func(_ supervise.Claim, detail string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.told = append(s.told, detail)
	})
	g.launcher(spec.Claim)
	return s
}

// awaitExit is the runtime's observation of the process loc records once its launcher reports that
// process's exit: a real Oh My Pi that a store closes on can take thirty seconds to give up.
func (s *storeFaultRig) awaitExit(t *testing.T, loc runtime.Locator) runtime.Observation {
	t.Helper()
	if !testwait.Holds(2*time.Minute, testwait.Poll, func() bool {
		state, connected := s.g.r.launchers.state(s.spec.Claim, loc.Sandbox.PodUID)
		return connected && state.LastExit != nil && state.LastExit.Generation == loc.Sandbox.Generation
	}) {
		t.Fatalf("generation %d never exited; the launcher's log:\n%s", loc.Sandbox.Generation, s.relay.log())
	}
	obs, err := s.g.r.Probe(s.g.ctx, loc)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return obs
}

// sessionDatabaseOf is the test Postgres at dsn as the cloudsql-iam store's database a launcher
// proxies to, with the address its upstream dialer reaches it at. The login is the test database's
// own, whose password goes into the environment as libpq's PGPASSWORD, where the launcher's own
// client and the real Oh My Pi's both read it: the URL the launcher builds for the proxy's socket
// carries none, as an IAM login's does not.
func sessionDatabaseOf(t *testing.T, dsn string) (ompsessions.Database, string) {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := parsed.User.Password()
	t.Setenv("PGPASSWORD", password)
	address := parsed.Host
	if _, _, err := net.SplitHostPort(address); err != nil {
		address = net.JoinHostPort(address, "5432")
	}
	return ompsessions.Database{Instance: "example-project:us-central1:example-instance", Name: strings.TrimPrefix(parsed.Path, "/"),
		User: parsed.User.Username()}, address
}

// seedSession makes Oh My Pi's tables at dsn and puts a session at path in them, as an earlier
// generation left it.
func seedSession(t *testing.T, dsn, path string) {
	t.Helper()
	conn, err := ompsessions.ConnectURL(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := ompsessions.Import(context.Background(), conn, path, []byte(storeFaultTranscript), time.Now()); err != nil {
		t.Fatal(err)
	}
}

// storeDialer stands in for the Cloud SQL connector: each dial reaches the test's Postgres at
// address, or the address reach named in its place, unless refuseFrom names a dial from which on
// every dial is refused by the host, with refused, the error a real refused dial gave, or stall
// holds every dial until its context ends, as an instance that never answers does. The dials are
// counted from 1: a resume's are the proof dial, the launcher's own lookup, then the child's.
type storeDialer struct {
	address    string
	refused    error
	reached    atomic.Pointer[string]
	refuseFrom atomic.Int32
	stall      atomic.Bool
	dials      atomic.Int32
}

func (d *storeDialer) Dial(ctx context.Context, _ string) (net.Conn, error) {
	n := d.dials.Add(1)
	if d.stall.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if from := d.refuseFrom.Load(); from > 0 && n >= from {
		return nil, d.refused
	}
	address := d.address
	if reached := d.reached.Load(); reached != nil {
		address = *reached
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", address)
}

func (*storeDialer) Close() error { return nil }

func (d *storeDialer) build(context.Context, string) (sessionproxy.Dialer, error) { return d, nil }

// reach has every dial from now on reach address in the database's place.
func (d *storeDialer) reach(address string) {
	d.reached.Store(&address)
}

// answer has every dial from now on reach the database.
func (d *storeDialer) answer() {
	d.refuseFrom.Store(0)
	d.stall.Store(false)
	d.reached.Store(nil)
}

// realLauncher is a real `legion launcher` (launcher.Run) for one role, started for every pod the
// rig's controller makes for its issue, and the relay between this runtime and it, through which
// the runtime's launcher_start frames pass rewritten (rewrite) and every other frame as it is.
type realLauncher struct {
	t       *testing.T
	g       *rig
	token   claim.Token
	cfg     launcher.Config
	rewrite func(*realLauncher, shimwire.LauncherStart) shimwire.LauncherStart
	root    string
	logs    *testlog.Buffer

	mu      sync.Mutex
	next    int
	private string
	stop    context.CancelFunc
	done    chan struct{}
	starts  []shimwire.LauncherStart
}

// realLauncher has every pod of token's issue run a real launcher for token's role, configured as
// cfg, whose start frames pass through rewrite.
func (g *rig) realLauncher(t *testing.T, token claim.Token, cfg launcher.Config,
	rewrite func(*realLauncher, shimwire.LauncherStart) shimwire.LauncherStart) *realLauncher {
	t.Helper()
	// Each launcher's private directory holds its generations' session sockets, whose paths a unix
	// socket bounds (docs/testing.md): the root is short enough for the longest of them.
	root := testtmp.Dir(t, filepath.Join("99", shimwire.GenerationDir(999), ompsessions.SocketName))
	l := &realLauncher{t: t, g: g, token: token, cfg: cfg, rewrite: rewrite, root: root, logs: &testlog.Buffer{}}
	g.commandsMu.Lock()
	if g.real == nil {
		g.real = map[claim.Token]func(*corev1.Pod){}
	}
	g.real[token] = l.podReady
	g.commandsMu.Unlock()
	t.Cleanup(l.end)
	return l
}

// podReady starts the launcher of pod, ending the previous pod's: a pod's launcher is its own.
func (l *realLauncher) podReady(pod *corev1.Pod) {
	l.end()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(l.logs, "listen for the launcher of pod %s: %v\n", pod.Name, err)
		return
	}
	l.mu.Lock()
	l.next++
	private := filepath.Join(l.root, fmt.Sprint(l.next))
	l.mu.Unlock()
	if err := os.MkdirAll(private, 0o700); err != nil {
		fmt.Fprintf(l.logs, "make the launcher's private directory: %v\n", err)
		_ = listener.Close()
		return
	}
	cfg := l.cfg
	cfg.Connect, cfg.Token, cfg.Sandbox, cfg.Role = "tcp://"+listener.Addr().String(), "launcher-token", pod.Name, string(l.role())
	cfg.PodUID, cfg.PrivateDir, cfg.Stdout, cfg.Stderr = string(pod.UID), private, l.logs, l.logs
	ctx, cancel := context.WithCancel(l.g.ctx)
	done := make(chan struct{})
	l.mu.Lock()
	l.private, l.stop, l.done = private, cancel, done
	l.mu.Unlock()
	go func() {
		defer close(done)
		defer listener.Close()
		go l.accept(listener, pod)
		if err := launcher.Run(ctx, cfg); err != nil && ctx.Err() == nil {
			fmt.Fprintf(l.logs, "launcher of pod %s: %v\n", pod.Name, err)
		}
	}()
}

func (l *realLauncher) role() claim.Role {
	_, role, _ := l.token.Cut()
	return role
}

// accept takes each connection the launcher dials, answers its hello as the worker stream's
// listener does once the runtime's resolver accepts it, and relays it to the runtime.
func (l *realLauncher) accept(listener net.Listener, pod *corev1.Pod) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		reader := bufio.NewReader(conn)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			_ = conn.Close()
			continue
		}
		frame, err := shimwire.Decode(line)
		hello, ok := frame.(shimwire.LauncherHello)
		if err != nil || !ok {
			_ = conn.Close()
			continue
		}
		toLauncher := shimwire.NewWriter(conn)
		if err := toLauncher.WriteFrame(shimwire.LauncherHelloAck{}); err != nil {
			_ = conn.Close()
			continue
		}
		session := l.g.r.launchers.accept(l.token, launcherIssue(l.token), pod.CreationTimestamp.Time, l.g.keptSince(l.token), hello)
		daemonEnd, relayEnd := net.Pipe()
		go session.ServeLauncher(daemonEnd, bufio.NewReader(daemonEnd), shimwire.NewWriter(daemonEnd))
		go func() {
			_, _ = io.Copy(relayEnd, reader)
			_ = relayEnd.Close()
		}()
		go func() {
			defer conn.Close()
			fromDaemon := bufio.NewReader(relayEnd)
			for {
				line, err := fromDaemon.ReadBytes('\n')
				if err != nil {
					return
				}
				frame, err := shimwire.Decode(line)
				if err != nil {
					return
				}
				if start, ok := frame.(shimwire.LauncherStart); ok {
					l.mu.Lock()
					l.starts = append(l.starts, start)
					l.mu.Unlock()
					frame = l.rewrite(l, start)
				}
				if err := toLauncher.WriteFrame(frame); err != nil {
					return
				}
			}
		}()
	}
}

// end stops the running launcher, if any, and waits for it.
func (l *realLauncher) end() {
	l.mu.Lock()
	stop, done := l.stop, l.done
	l.stop, l.done = nil, nil
	l.mu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}

func (l *realLauncher) privateDir() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.private
}

// startCount is how many launcher_start frames the runtime sent, and lastStart the last of them
// as the runtime sent it.
func (l *realLauncher) startCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.starts)
}

func (l *realLauncher) lastStart() shimwire.LauncherStart {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.starts) == 0 {
		return shimwire.LauncherStart{}
	}
	return l.starts[len(l.starts)-1]
}

func (l *realLauncher) log() string { return l.logs.String() }
