// Package testlimits states, once, how many connections a Dispatch test suite needs from the
// Postgres it runs against, so that no CI lane, no pod's test server and no test holds a number of
// its own.
//
// Three places read it. .github/scripts/pg-connection-ceiling.sh raises a CI lane's server to
// ServerConnections plus that server's superuser reserve, through
// internal/dispatch/store/cmd/connections. packages/daemon/docker/legion-testpg starts a pod's test
// server at that size, as a literal that testlimits_test.go holds to ServerConnections. And every
// start of a Dispatch process in cmd/dispatch's tests is checked against MaxProcessesAtOnce and
// against the server it meets (cmd/dispatch/connection_ceiling_test.go), so a test binary that
// runs one process too many fails there by name rather than as a 53300 refusal inside a room, a
// lease or a socket.
package testlimits

import "github.com/sjawhar/envoy/internal/dispatch/store"

// MaxProcessesAtOnce is the most Dispatch server processes one test binary runs at once against
// one Postgres. The count is the binary's, not one test's: one `go test` binary is what one lane's
// server serves, and parallel root tests in it share that server. A process counts from its start
// until it exits, which is what it holds connections for. cmd/dispatch/connection_ceiling_test.go
// refuses the start of one more process than this and names this constant.
//
// The value is that same count's peak over the cmd/dispatch test binaries the lanes run (no tag,
// memory, multiprocess and twobuild), so it is what the guard holds rather than an estimate: a
// binary that comes to run more is refused by name, and this number is raised to what it runs.
const MaxProcessesAtOnce = 4

// RequiredConnections is what MaxProcessesAtOnce Dispatch processes may open together.
const RequiredConnections = MaxProcessesAtOnce * store.ConnectionsPerProcess

// SuiteHeadroom is the connections a test suite opens outside any Dispatch process: each
// package's own pool and the harness's reads. Nothing counts these at run time. A lane whose
// Postgres log shows a 53300 refusal that no guard refused first needs more.
const SuiteHeadroom = 20

// ServerConnections is what a test server must allow beyond its superuser reserve
// (superuser_reserved_connections).
const ServerConnections = RequiredConnections + SuiteHeadroom
