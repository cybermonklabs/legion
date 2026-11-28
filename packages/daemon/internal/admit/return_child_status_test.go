package admit

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The tester's red test of INFRA-4730 at 5e4d775a. R18 of dispatch://INFRA-3320: "A notice about
// an issue goes to its own architect while that claim holds its role, else to the nearest
// architect above it in Legion's work"; the architect's rulings
// dispatch://INFRA-4730/comment/96a9a2fa-1e0d-4724-9ebf-ddf76d564b06 item 1 ("the return tells the
// architect above the issue that it is back in work, with a child-status notice whose owner walk
// starts at the parent") and dispatch://INFRA-4730/comment/6d77e1c1-133b-4f9f-a168-46dd165158a8
// (on an unknown chain the notice is queued and waits until the chain is known, not dropped).

// rcsRouted is one notice row of the outbox with the architect the daemon would publish it to as
// the copy of Dispatch and the claims stand when it is read (the daemon's noticeArchitect): its
// own issue's for a catch-up, the one it names for a notice with To, else the owner walk
// (record.OwningArchitect) from the issue or, for a kind the architect above must hear
// (record.NoticeStartsAtParent), from its parent, by the claims that hold their role
// (supervise.HoldsRole over the claims table). A walk that meets an unknown chain waits.
type rcsRouted struct {
	id                     int64
	outboxKind, issue      string
	kind, role, to, reason string
	recipient              string
}

func (n rcsRouted) String() string {
	reason := n.reason
	if len(reason) > 200 {
		reason = reason[:200]
	}
	return fmt.Sprintf("row %d %s of %s: kind %s, role %q, to %q -> %s: %q", n.id, n.outboxKind, n.issue, n.kind, n.role, n.to, n.recipient, reason)
}

func rcsRoutedNotices(t *testing.T, pool *pgxpool.Pool) []rcsRouted {
	t.Helper()
	ctx := context.Background()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token of %s: %v", testProject, err)
	}
	holds := map[claim.Token]bool{}
	claimRows, err := pool.Query(ctx, `select token, state, coalesce(suspended_by, '') from claims`)
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	for claimRows.Next() {
		var token, state, by string
		if err := claimRows.Scan(&token, &state, &by); err != nil {
			t.Fatalf("scan a claim: %v", err)
		}
		holds[claim.Token(token)] = supervise.HoldsRole(supervise.ClaimState(state), supervise.SuspendSource(by))
	}
	claimRows.Close()
	if err := claimRows.Err(); err != nil {
		t.Fatalf("iterate the claims: %v", err)
	}
	rows, err := pool.Query(ctx, `select id, kind, issue, payload from outbox where kind in ('notice', 'controller_notice') order by id`)
	if err != nil {
		t.Fatalf("read the notices: %v", err)
	}
	var raw []record.OutboxRow
	for rows.Next() {
		var row record.OutboxRow
		var kind string
		if err := rows.Scan(&row.ID, &kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("scan a notice: %v", err)
		}
		row.Kind = record.OutboxKind(kind)
		raw = append(raw, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the notices: %v", err)
	}
	var routed []rcsRouted
	inTx(t, pool, func(tx pgx.Tx) {
		for _, row := range raw {
			n := rcsRouted{id: row.ID, outboxKind: string(row.Kind), issue: row.Issue}
			payload, err := record.DecodeOutboxPayload(row)
			if err != nil {
				t.Fatalf("decode notice row %d: %v", row.ID, err)
			}
			var notice record.Notice
			switch value := payload.(type) {
			case record.Notice:
				notice = value
			case record.ControllerNotice:
				notice = record.Notice(value)
				n.recipient = "the project's controller"
			default:
				t.Fatalf("notice row %d decodes to %T", row.ID, payload)
			}
			n.kind, n.role, n.to, n.reason = string(notice.Kind), string(notice.Role), notice.To, notice.Reason
			if n.recipient == "" {
				switch {
				case notice.CatchUp != nil:
					n.recipient = "architect of " + row.Issue
				case notice.To != "":
					n.recipient = "architect of " + notice.To
				default:
					chains, err := record.ReadChainsOf(ctx, tx, record.NewStore(), testProject, []string{row.Issue})
					if err != nil {
						t.Fatalf("read the chain of %s: %v", row.Issue, err)
					}
					owner := record.OwningIssue(project, chains, row.Issue, record.NoticeStartsAtParent(notice.Kind, notice.Role),
						func(token claim.Token) bool { return holds[token] })
					if owner == "" {
						n.recipient = "waits: the chain of " + row.Issue + " is unknown"
					} else {
						n.recipient = "architect of " + owner
					}
				}
			}
			routed = append(routed, n)
		}
	})
	return routed
}

// rcsLogRouted logs every notice row and its recipient, under when.
func rcsLogRouted(t *testing.T, pool *pgxpool.Pool, when string) []rcsRouted {
	t.Helper()
	routed := rcsRoutedNotices(t, pool)
	t.Logf("%s: %d notice rows", when, len(routed))
	for _, n := range routed {
		t.Logf("%s: %s", when, n)
	}
	return routed
}

// Attack R4-1's three cases: a person moves LEGION-500, at work under LEGION-208, under LEGION-700,
// which the copy lacks (seed S1); writes backlog; while stopped moves it under LEGION-300 and back
// under LEGION-700; and writes todo, so it returns under LEGION-700 (its chain unknown). The seeds
// are read only after the return, also while stopped under LEGION-700, or also while stopped under
// LEGION-300 (the failing case of R4-1). Admission passes run; then R16's read of the gap lands
// (LEGION-700 on the backlog under LEGION-600, a root at work whose architect works), the seeds
// are read again and admission passes run again. Every notice row is logged with the architect the
// daemon would publish it to. The child-status notice of LEGION-500's return ("LEGION-500 is todo
// again; it resumes implementing at a new phase entry", workflow's ReturnToWork) reaches the
// architect of LEGION-600, above it now, once.
func TestTheReturnsChildStatusReachesTheArchitectAboveOnceTheChainIsKnown(t *testing.T) {
	for _, readAt := range []string{"only after the return", "also while stopped under the seed's parent", "also while stopped under LEGION-300"} {
		t.Run("seeds read "+readAt, func(t *testing.T) {
			t.Parallel()
			pool, admission := rstFixture(t)
			rcsRoot(t, pool, rcsAboveGap)
			rstMove(t, pool, admission, "in_progress", rstUnread, 2)
			if seeds := rstSeeds(t, pool); len(seeds) != 1 || seeds[0].seed.Parent != rstUnread {
				t.Fatalf("the re-parent seeds of %s = %+v, want one, to %s: the case's premise", rstChild, seeds, rstUnread)
			}
			rstMove(t, pool, admission, "backlog", rstUnread, 3)
			var last int64
			if readAt == "also while stopped under the seed's parent" {
				last = rstReadSeeds(t, pool, admission, last)
			}
			rstMove(t, pool, admission, "backlog", rstNew, 4)
			if readAt == "also while stopped under LEGION-300" {
				last = rstReadSeeds(t, pool, admission, last)
			}
			rstMove(t, pool, admission, "backlog", rstUnread, 5)
			rstMove(t, pool, admission, "todo", rstUnread, 6)
			back := issue(t, pool, rstChild)
			if !record.InLegionsWork(back) || back.Parent == nil || *back.Parent != rstUnread || back.Generation != 1 {
				t.Fatalf("%s after the todo = %s/%s generation %d under %v, want it back in Legion's work under %s at generation 1: the case's premise",
					rstChild, back.Phase, back.Status, back.Generation, back.Parent, rstUnread)
			}
			last = rstReadSeeds(t, pool, admission, last)
			for i := range 2 {
				tick(t, pool, admission, fmt.Sprintf("rcs-unknown-%d", i))
			}
			rcsLogRouted(t, pool, "before the chain is known")
			spMirror(t, pool, rstUnread, "backlog", rcsAboveGap, 9)
			last = rstReadSeeds(t, pool, admission, last)
			for i := range 3 {
				tick(t, pool, admission, fmt.Sprintf("rcs-known-%d", i))
			}
			rstReadSeeds(t, pool, admission, last)
			routed := rcsLogRouted(t, pool, "once the chain is known")
			got := issue(t, pool, rstChild)
			t.Logf("%s once the chain is known: phase %s status %s generation %d told_parent %q; holds a slot %t", rstChild, got.Phase, got.Status, got.Generation, got.ToldParent, holdsSlot(t, pool, rstChild))

			byRecipient := map[string][]string{}
			for _, n := range routed {
				byRecipient[n.recipient] = append(byRecipient[n.recipient], n.kind)
			}
			for recipient, kinds := range byRecipient {
				t.Logf("recipient %s: kinds %v", recipient, kinds)
			}
			above := "architect of " + rcsAboveGap
			returns, toAbove := 0, 0
			for _, n := range routed {
				if n.issue != rstChild || n.kind != "child-status" || !strings.Contains(n.reason, rstChild+" is todo again") {
					continue
				}
				returns++
				if n.recipient == above {
					toAbove++
				}
			}
			now := rcsCount(rcsTold(t, pool), rcsAboveGap, rcsNowWords(rcsAboveGap))
			t.Logf("summary: return child-status rows %d, of which routed to %s %d; reparented notices to %s naming it above now %d",
				returns, above, toAbove, rcsAboveGap, now)
			if returns != 1 || toAbove != 1 {
				t.Errorf("the child-status notice of %s's return: %d rows, %d of them routed to the %s once the chain is known; want one, to it (R18; rulings 96a9a2fa item 1 and 6d77e1c1)",
					rstChild, returns, toAbove, above)
			}
		})
	}
}

// rcsAboveGap is a root at work with its architect working; R16's read of the gap puts LEGION-700
// under it.
const rcsAboveGap = "LEGION-600"

// rcsRoot records key as a root at work in planning with its architect working.
func rcsRoot(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	seedStarted(t, pool, key, phase.Planning, nil)
	putWorkerClaim(t, pool, key, key, claim.RoleArchitect, 1, supervise.StateWorking, "")
}

type rcsNotice struct{ to, reason string }

// rcsTold is every reparented notice of LEGION-500 the outbox holds, oldest first.
func rcsTold(t *testing.T, pool *pgxpool.Pool) []rcsNotice {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select coalesce(payload->>'to', ''), coalesce(payload->>'reason', '') from outbox
		where issue = $1 and kind = 'notice' and payload->>'kind' = 'reparented' order by id`, rstChild)
	if err != nil {
		t.Fatalf("read the reparented notices: %v", err)
	}
	defer rows.Close()
	var told []rcsNotice
	for rows.Next() {
		var n rcsNotice
		if err := rows.Scan(&n.to, &n.reason); err != nil {
			t.Fatalf("scan a reparented notice: %v", err)
		}
		told = append(told, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the reparented notices: %v", err)
	}
	return told
}

// rcsNowWords is the sentence of a re-parent notice that names key's architect as the architect
// above the moved issue now (workflow's reparentedWords).
func rcsNowWords(key string) string { return "The architect of " + key + " now stands above it" }

// rcsCount is the number of notices to to whose words hold all of parts.
func rcsCount(told []rcsNotice, to string, parts ...string) int {
	n := 0
	for _, notice := range told {
		if notice.to != to {
			continue
		}
		all := true
		for _, part := range parts {
			if !strings.Contains(notice.reason, part) {
				all = false
			}
		}
		if all {
			n++
		}
	}
	return n
}
