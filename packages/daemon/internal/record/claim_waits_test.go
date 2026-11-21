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

// The reconciler's one read (StaleWaits) keeps a wait with no end only once its record has moved
// under it, and each column a term of the rule reads keeps it alone: the issue's phase, its
// generation, its status out of the workflow with its phase unmoved, its root's linger, the phases
// row's entry, a completion on that row, and the claim's session. A wait whose record stands as it
// was declared is not read, nor one whose role has no phases row and that was declared on entry 0,
// nor an ended wait, nor another project's. Each wait read comes, by token, with what the record
// holds now of its issue, its root, its role's row and its claim's session.
func TestStaleWaitsReadsOnlyTheWaitsWhoseRecordMovedUnderThem(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	root := func(key string) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) {
			must(t, records.PutIssue(ctx, tx, Issue{Key: key, Tree: key, Project: "LEGION", Title: key, Phase: phase.Planning,
				Generation: 3, Status: "in_progress", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main",
				GitHubRepo: "acme/widgets"}))
		})
	}
	// declare records key implementing in tree at generation 3, the claim of its implementer in
	// session ses-<key>, the role's phases row on entry 4 (none, and the wait on entry 0, for
	// noRow), and the claim's wait on all of it.
	declare := func(project, key, tree string, noRow bool) ClaimWait {
		t.Helper()
		token, err := claim.NewToken(project, key, claim.RoleImplementer)
		must(t, err)
		wait := ClaimWait{ID: "wait-" + key, Project: project, Token: token, Issue: key, Role: claim.RoleImplementer,
			Generation: 3, Phase: phase.Implementing, PhaseEntry: 4, TakenEntry: 4, Session: "ses-" + key,
			WaitingOn: "the run on the branch's head", WakeAt: waitDeclaredAt.Add(2 * time.Hour), DeclaredAt: waitDeclaredAt}
		if noRow {
			wait.PhaseEntry = 0
		}
		must(t, st.PutClaim(ctx, supervise.Claim{Token: token, Project: project, Tree: tree, Issue: key, Role: claim.RoleImplementer,
			Generation: 1, State: supervise.StateIdle, Session: wait.Session, SessionFile: "/tmp/" + key + ".jsonl"}))
		inTx(t, st, func(tx pgx.Tx) {
			must(t, records.PutIssue(ctx, tx, Issue{Key: key, Tree: tree, Project: "LEGION", Title: key, Parent: &tree,
				Phase: phase.Implementing, Generation: 3, Status: "in_progress", Rank: "U", HandedOver: true,
				Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}))
			if !noRow {
				must(t, records.PutPhase(ctx, tx, PhaseRow{Issue: key, Role: claim.RoleImplementer, Claim: token, Entry: 4}))
			}
			_, err := records.DeclareWait(ctx, tx, wait)
			must(t, err)
		})
		return wait
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.Pool().Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	root("LEGION-300")
	root("LEGION-310")
	root("OTHER-1")
	completedAt, lingerUntil := waitDeclaredAt.Add(10*time.Minute), waitDeclaredAt.Add(time.Hour)

	declare("legion", "LEGION-301", "LEGION-300", false)
	declare("legion", "LEGION-309", "LEGION-300", true)
	moved := []ClaimWait{
		declare("legion", "LEGION-302", "LEGION-300", false),
		declare("legion", "LEGION-303", "LEGION-300", false),
		declare("legion", "LEGION-304", "LEGION-300", false),
		declare("legion", "LEGION-305", "LEGION-300", false),
		declare("legion", "LEGION-306", "LEGION-300", false),
		declare("legion", "LEGION-307", "LEGION-300", false),
		declare("legion", "LEGION-308", "LEGION-310", false),
	}
	ended := declare("legion", "LEGION-311", "LEGION-300", false)
	inTx(t, st, func(tx pgx.Tx) {
		_, err := records.EndWait(ctx, tx, ended.Project, ended.Token, ended.ID, "completed", "api/handoff-1", completedAt)
		must(t, err)
	})
	declare("other", "OTHER-2", "OTHER-1", false)
	exec(`update issues set phase = 'testing' where key in ('LEGION-302', 'LEGION-311', 'OTHER-2')`)
	exec(`update issues set generation = 4 where key = 'LEGION-303'`)
	exec(`update issues set status = 'backlog' where key = 'LEGION-304'`)
	exec(`update phases set entry = 5 where issue = 'LEGION-305'`)
	exec(`update phases set completed_at = $1 where issue = 'LEGION-306'`, completedAt)
	exec(`update claims set session = 'ses-fresh' where issue = 'LEGION-307'`)
	exec(`update issues set linger_until = $1 where key = 'LEGION-310'`, lingerUntil)

	var readings []WaitReading
	inTx(t, st, func(tx pgx.Tx) {
		var err error
		readings, err = records.StaleWaits(ctx, tx, "legion")
		must(t, err)
	})
	if len(readings) != len(moved) {
		t.Fatalf("the stale waits = %+v, want the %d whose record moved", readings, len(moved))
	}
	for i, reading := range readings {
		wait := moved[i]
		if reading.Claim.Wait == nil || !sameWait(*reading.Claim.Wait, wait) {
			t.Fatalf("stale wait %d = %+v, want %+v", i, reading.Claim.Wait, wait)
		}
		want := WaitReading{
			Issue: Issue{Key: wait.Issue, Tree: "LEGION-300", Phase: phase.Implementing, Generation: 3, Status: "in_progress"},
			Root:  Issue{Key: "LEGION-300", Tree: "LEGION-300"},
			Row:   PhaseRow{Issue: wait.Issue, Role: claim.RoleImplementer, Entry: 4},
			Claim: RoleClaim{Session: wait.Session},
		}
		switch wait.Issue {
		case "LEGION-302":
			want.Issue.Phase = phase.Testing
		case "LEGION-303":
			want.Issue.Generation = 4
		case "LEGION-304":
			want.Issue.Status = "backlog"
		case "LEGION-305":
			want.Row.Entry = 5
		case "LEGION-306":
			want.Row.CompletedAt = completedAt
		case "LEGION-307":
			want.Claim.Session = "ses-fresh"
		case "LEGION-308":
			want.Issue.Tree, want.Root = "LEGION-310", Issue{Key: "LEGION-310", Tree: "LEGION-310", LingerUntil: &lingerUntil}
		}
		got := reading
		if got.Issue.Key != want.Issue.Key || got.Issue.Tree != want.Issue.Tree || got.Issue.Phase != want.Issue.Phase ||
			got.Issue.Generation != want.Issue.Generation || got.Issue.Status != want.Issue.Status || got.Issue.LingerUntil != nil {
			t.Errorf("the issue of %s reads %+v, want %+v", wait.ID, got.Issue, want.Issue)
		}
		if got.Root.Key != want.Root.Key || got.Root.Tree != want.Root.Tree || (got.Root.LingerUntil == nil) != (want.Root.LingerUntil == nil) ||
			(got.Root.LingerUntil != nil && !got.Root.LingerUntil.Equal(*want.Root.LingerUntil)) {
			t.Errorf("the root of %s reads %+v, want %+v", wait.ID, got.Root, want.Root)
		}
		if got.Row.Issue != want.Row.Issue || got.Row.Role != want.Row.Role || got.Row.Entry != want.Row.Entry ||
			!got.Row.CompletedAt.Equal(want.Row.CompletedAt) {
			t.Errorf("the phases row of %s reads %+v, want %+v", wait.ID, got.Row, want.Row)
		}
		if got.Claim.Session != want.Claim.Session {
			t.Errorf("the claim of %s runs session %q, want %q", wait.ID, got.Claim.Session, want.Claim.Session)
		}
		if run := reading.Run(); run.Claim == nil || run.Claim.Wait != reading.Claim.Wait {
			t.Errorf("the run of %s = %+v, want the reading's claim", wait.ID, run)
		}
	}
}
