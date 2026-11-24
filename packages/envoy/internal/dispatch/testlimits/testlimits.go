// Package testlimits states, once, how many connections a Dispatch test suite needs from the
// Postgres it runs against, so that no CI lane, no pod's test server and no test holds a number of
// its own.
//
// Three places read it. .github/scripts/pg-connection-ceiling.sh raises a CI lane's server to
// ServerConnections plus that server's superuser reserve, through
// internal/dispatch/store/cmd/connections. packages/daemon/docker/legion-testpg starts a pod's test
// server at that size, as a literal that testlimits_test.go holds to ServerConnections. And every
// start of a Dispatch process in cmd/dispatch's tests takes one of MaxProcessesAtOnce slots first
// and is checked against the server it meets (cmd/dispatch/process_slots_test.go and
// connection_ceiling_test.go), so a server sized below the suite fails there by name rather than
// as a 53300 refusal inside a room, a lease or a socket.
package testlimits

import "github.com/sjawhar/envoy/internal/dispatch/store"

// MaxProcessesAtOnce is the most Dispatch server processes one test binary runs at once against
// one Postgres. The count is the binary's, not one test's: one `go test` binary is what one lane's
// server serves, and parallel root tests in it share that server. A process counts from its start
// until it exits, which is what it holds connections for. cmd/dispatch/process_slots_test.go holds
// every start to one of this many slots, and refuses by name a test that asks for more at once.
//
// Each binary of cmd/dispatch runs up to 4 at once, as follows. With no tag, most tests are
// parallel and share the 4 slots: a test's first process waits for a free slot, and the four that
// run two processes at once reserve both before their first start. The memory binary's tests are
// parallel too, and TestTheHeaviestStoredDocumentsStayWithinTheMemoryBound reserves its writer and
// reader. The multiprocess, twobuild and basebuild tests run one at a time, and each takes a free
// slot for every process it runs at once: three in multiprocess's three-process test, four in
// twobuild's TestTwoBuildsAnswerEachOthersCLI. A test that comes to need more is refused by name,
// and this number is raised to what it runs.
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
