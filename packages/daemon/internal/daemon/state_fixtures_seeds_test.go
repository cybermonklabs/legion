package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/apifixture"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// seedStage3 is a project at Stage 3: one root in review whose Dispatch status write has failed
// twice and waits on its backoff, and a root queued behind it, on the smallest tmux file.
func seedStage3(s *seeding) composition {
	cfg := apifixture.LoadSettings(s.t, apiSettings(s.t, "minimalSettingsFile"))
	reviewing := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "LEGION-208", Phase: phase.Reviewing,
		Status: "needs_review", DispatchStatus: "needs_review", Generation: 4, LastDispatchSeq: 9,
		Repo: "sjawhar/legion-smoke", DefaultBranch: "main", GitHubRepo: "sjawhar/legion-smoke", HandedOver: true}
	queued := record.Issue{Key: "LEGION-209", Tree: "LEGION-209", Project: "LEGION", Title: "LEGION-209", Phase: phase.Admitted,
		Status: "todo", DispatchStatus: "todo", Generation: 1, LastDispatchSeq: 10, Rank: "00009",
		Repo: "sjawhar/legion-smoke", DefaultBranch: "main", GitHubRepo: "sjawhar/legion-smoke", HandedOver: true}
	reading := func(key, status string, seq int64) record.DispatchReading {
		return record.DispatchReading{Key: key, Project: "LEGION", Seq: seq, Status: record.Carry(status), Parent: record.Carry(""),
			Blockers: record.Carry([]string{}), Applied: true}
	}
	s.putRecorded(
		recorded{reviewing, reading("LEGION-208", "needs_review", 9), time.Date(2126, 9, 22, 17, 24, 0, 0, time.UTC)},
		recorded{queued, reading("LEGION-209", "todo", 10), time.Date(2126, 9, 22, 16, 2, 0, 0, time.UTC)},
	)
	s.in("the slot of LEGION-208", func(ctx context.Context, tx pgx.Tx) error {
		return s.records.PutSlot(ctx, tx, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2126, 9, 22, 14, 0, 0, 0, time.UTC)})
	})
	s.openTrees("LEGION", "LEGION-208")
	// The move to review wrote LEGION-208's Dispatch status (workflow's status write,
	// record.OrdinaryStatusWrite), and two attempts of it failed (the outbox's RetryOutbox).
	write := s.enqueue("LEGION-208", record.OrdinaryStatusWrite("needs_review", "in_progress", ""), time.Date(2126, 9, 22, 17, 24, 0, 0, time.UTC))
	for attempt, leasedAt := range []time.Time{time.Date(2126, 9, 22, 17, 24, 0, 0, time.UTC), time.Date(2126, 9, 22, 17, 28, 0, 0, time.UTC)} {
		row := s.lease(write, leasedAt, time.Minute)
		nextAt := time.Date(2126, 9, 22, 17, 28, 0, 0, time.UTC)
		if attempt == 1 {
			nextAt = time.Date(2126, 9, 22, 17, 32, 0, 0, time.UTC)
		}
		s.in("a failed attempt of LEGION-208's status write", func(ctx context.Context, tx pgx.Tx) error {
			return s.records.RetryOutbox(ctx, tx, row.ID, row.LeaseToken, nextAt, "Dispatch unavailable")
		})
	}
	stage3At := time.Date(2126, 9, 22, 17, 30, 0, 0, time.UTC)
	for _, booted := range []time.Time{time.Date(2126, 9, 18, 14, 3, 27, 0, time.UTC), time.Date(2126, 9, 19, 9, 0, 0, 0, time.UTC),
		time.Date(2126, 9, 20, 9, 0, 0, 0, time.UTC), time.Date(2126, 9, 21, 9, 0, 0, 0, time.UTC), stage3At} {
		_, err := s.st.RecordBoot(s.ctx, "LEGION", booted)
		s.must("a boot", err)
	}
	return composition{cfg: cfg, now: time.Date(2126, 9, 22, 17, 30, 30, 0, time.UTC), startedAt: stage3At}
}

// seedOperatorClaim is an operator's spawn of an architect on an issue the workflow keeps no record
// of (internal/api's spawn route: store.Store.OpenOperatorTreeLifecycle, then the claim's admission),
// idle in its tmux pane, on the smallest tmux file of its own project. No record moves, so its
// moments are the wall clock's month.
func seedOperatorClaim(s *seeding) composition {
	const project = "AC91849371"
	minimal := apiSettings(s.t, "minimalSettingsFile")
	body, found := strings.CutPrefix(minimal, "project: LEGION\n")
	if !found {
		s.t.Fatalf("the smallest settings file %q does not begin with its project", minimal)
	}
	cfg := apifixture.LoadSettings(s.t, "project: "+project+"\n"+body)
	spawnedAt := time.Date(2026, 9, 25, 5, 50, 0, 0, time.UTC)
	_, err := s.st.RecordBoot(s.ctx, project, spawnedAt)
	s.must("the boot", err)
	_, err = s.st.OpenOperatorTreeLifecycle(s.ctx, "ac91849371", "AC91849371-900", func(context.Context, pgx.Tx) error { return nil })
	s.must("the operator's tree", err)
	token := s.token(project, "AC91849371-900", claim.RoleArchitect)
	s.claimAs(supervise.Claim{Token: token, Project: "ac91849371", Tree: "AC91849371-900", Issue: "AC91849371-900", Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateIdle, Session: "01a0d71d-aace-7448-a0d4-e401d736438f",
		Locator: &runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: "1618075:268131172", Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}}})
	return composition{cfg: cfg, now: spawnedAt.Add(5 * time.Minute), startedAt: spawnedAt}
}

// seedLastPause is a project that runs after a pause has ended: the operator paused it for a
// provider's outage, and the controller resumed it from its session (internal/pause's Service.Pause
// and Service.Resume), with the moments, the reason and the actors of the pause the pause routes'
// goldens answer (apifixture.Pause), on the deployment those routes serve; the store numbers it.
// Its daemon booted and its image passed the probe, and no session has reported to it, so
// every image row of its capability report is present and every live row unchecked: the state a
// boot reads. No record moves, so its moments are the wall clock's month.
func seedLastPause(s *seeding) composition {
	cfg := apifixture.LoadSettings(s.t, apiSettings(s.t, "settingsFixtureFile"))
	paused, ended := apifixture.Pause(false), apifixture.Pause(true)
	startedAt := time.Date(2026, 11, 15, 8, 30, 0, 0, time.UTC)
	for _, booted := range []time.Time{time.Date(2026, 11, 2, 14, 3, 27, 0, time.UTC), startedAt} {
		_, err := s.st.RecordBoot(s.ctx, "LEGION", booted)
		s.must("a boot", err)
	}
	_, err := pauseService(s, cfg, paused.Since).Pause(s.ctx, paused.By, paused.Reason)
	s.must("the pause", err)
	_, _, err = pauseService(s, cfg, ended.EndedAt).Resume(s.ctx, ended.EndedBy)
	s.must("the resume", err)
	// A daemon that holds a pause runs a workflow, whose three durable consumers are caught up.
	caughtUp := ended.EndedAt.Add(19 * time.Minute)
	return composition{cfg: cfg, now: ended.EndedAt.Add(20 * time.Minute), startedAt: startedAt,
		secretsIssued: true, probed: true, image: bootprobe.ImageReport{ModelFallback: "on"},
		consumers: []intake.ConsumerLag{
			{Name: "legion-go-LEGION-dispatch", LastHandledAt: caughtUp},
			{Name: "legion-go-LEGION-github", LastHandledAt: caughtUp},
			{Name: "legion-go-LEGION-wake", LastHandledAt: caughtUp},
		}}
}
