// Package testlimits states, once, how many connections a Dispatch test suite needs from the
// Postgres it runs against, so that no CI lane, no pod's test server and no test holds a number of
// its own.
//
// Three places read it. .github/scripts/pg-connection-ceiling.sh raises a CI lane's server to
// FullSizeServerConnections plus that server's superuser reserve, through
// internal/dispatch/store/cmd/connections, so every lane runs the Dispatch processes it starts at
// production's pool sizes. packages/daemon/docker/legion-testpg starts a pod's test server at
// ServerConnections plus the reserve, as a literal that testlimits_test.go holds to
// ServerConnections: one connection per process short of FullSizeServerConnections, so each
// process there runs its pools scaled to a budget one connection under production's. And every
// start of a Dispatch server process in cmd/dispatch's tests is checked against MaxProcessesAtOnce
// and against the server it meets (cmd/dispatch/connection_ceiling_test.go): a server smaller than
// FullSizeServerConnections gives each process a connection budget its pools scale to
// (PoolBudget), and a server too small for even the smallest pools refuses the start by name,
// rather than as a 53300 refusal inside a room, a lease or a socket. Two runs of the binary are
// not checked: TestDispatchRefusesAListenFDThatIsNotASocketAndBindsNothing, which exits before it
// reads its database, and startRedeliverWebhooks (cmd/dispatch/multiprocess_test.go), whose
// redeliver-webhooks run opens store.Open at production's sizes and is counted by nothing.
package testlimits

import (
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// MaxProcessesAtOnce is the most Dispatch server processes one test binary runs at once against
// one Postgres. The count is the binary's, not one test's: one `go test` binary is what one lane's
// server serves, and parallel root tests in it share that server. A process counts from its start
// until it exits, which is what it holds connections for. cmd/dispatch/connection_ceiling_test.go
// refuses the start of one more process than this and names this constant.
//
// It is the limit the guard enforces, measured with that same count over the cmd/dispatch test
// binaries whose rows ran: the twobuild binary reaches 4, in TestTwoBuildsAnswerEachOthersCLI, and
// so do the binaries with no tag and with multiprocess, in
// TestEveryProcessAHundredConnectionServerRunsStaysWithinItsBudget, which runs this many on purpose;
// their other rows reach 3. The memory binary's measured 1 is a floor, not its peak: in a gVisor
// pod each of its rows fails at /proc/<pid>/clear_refs after its first start, and its code bounds it
// at two, a writer and a reader (cmd/dispatch/memory_test.go,
// TestTheHeaviestStoredDocumentsStayWithinTheMemoryBound). A binary that comes to run more is
// refused by name, and this number is raised to what it runs.
const MaxProcessesAtOnce = 4

// RequiredConnections is what MaxProcessesAtOnce Dispatch processes would open together at
// production's pool sizes as a deployment runs them, store.ConnectionsPerProcess each: without the
// test hooks' listener a process started with DISPATCH_TEST_HOOKS=1 holds besides
// (store.ConnectionsPerTestProcess), which the memory harness's processes do not hold.
const RequiredConnections = MaxProcessesAtOnce * store.ConnectionsPerProcess

// SuiteHeadroom is the connections a test suite opens outside any Dispatch process: each
// package's own pool and the harness's reads. Nothing counts these at run time. A lane whose
// Postgres log shows a 53300 refusal that no guard refused first needs more.
const SuiteHeadroom = 20

// ServerConnections is RequiredConnections beside SuiteHeadroom, the test hooks' listeners left
// out: the size of a pod's test server beyond its superuser reserve
// (packages/daemon/docker/legion-testpg, whose literal testlimits_test.go holds to it plus
// Postgres's default reserve). It is FullSizeServerConnections less one connection per process,
// so PoolBudget gives each process there a budget one connection under production's sizes, and
// its pools scale to that.
const ServerConnections = RequiredConnections + SuiteHeadroom

// FullSizeServerConnections is what a test server must allow beyond its superuser reserve
// (superuser_reserved_connections) for every Dispatch process a suite starts to run at
// production's pool sizes: MaxProcessesAtOnce processes of store.ConnectionsPerTestProcess each,
// the test hooks' listener counted, beside SuiteHeadroom. Every CI lane's server allows it
// (.github/scripts/pg-connection-ceiling.sh, through internal/dispatch/store/cmd/connections). A
// smaller server scales them (PoolBudget).
const FullSizeServerConnections = MaxProcessesAtOnce*store.ConnectionsPerTestProcess + SuiteHeadroom

// MinServerConnections is the fewest connections a test server must allow beyond its superuser
// reserve for a suite to run at all: MaxProcessesAtOnce processes with every pool at its smallest
// (store.MinBudget), beside SuiteHeadroom.
func MinServerConnections() int { return MaxProcessesAtOnce*store.MinBudget() + SuiteHeadroom }

// PoolBudget is how many connections each Dispatch process a test starts may open on a server that
// allows maxConnections with reserved of them kept for superusers. It is zero on a server that
// allows FullSizeServerConnections beyond its reserve, as every CI lane's does that runs these
// tests: there every process runs production's pool sizes, beside the test hooks' listener of
// each process started with DISPATCH_TEST_HOOKS=1 (the memory harness's processes hold none). On
// a smaller server, such as a pod's test server (ServerConnections) or a Postgres at its default
// of 100, it is that server's share for one of MaxProcessesAtOnce processes beside SuiteHeadroom,
// (maxConnections - reserved - SuiteHeadroom) / MaxProcessesAtOnce rounded down, which the test
// harness hands each process it starts through startDispatchProcessWith
// (DISPATCH_TEST_POOL_BUDGET) and the process scales its pools to (store.OpenWithin). The memory
// harness hands no budget: it refuses a scaled server instead (requireProductionPools). A share
// below store.MinBudget is refused, and the error names the max_connections the server needs.
func PoolBudget(maxConnections, reserved int) (int, error) {
	if maxConnections-reserved >= FullSizeServerConnections {
		return 0, nil
	}
	budget := (maxConnections - reserved - SuiteHeadroom) / MaxProcessesAtOnce
	if budget >= store.MinBudget() {
		return budget, nil
	}
	return 0, fmt.Errorf("a Postgres that allows %d connections with %d of them reserved for superusers leaves each "+
		"of testlimits.MaxProcessesAtOnce=%d Dispatch processes %d beside the %d the suite opens itself "+
		"(testlimits.SuiteHeadroom), and one process needs %d with every pool at its smallest (store.MinBudget). "+
		"Restart the server with -c max_connections=%d or more (testlimits.MinServerConnections plus the reserve). "+
		"From %d (testlimits.FullSizeServerConnections plus the reserve), as every CI lane's server allows "+
		"(.github/scripts/pg-connection-ceiling.sh), every process runs production's pool sizes; below that, "+
		"each runs pools scaled to its share",
		maxConnections, reserved, MaxProcessesAtOnce, max(budget, 0), SuiteHeadroom, store.MinBudget(),
		MinServerConnections()+reserved, FullSizeServerConnections+reserved)
}
