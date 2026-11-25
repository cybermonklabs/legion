// revoke_approver.go is a person's revoke of a grant (RevokeByApprover): who may end it
// (mayRevoke), the transaction that ends it on the grant the person rule judged (revokeJudged),
// and the operator's withhold from every box of the grant's session (lockSessionEnrollments,
// withholdOnSession, withhold). The person rule it reads first is hide.go's.
package requests

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store"
)

// RevokeByApprover ends a grant on a person's login: the grant's approver, the person the
// enrollment that holds it acts for (its operator, operatorOf), or anyone at all for a grant whose
// approver is record.AnyoneApprover (mayRevoke). A login the grant names, its approver or its
// operator, ends it whatever Dispatch answers of the grant's projects; any other login must be a
// person the person rule shows the grant to (hideUnseenGrant). A login the
// rule hides the grant from is refused pgx.ErrNoRows, the refusal of a grant id no grant has, and
// a grant Dispatch could not answer for ErrProjectAccessUnavailable, each before the transaction
// and with nothing written. A person Dispatch calls a restricted member is no approver of a
// secret (restricted_approver.go), and ends a grant only as its operator: a login the grant admits
// only as an approver, by its own login, record.AnyoneApprover or a group, is asked its member kind
// after the rule and before the transaction (revokeApproves), and refused ErrNotApprover, in words
// that say why, when Dispatch calls it a restricted member. While Dispatch does not answer, only
// the person the grant names as its approver ends it as an approver, since a revoke only takes
// access away, and one held up by an outage would hold up whoever is ending a grant to stop a leak
// (RevokeUncheckedMessage). Every revoke through record.AnyoneApprover or a group then, whatever
// session holds the grant, and every other failure to read the kind, refuses it
// *projectaccess.MemberKindUncheckable, with nothing written, as it refuses every other read of a
// person's member kind. A grant on a person's session can still be ended by that person, its
// operator, who asks Dispatch nothing (namedOnGrant); a grant on a session that acts for no person
// (a service's pod, a person's pod BROKER_POD_OPERATORS no longer maps, or a machine's own
// session: operatorOf) is ended through the broker by nobody but its named approver until Dispatch
// answers. The operator's revoke asks nothing. actor is
// what every row it writes records as having done it, which the caller proved: "human:<login>" for
// a revoke Dispatch relays from its signed-in person,
// "launcher:<credential id>" for one the operator's own machine login makes. When the operator
// revokes, every name the grant's request got automatically is withheld from the grant's session
// from then on, and every other live grant of the session that got automatically a name served
// from the source of one of those names ends with it (withhold), so the session asks before it
// gets any of them again (Create). The withhold belongs to the session rather than the grant, so
// the operator's revoke of a grant already revoked (by its session, or by an earlier withhold,
// after the operator's Live grants list was loaded) withholds the same names, recorded as a
// grant.withheld audit row rather than a second grant.revoked. Another person's revoke (an
// approver's of the grant) ends the grant alone and withholds nothing: the session is not theirs.
// It locks the session's row before it reads or writes the grant: the order every writer that
// locks both takes them in, and the one lock Create and ApplyDecision read the withheld names
// under, so a request deciding on the session as this runs either is written first, and then
// ended here if it got a withheld name, or decides with the name withheld. A revoke that ends
// nothing and withholds nothing new succeeds and changes nothing.
// The enrollment it reads the operator off is the enrollment the grants row names now, so an
// approver's revoke reaches a grant that moved. An operator's withhold writes against that
// enrollment and every other live or dormant enrollment of the grant's session (revokeJudged), so
// it reaches the box the session runs on even where the grant stayed on a box the session left: a
// box that held nothing live when the session moved off it, whose grants and requests no adoption
// takes; and it ends the automatic grants of a dormant box, which the session's next adoption
// would otherwise carry to the box it runs on.
// The person rule reads the grant on the pool, before the transaction, since it may ask Dispatch,
// and the transaction reads it again (revokeJudged). An adoption that moves the grant between the
// two reads would let the transaction decide on a row the rule did not judge: a login exempt as
// the old enrollment's operator could then pass mayRevoke's arm for a grant whose approver is
// record.AnyoneApprover without the rule. So the transaction decides only on the enrollment and
// approver the rule judged, and starts again from the rule when either moved, at most
// revokeJudgeAttempts times, and then answers ErrGrantMoving, having revoked nothing.
func (m *Machine) RevokeByApprover(ctx context.Context, grantID, login, actor string) error {
	for attempt := 1; ; attempt++ {
		judged, err := m.hideUnseenGrant(ctx, grantID, login)
		if err != nil {
			return err
		}
		standing, err := m.revokeApproves(ctx, login, judged)
		if err != nil {
			return err
		}
		if m.testRevokeHook != nil {
			m.testRevokeHook()
		}
		err = m.revokeJudged(ctx, grantID, login, actor, judged, standing)
		if !errors.Is(err, errGrantMovedSinceJudged) {
			return err
		}
		if attempt == revokeJudgeAttempts {
			return fmt.Errorf("revoke grant %s: %w: it moved %d times in a row", grantID, ErrGrantMoving, attempt)
		}
	}
}

// errGrantMovedSinceJudged is revokeJudged's answer for a grant its transaction reads as held by
// another enrollment, or as naming another approver, than the person rule judged it by
// (grantJudged): an adoption moved it between the two reads. It writes nothing.
var errGrantMovedSinceJudged = errors.New("the grant moved to another box after the person rule judged it")

// ErrGrantMoving is RevokeByApprover's answer for a grant that moved to another box between the
// person rule and the transaction each of the revokeJudgeAttempts times it ran them
// (errGrantMovedSinceJudged): an adoption carries it to its session's new box, and a claim's
// hand-over to another pod. Nothing was revoked or withheld, and the same revoke ends the grant
// once it stays on one box; the API answers it 409 GRANT_MOVING.
var ErrGrantMoving = errors.New("the grant kept moving to another box while the revoke ran")

// revokeJudgeAttempts is how many times RevokeByApprover runs the person rule and its transaction
// for a grant that keeps moving between them. Each move is an adoption's commit, which lands once
// a session's process restarts, so a second attempt finds the grant still; a third is margin.
const revokeJudgeAttempts = 3

// revokeJudged is RevokeByApprover's transaction, for the grant as the person rule judged it, and
// standing is whether login may end it as an approver (revokeApproves, mayRevoke). It
// locks the enrollment the rule judged first, with every other live or dormant enrollment of its
// session (lockSessionEnrollments), in a statement of its own, and reads the grant in a later one.
// Every writer that moves a grant off an enrollment locks that enrollment's row before it moves
// the grant: an adoption takes its predecessors for update (enroll.adopt), and the end that hands a
// claim's rows to another pod (enroll.handOver) runs under the end's own lock of the enrollment.
// So the lock waits for such a move in flight to commit, and the later read, which under READ
// COMMITTED takes a snapshot of its own and sees what committed before it started, sees the grant
// where the move left it. One statement that read the grant and locked its enrollment together
// would read the grant as it was when the statement began, before the move it waited on, and
// decide on the old holder. Once the lock is held, nothing moves the grant off that enrollment
// until this transaction ends. The transaction asks for READ COMMITTED by name, so a changed
// default level cannot hand the later reads the first statement's snapshot. Under REPEATABLE READ
// the later statements would share the first one's snapshot, taken before the session lock's
// wait, and would miss what that wait committed.
// TestARevokeWaitingOnAnAdoptionDecidesOnWhereTheAdoptionLeftTheGrant holds the named level on a
// pool whose transactions default to REPEATABLE READ.
//
// Every revoke here takes the session's lock before it locks the session's enrollments, whoever
// revokes, since it has not yet read who the grant names (store.LockSession states the rule). An
// operator's withhold then writes against each locked enrollment: the names bind the session, and
// requests.Machine reads them by the enrollment's own id, so the box the session runs on must hold
// them itself. An enrollment's adoption takes the same lock before it reads its predecessors
// (enroll.adopt), so an enrollment of the session either committed before the lock statement, and
// is among the locked ones, or is adopted after this commits, and takes the names off the boxes
// this wrote them on, since a withheld name makes a box a predecessor whatever state it is in
// (adopt's withheld clause).
func (m *Machine) revokeJudged(ctx context.Context, grantID, login, actor string, judged grantJudged, standing approverStanding) error {
	tx, err := m.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	boxes, err := lockSessionEnrollments(ctx, tx, judged.holder)
	if err != nil {
		return err
	}
	holder := boxes[0]
	var session, requestID string
	var approver *string
	if err := tx.QueryRow(ctx, `select enrollment_id, request_id, approver from grants where id=$1`, grantID).Scan(&session, &requestID, &approver); err != nil {
		return err
	}
	if session != judged.holder || record.CanonicalLogin(deref(approver)) != judged.approver {
		return errGrantMovedSinceJudged
	}
	// The operator mayRevoke reads is the person rule's (operatorOf): a person's pod that
	// BROKER_POD_OPERATORS no longer maps acts for no person, so its former person is not its
	// operator here either, and ends its grant only as its approver or as anyone may, withholding
	// nothing from the pod.
	allowed, asOperator := mayRevoke(login, approver, m.operatorOf(holder), m.Groups, standing.approves)
	switch {
	case !allowed && standing.restricted:
		return errRestrictedRevoker
	case !allowed:
		return ErrNotApprover
	}
	tag, err := tx.Exec(ctx, `update grants set revoked_at=now(), revoked_by=$2 where id=$1 and revoked_at is null`, grantID, actor)
	if err != nil {
		return err
	}
	ended := tag.RowsAffected() > 0
	detail := auditDetail{}
	var withheld sessionWithhold
	if asOperator {
		if withheld, err = withholdOnSession(ctx, tx, m.Policy.Get(), boxes, requestID, actor); err != nil {
			return err
		}
		if len(withheld.names) > 0 {
			detail["withheld"] = withheld.names
			if len(withheld.on) > 0 {
				detail["withheld_on"] = withheld.on
			}
		}
	}
	kind := "grant.revoked"
	if !ended {
		if len(detail) == 0 {
			return nil
		}
		kind = "grant.withheld"
	}
	if err := audit(ctx, tx, kind, session, requestID, &grantID, actor, detail); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// One line per audit row this transaction wrote, not one per transaction: withhold can end
	// several other grants of the session, each with a grant.revoked row of its own. Those rows
	// were written before the revoke's own, so every withheld line goes out before it, the order
	// the review reads them in as well (store.ReadGrantLog orders by audit id). Among themselves
	// they are in no order the table holds: withhold's aggregate answers them by grant_id, a
	// UUID order, and the statement's audited CTE inserts them with no ordering at all. Each
	// names the request whose grant it ended, which is not the revoked grant's, and the
	// enrollment that grant hung off.
	for _, e := range withheld.ended {
		logGrantRow("grant.revoked", actor, e.enr, e.grant.RequestID, e.grant.GrantID, e.grant.Detail)
	}
	logGrantRow(kind, actor, holder, requestID, grantID, rawJSON(detail))
	return nil
}

// lockSessionEnrollments locks enrollment holder, and every other live or dormant enrollment of
// the session it belongs to, for no key update, in id order and in one statement, and answers
// them, holder first. Another enrollment of the session is one of the same session name under the
// same carry principal; a holder that names no session, or follows no principal, locks itself
// alone. A dormant one is among them because the session's next adoption moves its grants to the
// box the session runs on: an automatic grant of a withheld name left live there would be handed
// back.
//
// That is the spelling enroll.adopt's session rule (ofThisSession) matches a successor of the
// same principal by, and narrower than the rule: it also matches the principals of paired
// accounts and a pod's legacy rows. Those spellings are a pod's alone, and no pod that names a
// session follows a principal and withholds too, so the narrower set misses nothing a withhold
// needs: a service's pod, the one pod that names and carries a session, acts for no person, so
// its revoke never withholds; and a mapped person's pod, whose operator's revoke does withhold,
// enrols with no session name and no carry principal (a person's pod login), so it locks and
// withholds on itself alone and no session of it is adopted. For every holder that withholds
// across a session, a person's session, the two sets are the same, and so is the session lock's
// key, the session name and the carry principal, which adopt takes for the enrollment it makes.
//
// The session name and principal are read first and unlocked: both are written when the
// enrollment is created and never changed afterwards. It takes the session's lock
// (store.LockSession) between the two reads, so an adoption of the session in flight commits
// before the lock statement reads the session's enrollments, and one that starts later reads its
// predecessors only after this transaction ends. The order, the session's lock and then its
// enrollments in id order, is the one enroll.adopt takes them in, so neither waits on the other in
// a cycle.
func lockSessionEnrollments(ctx context.Context, tx pgx.Tx, holder string) ([]enrollmentRow, error) {
	var name string
	var carry *string
	if err := tx.QueryRow(ctx, `select coalesce(session_id, ''), carry_principal from enrollments where id=$1`, holder).Scan(&name, &carry); err != nil {
		return nil, err
	}
	if name != "" && carry != nil {
		if err := store.LockSession(ctx, tx, name, *carry); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx, `select id, kind, operator, subject, coalesce(session_id, ''), runtime_id from enrollments e
		where e.id=$1 or ($2::text <> '' and e.session_id=$2::text and e.carry_principal=$3::text and (e.revoked_at is null or e.dormant_since is not null))
		order by e.id for no key update`, holder, name, carry)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	boxes := []enrollmentRow{{}}
	found := false
	for rows.Next() {
		var r enrollmentRow
		if err := rows.Scan(&r.ID, &r.Kind, &r.Operator, &r.Subject, &r.SessionID, &r.RuntimeID); err != nil {
			return nil, err
		}
		if r.ID == holder {
			boxes[0], found = r, true
		} else {
			boxes = append(boxes, r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, pgx.ErrNoRows
	}
	return boxes, nil
}

// sessionWithhold is what withholdOnSession wrote: the names it newly withheld, sorted and each
// once; the boxes it wrote them on other than the grant's holder, for the audit row's
// withheld_on; and each grant it ended, with the box it hung off, for its log line.
type sessionWithhold struct {
	names []string
	on    []string
	ended []endedOn
}

// endedOn is a grant a withhold ended, with the enrollment it hung off.
type endedOn struct {
	enr   enrollmentRow
	grant endedGrant
}

// withholdOnSession withholds the names request requestID got automatically on each of boxes,
// the grant's holder first, as lockSessionEnrollments answers them locked: withhold on each, with
// set, the live policy, deciding which names share a source.
func withholdOnSession(ctx context.Context, tx pgx.Tx, set *policy.Set, boxes []enrollmentRow, requestID, actor string) (sessionWithhold, error) {
	var w sessionWithhold
	for i, enr := range boxes {
		names, ended, err := withhold(ctx, tx, set, enr.ID, requestID, actor)
		if err != nil {
			return sessionWithhold{}, err
		}
		w.names = append(w.names, names...)
		if i > 0 && len(names) > 0 {
			w.on = append(w.on, enr.ID)
		}
		for _, g := range ended {
			w.ended = append(w.ended, endedOn{enr, g})
		}
	}
	slices.Sort(w.names)
	w.names = slices.Compact(w.names)
	return w, nil
}

// withhold withholds from enrollmentID every name request requestID, whose grant its operator just
// revoked, got automatically and set did not already withhold from that enrollment, and ends every
// other live grant of the enrollment whose request got automatically a name those newly withheld
// names withhold (policy.Set.Withholds, which decides by source), each with a grant.revoked audit
// row by actor naming those of its names under "withheld". So a revoke of a read profile's grant
// ends the session's automatic grants of the profile's write sibling and of its source's own
// name, as a revoke of the source's own name ends those of each of its profiles, and no grant the
// session holds goes on releasing a name withheld from it without someone having approved it
// there. Which names share a source is set's, the live policy's, the one reading every reader of
// what is withheld decides by. A name set already withholds, by itself or by another name of its
// source, is not written and ends nothing: the first withhold of that source ended every grant
// that got one of its names without asking, and a grant whose request got one automatically
// since then was approved by someone the withheld name let approve it (currentPolicyAdmits),
// which stands. It answers the names it newly withheld, and the grant.revoked audit row it wrote
// for each grant it ended, so RevokeByApprover logs one grant-log line per row once the
// transaction has committed.
// It reads before it writes, in two statements of tx. RevokeByApprover holds every enrollment of
// the session locked from its first statement on, and Create and ApplyDecision take the
// enrollment's row for share before they write a grant on it (lockLiveEnrollment), so no grant
// reaches it between the two.
// Everything it writes and everything it ends is keyed by enrollmentID, one enrollment of the
// session RevokeByApprover holds locked: the one the grant hangs off now, or another live or
// dormant enrollment of the session, which is where the session runs once it left a box that held
// nothing, or a box whose grants the session's next adoption carries to where it runs.
func withhold(ctx context.Context, tx pgx.Tx, set *policy.Set, enrollmentID, requestID, actor string) ([]string, []endedGrant, error) {
	var asked, before, held []string
	if err := tx.QueryRow(ctx, `select
			array(select name from request_secrets where request_id=$2 and decision=$3),
			array(select name from withheld_secrets where enrollment_id=$1),
			array(select distinct a.name from grants g join request_secrets a on a.request_id=g.request_id and a.decision=$3
				where g.enrollment_id=$1 and g.revoked_at is null and `+grantUnexpired("g")+`)`,
		enrollmentID, requestID, policy.Automatic).Scan(&asked, &before, &held); err != nil {
		return nil, nil, err
	}
	already := policy.Requester{Withheld: before}
	var fresh []string
	for _, name := range asked {
		if !set.Withholds(already, name) {
			fresh = append(fresh, name)
		}
	}
	if len(fresh) == 0 {
		return nil, nil, nil
	}
	newly := policy.Requester{Withheld: fresh}
	var covered []string
	for _, name := range held {
		if set.Withholds(newly, name) {
			covered = append(covered, name)
		}
	}
	var names []string
	var ended []endedGrant
	err := tx.QueryRow(ctx, `with withheld as (
			insert into withheld_secrets (enrollment_id, name) select $1, unnest($4::text[])
			on conflict do nothing
			returning name
		), ended as (
			update grants g set revoked_at=now(), revoked_by=$2
			where g.enrollment_id=$1 and g.revoked_at is null and `+grantUnexpired("g")+`
			and exists (select 1 from request_secrets a where a.request_id=g.request_id and a.decision=$3 and a.name=any($5::text[]))
			returning g.id, g.request_id
		), audited as (
			insert into audit (kind, enrollment_id, request_id, grant_id, actor, detail)
			select 'grant.revoked', $1, e.request_id, e.id, $2, jsonb_build_object('withheld', (select jsonb_agg(a.name order by a.name)
				from request_secrets a where a.request_id=e.request_id and a.decision=$3 and a.name=any($5::text[])))
			from ended e
			returning grant_id, request_id, detail
		)
		select coalesce((select jsonb_agg(name order by name) from withheld), '[]'::jsonb),
			coalesce((select jsonb_agg(jsonb_build_object('grant', grant_id, 'request', request_id, 'detail', detail) order by grant_id) from audited), '[]'::jsonb)`,
		enrollmentID, actor, policy.Automatic, fresh, covered).Scan(&names, &ended)
	return names, ended, err
}

// mayRevoke answers whether login may revoke a grant, and whether as its enrollment's operator,
// whose revoke withholds (RevokeByApprover), rather than as its approver. operator is the person
// the enrollment that holds the grant acts for (operatorOf), the one the person rule exempts, so a
// former person of a person's pod BROKER_POD_OPERATORS no longer maps is not its operator to
// either. A person the grant names may (namedOnGrant), the operator first, so an operator who also
// approved the grant revokes it as the operator. A login the grant does not name is asked it only
// once the person rule shows it the grant (hideUnseenGrant): a grant of a private project is
// nobody else's to end outside it.
//
// A grant whose approver is the sentinel record.AnyoneApprover is every signed-in person's to
// end: the model judge approved it in anyone's stead, so it names no person who could end it,
// and GrantsForApprover lists it to everyone the person rule shows it to for that same reason.
// The revoke is the approver's and not the operator's, since whoever ends it stands in for the
// record's approver and withholds nothing from a session that is not theirs. A grant whose
// approver is a group (grantApprover) is each member's to end, as the one owner ends their own,
// by the members the group's last good read listed however old that read is
// (record.GroupStanding's listed, read without its freshness): a revoke only takes power away, so
// a member keeps the power to end the group's grant while the group is stale. A login outside the
// group ends nothing. The sentinel is no signed-in person itself, and neither is an empty login or
// one spelled as a group: none of them revokes anything.
//
// approves is whether login may stand as the grant's approver at all (revokeApproves): false for a
// person Dispatch calls a restricted member, who is no approver of a secret
// (restricted_approver.go), and ends a grant only as its operator, whatever its approver arm.
func mayRevoke(login string, approver, operator *string, groups record.Groups, approves bool) (allowed, asOperator bool) {
	if _, loginIsGroup := record.GroupOf(record.CanonicalLogin(login)); loginIsGroup {
		return false, false
	}
	if named, asOperator := namedOnGrant(login, approver, operator); named {
		return asOperator || approves, asOperator
	}
	login = record.CanonicalLogin(login)
	if login == "" || login == record.AnyoneApprover || !approves {
		return false, false
	}
	if approver == nil {
		return false, false
	}
	if record.CanonicalLogin(*approver) == record.AnyoneApprover {
		return true, false
	}
	_, listed, _, group := record.GroupStanding(groups, *approver, login)
	return group && listed, false
}

// revokeApproves answers mayRevoke's approves for login's revoke of the grant judged describes,
// read before the transaction: whether Dispatch calls login no restricted member
// (approvesSecrets), asked only where an approver arm of mayRevoke would admit login and login is
// not the operator of the enrollment that holds the grant. Everywhere else it approves nothing and
// asks nothing: the operator's revoke stands on the operator's arm alone, and a login no arm admits
// is refused whatever it is, so a group read that comes to admit login between this read and the
// transaction's admits nothing unasked. A machine with no client of Dispatch asks nothing, and
// every approver stands as before.
//
// It fails open only while Dispatch does not answer (projectaccess.Unanswered: the read's bound or
// deadline passing, a transport failure, a gateway's 502, 503 or 504, one of Dispatch's own codes
// for its outage, or a 500 naming no code or INTERNAL, which Dispatch answers while its database
// fails), unlike every other read of a person's member kind, and only for the person the grant
// names as its approver. It then approves and logs it (RevokeUncheckedMessage). A revoke only
// takes access away and releases nothing, and a revoke refused through an outage would delay the
// person ending a grant that leaks; what it costs is that a person Dispatch calls a restricted
// member could end a grant that names them as its approver while Dispatch does not answer. So it
// asks Dispatch itself (projectaccess.AskedNow), never answered by a failure an earlier read kept.
// Every other failure is an answer that lasts until somebody fixes it (Dispatch refusing the
// broker's own account, a kind this broker does not know, a status, a code or a body it cannot
// read, a certificate or a name that does not resolve), or a caller that went away, which says
// nothing of Dispatch; and a grant that admits login only through record.AnyoneApprover or a group
// is refused while Dispatch does not answer, whatever session holds it. One on a person's session
// can still be ended by that person, its operator, asking Dispatch nothing (namedOnGrant); one on a
// session that acts for no person (a service's pod, a person's pod BROKER_POD_OPERATORS no longer
// maps, or a machine's own session: operatorOf) is ended through the broker by nobody until
// Dispatch answers, and the way out meanwhile is cluster access (delete the pod). Each is the
// *projectaccess.MemberKindUncheckable it returns.
func (m *Machine) revokeApproves(ctx context.Context, login string, judged grantJudged) (approverStanding, error) {
	if m.Projects == nil {
		return approverStanding{approves: true}, nil
	}
	var approver *string
	if judged.approver != "" {
		approver = &judged.approver
	}
	if allowed, asOperator := mayRevoke(login, approver, judged.operator, m.Groups, true); !allowed || asOperator {
		return approverStanding{}, nil
	}
	approves, err := m.approvesSecrets(projectaccess.AskedNow(ctx), login)
	if err == nil {
		return approvesAsReadBy(approves), nil
	}
	if named, _ := namedOnGrant(login, approver, judged.operator); !named || !projectaccess.Unanswered(err) {
		return approverStanding{}, err
	}
	if ctx.Err() == nil {
		slog.Warn(RevokeUncheckedMessage, "person", record.CanonicalLogin(login), "error", err)
	}
	return approverStanding{approves: true}, nil
}

// RevokeUncheckedMessage is logged, at WARN, for each revoke that goes ahead although Dispatch did
// not answer whether the revoking person is a restricted member (revokeApproves): a revoke by the
// person the grant names as its approver, followed by person=<the canonical login> and
// error=<why it could not ask>.
const RevokeUncheckedMessage = "broker: a revoke as the grant's approver goes ahead though Dispatch could not say whether the person is a restricted member; a revoke only takes access away"

// namedOnGrant answers whether login is a person a grant names, and whether as the operator of
// the enrollment that holds it, which is read first: that operator, or the grant's approver, each
// compared through record.CanonicalLogin. Neither an empty login nor the sentinel
// record.AnyoneApprover is a person, so a grant whose approver is that sentinel names no approver.
// mayRevoke lets the people it names end the grant, and the person rule asks Dispatch nothing for
// them (hideUnseenGrant).
func namedOnGrant(login string, approver, operator *string) (named, asOperator bool) {
	login = record.CanonicalLogin(login)
	switch {
	case login == "" || login == record.AnyoneApprover:
		return false, false
	case operator != nil && record.CanonicalLogin(*operator) == login:
		return true, true
	case approver != nil && record.CanonicalLogin(*approver) == login:
		return true, false
	}
	return false, false
}
