package admit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// R18 of dispatch://INFRA-3320: "A notice about an issue goes to its own architect while that claim
// holds its role, else to the nearest architect above it in Legion's work"; the architect's ruling
// dispatch://INFRA-4730/comment/6d77e1c1-133b-4f9f-a168-46dd165158a8: an issue that leaves Legion's
// work on a chain the copy of Dispatch cannot read to its top has its child-status or child-closed
// queued, not dropped, and the notice waits until the chain is known, then reaches the architect
// above the issue, or, where the known chain shows nothing above the issue, ends undelivered
// (record.ErrNoArchitectAbove) rather than reach the issue's own architect.

// lcsNotice is one child-status or child-closed notice of LEGION-500 the outbox holds, with where
// the daemon's delivery would send it as the copy and the claims stand when it is read (lcsRoute).
type lcsNotice struct {
	kind, reason, routed string
}

// lcsNotices is every child-status and child-closed notice of LEGION-500 the outbox holds, oldest
// first, each with its route.
func lcsNotices(t *testing.T, pool *pgxpool.Pool) []lcsNotice {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload->>'kind', coalesce(payload->>'reason', '') from outbox
		where issue = $1 and kind = 'notice' and payload->>'kind' in ('child-status', 'child-closed') order by id`, rstChild)
	if err != nil {
		t.Fatalf("read the child notices of %s: %v", rstChild, err)
	}
	var notices []lcsNotice
	for rows.Next() {
		var n lcsNotice
		if err := rows.Scan(&n.kind, &n.reason); err != nil {
			t.Fatalf("scan a child notice of %s: %v", rstChild, err)
		}
		notices = append(notices, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the child notices of %s: %v", rstChild, err)
	}
	for i := range notices {
		notices[i].routed = lcsRoute(t, pool, rstChild, record.NoticeKind(notices[i].kind))
	}
	return notices
}

// lcsRoute is where the daemon's delivery sends a notice of kind about key's architect as the copy
// of Dispatch and the claims table stand (record.NoticeArchitect, which the daemon's
// noticeArchitect asks for every notice but a catch-up and a re-parent notice, over the claims that
// hold their role, supervise.HoldsRole): "architect of <KEY>"; "waits" while the owner walk meets a
// chain the copy cannot read to its top (the delivery's errNoticeWaits); or "no architect" for a
// notice it routes to nobody (record.ErrNoArchitectAbove), which the delivery ends undelivered.
func lcsRoute(t *testing.T, pool *pgxpool.Pool, key string, kind record.NoticeKind) string {
	t.Helper()
	ctx := context.Background()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token of %s: %v", testProject, err)
	}
	var routed string
	inTx(t, pool, func(tx pgx.Tx) {
		store := record.NewStore()
		chains, err := record.ReadChainsOf(ctx, tx, store, testProject, []string{key})
		if err != nil {
			t.Fatalf("read the chain of %s: %v", key, err)
		}
		runs, err := record.ArchitectRuns(ctx, store, tx, project, append([]string{key}, chains.Chain(key).Links...))
		if err != nil {
			t.Fatalf("read the architect claims on the chain of %s: %v", key, err)
		}
		architect, err := record.NoticeArchitect(project, chains, key, kind, claim.RoleArchitect, runs)
		switch {
		case errors.Is(err, record.ErrChainUnknown):
			routed = "waits"
			return
		case errors.Is(err, record.ErrNoArchitectAbove):
			routed = "no architect"
			return
		case err != nil:
			t.Fatalf("route the %s notice of %s: %v", kind, key, err)
		}
		routed = string(architect)
		for _, issue := range []string{rstChild, rstUnread, rcsAboveGap, rstOld, rstNew} {
			if token, err := claim.NewToken(project, issue, claim.RoleArchitect); err == nil && token == architect {
				routed = "architect of " + issue
			}
		}
	})
	return routed
}

// lcsLeft is the notices of LEGION-500's leave at status among notices: those whose reason starts
// as workflow's leftNotice starts it, "LEGION-500 is <status>".
func lcsLeft(notices []lcsNotice, status string) []lcsNotice {
	var left []lcsNotice
	for _, n := range notices {
		if n.reason == rstChild+" is "+status || strings.HasPrefix(n.reason, rstChild+" is "+status+".") {
			left = append(left, n)
		}
	}
	return left
}

// A person moves LEGION-500, at work under LEGION-208, under LEGION-700, which the copy lacks, and
// then out of Legion's work, to the backlog or done, so it leaves on a chain the copy cannot read
// to its top. Its child-status (backlog) or child-closed (done) is queued, and waits while admission
// passes run on the unknown chain. Then R16's read of the gap lands: LEGION-700 on the backlog
// under LEGION-600, a root at work whose architect works, or under no parent at all, and admission
// passes run again. Under LEGION-600 the notice reaches that architect. With nothing above
// LEGION-500 it goes to no architect, so its delivery ends it undelivered, never at LEGION-500's
// own architect: not while that architect's claim still holds its role in the claims table, and
// not after a todo brings LEGION-500 back into Legion's work, which tells nobody above it, since
// nobody is.
func TestALeavesNoticeOnAnUnknownChainReachesTheArchitectAboveOnceTheChainIsKnown(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		kind         record.NoticeKind
		// above is the issue LEGION-700 stands under once the chain is known, "" for none.
		above, want string
	}{
		{name: "a backlog under the architect of LEGION-600", status: "backlog", kind: "child-status", above: rcsAboveGap, want: "architect of " + rcsAboveGap},
		{name: "a done under the architect of LEGION-600", status: "done", kind: "child-closed", above: rcsAboveGap, want: "architect of " + rcsAboveGap},
		{name: "a backlog with nothing above it", status: "backlog", kind: "child-status", want: "no architect"},
		{name: "a done with nothing above it", status: "done", kind: "child-closed", want: "no architect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool, admission := rstFixture(t)
			if tc.above != "" {
				rcsRoot(t, pool, tc.above)
			}
			rstMove(t, pool, admission, "in_progress", rstUnread, 2)
			rstMove(t, pool, admission, tc.status, rstUnread, 3)
			if left := issue(t, pool, rstChild); record.InLegionsWork(left) || left.Parent == nil || *left.Parent != rstUnread {
				t.Fatalf("%s after the %s = %s/%s under %v, want it out of Legion's work under %s: the case's premise",
					rstChild, tc.status, left.Phase, left.Status, left.Parent, rstUnread)
			}
			for i := range 2 {
				tick(t, pool, admission, fmt.Sprintf("lcs-unknown-%d", i))
			}
			unknown := lcsNotices(t, pool)
			t.Logf("while the chain is unknown: %+v", unknown)
			if left := lcsLeft(unknown, tc.status); len(left) != 1 || left[0].kind != string(tc.kind) || left[0].routed != "waits" {
				t.Fatalf("the notices of %s's %s while its chain is unknown = %+v; want one %s, queued and waiting for the chain (ruling 6d77e1c1)",
					rstChild, tc.status, left, tc.kind)
			}

			spMirror(t, pool, rstUnread, "backlog", tc.above, 9)
			for i := range 3 {
				tick(t, pool, admission, fmt.Sprintf("lcs-known-%d", i))
			}
			known := lcsNotices(t, pool)
			t.Logf("once the chain is known: %+v", known)
			if left := lcsLeft(known, tc.status); len(left) != 1 || left[0].routed != tc.want {
				t.Fatalf("the notices of %s's %s once its chain is known = %+v; want one, routed to %s (R18; ruling 6d77e1c1)",
					rstChild, tc.status, left, tc.want)
			}
			if tc.above != "" {
				return
			}

			rstMove(t, pool, admission, "todo", rstUnread, 4)
			if back := issue(t, pool, rstChild); !record.InLegionsWork(back) {
				t.Fatalf("%s after the todo = %s/%s, want it back in Legion's work: the case's premise", rstChild, back.Phase, back.Status)
			}
			for i := range 2 {
				tick(t, pool, admission, fmt.Sprintf("lcs-back-%d", i))
			}
			back := lcsNotices(t, pool)
			t.Logf("after the return: %+v", back)
			if len(back) != 1 || len(lcsLeft(back, tc.status)) != 1 || back[0].routed != "no architect" {
				t.Fatalf("the child notices of %s after its return = %+v; want its %s's alone, still routed to no architect, never to its own",
					rstChild, back, tc.status)
			}
		})
	}
}
