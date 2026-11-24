package workflow

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The owner's catch-up at a launch names the open merge forward asked of it only where the owner's
// claim as it is ready now was not told it, by the one rule of told an owner-step is judged by
// (toldNow), and the launch queues no second message of the ask: the catch-up is the one telling.
// On `implementer:owner reviewer` at the reviewer's step, with the owner's claim in the session
// ses, the owner's claim is ready at a launch through the workflow, as the daemon applies the fact
// (claimReady, QueueCatchUp). Its one catch-up names the ask, in the words of their one builder
// (untoldAskWords), where the ask was told nobody, was told in an older session, or was told with
// no session at launch 3 and the ready is of launch 4. It does not where the ask was told in ses,
// or with no session at launch 3 and the ready is of launch 3, or where the ask's own row is still
// queued, whose delivery tells it (record.Store.MergeForwardAskPending). The merge-forward
// messages the outbox holds after the ready are the ones it held before: none, or that queued ask
// alone. The control: the ask of a fresh implementer, whose message no catch-up of the owner
// names. A catch-up that names no ask, as QueueCatchUp's did before it asked untoldMergeForward,
// fails the three cases that name it.
func TestTheOwnersCatchUpNamesAnOpenAskOnlyWhereItsClaimWasNotToldIt(t *testing.T) {
	owner := claim.Token(treeRoleToken(t, "LEGION-208", claim.RoleArchitect))
	implementer := claim.Token(treeRoleToken(t, "LEGION-208", claim.RoleImplementer))
	reviewer := claim.Token(treeRoleToken(t, "LEGION-208", claim.RoleReviewer))
	for _, tc := range []struct {
		name   string
		agent  claim.Token
		told   record.OwnerTold
		queued bool
		launch int
		names  bool
	}{
		{name: "an ask told nobody", agent: owner, launch: 3, names: true},
		{name: "an ask told in the session the claim runs in", agent: owner, told: record.OwnerTold{Launch: 3, Session: "ses"}, launch: 3},
		{name: "an ask told in an older session", agent: owner, told: record.OwnerTold{Launch: 2, Session: "ses_old"}, launch: 3, names: true},
		{name: "an ask told at this launch, no session recorded", agent: owner, told: record.OwnerTold{Launch: 3}, launch: 3},
		{name: "an ask told at an earlier launch, no session recorded", agent: owner, told: record.OwnerTold{Launch: 3}, launch: 4, names: true},
		{name: "an ask whose row is still queued", agent: owner, queued: true, launch: 3},
		{name: "control: the ask of a fresh implementer", agent: implementer, launch: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			words := []string{"implementer:owner", "reviewer"}
			if tc.agent == implementer {
				words[0] = "implementer"
			}
			pool := standsAtStep(t, phase.Reviewing, 2, words, []claim.Token{tc.agent, reviewer}, tc.agent)
			seedIdleOwner(t, pool, "LEGION-208", "LEGION-208", 3)
			pr := recordedPR(t, pool, "LEGION-208")
			pr.MergeForward.Told = tc.told
			seedPR(t, pool, pr)
			engine := returnEngine()
			if tc.queued {
				seedRecord(t, pool, func(tx pgx.Tx) error {
					return engine.enqueue(t.Context(), tx, "LEGION-208", pr.MergeForward.Ask(pr.Number, ""))
				})
			}
			before := mergeForwardMessages(t, pool)
			if tc.queued && len(before) != 1 {
				t.Fatalf("the outbox holds the merge-forward messages %+v, want the one ask queued: this case proves nothing", before)
			}
			claimReady(t, pool, engine, "LEGION-208", claim.RoleArchitect, tc.launch)
			reasons := []string{}
			for _, payload := range noticeRows(t, pool) {
				if notice, ok := payload.(record.Notice); ok && notice.Kind == "catch-up" {
					reasons = append(reasons, notice.Reason)
				}
			}
			asked := untoldAskWords(issueOf(t, pool, engine, "LEGION-208"), pr)
			if len(reasons) != 1 || strings.Contains(reasons[0], asked) != tc.names {
				t.Fatalf("the ready of launch %d queued the catch-ups %q, want one that names the open ask of #42 at head-42 (%q): %t",
					tc.launch, reasons, asked, tc.names)
			}
			if after := mergeForwardMessages(t, pool); !reflect.DeepEqual(after, before) {
				t.Fatalf("after the ready the outbox holds the merge-forward messages %+v, want %+v as before it: the catch-up is the one telling", after, before)
			}
		})
	}
}

// A merge forward's record says in one way that its ask reached nobody: the zero OwnerTold
// (record.MergeForward.Told), which the jsonb leaves out and toldNow reads as told to nobody. On
// `implementer:owner reviewer` at the reviewer's step, with the owner's claim in the session ses,
// the record is read back through the store (record.Store.PullRequest) as each writer left it:
//   - the ask as this release writes it before any delivery (record.Store.PutPullRequest): the
//     stored jsonb holds no told key, and the record reads back with the zero mark;
//   - the ask as the release before wrote it, with no agent, no role and no told key: it reads back
//     with the zero mark too.
//
// toldNow reads each zero mark as told to nobody at launch 3. The control is the mark a delivery
// to the session ses writes (record.Store.MarkMergeForwardTold): the stored jsonb holds it under
// told, the record reads back with it, and toldNow reads it as told to the claim in ses. A writer
// that stored the zero mark as a key of its own, as `omitempty` does on a struct, which encodes it
// as {}, fails the first case.
func TestAMergeForwardsRecordHoldsOneEncodingOfAnAskToldNobody(t *testing.T) {
	owner := claim.Token(treeRoleToken(t, "LEGION-208", claim.RoleArchitect))
	reviewer := claim.Token(treeRoleToken(t, "LEGION-208", claim.RoleReviewer))
	ses := record.OwnerTold{Launch: 3, Session: "ses"}
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, pool *pgxpool.Pool)
		want  record.OwnerTold
		told  bool
	}{
		{name: "an ask this release wrote before any delivery", write: func(*testing.T, *pgxpool.Pool) {}},
		{name: "an ask the release before wrote", write: func(t *testing.T, pool *pgxpool.Pool) {
			if _, err := pool.Exec(t.Context(), "update pull_requests set merge_forward = $2::jsonb where issue = $1", "LEGION-208",
				`{"head":"head-42","base":"main","phase":"reviewing","why":"required-check-not-green","askedAt":"2026-11-01T00:00:00Z"}`); err != nil {
				t.Fatalf("write the ask as the release before wrote it: %v", err)
			}
		}},
		{name: "control: the mark a delivery to the session ses wrote", write: func(t *testing.T, pool *pgxpool.Pool) {
			seedRecord(t, pool, func(tx pgx.Tx) error {
				return record.NewStore().MarkMergeForwardTold(t.Context(), tx, "LEGION-208", "head-42", owner, ses)
			})
		}, want: ses, told: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := standsAtStep(t, phase.Reviewing, 2, []string{"implementer:owner", "reviewer"}, []claim.Token{owner, reviewer}, owner)
			seedIdleOwner(t, pool, "LEGION-208", "LEGION-208", 3)
			tc.write(t, pool)
			var stored []byte
			if err := pool.QueryRow(t.Context(), "select merge_forward->'told' from pull_requests where issue = $1", "LEGION-208").Scan(&stored); err != nil {
				t.Fatalf("read the stored told key of LEGION-208's merge forward: %v", err)
			}
			if keyed := tc.want != (record.OwnerTold{}); (stored != nil) != keyed {
				t.Fatalf("the stored merge forward holds the told key %s, want a told key: %t", stored, keyed)
			}
			asked := recordedPR(t, pool, "LEGION-208").MergeForward
			if asked == nil || asked.Head != "head-42" || asked.Told != tc.want {
				t.Fatalf("the merge forward reads back as %+v, want the ask of head-42 told %+v", asked, tc.want)
			}
			issue := issueOf(t, pool, returnEngine(), "LEGION-208")
			var told bool
			seedRecord(t, pool, func(tx pgx.Tx) (err error) {
				told, err = toldNow(t.Context(), tx, record.NewStore(), issue, asked.Told, 3)
				return err
			})
			if told != tc.told {
				t.Fatalf("toldNow reads the mark %+v at launch 3 as told: %t, want %t", asked.Told, told, tc.told)
			}
		})
	}
}
