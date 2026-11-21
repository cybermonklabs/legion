package daemon

// The project release's hold for a role's background work (dispatch://INFRA-3583 R6, check E19),
// run on the idle rig through the `legion` an operator runs: the whole daemon on a scratch
// Postgres, the Sandbox runtime over the rig's stand-in cluster, a real NATS and Envoy listener,
// and a scripted role process in each pod, which reports its session's background work through
// the daemon's own activity route as the Oh My Pi extension does (idleRigPlan.Background). Each
// line the test logs begins with its check's name.

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
)

// E19 (dispatch://INFRA-3583 R6): a release asks and waits for a role that reports a running
// subagent or background job, as for a role in a turn, and its dry run names it. Two pods read
// clean, and no role of either is in a turn. The role of the first reports a running subagent;
// the role of the second reported an async job and then, once that job ended, that nothing runs.
// Both reports come after each role's last turn, so each is read against that turn
// (claim.ReadSessionWork). The dry run names the first pod's role as one a release would ask, and
// no role of the second. The release then asks the first pod's role, in an ask that states the
// rule the pod is held to, and the role answers. The release holds that pod while the report
// names the subagent, past a reread of the pod after the answer; once the subagent ends and the
// session reports that nothing runs, the pod is read a last time and deleted, before the bound.
// The second pod's role is asked nothing, and its pod is deleted at once after its last read,
// while the first pod is still held.
func TestProjectReleaseAsksARoleThatReportsBackgroundWork(t *testing.T) {
	const check = "E19"
	answers := idleRigPlan{TakeTurn: true, AnswerRelease: true}
	rig := newIdleWakeRig(t, map[string]idleRigPlan{"background": answers, "ended": answers}, "background", "ended")
	legion := buildLegion(t)
	background, ended := rig.issue("background"), rig.issue("ended")

	// Each role takes its start task's turn first. Only once the daemon has seen that turn end is
	// the host's work set going, so each report the session sends of it is read after that turn
	// started, as a report sent between turns is.
	for _, issue := range []idleRigIssue{background, ended} {
		token := rig.token(issue)
		idleRigEventually(t, idleRigRecordWait, "the end of the start task's turn of "+issue.key+"'s role", func() bool {
			return rig.api.claim(token).LastTurnEnd != nil
		})
	}
	subagent := answers
	subagent.Background = []string{"subagent"}
	job := answers
	job.Background = []string{"async-job"}
	rig.agents.setPlan(background.key, string(background.role), subagent)
	rig.agents.setPlan(ended.key, string(ended.role), job)
	rig.awaitReported(rig.token(background), "subagent")
	rig.awaitReported(rig.token(ended), "async-job")
	rig.agents.setPlan(ended.key, string(ended.role), answers)
	rig.awaitReported(rig.token(ended), "")
	t.Logf("%s: %s's %s reports a running subagent; %s's %s reported an async job, then that nothing runs",
		check, background.pod, background.role, ended.pod, ended.role)

	ran, dry := rig.projectRelease(legion, "--dry-run")
	if ran.code != 0 || !dry.DryRun {
		t.Fatalf("the dry run exited %d with %+v (stderr %q), want 0 and a dry run", ran.code, dry, ran.stderr)
	}
	dryAsked := map[string][]claim.Role{}
	for _, issue := range []idleRigIssue{background, ended} {
		line := releaseLine(t, dry, issue.pod)
		for _, asked := range line.Asked {
			dryAsked[issue.pod] = append(dryAsked[issue.pod], asked.Role)
		}
		t.Logf("%s: the dry run gives %s the outcome %s and names the roles a release would ask: %v",
			check, issue.pod, line.Outcome, dryAsked[issue.pod])
	}

	// Every rig release waits first for the reports it reads; each report above postdates its
	// role's last turn, so here the wait passes at once.
	rig.awaitReportsSinceLastTurn()
	args := rig.releaseArgs("--bound", "90s")
	done := make(chan idleRigCommand, 1)
	go func() { done <- legionRun(legion, args...) }()
	var finishOnce sync.Once
	var finished api.ProjectReleaseResponse
	finish := func(t *testing.T) api.ProjectReleaseResponse {
		t.Helper()
		finishOnce.Do(func() {
			var ran idleRigCommand
			ran, finished = awaitReleaseCommand(t, done)
			if finished.FinishedAt == nil || ran.code != 0 {
				t.Fatalf("legion project release exited %d printing %q (stderr %q), want 0 and a finished release",
					ran.code, ran.stdout, ran.stderr)
			}
			t.Logf("%s: legion project release exited 0; release %d started at %s, bound at %s, finished at %s", check, finished.ID,
				finished.StartedAt.UTC().Format(time.RFC3339Nano), finished.BoundAt.UTC().Format(time.RFC3339Nano),
				finished.FinishedAt.UTC().Format(time.RFC3339Nano))
		})
		if finished.FinishedAt == nil {
			t.Fatal("the release did not finish")
		}
		return finished
	}
	// endedGone is when the second pod's Sandbox was first seen gone, and noteEndedGone looks.
	var endedGone time.Time
	noteEndedGone := func() {
		if endedGone.IsZero() && rig.kube.sandbox(ended.pod) == nil {
			endedGone = time.Now()
		}
	}
	var reportEnded time.Time

	t.Run("a role reporting background work is asked and held", func(t *testing.T) {
		if want := []claim.Role{background.role}; !slices.Equal(dryAsked[background.pod], want) {
			t.Errorf("the dry run names %v as the roles a release would ask of %s, want %v: its role reports a running subagent",
				dryAsked[background.pod], background.pod, want)
		}

		var answered time.Time
		idleRigEventually(t, idleRigRecordWait, "the answer of "+background.pod+"'s role to the release's ask", func() bool {
			noteEndedGone()
			release, ok := rig.releaseNow()
			if !ok {
				return false
			}
			line, ok := releaseLineNow(release, background.pod)
			if !ok {
				return false
			}
			if line.Outcome != api.ReleaseWaiting {
				t.Fatalf("the release ended %s as %s with its asked roles %+v while its role reports a running subagent, want it asked and held",
					background.pod, lineJSON(line), line.Asked)
			}
			if len(line.Asked) == 1 && line.Asked[0].AnsweredAt != nil {
				answered = *line.Asked[0].AnsweredAt
				return true
			}
			return false
		})
		asks := rig.releaseAsks(background.key, background.role)
		if len(asks) != 1 {
			t.Fatalf("%s's %s was given %d release asks, want one", background.pod, background.role, len(asks))
		}
		t.Logf("%s: the release asked %s's %s at %s and it answered at %s; the ask says: %q", check, background.pod, background.role,
			asks[0].At.UTC().Format(time.RFC3339Nano), answered.UTC().Format(time.RFC3339Nano), asks[0].Text)
		if rule := "each role's last report names no subagent and no background job that runs"; !strings.Contains(asks[0].Text, rule) {
			t.Errorf("the ask to %s's %s does not say %q, the rule the pod is held to", background.pod, background.role, rule)
		}

		// Every asked role has answered and no turn runs, so only the report holds the pod: it
		// must stand past the pod's next reread (projectReleaseReread) as well as the read the
		// answer itself makes.
		holdUntil := time.Now().Add(projectReleaseReread + 2*time.Second)
		idleRigEventually(t, idleRigRecordWait, background.pod+" held past a reread after its role answered", func() bool {
			noteEndedGone()
			if rig.kube.sandbox(background.pod) == nil {
				t.Fatalf("%s's Sandbox is gone %s after its role answered, while its role reports a running subagent", background.pod,
					time.Since(answered))
			}
			if release, ok := rig.releaseNow(); ok {
				if line, ok := releaseLineNow(release, background.pod); ok && line.Outcome != api.ReleaseWaiting {
					t.Fatalf("the release ended %s as %s while its role reports a running subagent", background.pod, lineJSON(line))
				}
			}
			return time.Now().After(holdUntil) && !endedGone.IsZero()
		})
		t.Logf("%s: %s stood %s after its role answered, its role still reporting a running subagent", check, background.pod,
			time.Since(answered).Round(time.Millisecond))

		// The subagent ends between turns, and the session reports that nothing runs.
		rig.agents.setPlan(background.key, string(background.role), answers)
		reportEnded = time.Now()
		rig.awaitReported(rig.token(background), "")
		reported := time.Now()
		var deleted time.Time
		idleRigEventually(t, idleRigProjectReleaseWait, background.pod+"'s Sandbox deleted once its role reports nothing running", func() bool {
			if rig.kube.sandbox(background.pod) != nil {
				return false
			}
			deleted = time.Now()
			return true
		})
		t.Logf("%s: the subagent of %s's %s ended at %s, its report that nothing runs was stored %s later, and %s was deleted %s after the subagent ended",
			check, background.pod, background.role, reportEnded.UTC().Format(time.RFC3339Nano), reported.Sub(reportEnded).Round(time.Millisecond),
			background.pod, deleted.Sub(reportEnded).Round(time.Millisecond))

		release := finish(t)
		if !deleted.Before(release.BoundAt) {
			t.Errorf("%s was deleted at %s, at or after the bound %s, want its delete to follow the report's end", background.pod,
				deleted.UTC().Format(time.RFC3339Nano), release.BoundAt.UTC().Format(time.RFC3339Nano))
		}
		line := releaseLine(t, release, background.pod)
		if line.Outcome != api.ReleaseDeletedClean || len(line.Asked) != 1 || line.Asked[0].Role != background.role ||
			line.Asked[0].AnsweredAt == nil || line.Asked[0].Bound {
			t.Errorf("%s's line is %s, want deleted_clean with its %s answered before the bound", background.pod, lineJSON(line), background.role)
		}
		if asks := rig.releaseAsks(background.key, background.role); len(asks) != 1 {
			t.Errorf("%s's %s was given %d release asks by the release's end, want one", background.pod, background.role, len(asks))
		}
	})

	t.Run("a report that has ended holds nothing", func(t *testing.T) {
		if len(dryAsked[ended.pod]) != 0 {
			t.Errorf("the dry run names %v as the roles a release would ask of %s, want none: its role reported that nothing runs",
				dryAsked[ended.pod], ended.pod)
		}
		release := finish(t)
		line := releaseLine(t, release, ended.pod)
		if line.Outcome != api.ReleaseDeletedClean || len(line.Asked) != 0 {
			t.Errorf("%s's line is %s, want deleted_clean with no role asked", ended.pod, lineJSON(line))
		}
		if asks := rig.releaseAsks(ended.key, ended.role); len(asks) != 0 {
			t.Errorf("%s's %s was given %d release asks, want none", ended.pod, ended.role, len(asks))
		}
		if rig.kube.sandbox(ended.pod) != nil {
			t.Fatalf("%s's Sandbox is still there after the release finished", ended.pod)
		}
		// The first pod is the clock: its role reported the subagent from before the release
		// started until reportEnded, and the release held that pod all along.
		switch {
		case endedGone.IsZero():
			t.Fatalf("%s's Sandbox was not seen gone while the release held %s", ended.pod, background.pod)
		case reportEnded.IsZero():
			t.Fatalf("%s's Sandbox was seen gone at %s, but the first subtest stopped before %s's subagent ended, so nothing shows the release held %s meanwhile",
				ended.pod, endedGone.UTC().Format(time.RFC3339Nano), background.pod, background.pod)
		case !endedGone.Before(reportEnded):
			t.Fatalf("%s's Sandbox was seen gone at %s, want it gone at once after its last read, while %s was still held by its report (until %s)",
				ended.pod, endedGone.UTC().Format(time.RFC3339Nano), background.pod, reportEnded.UTC().Format(time.RFC3339Nano))
		}
		t.Logf("%s: the release asked nothing of %s and deleted it %s after the release started, %s before %s's subagent ended",
			check, ended.pod, endedGone.Sub(release.StartedAt).Round(time.Millisecond), reportEnded.Sub(endedGone).Round(time.Millisecond),
			background.pod)
	})
}
