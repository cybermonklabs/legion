package launcher

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// shortenResumeLookup makes a lookup that cannot reach the session table give up in seconds rather
// than the thirty seconds a launch is allowed, so a test that expects an answer fails quickly when
// none comes.
func shortenResumeLookup(t *testing.T) {
	t.Helper()
	savedBackoff, savedTimeout := resumeLookupBackoff, resumeLookupTimeout
	resumeLookupBackoff, resumeLookupTimeout = []time.Duration{100 * time.Millisecond}, 3*time.Second
	t.Cleanup(func() { resumeLookupBackoff, resumeLookupTimeout = savedBackoff, savedTimeout })
}

// Under SQL session storage (either database kind of the runtime's session_store, postgres or
// cloudsql-iam: OMP_SESSION_STORAGE=sql and OMP_SESSION_SQL_DSN_FILE in the generation's
// environment) a resume is held to the session table,
// where the child's Oh My Pi will read it, never to the volume: a session the table does not hold is
// refused before anything starts, even when a file of that name is on disk, and a session the table
// holds starts with no file on disk at all.
func TestStartUnderSQLStorageResumesOnlyASessionTheTableHolds(t *testing.T) {
	t.Parallel()
	_, dsnFile := testdb.DSNFile(t, "legion_launcher_test")
	g := newRig(t)
	marker := filepath.Join(t.TempDir(), "marker")
	sessionFile := filepath.Join(t.TempDir(), "sessions", "2026-10-08T12-00-00-000Z_0001.jsonl")
	start := func(generation uint64) (string, bool) {
		command := g.childStart(generation, marker, nil)
		command.Env = append(command.Env, ompsessions.StorageVariable+"="+ompsessions.SQLStorage, ompsessions.DSNFileVariable+"="+dsnFile)
		command.ResumeFile = sessionFile
		got := g.start(command)
		return got.Error, got.OK
	}

	refusal, ok := start(5)
	if ok || !strings.Contains(refusal, "resume session "+sessionFile+": the session table holds no such session") {
		t.Fatalf("start of a session the table does not hold = %q (ok %t), want the refusal naming it", refusal, ok)
	}
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if refusal, ok := start(6); ok || !strings.Contains(refusal, "the session table holds no such session") {
		t.Fatalf("start with the file on disk but not in the table = %q (ok %t), want the table's refusal", refusal, ok)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused resume started the child")
	}

	if err := os.Remove(sessionFile); err != nil {
		t.Fatal(err)
	}
	conn, err := ompsessions.Connect(context.Background(), dsnFile)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := ompsessions.Import(context.Background(), conn, sessionFile, []byte("{}\n"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if refusal, ok := start(7); !ok {
		t.Fatalf("start of a session the table holds = %q, want generation 7 to run", refusal)
	}
	published(t, marker)
	g.stop(7)
}

// startByID starts generation naming session by its id alone (ResumeSession) under SQL session
// storage in the database dsnFile names, with `--resume=<session>` in its argv as the daemon sends
// it. The child is a shell that writes each argument it was started with after its own name, one
// per line, to told, and waits.
func (g *rig) startByID(generation uint64, dsnFile, session, told string) shimwire.LauncherStartResult {
	g.t.Helper()
	return g.start(shimwire.LauncherStart{
		ID: "start-" + strconv.FormatUint(generation, 10), Generation: generation,
		Argv: []string{"/bin/sh", "-c", `printf '%s\n' "$@" > "$TOLD.tmp" && mv "$TOLD.tmp" "$TOLD"; exec sleep 600`,
			"omp", "--mode", "rpc", "--resume=" + session},
		Env: []string{"TOLD=" + told, "LEGION_WORKSPACE=" + filepath.Join(g.t.TempDir(), "workspace"),
			ompsessions.StorageVariable + "=" + ompsessions.SQLStorage, ompsessions.DSNFileVariable + "=" + dsnFile},
		ResumeSession: session,
	})
}

// resumesTold waits for startByID's child to write the arguments it was started with, and answers
// those of them that name a session to resume.
func resumesTold(t *testing.T, told string) []string {
	t.Helper()
	var said string
	if !testwait.Holds(beforeDeadline(t), testwait.Poll, func() bool {
		body, err := os.ReadFile(told)
		if err != nil {
			return false
		}
		said = string(body)
		return true
	}) {
		t.Fatal("the child never said what it was started with")
	}
	var resumes []string
	for _, arg := range strings.Split(said, "\n") {
		if strings.HasPrefix(arg, "--resume") {
			resumes = append(resumes, arg)
		}
	}
	return resumes
}

// Under SQL session storage, a start that names its session by its id alone (ResumeSession: the
// daemon records no file of it) resumes the newest row of the session table whose path ends
// `_<id>.jsonl` (ompsessions.FindByID): its child is started with `--resume=` that row's path in
// place of the argv's `--resume=<id>`, and never with the id itself, which Oh My Pi would resolve
// on its own. The table also holds a subagent's file of the session and a session whose id shares
// the first 30 characters, written later, and neither is taken. A table holding no row of the
// session answers that it holds no such session (shimwire.SessionGone), and one that cannot be
// asked answers so (shimwire.SessionStoreUnreachable); neither starts a child.
func TestStartUnderSQLStorageResumesASessionNamedByItsIDAlone(t *testing.T) {
	// Not parallel: it writes the package variable resumeLookupBackoff (shortenResumeLookup), which
	// parallel tests read.
	const (
		sessions = "/home/legion/.omp/profiles/legion/agent/sessions"
		folder   = sessions + "/--legion-workspaces-cybermonklabs-legion-infra-4108--"
		session  = "01a1f5de-4835-7667-b005-e99e3e17edd3"
	)
	older := folder + "/2026-11-19T22-12-27-829Z_" + session + ".jsonl"
	newest := sessions + "/--state-workspaces-cybermonklabs-legion-infra-4108--/2026-11-19T23-12-27-829Z_" + session + ".jsonl"
	written := time.Date(2026, 11, 19, 22, 12, 27, 829_000_000, time.UTC)
	nearMisses := map[string]time.Time{
		strings.TrimSuffix(older, ".jsonl") + "/0-scout.jsonl":                     written.Add(2 * time.Hour),
		folder + "/2026-11-20T00-12-27-829Z_" + session[:30] + "28aa01" + ".jsonl": written.Add(2 * time.Hour),
	}
	table := func(t *testing.T, rows map[string]time.Time) string {
		t.Helper()
		_, dsnFile := testdb.DSNFile(t, "legion_launcher_by_id_test")
		conn, err := ompsessions.Connect(context.Background(), dsnFile)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())
		for path, at := range rows {
			if _, err := ompsessions.Import(context.Background(), conn, path, []byte("{}\n"), at); err != nil {
				t.Fatal(err)
			}
		}
		return dsnFile
	}
	refused := func(t *testing.T, got shimwire.LauncherStartResult, g *rig, told string) {
		t.Helper()
		if got.OK {
			resumes := resumesTold(t, told)
			g.stop(1)
			t.Fatalf("the start of session %s ran generation 1 with %q, want it refused", session, resumes)
		}
		if _, err := os.Stat(told); !errors.Is(err, os.ErrNotExist) {
			t.Error("a refused start started the child")
		}
	}

	t.Run("the newest row of the session", func(t *testing.T) {
		rows := map[string]time.Time{older: written, newest: written.Add(time.Hour)}
		for path, at := range nearMisses {
			rows[path] = at
		}
		dsnFile := table(t, rows)
		g := newRig(t)
		told := filepath.Join(t.TempDir(), "told")
		if got := g.startByID(1, dsnFile, session, told); !got.OK {
			t.Fatalf("the start of session %s = %#v, want generation 1 to run", session, got)
		}
		if resumes := resumesTold(t, told); len(resumes) != 1 || resumes[0] != "--resume="+newest {
			t.Errorf("the child was started with %q, want --resume=%s alone", resumes, newest)
		}
		g.stop(1)
	})

	t.Run("no row of the session", func(t *testing.T) {
		dsnFile := table(t, nearMisses)
		g := newRig(t)
		told := filepath.Join(t.TempDir(), "told")
		got := g.startByID(1, dsnFile, session, told)
		refused(t, got, g, told)
		if got.Session != shimwire.SessionGone || !strings.Contains(got.Error, session) ||
			!strings.Contains(got.Error, "the session table holds no such session") {
			t.Errorf("the refusal = %#v, want SessionGone naming %s and saying the table holds no such session", got, session)
		}
	})

	t.Run("a session table that cannot be asked", func(t *testing.T) {
		shortenResumeLookup(t)
		dsnFile := filepath.Join(t.TempDir(), "session-database-url")
		if err := os.WriteFile(dsnFile, []byte("postgres://legion@127.0.0.1:1/none?sslmode=disable&connect_timeout=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		g := newRig(t)
		told := filepath.Join(t.TempDir(), "told")
		got := g.startByID(1, dsnFile, session, told)
		refused(t, got, g, told)
		if got.Session != shimwire.SessionStoreUnreachable {
			t.Errorf("the refusal = %#v, want SessionStoreUnreachable", got)
		}
	})
}

// A generation with no URL file is refused before it starts, naming the variable: the launcher
// cannot look where Oh My Pi would.
func TestStartRefusesAResumeWithNoURLFile(t *testing.T) {
	t.Parallel()
	g := newRig(t)
	command := g.childStart(3, filepath.Join(t.TempDir(), "marker"), nil)
	command.ResumeFile = "/sessions/absent.jsonl"
	if got := g.start(command); got.OK || !strings.Contains(got.Error, "OMP_SESSION_SQL_DSN_FILE names no file") {
		t.Fatalf("start = %#v, want the refusal naming OMP_SESSION_SQL_DSN_FILE", got)
	}
}

// A session's last write is the table's mtime_ms, which Oh My Pi moves on every write and
// `legion sessions import` sets to the copied file's. A resumed generation is told whether its
// workspace (LEGION_WORKSPACE) was recreated since that write, which the Legion plugin tells the
// agent on its first turn: "true" once provisioning's creation record is later, "false" when it is
// earlier or there is none. A fresh generation resumes nothing and is told "false", and so is one
// with no workspace at all and every generation whatever the container's own environment says, so
// no other value reaches the child. A record that is empty or not an instant, which an init
// container killed mid-write can leave, only decides the notice: the resume starts, told "false",
// and the launcher logs the record.
func TestAResumeIsToldWhetherItsWorkspaceWasRecreatedSinceTheTableWroteItsSession(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process.
	t.Setenv(WorkspaceRecreatedVariable, "true")
	_, dsnFile := testdb.DSNFile(t, "legion_launcher_recreated_test")
	g := newRig(t)
	sessionFile := filepath.Join(t.TempDir(), "sessions", "2026-10-08T12-00-00-000Z_0002.jsonl")
	written := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	conn, err := ompsessions.Connect(context.Background(), dsnFile)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := ompsessions.Import(context.Background(), conn, sessionFile, []byte("{}\n"), written); err != nil {
		t.Fatal(err)
	}
	dir := newWorkspace(t)
	table := []string{ompsessions.DSNFileVariable + "=" + dsnFile}
	env := append([]string{"LEGION_WORKSPACE=" + dir}, table...)
	if got := g.toldRecreated(1, env, sessionFile); got != "false" {
		t.Errorf("a resume in a workspace with no creation record was told %q, want false", got)
	}
	if err := workspace.RecordCreated(dir, written.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := g.toldRecreated(2, env, sessionFile); got != "false" {
		t.Errorf("a resume in a workspace older than the table's last write was told %q, want false", got)
	}
	if err := workspace.RecordCreated(dir, written.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := g.toldRecreated(3, env, sessionFile); got != "true" {
		t.Errorf("a resume in a workspace recreated after the table's last write was told %q, want true", got)
	}
	if got := g.toldRecreated(4, env, ""); got != "false" {
		t.Errorf("a fresh generation was told %q, want false", got)
	}
	if got := g.toldRecreated(5, table, sessionFile); got != "false" {
		t.Errorf("a resume with no workspace was told %q, want false", got)
	}
	for i, record := range []string{"soon\n", ""} {
		if err := os.WriteFile(filepath.Join(dir, ".jj", "legion-created"), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := g.toldRecreated(uint64(6+i), env, sessionFile); got != "false" {
			t.Errorf("a resume in a workspace whose creation record is %q was told %q, want false", record, got)
		}
	}
	if logged := g.stderr.String(); !strings.Contains(logged, filepath.Join(dir, ".jj", "legion-created")) ||
		!strings.Contains(logged, "the workspace is taken as not recreated") {
		t.Errorf("the launcher logged %q, want the unreadable creation record named", logged)
	}
}

// A session database that cannot be reached for a moment does not fail the launch: the lookup is
// asked again after each backoff wait, reading the URL file afresh, until the table answers. One
// that stays unreachable past resumeLookupTimeout refuses the start, naming how often it asked.
func TestAResumeUnderSQLStorageWaitsOutAnUnreachableSessionDatabase(t *testing.T) {
	// Not parallel: it writes the package variable resumeLookupBackoff (shortenResumeLookup), which
	// parallel tests read.
	dsn, dsnFile := testdb.DSNFile(t, "legion_launcher_retry_test")
	sessionFile := filepath.Join(t.TempDir(), "sessions", "2026-10-08T12-00-00-000Z_0003.jsonl")
	conn, err := ompsessions.Connect(context.Background(), dsnFile)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := ompsessions.Import(context.Background(), conn, sessionFile, []byte("{}\n"), time.Now()); err != nil {
		t.Fatal(err)
	}
	shortenResumeLookup(t)
	// writeURL replaces the URL file whole, as a rename does, so a read never sees half of it.
	writeURL := func(url string) {
		staged := dsnFile + ".staged"
		if err := os.WriteFile(staged, []byte(url+"\n"), 0o600); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(staged, dsnFile); err != nil {
			t.Error(err)
		}
	}
	unreachable := "postgres://legion:unused@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"
	writeURL(unreachable)
	env := []string{ompsessions.StorageVariable + "=" + ompsessions.SQLStorage, ompsessions.DSNFileVariable + "=" + dsnFile}
	sessions, err := newSessionDial(env, "")
	if err != nil {
		t.Fatal(err)
	}
	back := make(chan struct{})
	go func() {
		defer close(back)
		time.Sleep(300 * time.Millisecond)
		writeURL(dsn)
	}()
	_, found, err := sessionWritten(context.Background(), sessions, sessionFile)
	if err != nil {
		t.Fatalf("a lookup whose database came back during the backoff = %v, want the session found", err)
	}
	closeSessions(found)
	<-back

	writeURL(unreachable)
	started := time.Now()
	_, _, err = sessionWritten(context.Background(), sessions, sessionFile)
	if err == nil || !strings.Contains(err.Error(), "resume session "+sessionFile) || !strings.Contains(err.Error(), "attempts in "+resumeLookupTimeout.String()) {
		t.Fatalf("a lookup whose database never came back = %v, want the refusal naming the session and the attempts", err)
	}
	if waited := time.Since(started); waited < resumeLookupTimeout-time.Second {
		t.Fatalf("the lookup gave up after %s, want it to keep asking for %s", waited, resumeLookupTimeout)
	}
}

// sessionDatabaseAt is the test Postgres as a session database a launcher proxies to, with the
// dialer that reaches it as the Cloud SQL connector would. The database and the login are the test
// database's own, since an IAM login exists only in a real project, and its password goes into the
// environment as libpq's PGPASSWORD: it is the only place left for one, the launcher's client
// building its URL from the endpoint alone (ompsessions.Endpoint.ClientURL), which carries none.
func sessionDatabaseAt(t *testing.T, dsn string) (ompsessions.Database, *sessionDialer) {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		// url.Parse's error, and the error it wraps, can quote the URL's password, so none of it is printed.
		t.Fatal("the test database's DSN does not parse")
	}
	password, _ := parsed.User.Password()
	t.Setenv("PGPASSWORD", password)
	address := parsed.Host
	if _, _, err := net.SplitHostPort(address); err != nil {
		address = net.JoinHostPort(address, "5432")
	}
	database := ompsessions.Database{Instance: testInstance, Name: strings.TrimPrefix(parsed.Path, "/"), User: parsed.User.Username()}
	return database, &sessionDialer{addr: address}
}

// Under a session database of the pod's own, the launcher's own resume lookup reaches the table
// through the generation's socket: a session the table holds resumes, and one it does not hold is
// refused with the same message as under any other SQL storage. The generation's environment points
// OMP_SESSION_SQL_DSN_FILE at the file the launcher wrote, as the pod manifest does, and that file
// holds the agent's spelling of the endpoint, which pgx does not read: a launcher that looked there
// would reach no database at all.
func TestAResumeUnderASessionDatabaseIsServedThroughTheProxy(t *testing.T) {
	// Not parallel: it writes the package variable resumeLookupBackoff (shortenResumeLookup), which
	// parallel tests read.
	dsn := testdb.PlainDatabase(t, "legion_launcher_proxy_test")
	database, dialer := sessionDatabaseAt(t, dsn)
	shortenResumeLookup(t)
	g := newSessionRig(t, Config{SessionDatabase: database, SessionDialer: dialer.build})
	marker := filepath.Join(t.TempDir(), "marker")
	sessionFile := filepath.Join(t.TempDir(), "sessions", "2026-10-08T12-00-00-000Z_0004.jsonl")
	start := func(generation uint64) (string, bool) {
		command := g.childStart(generation, marker, nil)
		command.Env = append(command.Env, ompsessions.StorageVariable+"="+ompsessions.SQLStorage,
			ompsessions.DSNFileVariable+"="+filepath.Join(g.generationDir(generation), ompsessions.DSNFileName))
		command.ResumeFile = sessionFile
		got := g.start(command)
		return got.Error, got.OK
	}

	refusal, ok := start(3)
	if ok || !strings.Contains(refusal, "resume session "+sessionFile+": the session table holds no such session") {
		t.Fatalf("start of a session the table does not hold = %q (ok %t), want the refusal naming it", refusal, ok)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused resume started the child")
	}
	conn, err := ompsessions.ConnectURL(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := ompsessions.Import(context.Background(), conn, sessionFile, []byte("{}\n"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if refusal, ok := start(4); !ok {
		t.Fatalf("start of a session the table holds = %q, want generation 4 to run", refusal)
	}
	if seen := publishedAll(t, marker); seen.socket != "bound" {
		t.Fatalf("the child found its session socket %s, want it already bound", seen.socket)
	}
	g.stop(4)
}

// identityStub stands in for the pod's own cloud identity: a credential with an expiry, which the
// dialer presents at every dial and which only a fresh minting renews.
type identityStub struct {
	mu      sync.Mutex
	expires time.Time
}

func (s *identityStub) mint() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expires = time.Now().Add(time.Minute)
}

func (s *identityStub) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expires = time.Now().Add(-time.Second)
}

// present is the credential work a dial does before it reaches the database: an expired credential
// reaches nothing.
func (s *identityStub) present() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !time.Now().Before(s.expires) {
		return errors.New("the session database refused an expired credential")
	}
	return nil
}

// An agent goes on writing its session past the expiry of the credential its first connection used,
// with nothing restarting its generation: the proxy asks its dialer for a credential at every
// connection, so a credential minted afresh serves the next one. What this proves is only that: the
// proxy never pins the credential of its first dial. What it does not test is the connector's own
// refresh-ahead certificate cache, which is the library's behaviour and is tested there; the stub
// here mints on demand instead. The agent's own client is Bun's, and pgx through the same socket
// stands in for it, which is also the client the launcher itself looks a resume up with.
func TestAnAgentKeepsWritingPastItsCredentialsExpiry(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (sessionDatabaseAt).
	database, dialer := sessionDatabaseAt(t, testdb.PlainDatabase(t, "legion_launcher_expiry_test"))
	identity := &identityStub{}
	identity.mint()
	dialer.check = identity.present
	g := newSessionRig(t, Config{SessionDatabase: database, SessionDialer: dialer.build})
	marker := filepath.Join(t.TempDir(), "marker")
	if got := g.start(g.childStart(2, marker, nil)); !got.OK {
		t.Fatalf("start result = %#v", got)
	}
	child := publishedAll(t, marker)
	endpoint := ompsessions.Endpoint{SocketDir: g.generationDir(2), Database: database.Name, User: database.User}
	ask := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), beforeDeadline(t))
		defer cancel()
		conn, err := ompsessions.ConnectURL(ctx, endpoint.ClientURL())
		if err != nil {
			return err
		}
		defer closeSessions(conn)
		_, _, err = ompsessions.Written(ctx, conn, "sessions/one.jsonl")
		return err
	}

	if err := ask(); err != nil {
		t.Fatalf("the first connection through the proxy: %v", err)
	}
	identity.expire()
	if err := ask(); err == nil {
		t.Fatal("a connection reached the database on an expired credential")
	}
	identity.mint()
	if err := ask(); err != nil {
		t.Fatalf("a connection after the credential was minted afresh: %v", err)
	}
	if err := syscall.Kill(child.descendant, 0); err != nil {
		t.Fatalf("generation 2 did not survive the expiry: its descendant %d is gone (%v)", child.descendant, err)
	}
	g.stop(2)
}

// In an issue pod's one container the launcher and the role see a generation's directory at two
// paths, so the connection file the agent reads names the socket where the agent finds it: under
// the fixed root its own namespaces bind the role's private directory back at, and never the
// launcher's path under the roles directory, where the socket is really bound.
func TestAnIssuePodsSessionURLNamesTheSocketAsTheRoleSeesIt(t *testing.T) {
	// Not parallel: it writes the package variable roleFixedRoot, which parallel tests read.
	fixed := t.TempDir()
	saved := roleFixedRoot
	roleFixedRoot = fixed
	t.Cleanup(func() { roleFixedRoot = saved })
	// The role's own view in a deployed pod, which the worker image makes the mount point for.
	if FixedRoot != "/var/run/legion" {
		t.Fatalf("the role's fixed root is %q, so a deployed role's view is not /var/run/legion", FixedRoot)
	}
	rolesDir := shortTempDir(t)
	role := "tester"
	for _, name := range []string{RoleLauncherDir, RolePrivateDir} {
		if err := os.MkdirAll(filepath.Join(rolesDir, role, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rolesDir, role, RoleLauncherDir, LauncherTokenFile), []byte("launcher-token\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	dialer := &sessionDialer{addr: idleUpstream(t)}
	managers, err := newManagers(Config{
		Connect: "tcp://127.0.0.1:1", Sandbox: "legion-legion-legion-208", RolesDir: rolesDir, Roles: []string{role},
		StopGrace: 2 * time.Second, SessionDatabase: testDatabase, SessionDialer: dialer.build,
	}, &pidGuard{})
	if err != nil {
		t.Fatal(err)
	}
	m := managers[0]
	if !m.namespaced {
		t.Fatal("the multi-role form built a manager that is not namespaced")
	}
	const generation = 4
	bound := generationDir(filepath.Join(rolesDir, role, RolePrivateDir), generation)
	if err := os.MkdirAll(bound, 0o700); err != nil {
		t.Fatal(err)
	}
	proxy, clientURL, err := m.startSession(context.Background(), shimwire.LauncherStart{Generation: generation}, nil, bound)
	if err != nil {
		t.Fatalf("startSession: %v; the launcher's log: %s", err, m.stderr)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	body, err := os.ReadFile(filepath.Join(bound, ompsessions.DSNFileName))
	if err != nil {
		t.Fatal(err)
	}
	seen := ompsessions.Endpoint{
		SocketDir: generationDir(filepath.Join(fixed, RolePrivateDir), generation),
		Database:  testDatabase.Name, User: testDatabase.User,
	}
	if string(body) != seen.AgentURL() {
		t.Fatalf("the connection file holds %q, want the role's own view %q", body, seen.AgentURL())
	}
	if strings.Contains(string(body), rolesDir) {
		t.Fatalf("the connection file names the launcher's own path under the roles directory: %q", body)
	}
	// The role's view in a deployed pod, which the test's fixed root stands in for.
	deployed := generationDir(FixedRoot+"/"+RolePrivateDir, generation)
	if deployed != "/var/run/legion/private/g4" {
		t.Fatalf("a deployed role reads its socket under %q, want /var/run/legion/private/g4", deployed)
	}

	// The socket itself is bound where the launcher is, under the role's directory of the roles
	// directory, and the URL the launcher's own client uses names that directory.
	socket := filepath.Join(bound, ompsessions.SocketName)
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("the generation's socket is not bound under the role's own directory: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is %s, not a socket", socket, info.Mode())
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the generation's socket: %v", err)
	}
	_ = conn.Close()
	want := ompsessions.Endpoint{SocketDir: bound, Database: testDatabase.Name, User: testDatabase.User}.ClientURL()
	if clientURL != want {
		t.Fatalf("the launcher's own client URL is %q, want %q", clientURL, want)
	}
}
