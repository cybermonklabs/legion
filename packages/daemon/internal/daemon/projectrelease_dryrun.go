package daemon

// A project release's dry run (`legion project release --dry-run`): what a release would do from
// the reads now, without the settle of replacement records, read with no ask, no stop and no
// record.

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// dryRun is the release's inventory, classification and reads, with no ask, no stop and no
// record: each Sandbox with the outcome a release would give it from the reads now, without the
// settle of replacement records (settleReplacements), each pod a release would ask with the roles
// it would ask, by the release's own decision (asks), a role whose activity report holds the pod
// included, and each claim a release would stop. A pod in which no role process runs is read
// through its launcher, as the release reads it before its delete, and asks no one (readAll). The
// stop set is read from the store as the release's own transaction reads it
// (store.ProjectReleaseStopSet).
func (r *projectReleaser) dryRun(ctx context.Context, bound time.Duration) (api.ProjectReleaseResponse, error) {
	startedAt := r.now().UTC()
	sandboxes, err := r.pods.ProjectSandboxes(ctx)
	if err != nil {
		return api.ProjectReleaseResponse{}, fmt.Errorf("list the project's Sandboxes: %w", err)
	}
	stop, err := r.store.ProjectReleaseStopSet(ctx, r.project)
	if err != nil {
		return api.ProjectReleaseResponse{}, err
	}
	run := &releaseRun{bound: startedAt.Add(bound), stop: stop, asked: map[claim.Token]releaseAsk{}}
	pods, others := r.inventory(run, sandboxes)
	reads := r.readAll(ctx, run, pods)
	left := map[string]bool{}
	for i, pod := range pods {
		line := &run.result.Sandboxes[pod.line]
		if pod.class == releaseLeaves {
			line.Outcome, line.Reason = api.ReleaseLeft, pod.why
			left[pod.sandbox.Issue] = true
			continue
		}
		if !reads[i].launcher {
			views := r.issueViews(pod.sandbox.Issue)
			if asked, _ := r.asks(ctx, reads[i], views); asked {
				for _, c := range runningClaims(views) {
					line.Asked = append(line.Asked, api.ReleasedAsk{Role: c.Role})
				}
			}
		}
		line.Outcome = deletedOutcome(reads[i])
	}
	for _, i := range others {
		line := &run.result.Sandboxes[i]
		line.Outcome = api.ReleaseDeleted
		if line.Kind == runtime.ProjectSandboxUnknown {
			line.Outcome, line.Reason = api.ReleaseLeft, releaseUnknownKind
		}
	}
	for i := range run.result.Claims {
		line := &run.result.Claims[i]
		line.Ended, line.BootStarts = api.ReleasedClaimEnd(supervise.SuspendedByProjectRelease), true
		if left[line.Issue] {
			line.Ended, line.BootStarts = api.ReleasedClaimEnd(supervise.SuspendedByOperator), false
		}
	}
	finished := r.now().UTC()
	return api.ProjectReleaseResponse{DryRun: true, StartedAt: startedAt, BoundAt: run.bound, Bound: bound.String(),
		FinishedAt: &finished, Result: run.result.Listed()}, nil
}
