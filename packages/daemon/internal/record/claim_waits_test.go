package record

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// waitDeclaredAt is a moment Postgres stores exactly (timestamptz keeps microseconds), the
// declaration time of the first wait below; each later one is declared a minute after the last.
var waitDeclaredAt = time.Date(2026, 11, 14, 4, 0, 0, 123456000, time.UTC)

// implementerWait is a wait the implementer of LEGION-209 declares at minute, with id id.
func implementerWait(id string, minute int) ClaimWait {
	declared := waitDeclaredAt.Add(time.Duration(minute) * time.Minute)
	return ClaimWait{ID: id, Project: "legion", Token: "legion-legion-legion-209-implementer", Issue: "LEGION-209",
		Role: claim.RoleImplementer, Generation: 3, Phase: phase.Implementing, PhaseEntry: 4, TakenEntry: 4,
		Session: "ses_implementer", WaitingOn: "the review of the root's pull request", WakeAt: declared.Add(2 * time.Hour),
		DeclaredAt: declared}
}

// sameWait says whether two waits hold the same columns, each moment compared as a moment.
func sameWait(got, want ClaimWait) bool {
	return got.ID == want.ID && got.Project == want.Project && got.Token == want.Token && got.Issue == want.Issue &&
		got.Role == want.Role && got.Generation == want.Generation && got.Phase == want.Phase &&
		got.PhaseEntry == want.PhaseEntry && got.TakenEntry == want.TakenEntry && got.Session == want.Session &&
		got.WaitingOn == want.WaitingOn && got.WakeAt.Equal(want.WakeAt) && got.DeclaredAt.Equal(want.DeclaredAt) &&
		got.EndedAt.Equal(want.EndedAt) && got.EndCause == want.EndCause && got.EndedBy == want.EndedBy
}

// waitRow is the wait id as claim_waits holds it, ended or not, read by id.
func waitRow(t *testing.T, tx pgx.Tx, id string) ClaimWait {
	t.Helper()
	wait, err := scanClaimWait(tx.QueryRow(context.Background(), `select `+claimWaitColumns+` from claim_waits w where w.id = $1`, id))
	if err != nil {
		t.Fatalf("read wait %s: %v", id, err)
	}
	if wait == nil {
		t.Fatalf("claim_waits holds no wait %s", id)
	}
	return *wait
}

// A claim has one wait with no end at most. The partial unique index claim_waits_standing refuses
// a second such row of the same claim written beside the first, so a declaration ends the standing
// wait before it records its own (DeclareWait): the old row is kept, ended as replaced, at the
// new declaration's time, by the new wait's id, and the declaration answers it. Another claim's
// wait stands beside it, and the project's read lists both, by token, and no other project's.
func TestOneWaitStandsPerClaimAndADeclarationReplacesIt(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	first, second := implementerWait("wait-1", 0), implementerWait("wait-2", 1)

	inTx(t, st, func(tx pgx.Tx) {
		replaced, err := records.DeclareWait(ctx, tx, first)
		must(t, err)
		if replaced != nil {
			t.Fatalf("the first declaration replaced %+v, want nothing: the claim had no wait", replaced)
		}
	})

	_, err := st.Pool().Exec(ctx, `insert into claim_waits (id, project, claim_token, issue, role, generation, phase,
		phase_entry, taken_entry, session, waiting_on, wake_at, declared_at)
		values ('wait-beside', $1, $2, 'LEGION-209', 'implementer', 3, 'implementing', 4, 4, 'ses_implementer',
		'a second wait beside the first', now(), now())`, first.Project, string(first.Token))
	var refused *pgconn.PgError
	if !errors.As(err, &refused) || refused.Code != "23505" || refused.ConstraintName != "claim_waits_standing" {
		t.Fatalf("a second wait with no end of the claim, written beside the first = %v, want it refused by claim_waits_standing", err)
	}

	tester := ClaimWait{ID: "wait-tester", Project: "legion", Token: "legion-legion-legion-209-tester", Issue: "LEGION-209",
		Role: claim.RoleTester, Generation: 3, Phase: phase.Testing, PhaseEntry: 2, Session: "ses_tester",
		WaitingOn: "the proof run", WakeAt: waitDeclaredAt.Add(time.Hour), DeclaredAt: waitDeclaredAt}
	elsewhere := implementerWait("wait-elsewhere", 0)
	elsewhere.Project, elsewhere.Token = "infra", "legion-infra-infra-12-implementer"
	inTx(t, st, func(tx pgx.Tx) {
		replaced, err := records.DeclareWait(ctx, tx, second)
		must(t, err)
		ended := first
		ended.EndedAt, ended.EndCause, ended.EndedBy = second.DeclaredAt, WaitReplaced, second.ID
		if replaced == nil || !sameWait(*replaced, ended) {
			t.Fatalf("the second declaration replaced %+v, want the first wait ended as replaced by %s at its declaration: %+v",
				replaced, second.ID, ended)
		}
		if kept := waitRow(t, tx, first.ID); !sameWait(kept, ended) {
			t.Fatalf("the replaced wait reads %+v, want it kept, ended as replaced: %+v", kept, ended)
		}
		for _, other := range []ClaimWait{tester, elsewhere} {
			if replaced, err := records.DeclareWait(ctx, tx, other); err != nil || replaced != nil {
				t.Fatalf("declare %s of %s = %+v, %v, want it recorded beside the implementer's, replacing nothing", other.ID, other.Token, replaced, err)
			}
		}
	})

	inTx(t, st, func(tx pgx.Tx) {
		standing, err := records.StandingWait(ctx, tx, first.Token)
		must(t, err)
		if standing == nil || !sameWait(*standing, second) {
			t.Fatalf("the claim's standing wait = %+v, want the second declaration's %+v", standing, second)
		}
		waits, err := records.StandingWaits(ctx, tx, "legion")
		must(t, err)
		if len(waits) != 2 || !sameWait(waits[0], second) || !sameWait(waits[1], tester) {
			t.Fatalf("the project's standing waits = %+v, want the implementer's second wait and the tester's, by token", waits)
		}
		none, err := records.StandingWait(ctx, tx, "legion-legion-legion-209-reviewer")
		must(t, err)
		if none != nil {
			t.Fatalf("the standing wait of a claim that declared none = %+v, want none", none)
		}
	})
}

// An end is a compare-and-set on the wait's id and on the row having no end (EndWait). An end
// that names the wait a newer declaration replaced, or a wait already ended, changes nothing and
// answers false, so a report or a timer that names an old wait cannot end the claim's new one or
// rewrite how the old one ended; nor does an end that names the wait under another claim or
// another project. An end that names the standing wait ends it, with its cause, the fact that
// ended it and its moment, and the claim then has no wait, so the next declaration replaces
// nothing.
func TestEndWaitEndsOnlyTheStandingWaitItNames(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	first, second := implementerWait("wait-1", 0), implementerWait("wait-2", 1)
	inTx(t, st, func(tx pgx.Tx) {
		for _, wait := range []ClaimWait{first, second} {
			_, err := records.DeclareWait(ctx, tx, wait)
			must(t, err)
		}
	})
	replaced := first
	replaced.EndedAt, replaced.EndCause, replaced.EndedBy = second.DeclaredAt, WaitReplaced, second.ID
	endedAt := second.DeclaredAt.Add(time.Hour)

	inTx(t, st, func(tx pgx.Tx) {
		for _, end := range []struct {
			name    string
			project string
			token   claim.Token
			id      string
		}{
			{name: "the replaced wait", project: first.Project, token: first.Token, id: first.ID},
			{name: "the standing wait under another claim", project: second.Project, token: "legion-legion-legion-209-tester", id: second.ID},
			{name: "the standing wait under another project", project: "infra", token: second.Token, id: second.ID},
		} {
			ended, err := records.EndWait(ctx, tx, end.project, end.token, end.id, "news reached the session", "api:wait-news:"+end.id, endedAt)
			must(t, err)
			if ended {
				t.Fatalf("an end that names %s ended a wait, want it to change nothing", end.name)
			}
		}
		if kept := waitRow(t, tx, first.ID); !sameWait(kept, replaced) {
			t.Fatalf("the replaced wait reads %+v after an end named it, want it as its replacement ended it: %+v", kept, replaced)
		}
		if kept := waitRow(t, tx, second.ID); !sameWait(kept, second) {
			t.Fatalf("the standing wait reads %+v after ends that named it under another claim, want it as declared: %+v", kept, second)
		}

		ended, err := records.EndWait(ctx, tx, second.Project, second.Token, second.ID, "fresh session", "workflow:fact-7", endedAt)
		must(t, err)
		if !ended {
			t.Fatal("an end that names the standing wait ended nothing")
		}
	})
	done := second
	done.EndedAt, done.EndCause, done.EndedBy = endedAt, "fresh session", "workflow:fact-7"

	inTx(t, st, func(tx pgx.Tx) {
		again, err := records.EndWait(ctx, tx, second.Project, second.Token, second.ID, "its time came", "outbox:wait-due:wait-2", endedAt.Add(time.Hour))
		must(t, err)
		if again {
			t.Fatal("a second end of an ended wait answered true, want false")
		}
		if kept := waitRow(t, tx, second.ID); !sameWait(kept, done) {
			t.Fatalf("the ended wait reads %+v after a second end, want its first end kept: %+v", kept, done)
		}
		standing, err := records.StandingWait(ctx, tx, second.Token)
		must(t, err)
		if standing != nil {
			t.Fatalf("the claim's standing wait after its end = %+v, want none", standing)
		}
		next := implementerWait("wait-3", 90)
		replacedNow, err := records.DeclareWait(ctx, tx, next)
		must(t, err)
		if replacedNow != nil {
			t.Fatalf("a declaration after the claim's wait ended replaced %+v, want nothing", replacedNow)
		}
	})
}

// The claim read every rule over a run takes (readRoleClaims, through RoleClaims and RoleRuns)
// carries the session the claim runs and its wait row with no end, which is what the wait's rule
// weighs against each other (workflow.WaitStands): nil before a declaration, the declared wait
// after it, the newer one after a replacement, and nil again once the wait ends, while the
// session stays the claim's.
func TestTheClaimReadCarriesTheClaimsSessionAndItsWaitWithNoEnd(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	first, second := implementerWait("wait-1", 0), implementerWait("wait-2", 1)
	held := supervise.Claim{Token: first.Token, Project: first.Project, Tree: "LEGION-208", Issue: first.Issue, Role: first.Role,
		Generation: 2, State: supervise.StateIdle, Session: first.Session, SessionFile: "/tmp/implementer.jsonl"}
	if err := st.PutClaim(ctx, held); err != nil {
		t.Fatalf("record the implementer's claim: %v", err)
	}
	key := RoleRunKey{Token: first.Token, Issue: first.Issue, Role: first.Role, Generation: first.Generation}
	read := func(t *testing.T, want *ClaimWait, when string) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) {
			claims, err := records.RoleClaims(ctx, tx, []claim.Token{first.Token})
			must(t, err)
			runs, err := records.RoleRuns(ctx, tx, []RoleRunKey{key})
			must(t, err)
			for name, got := range map[string]*RoleClaim{"RoleClaims": claims[first.Token], "RoleRuns": runs[first.Token].Claim} {
				if got == nil {
					t.Fatalf("%s holds no claim of %s %s", name, first.Token, when)
				}
				if got.Session != held.Session {
					t.Errorf("%s reads the session %q %s, want the claim's %q", name, got.Session, when, held.Session)
				}
				switch {
				case want == nil && got.Wait != nil:
					t.Errorf("%s reads the wait %+v %s, want none", name, got.Wait, when)
				case want != nil && (got.Wait == nil || !sameWait(*got.Wait, *want)):
					t.Errorf("%s reads the wait %+v %s, want %+v", name, got.Wait, when, *want)
				}
			}
		})
	}

	read(t, nil, "before any declaration")
	inTx(t, st, func(tx pgx.Tx) {
		_, err := records.DeclareWait(ctx, tx, first)
		must(t, err)
	})
	read(t, &first, "after the declaration")
	inTx(t, st, func(tx pgx.Tx) {
		_, err := records.DeclareWait(ctx, tx, second)
		must(t, err)
	})
	read(t, &second, "after the replacement")
	inTx(t, st, func(tx pgx.Tx) {
		ended, err := records.EndWait(ctx, tx, second.Project, second.Token, second.ID, "completed", "workflow:fact-9", second.DeclaredAt.Add(time.Hour))
		must(t, err)
		if !ended {
			t.Fatal("the end of the standing wait ended nothing")
		}
	})
	read(t, nil, "after the wait ended")
}
