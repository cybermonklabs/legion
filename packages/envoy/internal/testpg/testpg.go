// Package testpg gives a test that brings its own servers, as a natslive test does with testnats,
// an empty Postgres database and the URL to hand a binary. The caller migrates it itself. URL is
// the helper for a test that may have no server: it takes a database on the server
// DISPATCH_TEST_DATABASE_URL names, or starts a container when that variable is unset.
//
// This package is the container path, and the one that links testcontainers. Everything else - a
// database on a server a variable names (testdb.Server), a database on a server the test already
// holds (testdb.DatabaseOn), a query parameter on such a URL (testdb.WithParameter) and the
// migrated template Dispatch's and the broker's storetest packages clone (testdb.Template) - is
// internal/testpg/testdb's, which reads the variable for this package too and links no
// testcontainers package. A test of either store that wants a migrated store rather than a URL
// calls its storetest instead.
package testpg

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/testcontainers/testcontainers-go"

	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testtmp"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Image is the pgvector server image CI uses for Dispatch's Postgres service: pgvector 0.8.7 on
// PostgreSQL 18 (tag 0.8.7-pg18), as ghcr.io/cybermonklabs/legion/ci holds it, the copy of
// pgvector/pgvector (.github/ci-images.yaml). The envoy-infra-go job pulls it before the natslive
// tests run, so a test does not fetch it in the middle of a lane.
const Image = "ghcr.io/cybermonklabs/legion/ci@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11"

// URL returns the connection URL, on this machine, of an empty Postgres database of this test's
// own, dropped when the test ends. It is reached two ways.
//
// With DISPATCH_TEST_DATABASE_URL set, the database is created on the server that variable names
// — the Postgres service the envoy-go job of .github/workflows/ci.yaml already runs — and
// dropped from it afterwards (testdb.DispatchServer.Database). That variable is reused rather
// than doubled: it is how every Postgres-backed Dispatch test is already told which server CI
// provides, and a second name would have to be set beside it on every job and in every
// developer's shell for the same answer.
//
// With it unset, Image is started as a container and the whole server ends with the test. A
// developer with a container runtime and no server therefore needs no setup, and a lane that has
// a server needs no runtime.
//
// Either way the URL carries the test binary's deadline as its connect bound
// (testdb.WithConnectDeadline), so a server slow to answer a new connection only slows the test.
func URL(t testing.TB) string {
	t.Helper()
	if testdb.DispatchServer.Available() {
		return testdb.DispatchServer.Database(t, "testpg")
	}
	return testdb.WithConnectDeadline(t, containerURL(t))
}

// socketFile is the server's socket in its socket directory, for the port it keeps, 5432.
const socketFile = ".s.PGSQL.5432"

// ready is the line the image's server logs each time it can take connections: once when the
// entrypoint's first server has made the database, which then stops, and again for the server
// that stays.
const ready = "database system is ready to accept connections"

// containerURL starts Image, waits for Postgres to finish its first initialization and be ready
// again, and returns its postgres database's connection URL. The container ends with t.
//
// The server listens on no TCP port (listen_addresses empty) and on a unix socket in a directory
// of the test's own (testtmp.Dir), mounted at /sockets, beside the image's own
// /var/run/postgresql, where the entrypoint's own psql reaches it. A port Docker publishes is one
// its own allocator chooses, without asking the kernel whether another program holds it, and ten
// taken ports in a row fail the start; testcontainers publishes the image's declared 5432 on
// Docker's bridge network, so the container runs on the host's network, where Docker publishes
// nothing and the server binds no port (docs/testing.md, "A container a test starts publishes no
// port"). The directory is mode 0777, since the image's server runs as its own user, and it is
// not the sticky /var/run/postgresql, so a socket a killed server leaves in it goes with the
// directory.
func containerURL(t testing.TB) string {
	t.Helper()
	ctx := context.Background()
	sockets := testtmp.Dir(t, socketFile)
	if err := os.Chmod(sockets, 0o777); err != nil {
		t.Fatalf("open the socket directory %s to the image's user: %v", sockets, err)
	}
	started, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: Image,
			Env:   map[string]string{"POSTGRES_PASSWORD": "test"},
			Cmd:   []string{"-c", "listen_addresses=", "-c", "unix_socket_directories=/var/run/postgresql,/sockets"},
			// The data directory's parent on tmpfs leaves no anonymous volume behind (the image
			// declares one at /var/lib/postgresql, and its PostgreSQL 18 entrypoint refuses a mount at
			// the old /var/lib/postgresql/data), and with it the server fits in two CPUs and 2 GiB, as
			// every container the repository's tests start is capped on a shared host; tmpfs counts
			// against that memory.
			Tmpfs: map[string]string{"/var/lib/postgresql": "rw"},
			HostConfigModifier: func(hostConfig *container.HostConfig) {
				hostConfig.NetworkMode = "host"
				hostConfig.Binds = append(hostConfig.Binds, sockets+":/sockets")
				hostConfig.NanoCPUs = 2_000_000_000
				hostConfig.Memory = 2 << 30
			},
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, started)
	if err != nil {
		t.Fatalf("start Postgres: %v", err)
	}
	testwait.Until(t, fmt.Sprintf("Postgres in container %s to log %q a second time", started.GetContainerID(), ready), func() (string, bool) {
		logs, err := started.Logs(ctx)
		if err != nil {
			return err.Error(), false
		}
		defer logs.Close()
		text, err := io.ReadAll(logs)
		if err != nil {
			return err.Error(), false
		}
		count := strings.Count(string(text), ready)
		return fmt.Sprintf("%d times", count), count >= 2
	})
	return "postgres://postgres:test@/postgres?host=" + url.QueryEscape(sockets) + "&sslmode=disable"
}
