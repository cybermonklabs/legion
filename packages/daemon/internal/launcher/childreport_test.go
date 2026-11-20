package launcher

// A child under a URL file reports a session database it could not open itself, on its standard
// error, and that report ties its exit to the store where the launcher's own looks missed the
// fault: a store that refused or ended the child's connection before any answer of its own, or a
// server's own answer whose SQLSTATE the report carries and that is of the server's own state. The
// pinned Oh My Pi's report of a server's answer carries no SQLSTATE, so it decides nothing.

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// The controller pod's shim is handed the container's own stdout and stderr, so its descriptors
// are those files and no pipe through the launcher (command), but for a child whose report the
// launcher reads, one that dials a URL file's store itself with no proxy of the generation's
// (start): its standard error passes through the launcher, to the same file and to the report.
// Each child is started through the launcher's own start, whose choice of the child it reads a
// report of is what this holds, with the launcher's Config.Stdout and Stderr a file, as a
// controller pod's container hands it, and each child says what its standard error is itself,
// from /proc: a child with no session store and a child under the generation's proxy see that
// file, and a child under a URL file sees a pipe.
func TestOnlyAChildWhoseReportIsReadHasItsStandardErrorPipedThroughTheLauncher(t *testing.T) {
	dsn := testdb.Database(t, "legion_launcher_report_pipe")
	for name, tc := range map[string]struct {
		proxied, urlFile bool
		pipe             bool
	}{
		"a child with no session store is handed the container's own file":         {},
		"a child under the generation's proxy is handed the container's own file":  {proxied: true},
		"a child that dials a URL file's store itself writes through the launcher": {urlFile: true, pipe: true},
	} {
		t.Run(name, func(t *testing.T) {
			container, err := os.CreateTemp(t.TempDir(), "container-stderr")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = container.Close() })
			cfg := Config{Stdout: container, Stderr: container}
			if tc.proxied {
				database, dialer := sessionDatabaseAt(t, dsn)
				cfg.SessionDatabase, cfg.SessionDialer = database, dialer.build
			}
			g := newSessionRig(t, cfg)
			var env []string
			switch {
			case tc.proxied:
				env = []string{ompsessions.StorageVariable + "=" + ompsessions.SQLStorage,
					ompsessions.DSNFileVariable + "=" + filepath.Join(g.generationDir(1), ompsessions.DSNFileName)}
			case tc.urlFile:
				urlFile := filepath.Join(t.TempDir(), "dsn")
				if err := replaceURLFile(urlFile, dsn); err != nil {
					t.Fatal(err)
				}
				env = []string{ompsessions.StorageVariable + "=" + ompsessions.SQLStorage, ompsessions.DSNFileVariable + "=" + urlFile}
			}
			seen := filepath.Join(t.TempDir(), "stderr")
			if got := g.start(shimwire.LauncherStart{ID: "start-1", Generation: 1,
				Argv: []string{"/bin/sh", "-c", `readlink /proc/self/fd/2 > "$1"`, "sh", seen}, Env: env}); !got.OK {
				t.Fatalf("start = %#v, want the child started", got)
			}
			if exit := g.awaitExitWithin(1, time.Minute); exit.Code != 0 {
				t.Fatalf("the child's exit = %+v, want 0", exit)
			}
			body, err := os.ReadFile(seen)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.TrimSpace(string(body))
			if piped := strings.HasPrefix(got, "pipe:"); piped != tc.pipe || !piped && got != container.Name() {
				t.Fatalf("the child's standard error is %q, want a pipe %t (the container's file is %s)", got, tc.pipe, container.Name())
			}
		})
	}
}

// The lines are the pinned Oh My Pi's own, as it wrote them for each store, with only the URL
// file's path shortened, and the form an Oh My Pi that prints a server's SQLSTATE after its code
// writes.
func TestAChildsOwnReportOfAStoreItCouldNotOpenIsReadByWhereItsFailureCameFrom(t *testing.T) {
	const prefix = "Error: OMP_SESSION_SQL_DSN_FILE names /tmp/dsn, but the session database could not be opened: "
	for name, tc := range map[string]struct {
		written []string
		fault   bool
	}{
		"the store refused the connection":                                           {written: []string{prefix + "Failed to connect (ERR_POSTGRES_CONNECTION_REFUSED)\n"}, fault: true},
		"the store ended the connection while it opened":                             {written: []string{prefix + "Connection closed before the connection was established (ERR_POSTGRES_CONNECTION_FAILED)\n"}, fault: true},
		"the report in pieces, and coloured":                                         {written: []string{"Still starting after 10s\n\x1b[31m" + prefix[:20], prefix[20:] + "Failed to connect (ERR_POSTG", "RES_CONNECTION_REFUSED)\x1b[39m\n"}, fault: true},
		"a server's own answer with its SQLSTATE, the store starting up":             {written: []string{prefix + "the database system is starting up (ERR_POSTGRES_SERVER_ERROR 57P03)\n"}, fault: true},
		"a server's own answer with its SQLSTATE, no client slot left":               {written: []string{prefix + "sorry, too many clients already (ERR_POSTGRES_SERVER_ERROR 53300)\n"}, fault: true},
		"control: a server's own answer with its SQLSTATE, a password it refuses":    {written: []string{prefix + "password authentication failed for user \"legion\" (ERR_POSTGRES_SERVER_ERROR 28P01)\n"}},
		"control: a server's own answer whose SQLSTATE is no SQLSTATE":               {written: []string{prefix + "the database system is starting up (ERR_POSTGRES_SERVER_ERROR 57p3)\n"}},
		"control: five characters of the server's own class, one of them lower case": {written: []string{prefix + "the database system is starting up (ERR_POSTGRES_SERVER_ERROR 57p03)\n"}},
		"control: five characters of the server's own class, one of them a symbol":   {written: []string{prefix + "the database system is starting up (ERR_POSTGRES_SERVER_ERROR 57P0!)\n"}},
		"control: a server's own answer, the store starting up (57P03)":              {written: []string{prefix + "the database system is starting up (ERR_POSTGRES_SERVER_ERROR)\n"}},
		"control: a server's own answer, a password it refuses (28P01)":              {written: []string{prefix + "password authentication failed for user \"legion\" (ERR_POSTGRES_SERVER_ERROR)\n"}},
		"control: a report with no end of line":                                      {written: []string{prefix + "Failed to connect (ERR_POSTGRES_CONNECTION_REFUSED)"}},
		"control: a line too long to be a report":                                    {written: []string{strings.Repeat("x", reportLine) + prefix + "Failed to connect (ERR_POSTGRES_CONNECTION_REFUSED)\n"}},
		"control: no report": {written: []string{"Still starting after 10s — phase: resolveSessionStorage\n"}},
	} {
		t.Run(name, func(t *testing.T) {
			report := &childReport{}
			for _, w := range tc.written {
				if n, err := report.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(w))
				}
			}
			if got := report.storeFault(); (got != nil) != tc.fault {
				t.Fatalf("the report read as %v; want a store fault %t", got, tc.fault)
			}
		})
	}
}

// The pinned Oh My Pi's own report decides its exit where the launcher's looks missed the fault,
// once the launcher's own connect to the same URL file answers: the store answers the launcher's
// look at the child's start, then the URL file names an address the host refuses, so the pinned Oh
// My Pi's first open is refused, and the store answers again before the launcher's ask after the
// exit. A URL file that is wrong is charged whatever the report says: Bun names a host that does
// not resolve, and a port where a web server listens (after thirty seconds of its own retries), as
// it names a refused dial (connectionFailed), and the launcher's own connect reads each as a fault
// of the configuration, as a resume's lookup of the same URL file does
// (TestAStartWhoseSessionStoreIsConfiguredWrongIsChargedNamingItsCause). The other controls are a
// store that still refuses the launcher's ask, the store's fault, and a server that refuses the
// password (28P01), which Oh My Pi reports as the server's answer, deciding nothing.
func TestAPinnedOhMyPiThatReportsItsStoreRefusedItExitsAsAStoreFault(t *testing.T) {
	ompBinary := testbin.OMP(t)
	home := t.TempDir()
	testbin.OMPHome(t, ompBinary, home)
	testbin.WriteOMPProfile(t, home, "providers:\n  operator:\n    baseUrl: http://127.0.0.1:9\n    auth: apiKey\n    api: anthropic-messages\n"+
		"    apiKey: unused\n    models:\n      - id: store-fault-test\n        name: Store fault test\n", "modelRoles:\n  default: operator/store-fault-test\n")
	for name, tc := range map[string]struct {
		// url, when set, is what the URL file names throughout. Otherwise it names the store, which
		// answers the launcher's look at the start, then an address the host refuses for Oh My Pi's
		// open, and the store again for the launcher's ask after the exit when answered is set.
		url      func(t *testing.T) string
		answered bool
		// connection is whether Oh My Pi reports a connection that failed before the server
		// answered it (connectionFailed), so the report is the child's word for a store fault.
		connection bool
		want       shimwire.SessionRefusal
	}{
		"the store refuses Oh My Pi's open and answers the launcher's ask after the exit": {answered: true, connection: true,
			want: shimwire.SessionStoreUnreachable},
		"control: the store refuses Oh My Pi's open and the launcher's ask after the exit": {connection: true,
			want: shimwire.SessionStoreUnreachable},
		"control: the URL file names a host that does not resolve": {connection: true, url: func(*testing.T) string {
			return "postgres://legion@no-such-session-store.invalid:5432/none?sslmode=disable"
		}},
		"control: the URL file names a port where a web server listens": {connection: true, url: func(t *testing.T) string {
			return "postgres://legion@" + webServer(t) + "/none?sslmode=disable"
		}},
		"control: the server refuses Oh My Pi's password (28P01)": {url: func(t *testing.T) string {
			return "postgres://legion@" + testdb.Refusing(t, "28P01", `password authentication failed for user "legion"`) + "/none?sslmode=disable"
		}},
	} {
		t.Run(name, func(t *testing.T) {
			lookEvery(t, time.Hour)
			var store *testdb.Flapping
			var url string
			if tc.url != nil {
				url = tc.url(t)
			} else {
				store = testdb.NewFlapping(t, testdb.Database(t, "legion_launcher_child_report_test"), false)
				url = store.URL
			}
			urlFile := filepath.Join(t.TempDir(), "dsn")
			if err := replaceURLFile(urlFile, url); err != nil {
				t.Fatal(err)
			}
			g := newSessionRig(t, Config{})
			marker := filepath.Join(t.TempDir(), "child")
			env := append(testbin.OMPEnviron(home), ompsessions.StorageVariable+"="+ompsessions.SQLStorage,
				ompsessions.DSNFileVariable+"="+urlFile, "LEGION_WORKSPACE="+filepath.Join(t.TempDir(), "workspace"))
			// The child is the pinned Oh My Pi between two waits: it starts once the test has it go
			// (.go), and its exit reaches the launcher once the test lets it (.exit), after the test
			// has seen Oh My Pi itself exit (.exited).
			argv := []string{"/bin/sh", "-c", `while [ ! -e "$1.go" ]; do sleep 0.02; done; "$2" --mode rpc; code=$?; : > "$1.exited"; ` +
				`while [ ! -e "$1.exit" ]; do sleep 0.02; done; exit $code`, "sh", marker, ompBinary}
			if got := g.start(shimwire.LauncherStart{ID: "start-1", Generation: 1, Argv: argv, Env: env}); !got.OK {
				t.Fatalf("start = %#v, want the child started", got)
			}
			if store != nil {
				awaitAtLeast(t, "connections passed", &store.Passed, 1)
				if err := replaceURLFile(urlFile, refusingURL(t)); err != nil {
					t.Fatal(err)
				}
			}
			markFile(t, marker+".go")
			awaitFile(t, marker+".exited", 2*time.Minute)
			if tc.answered {
				if err := replaceURLFile(urlFile, store.URL); err != nil {
					t.Fatal(err)
				}
			}
			markFile(t, marker+".exit")
			exit := g.awaitExitWithin(1, 2*time.Minute)
			report := ""
			for _, line := range strings.Split(g.stderr.String(), "\n") {
				if strings.Contains(line, openFailed) {
					report = line
				}
			}
			if report == "" || strings.Contains(report, connectionFailed) != tc.connection {
				t.Fatalf("Oh My Pi's report is %q, want one that names a connection that failed before the server answered it %t; launcher log:\n%s",
					report, tc.connection, g.stderr.String())
			}
			if exit.Code != 1 || exit.Session != tc.want {
				t.Fatalf("the child's exit = %+v, want status 1 and session refusal %q; Oh My Pi's report: %s\nlauncher log:\n%s",
					exit, tc.want, report, g.stderr.String())
			}
		})
	}
}

// A role's generation starts in namespaces of its own, as every issue pod's roles start
// (newRoleProcess), and its child's standard error reaches the launcher through them, so the
// child's own report of a first open that failed is read there as it is for a child the launcher
// starts itself. The store answers the launcher's look at the start and its ask after the exit, so
// only the report can mark the exit the store's. The control is a report of the server's own answer
// with no SQLSTATE, which decides nothing.
func TestARoleChildsReportOfItsFailedOpenIsReadThroughItsNamespaces(t *testing.T) {
	requireRoleNamespaces(t)
	lookEvery(t, time.Hour)
	dsn := testdb.Database(t, "legion_launcher_role_report_test")
	for name, tc := range map[string]struct {
		detail string
		want   shimwire.SessionRefusal
	}{
		"the child reports its store refused the connection": {detail: "Failed to connect (ERR_POSTGRES_CONNECTION_REFUSED)",
			want: shimwire.SessionStoreUnreachable},
		"control: the child reports its server refused the password": {detail: `password authentication failed for user "legion" (ERR_POSTGRES_SERVER_ERROR)`},
	} {
		t.Run(name, func(t *testing.T) {
			urlFile := filepath.Join(t.TempDir(), "dsn")
			if err := replaceURLFile(urlFile, dsn); err != nil {
				t.Fatal(err)
			}
			f := newFleet(t, 10*time.Second, "tester")
			report := "Error: " + ompsessions.DSNFileVariable + " names " + urlFile + ", but the session database could not be opened: " + tc.detail
			if got := f.role("tester").start(shimwire.LauncherStart{
				ID: "start-tester", Generation: 1, Argv: []string{"/bin/sh", "-c", `printf '%s\n' "$1" >&2; exit 1`, "sh", report},
				Env: []string{ompsessions.StorageVariable + "=" + ompsessions.SQLStorage, ompsessions.DSNFileVariable + "=" + urlFile},
			}); !got.OK {
				t.Fatalf("start = %#v; the launcher's log: %s", got, f.stderr.String())
			}
			if exit := f.role("tester").awaitExit(1); exit.Code != 1 || exit.Session != tc.want {
				t.Fatalf("the role's exit = %+v, want status 1 and session refusal %q; the launcher's log: %s", exit, tc.want, f.stderr.String())
			}
		})
	}
}

// webServer is the address of a server on 127.0.0.1 that answers whatever it is sent as a web
// server answers a request it cannot read, and closes: a URL file whose port names some other
// service. Its listener closes when the test ends.
func webServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				_, _ = conn.Read(make([]byte, 512))
				_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		wg.Wait()
	})
	return listener.Addr().String()
}
