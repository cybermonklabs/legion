// carried_test.go is Service.Carried: which pod enrollments a daemon's own account launched still
// carry something adopt would hand to the next enrollment of the same session id, the question
// a Legion daemon asks before it frees a session id it holds (plan E's t-e-54).
package enroll

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// carriedWindow is the carry window of these tests' Services: neither 0 nor the lease, so an
// answer that read the wrong one would name the wrong end.
const carriedWindow = 12 * time.Hour

// carriedDaemons is twoDaemons with carriedWindow as its carry window.
func carriedDaemons(t *testing.T) (*Service, func(cred Credential, session, podUID string) Enrollment, Credential, Credential) {
	t.Helper()
	svc, issuer, key, acme, globex := twoDaemons(t)
	svc.Carry = carriedWindow
	enrol := func(cred Credential, session, podUID string) Enrollment {
		t.Helper()
		return enrolPod(t, svc, issuer, key, cred, session, podUID)
	}
	return svc, enrol, acme, globex
}

// askCarried is Carried, failing t on an error.
func askCarried(t *testing.T, svc *Service, cred Credential, sessions ...string) []CarriedEnrollment {
	t.Helper()
	got, err := svc.Carried(context.Background(), cred, sessions)
	if err != nil {
		t.Fatalf("Carried(%v): %v", sessions, err)
	}
	if got == nil {
		t.Fatalf("Carried(%v) answered nil, want a list, empty when nothing carries", sessions)
	}
	return got
}

// leaseEnd is enrollment id's lease_expires_at, the end a live row is answered with.
func leaseEnd(t *testing.T, svc *Service, id uuid.UUID) time.Time {
	t.Helper()
	var at time.Time
	if err := svc.Store.Pool.QueryRow(context.Background(), `select lease_expires_at from enrollments where id=$1`, id).Scan(&at); err != nil {
		t.Fatalf("read the lease of %s: %v", id, err)
	}
	return at
}

// carryWindowEnd is enrollment id's dormant_since plus carriedWindow, the end a dormant row is
// answered with.
func carryWindowEnd(t *testing.T, svc *Service, id uuid.UUID) time.Time {
	t.Helper()
	var since *time.Time
	if err := svc.Store.Pool.QueryRow(context.Background(), `select dormant_since from enrollments where id=$1`, id).Scan(&since); err != nil {
		t.Fatalf("read the dormancy of %s: %v", id, err)
	}
	if since == nil {
		t.Fatalf("enrollment %s is not dormant", id)
	}
	return since.Add(carriedWindow)
}

// describeCarried prints an answer as its rows, so a failure names what was answered.
func describeCarried(rows []CarriedEnrollment) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = fmt.Sprintf("{%s %s %s ends %s}", r.SessionID, r.EnrollmentID, r.State, r.EndsAt.UTC().Format(time.RFC3339Nano))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// wantCarried fails t unless got is exactly want, in order, each end the same instant.
func wantCarried(t *testing.T, what string, got []CarriedEnrollment, want ...CarriedEnrollment) {
	t.Helper()
	same := len(got) == len(want)
	for i := 0; same && i < len(got); i++ {
		g, w := got[i], want[i]
		same = g.SessionID == w.SessionID && g.EnrollmentID == w.EnrollmentID && g.State == w.State && g.EndsAt.Equal(w.EndsAt)
	}
	if !same {
		t.Fatalf("%s: Carried answered %s, want %s", what, describeCarried(got), describeCarried(want))
	}
}

// TestCarriedAnswersADaemonsOwnPodEnrollments: each shape of row a daemon's own login launched
// that the next enrollment of its session under that login would take something from is
// answered, with the end it is answered with. A live row is answered whatever it holds, nothing
// among it, because it can still be granted something or ask for something before that
// enrollment comes; a dormant one is answered while it holds a live grant or a pending request.
// A row an older broker replica wrote under the account alone is the daemon's own all the same.
func TestCarriedAnswersADaemonsOwnPodEnrollments(t *testing.T) {
	const session = "ses_acme"
	t.Run("a live row holding a grant is answered live with its lease end", func(t *testing.T) {
		svc, enrol, acme, _ := carriedDaemons(t)
		row := enrol(acme, session, "acme-pod-1")
		insertLiveGrant(t, svc, row.ID)
		wantCarried(t, "a live row holding a grant", askCarried(t, svc, acme, session),
			CarriedEnrollment{SessionID: session, EnrollmentID: row.ID, State: "live", EndsAt: leaseEnd(t, svc, row.ID)})
	})
	t.Run("a live row holding nothing is answered live", func(t *testing.T) {
		svc, enrol, acme, _ := carriedDaemons(t)
		row := enrol(acme, session, "acme-pod-1")
		if held := readCarryState(t, svc, row.ID); held.LiveGrants != 0 || held.Pending != 0 {
			t.Fatalf("the fresh row holds %+v, want nothing", held)
		}
		wantCarried(t, "a live row holding nothing", askCarried(t, svc, acme, session),
			CarriedEnrollment{SessionID: session, EnrollmentID: row.ID, State: "live", EndsAt: leaseEnd(t, svc, row.ID)})
	})
	t.Run("a dormant row holding a live grant is answered dormant with its carry window end", func(t *testing.T) {
		svc, enrol, acme, _ := carriedDaemons(t)
		row := enrol(acme, session, "acme-pod-1")
		insertLiveGrant(t, svc, row.ID)
		endLeavingDormant(t, svc, acme, row)
		wantCarried(t, "a dormant row holding a grant", askCarried(t, svc, acme, session),
			CarriedEnrollment{SessionID: session, EnrollmentID: row.ID, State: "dormant", EndsAt: carryWindowEnd(t, svc, row.ID)})
	})
	t.Run("a dormant row holding only a pending request is answered dormant", func(t *testing.T) {
		svc, enrol, acme, _ := carriedDaemons(t)
		row := enrol(acme, session, "acme-pod-1")
		insertPendingRequest(t, svc, row.ID)
		if err := svc.Revoke(context.Background(), acme, row.ID.String(), "launcher"); err != nil {
			t.Fatalf("revoke %s: %v", row.ID, err)
		}
		if left := readCarryState(t, svc, row.ID); !left.Dormant || left.LiveGrants != 0 || left.Pending != 1 {
			t.Fatalf("the end left %+v, want it dormant, holding its one pending request and no grant", left)
		}
		wantCarried(t, "a dormant row holding only a pending request", askCarried(t, svc, acme, session),
			CarriedEnrollment{SessionID: session, EnrollmentID: row.ID, State: "dormant", EndsAt: carryWindowEnd(t, svc, row.ID)})
	})
	t.Run("a row an older broker replica wrote under the account alone is answered", func(t *testing.T) {
		svc, enrol, acme, _ := carriedDaemons(t)
		row := enrol(acme, session, "acme-pod-1")
		// No route writes this spelling any more: an older replica does while the broker rolls
		// (adopt, migration 0033), so the row is rewritten to it here.
		spellAccountAlone(t, svc, row.ID)
		wantCarried(t, "a legacy row of the daemon's own login", askCarried(t, svc, acme, session),
			CarriedEnrollment{SessionID: session, EnrollmentID: row.ID, State: "live", EndsAt: leaseEnd(t, svc, row.ID)})
	})
}

// TestCarriedLeavesOutARowThatCanHandNothingOn: a row that holds nothing and can gain nothing is
// not answered, since adopt takes nothing from it: a row ended for good, a dormant row whose
// grant has been revoked since it went dormant, and a dormant row the carry sweep ended.
func TestCarriedLeavesOutARowThatCanHandNothingOn(t *testing.T) {
	const session = "ses_acme"
	t.Run("a row ended for good", func(t *testing.T) {
		svc, enrol, acme, _ := carriedDaemons(t)
		row := enrol(acme, session, "acme-pod-1")
		if err := svc.Revoke(context.Background(), acme, row.ID.String(), "launcher"); err != nil {
			t.Fatalf("revoke %s: %v", row.ID, err)
		}
		if left := readCarryState(t, svc, row.ID); !left.Ended || left.Dormant {
			t.Fatalf("the end left %+v, want it ended for good", left)
		}
		wantCarried(t, "a row ended for good", askCarried(t, svc, acme, session))
	})
	t.Run("a dormant row whose grant was revoked since", func(t *testing.T) {
		svc, enrol, acme, _ := carriedDaemons(t)
		row := enrol(acme, session, "acme-pod-1")
		grant := insertLiveGrant(t, svc, row.ID)
		endLeavingDormant(t, svc, acme, row)
		// An operator's revoke of the grant itself, which leaves the row dormant (requests.Machine).
		if _, err := svc.Store.Pool.Exec(context.Background(), `update grants set revoked_at=now(), revoked_by='human:ada@example.com' where id=$1`, grant); err != nil {
			t.Fatalf("revoke grant %s: %v", grant, err)
		}
		if left := readCarryState(t, svc, row.ID); !left.Dormant || left.LiveGrants != 0 {
			t.Fatalf("the row is %+v, want it dormant and holding no live grant", left)
		}
		wantCarried(t, "a dormant row holding nothing", askCarried(t, svc, acme, session))
	})
	t.Run("a dormant row the carry sweep ended", func(t *testing.T) {
		svc, enrol, acme, _ := carriedDaemons(t)
		row := enrol(acme, session, "acme-pod-1")
		insertLiveGrant(t, svc, row.ID)
		endLeavingDormant(t, svc, acme, row)
		backdateDormancy(t, svc, row.ID, carriedWindow+time.Minute)
		if ended, err := svc.EndCarried(context.Background()); err != nil || len(ended) != 1 {
			t.Fatalf("EndCarried = %+v, %v, want the one dormant row ended", ended, err)
		}
		wantCarried(t, "a row the carry sweep ended", askCarried(t, svc, acme, session))
	})
}

// TestCarriedAnswersNoOtherDaemonsEnrollment: every public Legion project's pods run as one
// account, so another daemon's pod may name the same session id. GLOBEX's live pod of S, holding
// a grant, is not answered to ACME, and once ACME's own pod of S enrols each daemon is answered
// its own row alone. The same holds of rows an older broker replica wrote under the account
// alone, which the credential that enrolled them tells apart.
//
// The control: drop the caller's account from both spellings of the carry principal (the pair
// matched on its pod account alone, the legacy row on its credential having any subject); ACME
// is answered GLOBEX's row, and the test fails naming it (the pull request records the run).
func TestCarriedAnswersNoOtherDaemonsEnrollment(t *testing.T) {
	for _, spelling := range []struct {
		name   string
		legacy bool
	}{
		{"rows of this broker", false},
		{"rows an older broker replica wrote under the account alone", true},
	} {
		t.Run(spelling.name, func(t *testing.T) {
			svc, enrol, acme, globex := carriedDaemons(t)
			const session = "ses_shared"
			globexRow := enrol(globex, session, "globex-pod-1")
			insertLiveGrant(t, svc, globexRow.ID)
			if spelling.legacy {
				spellAccountAlone(t, svc, globexRow.ID)
			}
			wantCarried(t, "ACME asking while only GLOBEX's pod names the session", askCarried(t, svc, acme, session))

			acmeRow := enrol(acme, session, "acme-pod-1")
			if spelling.legacy {
				spellAccountAlone(t, svc, acmeRow.ID)
			}
			wantCarried(t, "ACME asking once both daemons' pods name the session", askCarried(t, svc, acme, session),
				CarriedEnrollment{SessionID: session, EnrollmentID: acmeRow.ID, State: "live", EndsAt: leaseEnd(t, svc, acmeRow.ID)})
			wantCarried(t, "GLOBEX asking once both daemons' pods name the session", askCarried(t, svc, globex, session),
				CarriedEnrollment{SessionID: session, EnrollmentID: globexRow.ID, State: "live", EndsAt: leaseEnd(t, svc, globexRow.ID)})
		})
	}
}

// TestCarriedAnswersNoPersonsSessionOfTheSameID: a person's own session enrollment of the id a
// daemon asks about, live and holding a grant, is another principal's (carryPrincipal), which no
// pod adopts from, so it is not answered.
func TestCarriedAnswersNoPersonsSessionOfTheSameID(t *testing.T) {
	svc, _, acme, _ := carriedDaemons(t)
	const session = "ses_person"
	person := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	row, err := svc.Create(context.Background(), person, Enrollment{Kind: "session", RuntimeID: "box-1", Thumbprint: "tp-box-1", SessionID: str(session)})
	if err != nil {
		t.Fatalf("enrol the person's session: %v", err)
	}
	insertLiveGrant(t, svc, row.ID)
	wantCarried(t, "a person's session of the same id", askCarried(t, svc, acme, session))
}

// TestCarriedSortsItsAnswerAndAnswersAnIDOnce: the answer is sorted by session id, byte for
// byte, then by enrollment id, and an id named twice is answered once; an id nothing names is
// simply absent.
func TestCarriedSortsItsAnswerAndAnswersAnIDOnce(t *testing.T) {
	svc, enrol, acme, _ := carriedDaemons(t)
	// "Z" sorts before "a" byte for byte, and after it in most collations, so a sort by the
	// database's collation answers them the other way round.
	a1, a2 := enrol(acme, "a-session", "acme-pod-a1"), enrol(acme, "a-session", "acme-pod-a2")
	z := enrol(acme, "Z-session", "acme-pod-z")
	if a2.ID.String() < a1.ID.String() {
		a1, a2 = a2, a1
	}
	live := func(session string, row Enrollment) CarriedEnrollment {
		return CarriedEnrollment{SessionID: session, EnrollmentID: row.ID, State: "live", EndsAt: leaseEnd(t, svc, row.ID)}
	}
	wantCarried(t, "three rows of two sessions, one named twice", askCarried(t, svc, acme, "a-session", "nobody", "Z-session", "a-session"),
		live("Z-session", z), live("a-session", a1), live("a-session", a2))
}

// TestCarriedAnswersAReplacedDaemonPodItsPredecessorsRows: a replaced daemon pod logs in under
// a credential of its own (the per-pod schema, IssueTokenLoginTx) and enrols under the same pair
// its predecessor did, so the rows its predecessor launched are its own to be answered.
func TestCarriedAnswersAReplacedDaemonPodItsPredecessorsRows(t *testing.T) {
	svc, enrol, _, _ := carriedDaemons(t)
	storetest.PerPodLoginSchema(t, svc.Store)
	rig := podRig{svc: svc}
	first, second := rig.login(t, acmeDaemonAccount, "daemon-pod-1"), rig.login(t, acmeDaemonAccount, "daemon-pod-2")
	if first.ID == second.ID {
		t.Fatalf("both daemon pods hold credential %s, want one each", first.ID)
	}
	const session = "ses_acme"
	row := enrol(first, session, "acme-pod-1")
	insertLiveGrant(t, svc, row.ID)
	endLeavingDormant(t, svc, first, row)
	wantCarried(t, "the replacing daemon pod asking", askCarried(t, svc, second, session),
		CarriedEnrollment{SessionID: session, EnrollmentID: row.ID, State: "dormant", EndsAt: carryWindowEnd(t, svc, row.ID)})
}

// TestCarriedRefusesALoginThatProvedNoAccount: a credential a person approved carries no
// subject, launched nothing Carried could answer for, and is refused ErrServiceLoginRequired
// before anything is read.
func TestCarriedRefusesALoginThatProvedNoAccount(t *testing.T) {
	svc, _, _, _ := carriedDaemons(t)
	for _, cred := range []Credential{
		mintCredential(t, svc, str("ada@example.com"), nil, "devbox"),
		mintCredential(t, svc, nil, str("legion-daemon"), "cluster.example"),
	} {
		if _, err := svc.Carried(context.Background(), cred, []string{"ses_acme"}); !errors.Is(err, ErrServiceLoginRequired) {
			t.Fatalf("Carried under credential %s (operator %v, service %v) = %v, want ErrServiceLoginRequired", cred.ID, cred.Operator, cred.Service, err)
		}
	}
}

// TestCarriedRefusesSessionIDsOutsideItsBounds: no id, more than 4096, an empty one, one of 256
// bytes, and one that no session_id column can hold are each ErrInvalidSessionIDs; 4096 ids of
// 255 bytes are answered.
func TestCarriedRefusesSessionIDsOutsideItsBounds(t *testing.T) {
	svc, _, acme, _ := carriedDaemons(t)
	many := func(n, size int) []string {
		ids := make([]string, n)
		for i := range ids {
			id := fmt.Sprintf("s%d-", i)
			ids[i] = id + strings.Repeat("x", size-len(id))
		}
		return ids
	}
	for _, c := range []struct {
		name string
		ids  []string
	}{
		{"no id", nil},
		{"4097 ids", many(4097, 10)},
		{"an empty id", []string{"ses_acme", ""}},
		{"an id of 256 bytes", []string{strings.Repeat("x", 256)}},
		{"an id holding a NUL", []string{"ses\x00acme"}},
		{"an id that is not UTF-8", []string{"ses\xffacme"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := svc.Carried(context.Background(), acme, c.ids); !errors.Is(err, ErrInvalidSessionIDs) {
				t.Fatalf("Carried(%s) = %v, want ErrInvalidSessionIDs", c.name, err)
			}
		})
	}
	t.Run("4096 ids of 255 bytes", func(t *testing.T) {
		wantCarried(t, "the largest question", askCarried(t, svc, acme, many(4096, 255)...))
	})
}

// TestTheInvalidSessionIDsTextNamesItsBounds: ErrInvalidSessionIDs's text is a literal, so the
// broker's reference prints it, and it states the two bounds Carried applies.
//
// The control (the pull request records the run): MaxCarriedSessionIDs set to 2048 fails the row.
func TestTheInvalidSessionIDsTextNamesItsBounds(t *testing.T) {
	want := fmt.Sprintf("session_ids must name 1 to %d session ids, each 1 to %d bytes of text", MaxCarriedSessionIDs, MaxCarriedSessionIDBytes)
	if got := ErrInvalidSessionIDs.Error(); got != want {
		t.Fatalf("ErrInvalidSessionIDs says %q, want %q", got, want)
	}
}

// TestAnEmptyCarriedAnswerHandsTheNextEnrolmentNothing binds Carried's answer to adopt, whose
// principal rule is written apart from Carried's (side review of #4355): for every shape of row
// the tests above ask about, ACME asks about the row's session id and then enrols a pod of its own
// under that id, and a grant or a pending request that moves off the row onto the new enrolment
// (adopt) must come from a row the answer named. So an empty answer, which is what lets a Legion
// daemon free the id, hands the next enrolment nothing. Withheld names are left out, as Carried
// leaves them out: a withhold grants nothing. Each shape also says whether its row hands anything
// on, so a run in which nothing ever moves cannot pass for one in which nothing should. A person's
// session of the same id is no shape here: while it is live the broker refuses the pod's
// enrolment under a name another principal holds, so there is no next enrolment to take from it,
// and TestCarriedAnswersNoPersonsSessionOfTheSameID is its answer. The one shape whose asker is not
// the credential that enrolled the row, a replaced daemon pod's, is
// TestAReplacedDaemonPodsNextEnrolmentTakesOnlyWhatCarriedAnswered.
//
// The control (the pull request records the run): adopt's legacy clause taking a row whatever
// login's credential enrolled it (`c.subject=$5::text` widened to any subject) moves GLOBEX's
// legacy row's grant to ACME's enrolment, which Carried did not answer, and the test fails naming
// it.
func TestAnEmptyCarriedAnswerHandsTheNextEnrolmentNothing(t *testing.T) {
	const session = "ses_next"
	type rig struct {
		svc          *Service
		enrol        func(cred Credential, session, podUID string) Enrollment
		acme, globex Credential
	}
	for _, c := range []struct {
		name  string
		row   func(t *testing.T, r rig) uuid.UUID
		moves bool
	}{
		{"ACME's live row holding a grant", func(t *testing.T, r rig) uuid.UUID {
			row := r.enrol(r.acme, session, "acme-pod-1")
			insertLiveGrant(t, r.svc, row.ID)
			return row.ID
		}, true},
		{"ACME's live row holding nothing", func(t *testing.T, r rig) uuid.UUID {
			return r.enrol(r.acme, session, "acme-pod-1").ID
		}, false},
		{"ACME's dormant row holding a live grant", func(t *testing.T, r rig) uuid.UUID {
			row := r.enrol(r.acme, session, "acme-pod-1")
			insertLiveGrant(t, r.svc, row.ID)
			endLeavingDormant(t, r.svc, r.acme, row)
			return row.ID
		}, true},
		{"ACME's dormant row holding only a pending request", func(t *testing.T, r rig) uuid.UUID {
			row := r.enrol(r.acme, session, "acme-pod-1")
			insertPendingRequest(t, r.svc, row.ID)
			if err := r.svc.Revoke(context.Background(), r.acme, row.ID.String(), "launcher"); err != nil {
				t.Fatalf("revoke %s: %v", row.ID, err)
			}
			return row.ID
		}, true},
		{"ACME's legacy row holding a grant", func(t *testing.T, r rig) uuid.UUID {
			row := r.enrol(r.acme, session, "acme-pod-1")
			insertLiveGrant(t, r.svc, row.ID)
			spellAccountAlone(t, r.svc, row.ID)
			return row.ID
		}, true},
		{"ACME's row ended for good", func(t *testing.T, r rig) uuid.UUID {
			row := r.enrol(r.acme, session, "acme-pod-1")
			if err := r.svc.Revoke(context.Background(), r.acme, row.ID.String(), "launcher"); err != nil {
				t.Fatalf("revoke %s: %v", row.ID, err)
			}
			return row.ID
		}, false},
		{"ACME's dormant row whose grant was revoked since", func(t *testing.T, r rig) uuid.UUID {
			row := r.enrol(r.acme, session, "acme-pod-1")
			grant := insertLiveGrant(t, r.svc, row.ID)
			endLeavingDormant(t, r.svc, r.acme, row)
			if _, err := r.svc.Store.Pool.Exec(context.Background(), `update grants set revoked_at=now(), revoked_by='human:ada@example.com' where id=$1`, grant); err != nil {
				t.Fatalf("revoke grant %s: %v", grant, err)
			}
			return row.ID
		}, false},
		{"ACME's dormant row the carry sweep ended", func(t *testing.T, r rig) uuid.UUID {
			row := r.enrol(r.acme, session, "acme-pod-1")
			insertLiveGrant(t, r.svc, row.ID)
			endLeavingDormant(t, r.svc, r.acme, row)
			backdateDormancy(t, r.svc, row.ID, carriedWindow+time.Minute)
			if _, err := r.svc.EndCarried(context.Background()); err != nil {
				t.Fatalf("EndCarried: %v", err)
			}
			return row.ID
		}, false},
		{"GLOBEX's live row holding a grant", func(t *testing.T, r rig) uuid.UUID {
			row := r.enrol(r.globex, session, "globex-pod-1")
			insertLiveGrant(t, r.svc, row.ID)
			return row.ID
		}, false},
		{"GLOBEX's legacy row holding a grant", func(t *testing.T, r rig) uuid.UUID {
			row := r.enrol(r.globex, session, "globex-pod-1")
			insertLiveGrant(t, r.svc, row.ID)
			spellAccountAlone(t, r.svc, row.ID)
			return row.ID
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc, enrol, acme, globex := carriedDaemons(t)
			id := c.row(t, rig{svc: svc, enrol: enrol, acme: acme, globex: globex})
			wantNextEnrolmentBound(t, svc, acme, session, id, func() Enrollment { return enrol(acme, session, "acme-pod-next") }, c.moves)
		})
	}
}

// TestAReplacedDaemonPodsNextEnrolmentTakesOnlyWhatCarriedAnswered is the binding for the one
// shape whose asker is not the credential that enrolled the row (acceptance of #4355, N3): a
// replaced daemon pod logs in under a credential of its own (the per-pod schema) and asks about,
// then enrols, the session id its predecessor's dormant row still holds a grant for. Carried
// answers that row to the replacing pod (TestCarriedAnswersAReplacedDaemonPodItsPredecessorsRows),
// and the replacing pod's enrolment takes the grant.
//
// The control (the pull request records the run): Carried answering only the asking credential's
// own rows leaves the predecessor's row out while adopt still takes its grant, and the test fails
// naming it; TestAnEmptyCarriedAnswerHandsTheNextEnrolmentNothing passes under that control, which
// is the gap this row closes.
func TestAReplacedDaemonPodsNextEnrolmentTakesOnlyWhatCarriedAnswered(t *testing.T) {
	const session = "ses_next"
	svc, enrol, _, _ := carriedDaemons(t)
	storetest.PerPodLoginSchema(t, svc.Store)
	rig := podRig{svc: svc}
	first, second := rig.login(t, acmeDaemonAccount, "daemon-pod-1"), rig.login(t, acmeDaemonAccount, "daemon-pod-2")
	if first.ID == second.ID {
		t.Fatalf("both daemon pods hold credential %s, want one each", first.ID)
	}
	row := enrol(first, session, "acme-pod-1")
	insertLiveGrant(t, svc, row.ID)
	endLeavingDormant(t, svc, first, row)
	wantNextEnrolmentBound(t, svc, second, session, row.ID, func() Enrollment { return enrol(second, session, "acme-pod-next") }, true)
}

// wantNextEnrolmentBound is the binding's one check: asker asks Carried about session, enrolNext
// enrols asker's next pod of it, and a grant or a pending request that moved off row onto that
// enrolment (adopt) must come from a row the answer named. moves is whether the row hands anything
// on, so a run in which nothing moves cannot pass for one in which nothing should.
func wantNextEnrolmentBound(t *testing.T, svc *Service, asker Credential, session string, row uuid.UUID, enrolNext func() Enrollment, moves bool) {
	t.Helper()
	answered := false
	for _, a := range askCarried(t, svc, asker, session) {
		answered = answered || a.EnrollmentID == row
	}
	before := readCarryState(t, svc, row)
	next := enrolNext()
	after, took := readCarryState(t, svc, row), readCarryState(t, svc, next.ID)
	moved := took.LiveGrants > 0 || took.Pending > 0 || after.LiveGrants < before.LiveGrants || after.Pending < before.Pending
	if moved && !answered {
		t.Fatalf("ACME's next enrolment of %s took %+v from %s (left %+v of %+v), which Carried did not answer: a daemon would have freed the id", session, took, row, after, before)
	}
	if moved != moves {
		t.Fatalf("ACME's next enrolment of %s took %+v from the row (left %+v of %+v); want a move %v", session, took, after, before, moves)
	}
}
