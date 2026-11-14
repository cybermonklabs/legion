package daemon

// A whole daemon over a pool of one connection (INFRA-3169): the two paths that judge the
// released-claim rule — the state route and the boot's stranded report — each read the pod
// resumes from inside a transaction they already hold, so neither asks the pool for a second
// connection while holding its only one. On the deployment where this was measured all 32
// connections sat `idle in transaction`, and a pool of one is that deployment in miniature: a
// path that acquires twice waits for a connection its own transaction holds, for ever.
//
// Each test bounds the operation it drives with a deadline of its own, so a daemon that goes
// back to acquiring twice fails the test where it is measured instead of hanging the package's
// run until the binary's timeout kills it.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// unfinishedResumesFailure is what the store says when the read of a project's unfinished resumes
// does not answer, the pool timing out under a transaction of its own included
// (store.UnfinishedPodResumesTx).
const unfinishedResumesFailure = "read the unfinished resumes of"

// strandedReadFailure is what the boot logs when its report cannot read, which is what a report
// that waited for a connection its own transaction held leaves behind (reportStranded).
const strandedReadFailure = "read the issue record to report the claims nothing will wake"

// The state route answers over real HTTP while the daemon's pool holds one connection. The route
// reads its whole state in one repeatable-read transaction, the released-claim rule's wakes among
// them, so a read of those wakes that went to the pool would wait on the connection the route's
// own transaction holds and the request would never be answered. The deadline on the request is
// what makes that a failure here rather than a hung package.
func TestTheStateRouteAnswersWhenThePoolHasOneConnection(t *testing.T) {
	cfg := oneConnectionDaemonOfAReleasedPod(t)
	d := startDaemon(t, cfg, fakeRuntime(fake.NewRuntime(), &built{}))

	ctx, cancel := context.WithTimeout(context.Background(), testwait.Bounded(t, 10*time.Second, time.Second))
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"/legion/v1/state", nil)
	if err != nil {
		t.Fatalf("build the state request: %v", err)
	}
	response, err := d.client.Do(request)
	if err != nil {
		t.Fatalf("GET the state on a pool of one connection: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the state's body: %v", err)
	}
	if response.StatusCode != http.StatusOK || len(body) == 0 {
		t.Fatalf("GET the state = %d with %d bytes, want 200 with a body", response.StatusCode, len(body))
	}
}

// The boot's report of the claims nothing will wake finishes, and reads everything it needed,
// while the daemon's pool holds one connection. The report runs its whole judgement in one
// transaction, so a read of the released pods' wakes that went to the pool would wait on that
// transaction's own connection until strandedReportTimeout cancelled it and the boot would log a
// failed read instead of a report. The hook the daemon offers a test (strandedReported) is what
// says the report has finished, and the deadline on waiting for it is the test's own.
func TestTheBootReportAnswersWhenThePoolHasOneConnection(t *testing.T) {
	cfg := oneConnectionDaemonOfAReleasedPod(t)
	var logs syncBuffer
	o := fakeRuntime(fake.NewRuntime(), &built{})
	reported := strandedReportDone(&o)

	startDaemonLogging(t, cfg, o, slog.New(slog.NewJSONHandler(&logs, nil)))

	ctx, cancel := context.WithTimeout(context.Background(), testwait.Bounded(t, strandedReportTimeout+10*time.Second, time.Second))
	defer cancel()
	select {
	case <-reported:
	case <-ctx.Done():
		t.Fatalf("the boot's stranded report did not finish on a pool of one connection: %v", ctx.Err())
	}
	for _, failure := range []string{unfinishedResumesFailure, strandedReadFailure} {
		if strings.Contains(logs.String(), failure) {
			t.Fatalf("the boot logged %q: %s", failure, logs.String())
		}
	}
}

// oneConnectionDaemonOfAReleasedPod is a configuration for a daemon with a database of its own
// whose pool holds one connection (daemonDSN), already carrying what makes both paths read the
// released pods' wakes at all: an issue at work whose implementer a release left released, which
// is the one claim strandedReleaseWakes needs before it reads anything. A released claim is the
// cheapest honest way to that read — the rule looks at the claims and nothing else — and it costs
// no pod, no wake and no resume row.
//
// The fixture is written over the test's own pool, before the one-connection DSN becomes the
// daemon's: a seeding that shared that single connection would say nothing about the routes this
// file measures, and the daemon migrates the database on its boot either way.
func oneConnectionDaemonOfAReleasedPod(t *testing.T) config.Config {
	t.Helper()
	pool := isolatedOutboxPool(t)
	cfg := testConfig(t)
	cfg.PostgresDSN = pool.Config().ConnString()
	issue := cfg.Project + "-1"
	putPreviousRun(t, cfg, previousRun{
		issues: []record.Issue{runningRoot(issue, phase.Implementing)},
		slots:  []string{issue},
		claims: []supervise.Claim{releaseHeld(previousClaim(t, cfg, issue, issue, claim.RoleImplementer, supervise.StateSuspended))},
	})
	cfg.PostgresDSN = daemonDSN(t, pool, "legion-daemon-"+strings.ToLower(randomSuffix(t)))
	return cfg
}

// releaseHeld is c as the idle rule's release of its pod leaves it: suspended, recorded as the
// release's, which is what supervise.Claim.Released answers to.
func releaseHeld(c supervise.Claim) supervise.Claim {
	c.SuspendedBy = supervise.SuspendedByReleased
	return c
}
