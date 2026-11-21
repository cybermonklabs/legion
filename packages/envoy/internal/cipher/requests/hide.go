// packages/envoy/internal/cipher/requests/hide.go
// hide.go is the private-project rule (dispatch://INFRA-3862) that every route taking a record,
// request or grant id from a caller reads before it answers anything else of the row: the person
// rule, for a person Dispatch's server relays or a person's own machine login (HideUnseen,
// ReadRecordFor, hideUnseenGrant), and the session rule, for a session's own routes
// (HideRequestFromSession, hideGrantFromSession). Each answers a row it hides exactly as an id no
// row has, pgx.ErrNoRows, and a row Dispatch could not answer for ErrProjectAccessUnavailable,
// which the route answers 503 having written nothing. What the rule reads of a row comes of one
// statement (readRowFacts), keyed by the row it starts from, so a column the rule reads is read
// for every route that judges a row, and for the approve path's decision (decisionFacts) too. The
// one read of a record for a person (ReadRecordFor) judges the record from the statement that reads
// what it answers (readRecord), which selects the same columns, so it answers the row it judged.
package requests

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// rowFacts is what a private-project rule reads of a record, a request or a grant (readRowFacts):
// the record's kind, empty for a request the policy answered at once and so wrote no record; the
// person the row names as its approver, nil for none; the enrollment that holds the row now
// (holder), which an adoption rewrites, with its operator and its launcher credential's service, as
// RevokeByApprover and the approve path's evaluation read it; the names the row asks for, or, for a
// grant and a decision, the names it releases; the projects of both credentials its request has
// had (credentialsProjects), the one that asked, which its record names, and the one it hangs off
// now; and its request's signed reason.
type rowFacts struct {
	kind            string
	approver        *string
	holder          enrollmentRow
	names           []string
	sessionProjects []string
	reason          string
}

// rowKey is which row a read of rowFacts starts from, and how it reaches the rest: from joins the
// request (r) and its record (cr), approver is the column naming the row's approver, held the
// column naming the enrollment that holds the row now, where names the row by $1, and released
// leaves out each name the request was denied.
type rowKey struct {
	from, approver, held, where string
	released                    bool
}

var (
	// recordKey is a credential-request record by its id, as HideUnseen reads it. A machine
	// login's record has no request, and answers its kind and nothing else of one.
	recordKey = rowKey{from: `credential_requests cr left join requests r on r.record_id = cr.id`, approver: `cr.approver`, held: `r.enrollment_id`, where: `cr.id = $1`}
	// requestKey is a request by its id, with every name it asked for, as HideRequestFromSession
	// reads it. A request the policy answered at once has no record, so its only credential is the
	// one it hangs off, and it names no approver.
	requestKey = rowKey{from: `requests r left join credential_requests cr on cr.id = r.record_id`, approver: `cr.approver`, held: `r.enrollment_id`, where: `r.id = $1`}
	// grantKey is a grant by its id, as GrantsForApprover lists it: its names are the ones it
	// releases, so a name its request was denied is left out; its approver is the grant's own,
	// record.AnyoneApprover where the model judge approved it in anyone's stead, as mayRevoke reads
	// it; and the enrollment that holds it is the one the grants row names.
	grantKey = rowKey{from: `grants g join requests r on r.id = g.request_id left join credential_requests cr on cr.id = r.record_id`, approver: `g.approver`, held: `g.enrollment_id`, where: `g.id = $1`, released: true}
	// decisionKey is the request a record id names, as the approve path reads it before its
	// transaction (applyDecisionAttempt): a record with no request is decided elsewhere, and the
	// names are the ones a grant of the request would release.
	decisionKey = rowKey{from: requestKey.from, approver: `cr.approver`, held: `r.enrollment_id`, where: `r.record_id = $1`, released: true}
)

// requestNamesSQL is the names a private-project rule reads of the request the alias request
// names, sorted: every name it asked for, or, released, each it was not denied. readRowFacts and
// readRecord both select it, so every reader judges a row by the same names.
func requestNamesSQL(request string, released bool) string {
	where := `rs.request_id = ` + request + `.id`
	if released {
		where += ` and rs.decision <> 'deny'`
	}
	return `coalesce((select array_agg(rs.name order by rs.name) from request_secrets rs where ` + where + `), '{}')`
}

// readRowFacts is the rowFacts of the row key names by id, read on the pool in one statement, or
// pgx.ErrNoRows when no row has the id.
func (m *Machine) readRowFacts(ctx context.Context, key rowKey, id string) (rowFacts, error) {
	var f rowFacts
	var asked enrollmentRow
	err := m.Store.Pool.QueryRow(ctx, `select coalesce(cr.kind, ''), `+key.approver+`, coalesce(`+key.held+`::text, ''), coalesce(r.reason, ''),
			`+requestNamesSQL("r", key.released)+`,
			`+credentialColumns+`, `+currentCredentialColumns+`, c.service
		from `+key.from+`
		left join enrollments enr on enr.id = cr.enrollment_id
		left join enrollments cur on cur.id = `+key.held+`
		left join launcher_credentials c on c.id = cur.launcher_credential_id
		where `+key.where, id).
		Scan(&f.kind, &f.approver, &f.holder.ID, &f.reason, &f.names, &asked.Kind, &asked.Operator, &asked.Subject, &asked.Projects,
			&f.holder.Kind, &f.holder.Operator, &f.holder.Subject, &f.holder.Projects, &f.holder.Service)
	if err != nil {
		return rowFacts{}, err
	}
	f.sessionProjects = m.credentialsProjects(asked, f.holder)
	return f, nil
}

// HideUnseen is the person rule's answer to a route that names record recordID, which its caller
// gave it, for login: pgx.ErrNoRows for an id no record has and for a secret request login may not
// see (hideUnseenRow: one of a private project they are not on), one answer for both, so the route
// answers login exactly as it answers an id no record has; ErrProjectAccessUnavailable when
// Dispatch could not say whether login may see it, which the route answers 503 having written
// nothing; and nil for a secret request login may see and for a machine login, which belongs to no
// project and asks Dispatch nothing, as the pending list shows it (PendingForApprover). Every route
// that takes a record id from a caller calls it before it answers anything else of the record, and
// a write before its transaction, since it may ask Dispatch: the dismissal (Dismiss) and the two
// decide routes for a secret request (api.decideRecord). The approval page reads every record it
// prints through ReadRecordFor, which asks the same rule (hideUnseenRow), and its decision is
// ApplyDecision's own, whose person rule (decisionFacts) answers the same pgx.ErrNoRows.
//
// It reads what the rule asks of the record from the rows Create wrote beside it (recordKey) —
// every name the request asked for, its signed reason, and the projects of the credentials it has
// had (credentialsProjects) — and not from its signed request object, so it answers for a record
// signed for another broker address too, which ReadRecord cannot verify here and no list of this
// broker shows. Neither an empty login nor the sentinel record.AnyoneApprover names a person, and
// either is shown what a reader naming no person is shown: no private project.
func (m *Machine) HideUnseen(ctx context.Context, recordID, login string) error {
	facts, err := m.readRowFacts(ctx, recordKey, recordID)
	if err != nil || facts.kind != record.KindAgentSecret {
		return err
	}
	_, err = m.hideUnseenRow(ctx, login, facts)
	return err
}

// ReadRecordFor is ReadRecord for login, a person: the record once the person rule shows it to
// them, and the rule's own answer otherwise, HideUnseen's — pgx.ErrNoRows for a record they may
// not see, as for an id no record has, and ErrProjectAccessUnavailable when Dispatch could not say
// — with nothing more of the record read. It is the one read of a record for a person, so the
// approval page cannot print a record without the rule having shown it: the record it names, each
// other record it prints and each record an outcome line names. The rule is read from the record's
// rows before ReadRecord verifies its signed request, so a record signed for another broker address
// answers a person it hides from pgx.ErrNoRows too, not ReadRecord's error.
//
// What it answers is what the rule judged: the record's rows come of the one statement the rule
// reads them from (readRecord), and its secrets' projects (SecretProjects) are those the rule
// decided on (seenSecrets), of the policy version it read the tags from, so neither a tag the
// policy gained nor a request moved to another credential after the rule read them reaches the
// person unchecked.
func (m *Machine) ReadRecordFor(ctx context.Context, recordID, login string) (RecordDetail, error) {
	return m.readRecord(ctx, recordID, func(facts rowFacts) ([]SecretProject, error) {
		if facts.kind != record.KindAgentSecret {
			return nil, nil
		}
		return m.hideUnseenRow(ctx, login, facts)
	})
}

// hideUnseenRow is pgx.ErrNoRows for a row login may not see (maySeeRecord: its names under the
// live policy, the projects of the credentials it has had and its reason),
// ErrProjectAccessUnavailable when Dispatch could not say, and for a row login may see, the project
// of each of its names the rule decided on (seenSecrets): the one answer HideUnseen gives for a
// record and RevokeByApprover for a grant, so both answer a person the rule hides the row from as
// they answer an id no row has, and the projects ReadRecordFor answers the record with.
func (m *Machine) hideUnseenRow(ctx context.Context, login string, facts rowFacts) ([]SecretProject, error) {
	if login = record.CanonicalLogin(login); login == record.AnyoneApprover {
		login = ""
	}
	secrets, may, err := m.seenSecrets(ctx, login, facts.names, facts.sessionProjects, facts.reason)
	switch {
	case err != nil:
		return nil, err
	case !may:
		return nil, pgx.ErrNoRows
	}
	return secrets, nil
}

// hideUnseenGrant is the person rule over grant grantID for login, read on the pool before
// RevokeByApprover's transaction, since it may ask Dispatch. A person the grant names — its
// approver or the operator of the enrollment that holds it, the people mayRevoke allows by name
// (namedOnGrant) — is answered nil and asks Dispatch nothing, as the session that holds a row is
// answered nil by the session rule: the grant row names them, so they already know it exists, and
// a revoke only takes access away, so neither a Dispatch outage nor their leaving the grant's
// project keeps them from ending it. A person's pod that BROKER_POD_OPERATORS no longer maps acts
// for no person (unmappedPersonPod), and names its exchange entry's project, not its operator's,
// so its operator column names whom it once acted for, not a person the grant names: operatorOf
// gives none for it. Anyone else is asked the rule (hideUnseenRow): pgx.ErrNoRows for an id no
// grant has and for a grant they may not see, ErrProjectAccessUnavailable when Dispatch could not
// say, and nil for a grant they may see, which mayRevoke then lets them end only where its
// approver is record.AnyoneApprover: mayRevoke reads the same operator (operatorOf), so a former
// person of such a pod ends its grants only as anyone else may. It reads the grant as
// GrantsForApprover lists it (grantKey), so such a person revokes exactly the grants their own
// list may show them. It answers the grant as it judged it (grantJudged), which RevokeByApprover's
// transaction compares with its own read.
func (m *Machine) hideUnseenGrant(ctx context.Context, grantID, login string) (grantJudged, error) {
	facts, err := m.readRowFacts(ctx, grantKey, grantID)
	if err != nil {
		return grantJudged{}, err
	}
	judged := grantJudged{holder: facts.holder.ID, approver: record.CanonicalLogin(deref(facts.approver))}
	if named, _ := namedOnGrant(login, facts.approver, m.operatorOf(facts.holder)); named {
		return judged, nil
	}
	_, err = m.hideUnseenRow(ctx, login, facts)
	return judged, err
}

// grantJudged is what hideUnseenGrant judged a grant by: the enrollment that held it and its
// approver. RevokeByApprover's transaction reads both again, and a grant whose read differs moved
// between the two (an adoption's commit), so the rule judged another row, and the revoke starts
// again from the rule.
type grantJudged struct {
	holder, approver string
}

// operatorOf is the person the enrollment that holds a grant acts for, as the person rule's
// exemption reads it (namedOnGrant): enr's operator, and nil for a person's pod that
// BROKER_POD_OPERATORS no longer maps (unmappedPersonPod), which acts for no person.
func (m *Machine) operatorOf(enr enrollmentRow) *string {
	if m.unmappedPersonPod(enr) {
		return nil
	}
	return enr.Operator
}

// HideRequestFromSession is the private-project rule a session's own routes read before their
// owner check, for request requestID and the calling session enrollmentID: pgx.ErrNoRows for an id
// no request has and for a request of a private project the calling session cannot see
// (hideFromSession), whoever owns it, one answer for both; ErrProjectAccessUnavailable when
// Dispatch could not say, which the route answers 503 having changed nothing; and nil otherwise.
// So the owner check's refusal (ErrNotOwner, 403 NOT_YOURS) says only that a request exists in a
// project the caller sees. GET /v1/requests/{id} reads it, and Cancel.
//
// The session that holds the request — the enrollment it hangs off now — is answered nil, whatever
// the request touches: it made the request, or adopted it with the session that made it, so it
// holds everything the request says, and a request whose reason cites a private project's issue
// its credential does not name is still its own to read while it waits and to cancel.
func (m *Machine) HideRequestFromSession(ctx context.Context, requestID, enrollmentID string) error {
	facts, err := m.readRowFacts(ctx, requestKey, requestID)
	if err != nil || facts.holder.ID == enrollmentID {
		return err
	}
	return m.hideFromSession(ctx, enrollmentID, facts)
}

// hideGrantFromSession is HideRequestFromSession for grant grantID, which Values and RevokeGrant
// read before their owner check. The session that holds the grant is answered nil: a release of a
// grant of a private project it no longer names is checkRelease's to refuse, which ends the grant
// for good.
func (m *Machine) hideGrantFromSession(ctx context.Context, grantID, enrollmentID string) error {
	facts, err := m.readRowFacts(ctx, grantKey, grantID)
	if err != nil || facts.holder.ID == enrollmentID {
		return err
	}
	return m.hideFromSession(ctx, enrollmentID, facts)
}

// hideFromSession is pgx.ErrNoRows when the row facts describe touches a private project the
// session enrollmentID cannot see, ErrProjectAccessUnavailable when Dispatch could not say, and nil
// otherwise. A row touches the project each of its names is tagged with, each project its
// credentials name and the project of each issue key its reason names, as the person rule reads
// them (maySeeRecord). The session sees a public project, a key Dispatch holds no project for, and
// a private project its own credential names (credentialProjects: a person's session's list, the
// project a pod's or a machine's operator entry maps it to) whose list still holds the person that
// credential acts for, asked as a release asks it (projectFacts): a private project's secret goes
// only to a credential that names it, and a row of one only to such a credential's session. A
// session whose credential names no private project sees none. The answers it rests on are held to
// atDecision's bound together.
func (m *Machine) hideFromSession(ctx context.Context, enrollmentID string, facts rowFacts) error {
	var caller enrollmentRow
	if err := m.Store.Pool.QueryRow(ctx, `select kind, operator, subject, coalesce(projects, '{}') from enrollments where id=$1`, enrollmentID).
		Scan(&caller.Kind, &caller.Operator, &caller.Subject, &caller.Projects); err != nil {
		return err
	}
	person, named := m.credentialProjects(caller)
	may, err := atDecision(ctx, m.Projects, func(ctx context.Context, _ bool) (bool, time.Time, error) {
		tagged, asked, err := m.secretProjects(ctx, facts.names)
		if err != nil {
			return false, time.Time{}, err
		}
		// A project the session's credential names is asked about for its person, who sees it
		// public or on its list; any other project for nobody, which Dispatch answers by its
		// visibility alone, so only a public one, or a key of no project, passes.
		var its, others []string
		for _, project := range slices.Concat(privateProjects(tagged), facts.sessionProjects, record.ReasonProjects(facts.reason)) {
			if slices.Contains(named, project) {
				its = append(its, project)
			} else {
				others = append(others, project)
			}
		}
		mayIts, seenIts, err := m.maySee(ctx, person, its, "")
		if err != nil || !mayIts {
			return false, time.Time{}, err
		}
		mayOthers, seenOthers, err := m.maySee(ctx, "", others, "")
		if err != nil || !mayOthers {
			return false, time.Time{}, err
		}
		return true, projectaccess.Oldest(asked, projectaccess.Oldest(seenIts, seenOthers)), nil
	})
	switch {
	case err != nil:
		return err
	case !may:
		return pgx.ErrNoRows
	}
	return nil
}
