package storetest

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testtmp"
	"github.com/sjawhar/envoy/internal/testwait"
)

// ownServerPort is the port a server of ownServer's is told. It binds no TCP port, since the
// server listens on no TCP address: it names the server's socket file, .s.PGSQL.5432, in a
// directory no other server uses, so every such server can have the same one.
const ownServerPort = 5432

// ownServer starts a Postgres server of t's own and returns its URL as the server's superuser,
// postgres, naming its postgres database: for a test whose server no other test binary may reach,
// such as one that holds a lock every broker test binary on a shared server takes. The programs
// are initdb and postgres (serverPrograms). A program that is missing, or a process that runs as
// root, which both programs refuse, skips t, naming it, and fails t on GitHub Actions
// (testdb.SkipOutsideCI).
//
// initdb makes the data directory under t.TempDir(), with trust authentication, UTF8 and the C
// locale whatever the machine's is, and syncs nothing to disk (--no-sync); what it writes is
// printed only when it fails. postgres runs as a child of the test process with fsync off,
// listening on no TCP address (listen_addresses is empty) and on one unix socket, in a directory
// from testtmp, since a socket under t.TempDir() can pass the 107 bytes a socket path holds
// (docs/testing.md, "Unix sockets in Go tests"). That directory is the test's alone (mode 0700),
// and the URL goes to no other binary. What postgres writes is kept, and printed when t has
// failed. ownServer returns once a connection to the postgres database succeeds (testwait.Until,
// whose failure names the last error), and fails t at once where postgres exits first. When t
// ends, a cleanup sends postgres SIGINT, its fast shutdown, waits for it to exit
// (testwait.Receive) and fails t unless it exited cleanly; the two directories go after it.
//
// A test binary that ends without running t's cleanups, killed or ended by go test's alarm
// (-timeout), sends postgres no SIGINT and removes neither directory. On Linux postgres stops with
// it all the same: it starts with SIGINT as its parent-death signal (startServer), which the kernel
// sends it when the thread that started it ends, as each of the binary's threads does when the
// binary ends; the two directories are left. On any other system postgres keeps running, fsync
// off, until someone stops it. pgrep -f 'postgres -D .*/data -p 5432 -c listen_addresses=' prints
// the pid of each such server still running, kill -INT <pid> stops one as the cleanup does, and
// ps -o command= -p <pid> shows its data directory after -D: data in t.TempDir(), under TMPDIR
// (GOTMPDIR where that is set) in a directory named for the test. That directory, and the socket
// directory after unix_socket_directories=, can go once postgres has exited.
//
// The URL has no host in its authority and names the socket directory in its host pair, which
// pgx reads as a unix socket's directory, so testdb.DatabaseURL and testdb.DatabaseOn, which
// replace the path and keep the other pairs byte for byte, name the server's other databases.
// Every connect to it waits until the test binary's deadline (testdb.WithConnectDeadline), as one
// to serverURL's does.
func ownServer(t *testing.T) url.URL {
	t.Helper()
	initdb, postgres := serverPrograms(t)
	if os.Geteuid() == 0 {
		testdb.SkipOutsideCI(t, "a Postgres server of the test's own cannot start: the test runs as root, and initdb and postgres refuse to run as root")
	}
	data := filepath.Join(t.TempDir(), "data")
	if out, err := exec.Command(initdb, "-D", data, "-U", "postgres", "--auth=trust", "-E", "UTF8", "--no-locale", "--no-sync", "--no-instructions").CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", initdb, err, out)
	}
	port := strconv.Itoa(ownServerPort)
	sockets := testtmp.Dir(t, ".s.PGSQL."+port)
	server := exec.Command(postgres, "-D", data, "-p", port,
		"-c", "listen_addresses=", "-c", "unix_socket_directories="+sockets, "-c", "fsync=off")
	// One writer for both, so exec copies them through one pipe on one goroutine; it is read only
	// once that goroutine is done, after the process has exited.
	var output bytes.Buffer
	server.Stdout, server.Stderr = &output, &output
	if err := startServer(server); err != nil {
		t.Fatalf("start %s: %v", postgres, err)
	}
	// exited is closed once postgres has exited, with exit its Wait's answer.
	exited := make(chan struct{})
	var exit error
	go func() {
		exit = server.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		if err := server.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("send the test's own Postgres server SIGINT: %v", err)
		}
		testwait.Receive(t, "the test's own Postgres server to exit after SIGINT, its fast shutdown", exited)
		if exit != nil {
			t.Errorf("the test's own Postgres server exited with %v", exit)
		}
		if t.Failed() {
			t.Logf("the test's own Postgres server wrote:\n%s", &output)
		}
	})
	own := url.URL{
		Scheme: "postgres",
		User:   url.User("postgres"),
		Path:   "/postgres",
		// url.QueryEscape writes a space as a plus, which pgx, as libpq, reads as a plus.
		RawQuery: "host=" + strings.ReplaceAll(url.QueryEscape(sockets), "+", "%20") + "&port=" + port,
	}
	raw := testdb.WithConnectDeadline(t, own.String())
	ctx := context.Background()
	testwait.Until(t, "the test's own Postgres server to take a connection to its postgres database", func() (error, bool) {
		select {
		case <-exited:
			t.Fatalf("the test's own Postgres server exited before it took a connection: %v", exit)
		default:
		}
		conn, err := pgx.Connect(ctx, raw)
		if err != nil {
			return err, false
		}
		conn.Close(ctx)
		return nil, true
	})
	bounded, err := url.Parse(raw)
	if err != nil {
		t.Fatal("the URL of the test's own Postgres server does not parse")
	}
	return *bounded
}

// serverPrograms is the paths of initdb and postgres: in the directory pg_config --bindir names
// where pg_config is on the PATH, as on a Debian or Ubuntu machine, whose server programs are in
// /usr/lib/postgresql/<major>/bin, off the PATH, and on the PATH where it is not. Both come from
// the one place, so the two are of one major. One missing there skips t, naming it, and fails t on
// GitHub Actions (testdb.SkipOutsideCI).
func serverPrograms(t *testing.T) (initdb, postgres string) {
	t.Helper()
	find := exec.LookPath
	if pgConfig, err := exec.LookPath("pg_config"); err == nil {
		out, err := exec.Command(pgConfig, "--bindir").Output()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				t.Fatalf("%s --bindir: %v: %s", pgConfig, err, exit.Stderr)
			}
			t.Fatalf("%s --bindir: %v", pgConfig, err)
		}
		bin := strings.TrimSpace(string(out))
		find = func(name string) (string, error) { return exec.LookPath(filepath.Join(bin, name)) }
	}
	program := func(name string) string {
		t.Helper()
		path, err := find(name)
		if err != nil {
			testdb.SkipOutsideCI(t, "a Postgres server of the test's own needs %s: %v", name, err)
		}
		return path
	}
	return program("initdb"), program("postgres")
}
