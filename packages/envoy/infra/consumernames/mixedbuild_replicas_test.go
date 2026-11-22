//go:build twobuild

package consumernames

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/holdrecord"
)

// mixedBuildIdleProject is the daemon project the replicas-0 row converts.
const mixedBuildIdleProject = "MIXEDIDLE"

// A project at replicas 0: a daemon project whose Deployment is at 0 replicas and whose store has
// no intake_consumers table. Control, which this file runs: left at 0, the project's store has no
// table, so `convert` stops naming the project, and the hold's release is refused.
//
// The row's main part waits, and is not in this file: the project set to 1 under the hold, its
// daemon's boot migrating the store before any NATS connect, `convert` writing its marker row, and
// the project returning to 0 inside the hold with its marker done and serving nothing after the
// release. It runs packages/daemon's own binary as the stack's daemon entry
// (holdtest.Stack.DaemonCredential) and waits on a line of the daemon's naming mode before its
// first NATS connect, which the daemon does not log (its one such line, intake's "intake: the
// durable consumers' naming", comes after the connect), and on the window's mode check that
// prints which pod it read.
func TestAReplicasZeroProjectIsConvertedInTheWindow(t *testing.T) {
	mixedBuildPreviousRelease(t)

	t.Run("control: left at 0, the project's store has no table and convert stops naming it", func(t *testing.T) {
		h := newStack(t)
		h.mixedBuildProvision()
		ctx := context.Background()
		nc, js := h.mixedBuildListenerConn(mixedBuildNewMachine)
		fixed := daemonFixedPrefix + mixedBuildIdleProject + "-dispatch"
		_, err := js.CreateConsumer(ctx, notifications, jetstream.ConsumerConfig{
			Durable: fixed, FilterSubject: "notifications.legion." + mixedBuildIdleProject + ".dispatch", AckPolicy: jetstream.AckExplicitPolicy,
		})
		require.NoError(t, err, "the idle project's fixed durable, as its daemon made it before it was scaled to 0")
		nc.Close()
		pool := h.database(daemonFamily(mixedBuildIdleProject))
		h.censusDeploy(censusDeploymentOf(mixedBuildIdleProject, pool))

		id := h.prepared(holdrecord.WorkflowCutover)
		h.mixedBuildRun(0, actionPrepare, "--hold", id)
		h.held(id)
		h.mixedBuildRun(0, actionCreateBucket, "--hold", id)
		code, stdout, stderr := h.run(actionConvert, "--hold", id, "--daemon-database", mixedBuildIdleProject+"="+h.enrollFile(pool))
		require.Equal(t, 1, code, "convert over a project store with no table:\n%s%s", stdout, stderr)
		require.Contains(t, stdout+stderr, mixedBuildIdleProject, "convert's stop names the project")
		require.Contains(t, stdout+stderr, "it has no intake_consumers table", "convert's stop names the missing table")
		require.Error(t, h.release(id), "the release of a cutover whose convert stopped")
	})
}
