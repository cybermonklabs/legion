package launcher

// A start whose session store did not answer says so in its answer's own field
// (shimwire.SessionStoreUnreachable), which the daemon reads as a store fault: the claim keeps its
// session, is charged nothing, and is started again a minute later (runtime/sandbox's
// startRefusal, supervise's startWaitsForStore). A store did not answer when it refused the dial
// or never answered it, or holds no session table (ompsessions.StoreFault). These are the faults a
// start meets past the resume's own lookup: the generation's proxy to a session database of the
// pod's own, and a Claude Code session's keeper dialing the table and restoring the session's files
// from it. A child that exits on a store that stopped answering it exits as such a fault too. Every
// other failure of a start's store is its configuration's, a refusal the daemon charges, naming it.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/sessionproxy"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// A generation whose proxy's proof dial the session database refused or never answered, or whose
// resume's lookup through the proxy the database refused, or answered that it cannot take a
// connection now (57P03), once the proof dial had passed, is refused as a session store that did
// not answer, having started nothing. The lookup's own client reads a refused dial as its
// connection closing; the proxy recorded it (Proxy.Fault). A server's own answer crosses the proxy
// as it is. The next start of the same session, once the database answers, runs.
func TestAProxyThatCannotReachTheSessionDatabaseIsAStoreFaultAndTheNextStartResumes(t *testing.T) {
	for name, fault := range map[string]struct {
		// refuseFrom is the first dial the database refuses, the proof dial being the first.
		refuseFrom int32
		stall      bool
		// startingUp is whether every dial reaches a server that is starting up instead.
		startingUp bool
		// names is what the refusal names of the step that met the fault.
		names string
	}{
		"the proof dial is refused":                         {refuseFrom: 1, names: "the session database proxy for generation 3"},
		"the proof dial is not answered":                    {stall: true, names: "the session database proxy for generation 3"},
		"the lookup is refused after the proof dial passed": {refuseFrom: 2, names: "resume session "},
		"the lookup meets an instance starting up (57P03)":  {startingUp: true, names: "the database system is starting up"},
	} {
		t.Run(name, func(t *testing.T) {
			dsn := testdb.Database(t, "legion_launcher_proxy_fault_test")
			createSessionTables(t, dsn)
			database, dialer := sessionDatabaseAt(t, dsn)
			shortenResumeLookup(t)
			refused := testdb.RefusedDial(t)
			var down atomic.Bool
			var dials atomic.Int32
			down.Store(true)
			dialer.check = func() error {
				if n := dials.Add(1); fault.refuseFrom > 0 && n >= fault.refuseFrom && down.Load() {
					return refused
				}
				return nil
			}
			cfg := Config{SessionDatabase: database, SessionDialer: dialer.build}
			if fault.stall {
				// A grace of 2 s bounds the stalled proof dial at 1 s (proofTimeout), and leaves
				// the next generation's real proof dial the same second.
				dialer.stallAt(silentUpstream(t))
				cfg.StopGrace = 2 * time.Second
			}
			if fault.startingUp {
				dialer.dialAt(testdb.Refusing(t, "57P03", "the database system is starting up"))
			}
			g := newSessionRig(t, cfg)
			sessionFile := filepath.Join(t.TempDir(), "sessions", "2026-10-08T12-00-00-000Z_0007.jsonl")
			importSession(t, dsn, sessionFile)
			marker := filepath.Join(t.TempDir(), "marker")
			start := func(generation uint64) shimwire.LauncherStartResult {
				command := g.childStart(generation, marker, nil)
				command.Env = append(command.Env, ompsessions.StorageVariable+"="+ompsessions.SQLStorage,
					ompsessions.DSNFileVariable+"="+filepath.Join(g.generationDir(generation), ompsessions.DSNFileName))
				command.ResumeFile = sessionFile
				return g.start(command)
			}

			if got := start(3); got.OK || got.Session != shimwire.SessionStoreUnreachable || !strings.Contains(got.Error, fault.names) {
				t.Fatalf("start over a proxy that cannot reach the session database = %#v, want a refusal naming %q as %q", got, fault.names, shimwire.SessionStoreUnreachable)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a generation whose proxy could not reach the session database started its child")
			}
			down.Store(false)
			dialer.stallAt("")
			if fault.startingUp {
				parsed, err := url.Parse(dsn)
				if err != nil {
					t.Fatal(err)
				}
				dialer.dialAt(parsed.Host)
			}
			if got := start(4); !got.OK {
				t.Fatalf("the next start of the same session once the database answers = %#v, want generation 4 to run", got)
			}
			if seen := publishedAll(t, marker); seen.socket != "bound" {
				t.Fatalf("generation 4's child found its session socket %s, want it already bound", seen.socket)
			}
			g.stop(4)
		})
	}
}

// A Claude Code start whose keeper's own dial of the table is refused or meets a server that cannot
// take a connection now (57P03), or whose restore cannot read the session's rows back, is refused
// as a session store that did not answer, having started nothing. The dial is a fresh start's,
// whose keeper reaches the table itself; the rows that cannot be read are ones another transaction
// holds locked past the start's bound, or ones whose read the store ends by resetting or closing
// the connection under it. The next start, once the store answers, runs, and a resume's child
// starts on the transcript the table holds.
func TestAClaudeKeeperThatCannotReachTheTableIsAStoreFaultAndTheNextStartRestores(t *testing.T) {
	// pointAt has the URL file name url for the start, and the database again once it is over.
	pointAt := func(url func(t *testing.T, dsn string) string) func(t *testing.T, c *claudeRig, dsn string) func() {
		return func(t *testing.T, c *claudeRig, dsn string) func() {
			if err := replaceURLFile(c.dsnFile, url(t, dsn)); err != nil {
				t.Fatal(err)
			}
			return func() {
				if err := replaceURLFile(c.dsnFile, dsn); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// endingTheRestore is the database through a proxy that ends the connection the restore reads
	// the session's parts on, as it sends that read.
	endingTheRestore := func(reset bool) func(t *testing.T, dsn string) string {
		return func(t *testing.T, dsn string) string {
			return testdb.Breaking(t, dsn, "omp_session_files_parts WHERE path", reset)
		}
	}
	restoreRefusal := func(c *claudeRig) string {
		return "keep Claude Code session " + claudeSession + ": restore " + c.transcript
	}
	for name, fault := range map[string]struct {
		// resume is whether the start resumes the transcript or starts the session fresh.
		resume bool
		// refusal is what the start's refusal names of the step that met the fault.
		refusal func(c *claudeRig) string
		// fail puts the fault in place and returns its end.
		fail func(t *testing.T, c *claudeRig, dsn string) (end func())
	}{
		"the keeper's own dial": {
			refusal: func(*claudeRig) string { return "keep Claude Code session " + claudeSession },
			fail:    pointAt(func(t *testing.T, _ string) string { return refusingURL(t) }),
		},
		"the keeper's own dial, on a server starting up (57P03)": {
			refusal: func(*claudeRig) string { return "keep Claude Code session " + claudeSession },
			fail:    pointAt(startingUp),
		},
		"the restore, whose connection the store resets": {resume: true, refusal: restoreRefusal, fail: pointAt(endingTheRestore(true))},
		"the restore, whose connection the store closes": {resume: true, refusal: restoreRefusal, fail: pointAt(endingTheRestore(false))},
		"the restore, whose rows another transaction holds locked": {
			resume:  true,
			refusal: restoreRefusal,
			fail: func(t *testing.T, _ *claudeRig, dsn string) func() {
				conn, err := ompsessions.ConnectURL(context.Background(), dsn)
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
				return func() {
					_ = tx.Rollback(context.Background())
					_ = conn.Close(context.Background())
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dsn, dsnFile := testdb.DSNFile(t, "legion_launcher_claude_fault_test")
			c := claudeRigOn(t, newSessionRig(t, Config{}), dsnFile)
			shortenResumeLookup(t)
			saved := "{\"n\":1}\n{\"n\":2}\n"
			c.seed(c.transcript, saved, time.Now())

			end := fault.fail(t, c, dsn)
			told := filepath.Join(t.TempDir(), "seen")
			got := c.start(c.command(1, seeing(c.transcript, told), fault.resume))
			end()
			if want := fault.refusal(c); got.OK || got.Session != shimwire.SessionStoreUnreachable || !strings.Contains(got.Error, want) {
				t.Fatalf("a Claude Code start whose store did not answer = %#v, want a refusal naming %q as %q", got, want, shimwire.SessionStoreUnreachable)
			}
			if _, err := os.Stat(told); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a Claude Code start whose store did not answer started its child")
			}

			_ = os.RemoveAll(filepath.Join(c.configDir, "projects"))
			if got := c.start(c.command(2, seeing(c.transcript, told), fault.resume)); !got.OK {
				t.Fatalf("the next start of the same session once the store answers = %#v, want generation 2 to run", got)
			}
			want := "missing"
			if fault.resume {
				want = saved
			}
			if found := seen(t, told); found != want {
				t.Errorf("generation 2's child found the transcript %q at its start, want %q", found, want)
			}
			c.stop(2)
		})
	}
}

// A start whose session store is configured wrong is the start's own failure, which the daemon
// charges, and its refusal names the cause: the store answered, or was never asked. A credential
// that is not there, an environment that names no URL file, a URL file that is not there, a
// password the server refuses, a database that takes no writes (an address that names a replica):
// each is refused plainly, having started nothing, on both harnesses, fresh or resumed. The
// control is a URL file whose store refuses the dial, which is the store's fault.
func TestAStartWhoseSessionStoreIsConfiguredWrongIsChargedNamingItsCause(t *testing.T) {
	dsn := testdb.Database(t, "legion_launcher_config_fault_test")
	createSessionTables(t, dsn)
	shortenResumeLookup(t)
	missing := filepath.Join(t.TempDir(), "absent-dsn")
	wrongPassword := filepath.Join(t.TempDir(), "wrong-password-dsn")
	if err := replaceURLFile(wrongPassword, withPassword(t, dsn, "not-the-password")); err != nil {
		t.Fatal(err)
	}
	refusing := filepath.Join(t.TempDir(), "refusing-dsn")
	if err := replaceURLFile(refusing, refusingURL(t)); err != nil {
		t.Fatal(err)
	}
	readOnly := readOnlyURLFile(t)
	noHost := filepath.Join(t.TempDir(), "no-host-dsn")
	if err := replaceURLFile(noHost, "postgres://legion@no-such-session-store.invalid:5432/none?sslmode=disable"); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(t.TempDir(), "sessions", "2026-10-08T12-00-00-000Z_0009.jsonl")
	importSession(t, dsn, sessionFile)
	omp := func(dsnFile string) func(t *testing.T) (shimwire.LauncherStartResult, string) {
		return func(t *testing.T) (shimwire.LauncherStartResult, string) {
			g := newRig(t)
			marker := filepath.Join(t.TempDir(), "marker")
			command := g.childStart(3, marker, nil)
			command.Env = append(command.Env, ompsessions.StorageVariable+"="+ompsessions.SQLStorage, ompsessions.DSNFileVariable+"="+dsnFile)
			command.ResumeFile = sessionFile
			return g.start(command), marker
		}
	}
	claude := func(dsnFile string, resume bool) func(t *testing.T) (shimwire.LauncherStartResult, string) {
		return func(t *testing.T) (shimwire.LauncherStartResult, string) {
			c := claudeRigOn(t, newRig(t), testdbFile(t, dsn))
			c.seed(c.transcript, "{\"n\":1}\n", time.Now())
			told := filepath.Join(t.TempDir(), "seen")
			return c.start(c.command(1, seeing(c.transcript, told), resume, ompsessions.DSNFileVariable+"="+dsnFile)), told
		}
	}
	for name, tc := range map[string]struct {
		start func(t *testing.T) (shimwire.LauncherStartResult, string)
		// names is what the refusal must name; storeFault is whether it is the store's.
		names      string
		storeFault bool
	}{
		"an Oh My Pi resume whose proxy's credential is not there": {names: "could not find default credentials",
			start: func(t *testing.T) (shimwire.LauncherStartResult, string) {
				database, _ := sessionDatabaseAt(t, dsn)
				g := newSessionRig(t, Config{SessionDatabase: database, SessionDialer: func(context.Context, string) (sessionproxy.Dialer, error) {
					return nil, errors.New("google: could not find default credentials")
				}})
				marker := filepath.Join(t.TempDir(), "marker")
				command := g.childStart(3, marker, nil)
				command.Env = append(command.Env, ompsessions.StorageVariable+"="+ompsessions.SQLStorage,
					ompsessions.DSNFileVariable+"="+filepath.Join(g.generationDir(3), ompsessions.DSNFileName))
				command.ResumeFile = sessionFile
				return g.start(command), marker
			}},
		"an Oh My Pi resume whose environment names no URL file": {names: ompsessions.DSNFileVariable + " names no file", start: omp("")},
		"an Oh My Pi resume whose URL file is not there":         {names: missing, start: omp(missing)},
		"an Oh My Pi resume whose server refuses the password":   {names: "password authentication failed", start: omp(wrongPassword)},
		"an Oh My Pi resume whose URL file names a host that does not resolve": {names: "no-such-session-store.invalid",
			start: omp(noHost)},
		"a fresh Claude Code start whose URL file is not there": {names: missing, start: claude(missing, false)},
		"a Claude Code resume whose environment names no URL file": {names: ompsessions.DSNFileVariable + " names no file",
			start: claude("", true)},
		"a fresh Claude Code start whose server refuses the password": {names: "password authentication failed", start: claude(wrongPassword, false)},
		"a fresh Claude Code start whose database takes no writes":    {names: "read-only transaction", start: claude(readOnly, false)},
		"control: an Oh My Pi resume whose store refuses the dial": {names: "the session store could not be asked", storeFault: true,
			start: omp(refusing)},
		"control: a fresh Claude Code start whose store refuses the dial": {names: "the session store could not be asked", storeFault: true,
			start: claude(refusing, false)},
	} {
		t.Run(name, func(t *testing.T) {
			got, marker := tc.start(t)
			want := shimwire.SessionRefusal("")
			if tc.storeFault {
				want = shimwire.SessionStoreUnreachable
			}
			if got.OK || got.Session != want || !strings.Contains(got.Error, tc.names) {
				t.Fatalf("start = %#v, want a refusal naming %q with session refusal %q", got, tc.names, want)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a refused start started its child")
			}
		})
	}
}

// A Claude Code child never dials the session database itself, so an exit of its own is its own
// whatever its keeper met: under the generation's proxy, a child that exits 3 while the store ends
// its keeper's connection and refuses every dial after, the keeper's last save after the exit
// among them, exits as itself. The control is the same exit with the store answering. A child that
// dials the store itself and is refused exits as a store fault
// (TestAChildThatExitsOnAStoreThatStoppedAnsweringItExitsAsAStoreFault).
func TestAClaudeCodeChildsOwnExitIsItsOwnWhateverItsKeeperMet(t *testing.T) {
	for name, down := range map[string]bool{"the store refuses the keeper": true, "control: the store answers": false} {
		t.Run(name, func(t *testing.T) {
			dsn, dsnFile := testdb.DSNFile(t, "legion_launcher_claude_own_exit_test")
			database, dialer := sessionDatabaseAt(t, dsn)
			refused := testdb.RefusedDial(t)
			var refusing atomic.Bool
			dialer.check = func() error {
				if refusing.Load() {
					return refused
				}
				return nil
			}
			c := claudeRigOn(t, newSessionRig(t, Config{SessionDatabase: database, SessionDialer: dialer.build}), dsnFile)
			trigger := filepath.Join(t.TempDir(), "exit-now")
			c.launch(1, `while [ ! -e '`+trigger+`' ]; do sleep 0.1; done; exit 3`, false)
			if err := os.MkdirAll(filepath.Dir(c.transcript), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(c.transcript, []byte("{\"n\":1}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			c.awaitRow(c.transcript, "{\"n\":1}\n")
			if down {
				refusing.Store(true)
				if _, err := c.conn.Exec(context.Background(),
					"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(c.transcript, []byte("{\"n\":1}\n{\"n\":2}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			time.Sleep(3 * saveInterval)
			if err := os.WriteFile(trigger, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if exit := c.awaitExitWithin(1, time.Minute); exit.Code != 3 || exit.Session != "" {
				t.Fatalf("the Claude Code child's own exit = %+v, want status 3 and no session refusal\nlauncher log:\n%s", exit, c.stderr.String())
			}
		})
	}
}

// A lookup whose last attempt the bound cuts short refuses with the last answer the database gave
// on an attempt that ended on its own (sessionDial.dial): a database that answered every attempt
// with a refusal of its own, here a statement it judged (22012), refused the start, which the daemon
// charges, and an attempt the bound cut short says nothing the one before it did not. Read as the
// cut, it would be a store that never answered, waited out for ever.
func TestALookupWhoseLastAttemptTheBoundCutsShortKeepsTheDatabasesLastAnswer(t *testing.T) {
	_, dsnFile := testdb.DSNFile(t, "legion_launcher_last_answer_test")
	savedBackoff, savedTimeout := resumeLookupBackoff, resumeLookupTimeout
	resumeLookupBackoff, resumeLookupTimeout = []time.Duration{100 * time.Millisecond}, 5*time.Second
	t.Cleanup(func() { resumeLookupBackoff, resumeLookupTimeout = savedBackoff, savedTimeout })
	attempts := 0
	conn, err := sessionDial{dsnFile: dsnFile}.dial(context.Background(), func(ctx context.Context, conn *pgx.Conn) error {
		attempts++
		statement := "SELECT 1/0"
		if attempts > 2 {
			// This attempt ends only when the bound does.
			statement = "SELECT pg_sleep(60)"
		}
		_, err := conn.Exec(ctx, statement)
		return err
	})
	closeSessions(conn)
	var answer *pgconn.PgError
	if attempts < 3 || !errors.As(err, &answer) || answer.Code != "22012" || ompsessions.StoreFault(err) {
		t.Fatalf("a lookup refused twice, then cut short by its bound, answered %v after %d attempts; want the database's own 22012, no store fault", err, attempts)
	}
}

// A child that keeps its sessions in the store and exits on its own with an error, on a store that
// stopped answering it after the start's checks passed, exits as a store fault
// (shimwire.LauncherExit.Session), under either session store. Under the generation's proxy the
// child's own connections decide it: a dial the database refuses, a served connection it resets.
// Its report is not read there: a child whose sign-in the instance refused after the proof dial
// reports a connection that failed before the server answered it, as it does a refused dial, and
// is charged. Under a URL file the child's own report decides where the launcher's own connect to
// the same URL file meets no fault of the configuration's: a server's answer whose SQLSTATE is of
// the server's own state, however the store answers the launcher; not a connection that failed
// before the server answered on a URL file that names a web server's port. With no fault there,
// the launcher asks the store once after the exit, and a store that refuses it or answers that it
// cannot take a connection now (57P03) marks the exit too: under the proxy, the ask's own dial the
// database refused, after a child it served. A child that exits while the store answers exits as
// itself, and so does a child that ends at 0 on a store that fails it. Under the proxy the child is
// the test binary dialing its socket as Oh My Pi does (TestLauncherStoreChild), or the pinned Oh My
// Pi itself, whose refused sign-in it reports only after thirty seconds of its own retries; under
// a URL file it is a shell, since what those cases test is how the launcher reads an exit, and one
// case runs the pinned Oh My Pi itself, which exits at once on a store that refuses it. A shell
// that reports writes the line an Oh My Pi whose first open failed writes (reports): an Oh My Pi's
// that prints the server's SQLSTATE, and, as unit rows of the launcher's reading alone, a replay of
// the pinned Oh My Pi's own line for a connection the proxy or a web server closed, which the
// pinned Oh My Pi's own rows here and in TestAPinnedOhMyPiThatReportsItsStoreRefusedItExitsAsAStoreFault
// prove it writes.
func TestAChildThatExitsOnAStoreThatStoppedAnsweringItExitsAsAStoreFault(t *testing.T) {
	// reports is a shell child that writes the report of an Oh My Pi whose first open of the store
	// failed, with detail after the URL file's name, and exits 1.
	reports := func(detail string) string {
		return `printf 'Error: %s names %s, but the session database could not be opened: %s\n' ` + ompsessions.DSNFileVariable +
			` "$` + ompsessions.DSNFileVariable + `" '` + detail + `' >&2; exit 1`
	}
	const closed = "Connection closed before the connection was established (ERR_POSTGRES_CONNECTION_FAILED)"
	for name, tc := range map[string]struct {
		// proxied is whether the generation has a proxy of its own, and target, given the test
		// database's URL, the URL of the store its child and the launcher reach.
		proxied bool
		target  func(t *testing.T, dsn string) string
		// refuseFrom is the first of the proxy's dials it refuses, 0 for none: the proof dial is
		// the first, the child's own the second, the launcher's ask at the exit the third; signIn
		// refuses them as an instance refuses a sign-in, a fault of the configuration's, rather
		// than as a database that is down, and refuseOnce refuses refuseFrom alone. send is what
		// the proxied child writes on its connection, and report what it writes on its standard
		// error before it exits.
		refuseFrom int32
		signIn     bool
		refuseOnce bool
		send       string
		report     string
		// shell, when set, is the child, run by /bin/sh; omp runs the pinned Oh My Pi. reported, when
		// set, is what the child's report of its failed open must name, so the case is one the
		// report could decide.
		shell    string
		omp      bool
		reported string
		code     int
		want     shimwire.SessionRefusal
	}{
		"under the proxy, the child's own connection is refused": {proxied: true, refuseFrom: 2, code: 1,
			want: shimwire.SessionStoreUnreachable},
		"under the proxy, the database resets the child's connection": {proxied: true, send: breakMarker, code: 1,
			target: func(t *testing.T, dsn string) string { return testdb.Breaking(t, dsn, breakMarker, true) },
			want:   shimwire.SessionStoreUnreachable},
		"under the proxy, the instance cannot take a connection now (57P03)": {proxied: true, code: 1,
			target: startingUp, want: shimwire.SessionStoreUnreachable},
		"under the proxy, the database serves the child, then refuses the launcher's ask": {proxied: true, refuseFrom: 3, code: 1,
			want: shimwire.SessionStoreUnreachable},
		"control: under the proxy, the store answers a child that fails": {proxied: true, code: 1},
		"control: under the proxy, the instance refuses the pinned Oh My Pi's sign-in after the proof dial": {proxied: true, omp: true,
			refuseFrom: 2, signIn: true, reported: connectionFailed, code: 1},
		"control: under the proxy, the instance refuses the child's own sign-in and answers the launcher's ask, the child reporting its connection closed": {proxied: true,
			refuseFrom: 2, refuseOnce: true, signIn: true, code: 1,
			report: "Error: " + ompsessions.DSNFileVariable + " names its file, but the session database could not be opened: Connection closed before the connection was established (ERR_POSTGRES_CONNECTION_FAILED)"},
		"control, a replay of the pinned Oh My Pi's line: under the proxy, the sign-in refused after the proof dial": {proxied: true,
			refuseFrom: 2, signIn: true, shell: reports(closed), code: 1},
		"under a URL file, the store refuses the pinned Oh My Pi and the launcher": {omp: true, code: 1,
			target: func(t *testing.T, _ string) string { return refusingURL(t) }, want: shimwire.SessionStoreUnreachable},
		"under a URL file, the store cannot take a connection now (57P03)": {shell: "exit 1", code: 1,
			target: startingUp, want: shimwire.SessionStoreUnreachable},
		"under a URL file, the child reports its server starting up (57P03) and the store answers the launcher": {code: 1,
			shell: reports("the database system is starting up (ERR_POSTGRES_SERVER_ERROR 57P03)"), want: shimwire.SessionStoreUnreachable},
		"under a URL file, the child reports its server has no client slot left (53300) and the store answers the launcher": {code: 1,
			shell: reports("sorry, too many clients already (ERR_POSTGRES_SERVER_ERROR 53300)"), want: shimwire.SessionStoreUnreachable},
		"control: under a URL file, the child reports its server refused the password (28P01)": {code: 1,
			shell: reports(`password authentication failed for user "legion" (ERR_POSTGRES_SERVER_ERROR 28P01)`)},
		"control, a replay of the pinned Oh My Pi's line: under a URL file that names a web server's port, the connection closed": {code: 1,
			shell: reports(closed), target: func(t *testing.T, _ string) string {
				return "postgres://legion@" + webServer(t) + "/none?sslmode=disable"
			}},
		"control: under a URL file, the store answers a child that fails":      {shell: "exit 3", code: 3},
		"control: under a URL file, the store answers and the child ends at 0": {shell: "exit 0"},
		"control: under a URL file, the store cannot take a connection now and the child ends at 0": {shell: "exit 0",
			target: startingUp},
	} {
		t.Run(name, func(t *testing.T) {
			dsn := testdb.Database(t, "legion_launcher_child_store_test")
			createSessionTables(t, dsn)
			target := dsn
			if tc.target != nil {
				target = tc.target(t, dsn)
			}
			var g *rig
			var env []string
			argv := []string{"/bin/sh", "-c", tc.shell}
			workspace := "LEGION_WORKSPACE=" + filepath.Join(t.TempDir(), "workspace")
			if tc.proxied {
				database, dialer := sessionDatabaseAt(t, target)
				refused := testdb.RefusedDial(t)
				if tc.signIn {
					refused = errors.New("the instance refused the connection's IAM sign-in")
				}
				var dials atomic.Int32
				dialer.check = func() error {
					if n := dials.Add(1); tc.refuseFrom > 0 && (n == tc.refuseFrom || n > tc.refuseFrom && !tc.refuseOnce) {
						return refused
					}
					return nil
				}
				g = newSessionRig(t, Config{SessionDatabase: database, SessionDialer: dialer.build})
				env = []string{ompsessions.StorageVariable + "=" + ompsessions.SQLStorage,
					ompsessions.DSNFileVariable + "=" + filepath.Join(g.generationDir(1), ompsessions.DSNFileName), workspace}
				if tc.shell == "" && !tc.omp {
					command := g.childStart(1, filepath.Join(t.TempDir(), "marker"), nil)
					argv = []string{command.Argv[0], "-test.run=^TestLauncherStoreChild$"}
					env = append(append(command.Env, launcherStoreChildEnv+"=1", storeChildSendEnv+"="+tc.send, storeChildReportEnv+"="+tc.report), env...)
				}
			} else {
				g = newRig(t)
				urlFile := filepath.Join(t.TempDir(), "dsn")
				if err := replaceURLFile(urlFile, target); err != nil {
					t.Fatal(err)
				}
				env = []string{ompsessions.StorageVariable + "=" + ompsessions.SQLStorage, ompsessions.DSNFileVariable + "=" + urlFile, workspace}
			}
			if tc.omp {
				ompBinary := testbin.OMP(t)
				home := t.TempDir()
				testbin.OMPHome(t, ompBinary, home)
				testbin.WriteOMPProfile(t, home, "providers:\n  operator:\n    baseUrl: http://127.0.0.1:9\n    auth: apiKey\n    api: anthropic-messages\n"+
					"    apiKey: unused\n    models:\n      - id: store-fault-test\n        name: Store fault test\n", "modelRoles:\n  default: operator/store-fault-test\n")
				argv = []string{ompBinary, "--mode", "rpc"}
				env = append(testbin.OMPEnviron(home), env...)
			}
			if got := g.start(shimwire.LauncherStart{ID: "start-1", Generation: 1, Argv: argv, Env: env}); !got.OK {
				t.Fatalf("start = %#v, want the child started", got)
			}
			exit := g.awaitExitWithin(1, 2*time.Minute)
			if exit.Session != tc.want || exit.Code != tc.code {
				t.Fatalf("the child's exit = %+v, want status %d and session refusal %q\nlauncher log:\n%s", exit, tc.code, tc.want, g.stderr.String())
			}
			if log := g.stderr.String(); tc.reported != "" && (!strings.Contains(log, openFailed) || !strings.Contains(log, tc.reported)) {
				t.Fatalf("the child reported no failed open naming %s; launcher log:\n%s", tc.reported, log)
			}
		})
	}
}

// launcherStoreChildEnv marks the test binary re-executed as a child that dials its session store
// as Oh My Pi does, through its generation's proxy (TestLauncherStoreChild), storeChildSendEnv is
// what that child writes on its connection, and storeChildReportEnv what it writes on its standard
// error before it exits.
const (
	launcherStoreChildEnv = "LEGION_LAUNCHER_STORE_CHILD"
	storeChildSendEnv     = "LEGION_LAUNCHER_STORE_CHILD_SEND"
	storeChildReportEnv   = "LEGION_LAUNCHER_STORE_CHILD_REPORT"
)

func init() {
	helperChildVariables = append(helperChildVariables, launcherStoreChildEnv)
}

// TestLauncherStoreChild is a child that dials its session store through the socket of its
// generation's proxy (LEGION_LAUNCHER_SOCKET), writes what storeChildSendEnv holds, waits until the
// connection ends or two seconds pass, writes the line storeChildReportEnv holds, if any, on its
// standard error, and exits 1, as Oh My Pi does on a store that fails it. Run as a test of its own,
// outside a launcher, it does nothing.
func TestLauncherStoreChild(t *testing.T) {
	if os.Getenv(launcherStoreChildEnv) == "" {
		return
	}
	if conn, err := net.Dial("unix", os.Getenv("LEGION_LAUNCHER_SOCKET")); err == nil {
		_, _ = io.WriteString(conn, os.Getenv(storeChildSendEnv))
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.Copy(io.Discard, conn)
		_ = conn.Close()
	}
	if line := os.Getenv(storeChildReportEnv); line != "" {
		_, _ = os.Stderr.WriteString(line + "\n")
	}
	os.Exit(1)
}

// breakMarker is what a child writes for testdb.Breaking to end its connection there.
const breakMarker = "the store ends this connection"

// startingUp is the URL of a stand-in for a Postgres server that is starting up, which answers
// every connection that it cannot take one now (SQLSTATE 57P03). It names a user and no password:
// the stand-in answers before any login.
func startingUp(t *testing.T, _ string) string {
	return "postgres://legion@" + testdb.Refusing(t, "57P03", "the database system is starting up") + "/sessions?sslmode=disable&connect_timeout=5"
}

// awaitExitWithin reads the launcher's frames until it reports generation's exit, within bound: a
// real Oh My Pi that meets a store which closes on it can take ten seconds and more to give up.
func (g *roleConn) awaitExitWithin(generation uint64, bound time.Duration) shimwire.LauncherExit {
	g.t.Helper()
	deadline := time.Now().Add(bound)
	for g.latest.LastExit == nil || g.latest.LastExit.Generation != generation {
		if err := g.conn.SetReadDeadline(deadline); err != nil {
			g.t.Fatal(err)
		}
		line, err := g.reader.ReadBytes('\n')
		if err != nil {
			g.t.Fatalf("waiting %s for generation %d's exit: %v", bound, generation, err)
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			g.t.Fatal(err)
		}
		if state, ok := frame.(shimwire.LauncherState); ok {
			g.latest = state
		}
	}
	return *g.latest.LastExit
}

// importSession puts a session at path into the session table dsn names, as an earlier
// generation's Oh My Pi left it.
func importSession(t *testing.T, dsn, path string) {
	t.Helper()
	conn, err := ompsessions.ConnectURL(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := ompsessions.Import(context.Background(), conn, path, []byte("{}\n"), time.Now()); err != nil {
		t.Fatal(err)
	}
}

// replaceURLFile replaces the session database URL file at file whole, as a rename does, so the
// launcher never reads half of it.
func replaceURLFile(file, dsn string) error {
	staged := file + ".staged"
	if err := os.WriteFile(staged, []byte(dsn+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(staged, file)
}

// refusingURL is a session database URL whose server refuses the dial. It names a user and no
// password: the dial is refused before any login.
func refusingURL(t *testing.T) string {
	return "postgres://legion@" + testdb.RefusedAddress(t) + "/none?sslmode=disable&connect_timeout=1"
}

// withPassword is dsn with its password replaced by password.
func withPassword(t *testing.T, dsn, password string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.User == nil {
		t.Fatalf("%s names no user whose password to replace: %v", dsn, err)
	}
	parsed.User = url.UserPassword(parsed.User.Username(), password)
	return parsed.String()
}

// readOnlyURLFile is a URL file naming a new database that takes no writes, as a replica does:
// every session on it starts read-only, so a write is refused with SQLSTATE 25006.
func readOnlyURLFile(t *testing.T) string {
	t.Helper()
	dsn := testdb.Database(t, "legion_launcher_read_only_test")
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := ompsessions.ConnectURL(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	name := strings.TrimPrefix(parsed.Path, "/")
	if _, err := conn.Exec(context.Background(), `ALTER DATABASE "`+name+`" SET default_transaction_read_only = on`); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "read-only-dsn")
	if err := replaceURLFile(file, dsn); err != nil {
		t.Fatal(err)
	}
	return file
}

// testdbFile is a URL file naming dsn, for a rig that reads the session table through one.
func testdbFile(t *testing.T, dsn string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "dsn")
	if err := replaceURLFile(file, dsn); err != nil {
		t.Fatal(err)
	}
	return file
}
