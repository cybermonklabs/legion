package enroll

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The bounds of one Carried question, which ErrInvalidSessionIDs's text states: 4,096 ids is
// twice the 2,048 names of every kind one Legion daemon claim holds at its cap (plan E's t-e-54),
// which the daemon asks about as they are, session ids and session file names alike, and 255
// bytes an id. Exported for the route, which stops reading a list at the entry past
// MaxCarriedSessionIDs and sizes its body cap to the longest list these allow (api's
// maxCarriedBodyBytes).
const (
	MaxCarriedSessionIDs     = 4096
	MaxCarriedSessionIDBytes = 255
)

// ErrServiceLoginRequired refuses Carried to a launcher credential whose login names no account
// (no subject): one a person approved through a machine-login code. Carried answers what a login
// of the caller's own account launched, and such a login names no account; the pods it enrols
// carry nothing in any case (carryPrincipal). Any other launcher credential is answered, with the
// rows its own login launched alone: a service's token login, and a machine's or a person's
// sign-in whose login names an account, which is answered whatever pods it launched, usually none.
var ErrServiceLoginRequired = errors.New("only a launcher credential whose login names an account may ask which of its enrollments carry something for a session")

// CheckCarriedCaller is Carried's check of who asks, ErrServiceLoginRequired for a credential whose
// login names no account, exported so the route makes it before it reads the question's body.
func CheckCarriedCaller(cred Credential) error {
	if cred.Subject == nil || *cred.Subject == "" {
		return ErrServiceLoginRequired
	}
	return nil
}

// ErrInvalidSessionIDs refuses a Carried question that names no session id, more than
// MaxCarriedSessionIDs of them, or one that is empty, longer than MaxCarriedSessionIDBytes, or
// not text a session_id column can hold (not UTF-8, or holding a NUL, which Postgres refuses in
// text and so no enrollment's session id is). It is wrapped with what was wrong and which entry,
// and never repeats an id. Its text is a literal, which the reference generator (cmd/broker-refgen)
// prints in the route's refusals, where a text built by fmt it prints as "…";
// TestTheInvalidSessionIDsTextNamesItsBounds pins the two numbers to MaxCarriedSessionIDs and
// MaxCarriedSessionIDBytes, so the text cannot drift from the bounds.
var ErrInvalidSessionIDs = errors.New("session_ids must name 1 to 4096 session ids, each 1 to 255 bytes of text")

// CarriedEnrollment is one enrollment Carried answers.
type CarriedEnrollment struct {
	// SessionID is the session id the enrollment names.
	SessionID    string
	EnrollmentID uuid.UUID
	// State is "live", an enrollment no end has reached, or "dormant", one a box's end left
	// holding a live grant or a pending request for its session's next enrollment.
	State string
	// EndsAt is when the row stops carrying unless something else ends it first: a live row's
	// lease end, which every renewal moves later and past which the sweep ends it (and may leave
	// it dormant, which starts a window of its own), and a dormant row's carry window end, when
	// the sweep revokes what it holds (EndCarried). It is no promise: a successor's enrolment
	// takes what a row holds at any time before.
	EndsAt time.Time
}

// Carried answers, for the session ids a launcher names, every pod enrollment a login of the
// caller's own account launched that still carries something adopt would hand to the next
// enrollment of the same session id under the same login: the question a Legion daemon asks
// before it frees a session id it holds (plan E's t-e-54), which it frees only when the
// answer is empty. The answer is sorted by session id, byte for byte, and then by enrollment id;
// it is empty, never nil, when nothing carries; and an id named twice is answered once.
//
// It is one statement, and each clause of its row rule is there for one reason:
//
//   - e.kind = 'pod': the daemon's sessions are pods. A person's session of the same id, or a
//     machine's own, is another principal's (carryPrincipal), which no pod adopts from, and so it
//     is never what keeps a daemon from freeing a name.
//   - e.session_id = any(ids): the carry is keyed on the session id, and only a row naming one of
//     the ids can hand anything to that id's next enrollment.
//   - The carry principal, in the two spellings adopt takes a pod's predecessor under, both read
//     from the row and the caller and from nothing the caller states. The pair is the row's own
//     account launched by the caller's account, which is what carryPrincipal writes for a pod
//     the caller's login enrolled (exchange.PodLaunchPrincipal: 'account:' <pod account>
//     ' launched-by ' 'account:' <login account>, spelled here in SQL as migration 0034 spells
//     it, and compared with the spelling Create writes by this package's carried tests, whose
//     live row is written through exchange). The legacy spelling is the row's account alone,
//     which an older broker replica still writes while the broker rolls, taken only when the
//     credential that enrolled the row proved the caller's account, as adopt takes it. The
//     account of the login is the test rather than the credential's id, because a replaced
//     daemon pod logs in under a credential of its own and still enrols under the same pair
//     (sameLauncher), so its predecessor's rows are its own. Another login's enrollments are
//     never answered: every public Legion project's pods run as one account, so another
//     daemon's session of the same id is no predecessor of this daemon's next enrollment and
//     must never hold its name.
//   - The state: a live row (revoked_at is null) counts whatever it holds, and a dormant one
//     (dormant_since is not null) only while it holds a live grant or a pending request
//     (holdsForItsSession, the text adopt and endEnrollment read). A row ended for good never
//     counts.
//
// The state rule is deliberately wider than adopt's carriesForItsSession, which a live row
// passes only while it holds something now. adopt reads a row at the successor's enrolment,
// which may be long after this answer, and a live row can still gain something meanwhile: an
// approval lands on its pending request, the policy grants what it asks for, it files a new
// request. So a live row holding nothing now is no proof that the next enrollment would take
// nothing, and only a row that can gain nothing more is left out. A dormant row is one: its key
// died with its box, so it files nothing new, and what it holds can only leave it (an adoption,
// a revoke, the carry sweep, an expiry) or turn from a pending request into a grant, which it is
// answered for already. A row ended for good holds nothing and never will. So an empty answer
// stays true until a login of the caller's account enrols one of the ids again.
//
// The principal clause is not adopt's text: adopt matches the rows one successor would take from,
// and this matches the rows any successor of the caller's login could, so the two are written
// apart. What holds them together is TestAnEmptyCarriedAnswerHandsTheNextEnrolmentNothing, which
// runs Create after the answer for every shape of row and fails when the enrolment takes a grant
// or a request from a row the answer left out; a widening of adopt's predecessor rule fails it.
//
// Withheld names are not counted, though adopt moves them off a row of any state: a withhold
// takes a secret away from whichever session holds it and grants nothing, so handing one to the
// next enrollment of a freed id gives that session nothing of another's.
//
// A credential whose login names no account is ErrServiceLoginRequired, and a list outside its
// bounds ErrInvalidSessionIDs; both are checked before the query.
func (s *Service) Carried(ctx context.Context, cred Credential, sessionIDs []string) ([]CarriedEnrollment, error) {
	if err := CheckCarriedCaller(cred); err != nil {
		return nil, err
	}
	if len(sessionIDs) == 0 || len(sessionIDs) > MaxCarriedSessionIDs {
		return nil, fmt.Errorf("%w: it names %d", ErrInvalidSessionIDs, len(sessionIDs))
	}
	for i, id := range sessionIDs {
		switch {
		case id == "" || len(id) > MaxCarriedSessionIDBytes:
			return nil, fmt.Errorf("%w: entry %d is %d bytes", ErrInvalidSessionIDs, i, len(id))
		case !utf8.ValidString(id) || strings.ContainsRune(id, 0):
			return nil, fmt.Errorf("%w: entry %d is not text a session id can be", ErrInvalidSessionIDs, i)
		}
	}
	rows, err := s.Store.Pool.Query(ctx, `select e.session_id, e.id,
			case when e.revoked_at is null then 'live' else 'dormant' end,
			case when e.revoked_at is null then e.lease_expires_at
				else e.dormant_since + make_interval(secs => $3) end
		from enrollments e
		where e.kind = 'pod' and e.session_id = any($1::text[])
			and (e.carry_principal = 'account:' || e.subject || ' launched-by ' || 'account:' || $2::text
				or (e.carry_principal = 'account:' || e.subject
					and exists (select 1 from launcher_credentials c
						where c.id = e.launcher_credential_id and c.subject = $2::text)))
			and (e.revoked_at is null
				or (e.dormant_since is not null and (`+holdsForItsSession+`)))
		order by e.session_id collate "C", e.id`, sessionIDs, *cred.Subject, s.Carry.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[CarriedEnrollment])
}
