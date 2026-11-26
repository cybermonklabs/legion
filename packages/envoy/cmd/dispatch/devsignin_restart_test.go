package main

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// registryRefusalLine is what a process logs when a live server process refuses its signing keys
// and it waits for that process's heartbeat to lapse (processes.Register).
const registryRefusalLine = "a live server process refuses this one's signing keys; waiting for its heartbeat to lapse"

// restartProcessHeartbeat is the registry pace of both processes of the row below
// (DISPATCH_TEST_PROCESS_HEARTBEAT), in place of production's five seconds. It is twice
// quickProcessHeartbeat, because this row drives the registry from the outside while the restart's
// wait runs: the wait is a live window and a heartbeat from the restart's first refusal, 8 s here,
// and every step the test takes inside it is one read of the restart's output and one statement.
const restartProcessHeartbeat = 2 * quickProcessHeartbeat

// A dev sign-in server signs its cookies with a key it mints for its own process
// (sessionSigningKey), so a restart carries a key set the process before it never held. Killed
// (SIGKILL, as Playwright stops packages/dispatch/e2e/run-server.sh's server), that process never
// deregisters, and its registry row counts as live until its heartbeat lapses. A server started
// on the same database at once must still come up: its registration is refused beside the dead
// row, waits for it to lapse (processes.Register) rather than refusing to start beside a process
// that no longer runs, and serves.
//
// Both processes keep the registry at restartProcessHeartbeat's pace, so the restart's wait for
// the dead row is a live window and a heartbeat, 8 s, and not production's 20 s. The row's
// heartbeat is set an hour ahead once the kill lands, so the restart meets a live dead row
// however long its boot and its wait for a process slot take. Once the restart has logged that it
// was refused, the row is given the heartbeat of a process last seen one beat ago, so it lapses
// two beats later: the restart's next try, at most one beat away, still finds it live and is
// refused again, and the try after that is admitted. So the row reads the whole order it is
// about - refused, refused again, admitted once the row lapsed - and a build that gave up at its
// first try after the refusal would exit instead. It leaves the test 4 s of the 8 s wait for its
// own read of the output and its one statement.
func TestADevSignInServerRestartedAfterAKillStarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	options := dispatchOptions{Env: map[string]string{
		"DISPATCH_DEV_SIGNIN":              "1",
		"DISPATCH_IDENTITY":                "cookie",
		"DISPATCH_IDENTITY_HEADER_TRUSTED": "",
		"DISPATCH_INSECURE_COOKIE":         "1",
		// The dev sign-in fence takes a loopback dashboard origin only; /healthz, all this test
		// asks, is served whatever Host a request names.
		"DISPATCH_SERVER_URL":             "http://127.0.0.1",
		"DISPATCH_TEST_PROCESS_HEARTBEAT": restartProcessHeartbeat.String(),
	}}
	first := startDispatchProcessWith(t, database.Pool.Config().ConnString(), options)
	first.waitHealthy(t)
	awaitRegistryAdmission(t, database, first)
	if err := first.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the first server: %v", err)
	}
	first.WaitExit(t, "SIGKILL")
	if tag, err := database.Pool.Exec(ctx, `update server_processes set heartbeat_at = now() + interval '1 hour'`); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("hold the killed server's row live: %v rows, %v", tag.RowsAffected(), err)
	}

	second := startDispatchProcessWith(t, database.Pool.Config().ConnString(), options)
	second.waitForLog(t, 30*time.Second, registryRefusalLine)
	if _, err := database.Pool.Exec(ctx, `update server_processes set heartbeat_at = clock_timestamp() - make_interval(secs => $1)`,
		restartProcessHeartbeat.Seconds()); err != nil {
		t.Fatalf("give the killed server's row the heartbeat of a process last seen a beat ago: %v", err)
	}
	second.waitHealthy(t)
}
