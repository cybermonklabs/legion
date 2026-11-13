// Package testlimits states, once, what a Dispatch test suite asks of the Postgres it runs
// against, so that no lane of CI and no test holds a number of its own.
//
// A lane's server is sized from RequiredConnections (.github/scripts/pg-connection-ceiling.sh,
// through internal/dispatch/store/cmd/connections), and every test that starts a process is held
// to the same statement (packages/envoy/cmd/dispatch/connection_ceiling_test.go): it refuses a
// start the server cannot serve, and it refuses a test that starts more processes than
// MaxProcessesPerTest, naming this constant. So a row that grows to a fourth process fails here
// by name rather than as a 53300 refusal inside a room, a lease or a socket, and raising it
// raises every lane's server with it.
package testlimits

import "github.com/sjawhar/envoy/internal/dispatch/store"

// MaxProcessesPerTest is the most Dispatch server processes one test RUNS AT ONCE against one
// Postgres: a process is counted from its start until it ends, which is what it holds the
// server's connections for, so a row that restarts a process one after another asks for one
// process's worth rather than one per start.
//
// Measured by the guard rather than counted by hand, which is the only way this number has been
// right: a count from the row names said three, the guard refused a row at four, then three rows
// at five, and counting starts rather than what runs would have climbed with every restart row.
// The guard refuses a test that runs more than this and names the constant, so a row that grows
// raises the number here, and every lane's server with it, rather than meeting a server nobody
// raised.
const MaxProcessesPerTest = 3

// RequiredConnections is what those processes may open at once: every lane's server allows at
// least this, beside the slots Postgres reserves for superusers and the headroom the suite's own
// clients take, which the sizing script adds from the server it is raising.
const RequiredConnections = MaxProcessesPerTest * store.ConnectionsPerProcess
