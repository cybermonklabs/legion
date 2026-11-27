// Package testlimits states, once, how many connections a Dispatch test suite needs from the
// Postgres it runs against, so that no CI lane, no pod's test server and no test holds a number of
// its own.
//
// Three places read it. .github/scripts/pg-connection-ceiling.sh checks that a CI lane's server
// allows ServerConnections plus that server's superuser reserve, through
// internal/dispatch/store/cmd/connections, and fails saying to raise max_connections in
// .github/scripts/postgres-service-settings.sh and every Postgres service's command: line with
// it. packages/daemon/docker/legion-testpg starts a pod's test server at that size, as a literal
// that testlimits_test.go holds to ServerConnections. And every start of a Dispatch process in
// cmd/dispatch's tests takes one of MaxProcessesAtOnce slots first and is checked against the
// server it meets (cmd/dispatch/process_slots_test.go and connection_ceiling_test.go), so a server
// sized below the suite fails there by name rather than as a 53300 refusal inside a room, a lease
// or a socket.
package testlimits

import "github.com/sjawhar/envoy/internal/dispatch/store"

// MaxProcessesAtOnce is the most Dispatch server processes one test binary runs at once against
// one Postgres. The count is the binary's, not one test's: one `go test` binary is what one lane's
// server serves, and parallel root tests in it share that server. A process counts from its start
// until it exits, which is what it holds connections for. Every start takes one of this many slots
// first (dispatchSlots, cmd/dispatch/process_slots_test.go, says how), and a test that asks for
// more at once is refused by name.
//
// Eight, so that cmd/dispatch's parallel tests do not queue for a slot. At 4, its tests waited
// 201 s and 216 s for slots in two runs, all starts summed, and a test whose own work was 0.3 s
// read 37 s; at 8 the waits summed 2 s and 0 s. The package's wall was the same at both: its
// serial tests run first, one at a time, and the parallel phase ends with its longest test.
// Two runs each at 4, 6 and 8, interleaved in one 4-CPU pod with Postgres in it, CI's service
// flags and no test failing: package 243.6/291.3 s, 251.0/226.7 s and 236.9/237.8 s, using 8-19%
// of the CPUs. The server cost is connections: 8 x ConnectionsPerProcess + SuiteHeadroom + 3
// reserved = 967, under the 1000 every CI lane's service allows
// (.github/scripts/postgres-service-settings.sh).
//
// The peak of each binary of cmd/dispatch with a tag: 3 with multiprocess
// (TestAProcessStoppedWhileWaitingForAdmissionReleasesItsDurables); 2 with twobuild and with
// basebuild. A test that comes to need more than this is refused by name, and this number is
// raised to what it runs.
const MaxProcessesAtOnce = 8

// RequiredConnections is what MaxProcessesAtOnce Dispatch processes may open together.
const RequiredConnections = MaxProcessesAtOnce * store.ConnectionsPerProcess

// SuiteHeadroom is the connections a test suite opens outside any Dispatch process: each
// package's own pool and the harness's reads. Nothing counts these at run time. A lane whose
// Postgres log shows a 53300 refusal that no guard refused first needs more.
const SuiteHeadroom = 20

// ServerConnections is what a test server must allow beyond its superuser reserve
// (superuser_reserved_connections).
const ServerConnections = RequiredConnections + SuiteHeadroom
