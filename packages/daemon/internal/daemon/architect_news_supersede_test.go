package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The cases here are where a newer entry's start reaches a tree's root architect that holds a task
// carrying news of the approved design that no turn has taken (dispatch://INFRA-175 meeting
// dispatch://INFRA-2614), run through the daemon's own parts on PostgreSQL (newsPathRig). The root
// stands at admitted with its spec gate open at version 5, and its architect's row of the design
// took version 4, so the architect is owed the summary of version 5. The held task is one of two:
//
//   - a task of news alone (the reconcile's news-only start), which carries no entry. It is not
//     a task of the kind the architect's rule leaves in place, a wake's, since everything it says
//     the start of an entry carries too, so the entry's task takes its place (supervise's
//     Machine.supersedes), and the architect is sent its entry's work and the news in one turn,
//     never the news first with its owed entry waiting a turn and a backoff behind it;
//   - the task of the entry before, carrying the news, which a newer entry's task supersedes by
//     the architect's rule (Delivery.SupersededBy).
//
// Whatever became of the held task first — never sent, sent and acknowledged with its process gone
// before any turn, or confirmed by a turn whose process then went, which took the confirmation
// back — its retirement unsent ends no credit, since it holds none: a task no turn confirmed is no
// credit, and the take-back of a confirmation ended the one that confirmation made (the claim
// store counts only the copy a confirmed row holds, design_news_told_in). So the architect's row
// still owes the news when the entry's task arrives, that task carries it, read from the record
// when its start ran (the start executor's newsForStart), and the one prompt the architect is sent
// is that task. Its turn's confirmation is the credit that takes the row to version 5, once.
func TestANewerEntrysTaskTakesTheUntakenNewsOfARootArchitectAndTellsIt(t *testing.T) {
	t.Parallel()
	token := mustClaimToken(t, pathRoot, claim.RoleArchitect)
	for _, held := range []struct {
		name string
		// newsOnly is whether the untaken task the architect holds is one of news alone, rather
		// than the task of the entry before carrying the news.
		newsOnly bool
	}{
		{"a task of news alone", true},
		{"the task of the entry before, carrying the news", false},
	} {
		for _, became := range []struct {
			name string
			// gone is whether the task was sent and acknowledged and its process then went, and
			// confirmed whether a turn took it before that.
			gone, confirmed bool
		}{
			{name: "never sent"},
			{name: "sent and acknowledged, then its process went before any turn", gone: true},
			{name: "confirmed by a turn its process's end then took back", gone: true, confirmed: true},
		} {
			t.Run(held.name+"/"+became.name, func(t *testing.T) {
				r := newNewsPathRig(t, 5, pathMember{key: pathRoot, at: phase.Admitted})
				r.told(pathRoot, record.AudienceArchitect, 4)
				var machine *supervise.Machine
				var conn *fake.Conn
				var newsTask supervise.Delivery
				if held.newsOnly {
					// The architect takes its entry-1 task before what changed is recorded, so
					// that task carries no news, and is then sent the news alone.
					r.architectRow(1, "Take the tree up.")
					r.queueArchitectStart(record.SuperviseRequest{Task: "Take the tree up.", Entry: 1})
					r.run(startOf(pathRoot, claim.RoleArchitect, false))
					machine, conn = r.connect(pathRoot, claim.RoleArchitect)
					r.delivered(machine)
					r.startTurn(machine)
					endTurn(r.ctx, t, machine, token)
					r.recordArchitectChanges()
					r.queueArchitectStart(record.SuperviseRequest{News: true})
					if !became.gone {
						ownTurn(r.ctx, t, machine, token)
					}
					if got := r.run(startOf(pathRoot, claim.RoleArchitect, true)); len(got) != 1 {
						t.Fatalf("the architect's news-only starts run = %d, want one", len(got))
					}
				} else {
					r.recordArchitectChanges()
					r.architectRow(1, "Take the tree up.")
					r.queueArchitectStart(record.SuperviseRequest{Task: "Take the tree up.", Entry: 1})
					r.run(startOf(pathRoot, claim.RoleArchitect, false))
					var found bool
					if machine, found = r.sup.Machine(token); !found {
						t.Fatal("the architect's start created no claim")
					}
					if became.gone {
						machine, conn = r.connect(pathRoot, claim.RoleArchitect)
					}
				}
				if became.gone {
					newsTask = r.delivered(machine)
					if became.confirmed {
						r.startTurn(machine)
						r.wantTold("after the turn confirmed the held task", pathRoot, record.AudienceArchitect, 5, false)
					}
					if err := machine.Handle(r.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
						Locator: *machine.Claim().Locator, Kind: runtime.Gone, At: time.Now()}}); err != nil {
						t.Fatalf("end the architect's process: %v", err)
					}
					machine.Wait()
				}
				p := machine.Claim().Pending
				if p == nil || p.NewsOnly != held.newsOnly || !p.ConfirmedAt.IsZero() || len(p.News) == 0 {
					t.Fatalf("the architect holds %+v, want an untaken task carrying the news (news alone %t): the case's own premise", p, held.newsOnly)
				}
				newsTask = *p
				r.wantTold("before the newer entry's start", pathRoot, record.AudienceArchitect, 4, false)

				// The prompts the architect's connection carried before the newer entry's start: a
				// process that went is replaced by one whose connection has carried none, and an
				// architect never connected has none either.
				before := 0
				if conn != nil && !became.gone {
					before = len(conn.Prompts())
				}
				r.architectRow(2, "Carry the tree on.")
				r.queueArchitectStart(record.SuperviseRequest{Task: "Carry the tree on.", Entry: 2})
				r.run(startOf(pathRoot, claim.RoleArchitect, false))

				entryTask := machine.Claim().Pending
				if entryTask == nil || entryTask.ID == newsTask.ID || entryTask.NewsOnly || entryTask.Entry != 2 {
					t.Fatalf("the architect holds %+v after the entry-2 start, want that entry's task in place of %s", entryTask, newsTask.ID)
				}
				var carried record.DeliveryNews
				if err := json.Unmarshal(entryTask.News, &carried); err != nil || len(carried.Items) != 1 ||
					carried.Items[0].Audience != record.AudienceArchitect || carried.Items[0].To != 5 {
					t.Fatalf("the entry's task carries %s (%v), want the architect's summary of version 5", entryTask.News, err)
				}
				r.wantTold("after the newer entry's task took the held task's place", pathRoot, record.AudienceArchitect, 4, false)

				if became.gone || !held.newsOnly {
					machine, conn = r.connect(pathRoot, claim.RoleArchitect)
				} else {
					endTurn(r.ctx, t, machine, token)
				}
				sent := r.delivered(machine)
				prompts := conn.Prompts()[before:]
				if len(prompts) != 1 || prompts[0].DeliveryID != sent.ID || !strings.Contains(prompts[0].Message, "Carry the tree on.") ||
					!strings.Contains(prompts[0].Message, "version 5") {
					t.Fatalf("the architect's prompts once it could take one = %+v, want the entry's task alone, carrying the news of version 5", prompts)
				}
				r.startTurn(machine)
				r.wantTold("after the entry's task was confirmed", pathRoot, record.AudienceArchitect, 5, false)
			})
		}
	}
}

// A root's architect holds a phases row of the entries it is handed, as a phase worker does
// (dispatch://INFRA-2614), and its claim records the entry it was given. So the reconcile queues
// the news-only start of its summary, and that start reaches its claim, on the one rule every role's
// news is held to (record.NewsClaimTakes): the claim was given the entry its row names. An architect level
// with its row is told its summary by the news-only start now. An architect behind its row is owed
// that entry's start, which carries the summary itself (the start executor's newsForStart), so no
// news-only start is queued in front of it. Read as an architect that works no entry, as it was
// before its row held one, every architect given an entry would read as behind and be told no
// summary by any news-only start.
func TestTheRootArchitectsSummaryReachesItAtTheEntryItsRowNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		given   int64
		reaches bool
		why     string
	}{
		{name: "an architect given the entry its row names", given: 2, reaches: true,
			why: "its claim holds the work its row states, so the summary goes by a task of its own"},
		{name: "an architect behind the entry its row names", given: 1,
			why: "its owed entry's start carries the summary, so no task of news alone goes in front of it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsReconcileRig(t)
			r.member(reconcileRoot, "", phase.Implementing)
			r.told(reconcileRoot, record.AudienceHolder, 5)
			r.told(reconcileRoot, record.AudienceArchitect, 4)
			token := r.holder(reconcileRoot, claim.RoleArchitect, supervise.StateIdle, "", tc.given)
			r.inTx(func(tx pgx.Tx) error {
				return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: reconcileRoot, Role: claim.RoleArchitect, Claim: token, Entry: 2,
					EntryTask: "Carry the tree on.", EntryRaisedAt: time.Now().UTC()})
			})

			if err := r.reconcile(0); err != nil {
				t.Fatalf("reconcile the tree's news: %v", err)
			}

			starts := r.newsStarts()
			if !tc.reaches {
				if len(starts) != 0 {
					t.Fatalf("the news-only starts = %v, want none: %s", startsTo(starts), tc.why)
				}
				return
			}
			if got := startsTo(starts); len(got) != 1 || got[0] != reconcileRoot+"/architect@" {
				t.Fatalf("the news-only starts = %v, want the root architect's one: %s", got, tc.why)
			}
			machine := r.machine(reconcileRoot, claim.RoleArchitect, supervise.StateIdle, "", tc.given)
			if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
				t.Fatalf("run the root architect's news-only start: %v", err)
			}
			if _, news := pendingNews(t, machine); len(news.Items) != 1 || news.Items[0].Audience != record.AudienceArchitect || news.Items[0].To != 5 {
				t.Fatalf("the root architect's task carries %+v, want its summary of version 5: %s", news, tc.why)
			}
		})
	}
}

// architectRow writes the root architect's phases row at entry, with task as that entry's work,
// as the architect's start that raised it writes it (workflow.Engine.EnterArchitect).
func (r *newsPathRig) architectRow(entry int64, task string) {
	r.t.Helper()
	r.inTx(func(tx pgx.Tx) error {
		return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: r.root, Role: claim.RoleArchitect,
			Claim: mustClaimToken(r.t, r.root, claim.RoleArchitect), Entry: entry, EntryTask: task, EntryRaisedAt: time.Now().UTC()})
	})
}

// queueArchitectStart queues a start of the root architect of the root's run carrying what request
// carries (its task and entry, or the mark of a start for news alone), as the workflow and the
// reconcile queue one.
func (r *newsPathRig) queueArchitectStart(request record.SuperviseRequest) {
	r.t.Helper()
	request.Op, request.Tree, request.Role, request.Generation = "start", r.root, claim.RoleArchitect, 1
	r.inTx(func(tx pgx.Tx) error {
		row, err := record.NewOutboxRow(r.root, request, time.Now().UTC())
		if err != nil {
			return err
		}
		return r.records.Enqueue(r.ctx, tx, row)
	})
}

// recordArchitectChanges records what changed in the root's spec from version 4 to version 5, as
// the reconcile records what Dispatch read, so a start can carry the architect's summary of it.
func (r *newsPathRig) recordArchitectChanges() {
	r.t.Helper()
	r.inTx(func(tx pgx.Tx) error {
		return r.records.PutDesignChanges(r.ctx, tx, record.DesignChanges{VersionPair: record.VersionPair{ArtifactID: pathDoc, From: 4, To: 5},
			State: record.DesignChangesRead, ReadAt: time.Now().UTC(), Sections: []record.SectionChange{{Heading: "Design", Change: "changed"}}})
	})
}
