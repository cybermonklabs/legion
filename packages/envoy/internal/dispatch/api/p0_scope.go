package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// p0GrantClause is the clause every p0 refusal ends with, after the browser way to proceed that
// each says in its own words: the two other ways a person gives a credential the p0 scope. It is
// written once, so a new way to get p0 is one edit and no refusal tells a caller less than another.
const p0GrantClause = "gives p0 to a personal token in Settings, Agent tokens, " +
	"or ticks Set P0 for a Dispatch sign-in (dispatch login --device --scope person,p0)"

// p0ScopeRefusal is the message of the 403 OAUTH_SCOPE_REQUIRED a write that makes an issue's own
// priority 0 (P0) is refused with, where its credential does not hold the p0 scope. It names the
// scope and says how to get it, since the command prints it as it is.
const p0ScopeRefusal = "this request's credential does not hold the p0 scope, which a priority of 0 needs: " +
	"a person sets P0 at a browser, " + p0GrantClause

// p0MoveRefusal is the message of the 403 OAUTH_SCOPE_REQUIRED that refuses a move of the issue key
// under parent that makes key, and each issue under it that takes its priority from it, P0
// (refuseResolvedP0WithoutScope). Like p0ScopeRefusal it names the scope and says how to proceed,
// and it names the issue the move is under, since that issue is where the P0 comes from.
func p0MoveRefusal(key, parent string) string {
	return fmt.Sprintf("this request's credential does not hold the p0 scope, which a move that makes work P0 needs: "+
		"under %s, which reads P0, %s would take priority 0, and so would each issue under it that takes its priority from it. "+
		"Give %s a priority from 1 to 3 before the move, or ask a person with p0: a person moves it at a browser, %s",
		parent, key, key, p0GrantClause)
}

// p0ClearRefusal is the message of the 403 OAUTH_SCOPE_REQUIRED that refuses a clear of the issue
// key's own priority after which key takes P0 from the issue from, and so does each issue under key
// that takes its priority from it (refuseResolvedP0WithoutScope).
func p0ClearRefusal(key, from string) string {
	return fmt.Sprintf("this request's credential does not hold the p0 scope, which a clear that makes work P0 needs: "+
		"with no priority of its own, %s would take priority 0 from %s, and so would each issue under it that takes its priority from it. "+
		"Set a priority from 1 to 3 in place of the clear, or ask a person with p0: a person clears it at a browser, %s",
		key, from, p0GrantClause)
}

// p0NewChildRefusal is the message of the 403 OAUTH_SCOPE_REQUIRED that refuses a new issue with no
// priority of its own under parent, which reads P0, from a writer that is not parent's architect
// (refuseNewChildP0WithoutScope). It says the three ways to proceed.
func p0NewChildRefusal(parent string) string {
	return fmt.Sprintf("this request's credential does not hold the p0 scope, which a new issue with no priority under a P0 issue needs: "+
		"%s reads P0, so the new issue would take priority 0 from it. "+
		"Give it a priority from 1 to 3, file it as the architect of %s, or ask a person with p0: a person files it at a browser, %s",
		parent, parent, p0GrantClause)
}

// refuseP0WithoutScope is the one rule that holds an issue's own priority 0 to the p0 scope
// (model.ScopeP0): it answers the 403 OAUTH_SCOPE_REQUIRED, with the challenge naming p0
// (writeHandlerError writes it from the error), for a write that makes the issue's own priority 0,
// and nil for every other write. Every write that sets an issue's priority asks it: the create
// (createIssue), and the PATCH twice (writeIssue), under the issue's owner lock and, for a close,
// before the close settles the issue's documents (refuseP0BeforeSettlement).
//
// named is the priority the write names: nil for a write that names none and for one that clears
// the priority, since neither makes it 0. stored is the issue's own stored priority, the column
// and never a priority the issue's parent has, and nil for an issue that has none and for a new
// issue, which has none yet. A write makes the priority 0 when it names 0 and the issue's own is
// not 0 already, so a write that keeps P0, lowers it or sets 1 to 3 needs no scope. A clear, and a
// move under another parent, can still make the issue or an issue under it P0 by the priority it
// takes from above: refuseResolvedP0WithoutScope judges those, after this rule.
//
// credential is the credential of the request, from the caller that optionalActor resolved, and
// nil for a writer with none: it holds no scope, so a writer with no credential that names a
// priority of 0 is refused here like any other. R25's fix has no credential, and a status
// change's steps carry the request's; neither names a priority, so neither reaches a refusal.
func refuseP0WithoutScope(credential *model.Credential, named, stored *int) error {
	if !makesP0WithoutScope(credential, named, stored) {
		return nil
	}
	return missingScopeError(model.ScopeP0, p0ScopeRefusal)
}

// makesP0WithoutScope is the rule of refuseP0WithoutScope as a predicate: whether a write that
// names named, on an issue whose own priority is stored, makes that priority 0 from a credential
// that does not hold p0. It builds no refusal, so a caller that only asks whether a write could be
// refused allocates nothing.
func makesP0WithoutScope(credential *model.Credential, named, stored *int) bool {
	return named != nil && *named == 0 && (stored == nil || *stored != 0) && !credential.Holds(model.ScopeP0)
}

// isPriorityZero reports whether priority is 0 (P0): nil, which is none, is not.
func isPriorityZero(priority *int) bool {
	return priority != nil && *priority == 0
}

// p0Write is what a write of an issue names of the two fields its p0 checks judge: the issue's own
// priority and its parent, each beside whether the write names it at all (parseIssuePriority,
// parseIssueParent). A priority of null clears the issue's own priority, and a parent of null
// detaches the issue: it makes the issue a root.
type p0Write struct {
	priority      *int
	namesPriority bool
	parent        *string
	namesParent   bool
}

// movesOrClears reports whether the write is one whose move or clear can make work P0, which
// refuseResolvedP0WithoutScope judges: it names a parent that is an issue, or it clears the issue's
// own priority. A detach alone is not one: to take away the issues above an issue only turns a
// priority it took from them into none (J2 of dispatch://INFRA-5793), so a PATCH that sets no
// parent, and each unlink of a status change's unlink_children, which writes a parent of null, are
// never refused for it. Neither is a write that names a priority from 0 to 3 and no parent, which
// refuseP0WithoutScope judges alone.
func (write p0Write) movesOrClears() bool {
	return (write.namesParent && write.parent != nil) || (write.namesPriority && write.priority == nil)
}

// passedDown is what an issue passes down to each issue under it that takes its priority from it
// (refs.IssueInheritingDescendantRows): its own priority, from the issue itself, where it has one,
// else the priority it resolves to and the issue that one comes from (refs.IssuePriorityTerms).
// Each of those issues resolves to exactly this pair (J1 of dispatch://INFRA-5793), so a write of
// the issue that moves it moves theirs with it, and a write that keeps it moves none of theirs. Its
// priority is also the one the issue itself resolves to.
type passedDown struct {
	priority *int
	from     *string
}

// passedDownPriority is what the issue key passes down (passedDown), where own is its own
// priority, resolved the priority it resolves to and from the issue that one comes from.
func passedDownPriority(key string, own, resolved *int, from *string) passedDown {
	if own != nil {
		return passedDown{priority: own, from: &key}
	}
	return passedDown{priority: resolved, from: from}
}

// priorityMove is what a write that names the own priority or the parent of an issue does to what
// the issue passes down, as readPriorityMove reads it once for the write. Each rule of such a
// write that turns on what the issue passes down follows from this one value: which issues under
// it the write reads and locks, and the changed of the issue.updated it appends on each of them
// (changed), and the p0 check of its move or clear (makesP0, refuseResolvedP0WithoutScope). The
// zero value moves nothing and refuses nothing: it is the move of a write that names neither field,
// and of a key that no issue holds.
type priorityMove struct {
	// found is whether an issue holds the key the move was read for.
	found bool
	// own is the issue's own priority before the write, the column refuseP0WithoutScope judges.
	own *int
	// before and after are what the issue passes down before the write and after it. after.from is
	// the issue the priority after the write comes from, which a refused clear names
	// (p0ClearRefusal).
	before, after passedDown
	// changed names each of resolved_priority and priority_from whose half of what the issue passes
	// down the write moves, in the order issueChangedFields names them, and none where it moves
	// neither. A move of priority_from alone counts, as when an own 0 is set or cleared under a P0
	// issue.
	changed []string
}

// makesP0 reports whether the write makes the priority the issue resolves to go from 1 to 3, or
// none, to 0: the priority of what an issue passes down is the one it resolves to, before the write
// and after it.
func (move priorityMove) makesP0() bool {
	return !isPriorityZero(move.before.priority) && isPriorityZero(move.after.priority)
}

// readPriorityMove reads through q what write, a write that names the own priority or the parent
// of the issue key, does to what key passes down. Before the write, that is key's own priority
// where it has one, else the priority it resolves to (readIssuePriorityStanding). After it, it is
// key's own priority after the write where it has one; else what its parent after the write passes
// down, the parent the write names or the one key has where the write names none, read by the same
// walk; else none, for a root with no priority. A parent that no issue holds passes down nothing:
// the write's own read of its parent refuses it (validateIssueParent). A key that no issue holds
// answers the zero priorityMove and reads no parent.
//
// It takes no lock. writeIssue reads it once, under the dependency lock that every write of a
// priority or a parent takes and before any owner lock, so the priorities and the parents it reads
// stand until the write commits, and a write that moves nothing under key locks no issue there;
// refuseP0BeforeSettlement reads it on the pool, before a close settles the issue's documents.
func readPriorityMove(ctx context.Context, q queryer, key string, write p0Write) (priorityMove, error) {
	standing, found, err := readIssuePriorityStanding(ctx, q, key)
	if err != nil || !found {
		return priorityMove{}, err
	}
	move := priorityMove{
		found: true, own: standing.own,
		before: passedDownPriority(key, standing.own, standing.resolved, standing.from),
	}
	own, parent := standing.own, standing.parent
	if write.namesPriority {
		own = write.priority
	}
	if write.namesParent {
		parent = write.parent
	}
	if own != nil || parent == nil {
		move.after = passedDownPriority(key, own, nil, nil)
	} else {
		above, _, err := readIssuePriorityStanding(ctx, q, *parent)
		if err != nil {
			return priorityMove{}, err
		}
		move.after = passedDownPriority(*parent, above.own, above.resolved, above.from)
	}
	if !intPointersEqual(move.before.priority, move.after.priority) {
		move.changed = append(move.changed, "resolved_priority")
	}
	if !stringPointersEqual(move.before.from, move.after.from) {
		move.changed = append(move.changed, "priority_from")
	}
	return move, nil
}

// issuePriorityStanding is what the p0 checks read of one issue: its own priority, its parent, the
// priority it resolves to (refs.IssuePriorityTerms) and the issue that priority comes from (nil
// where it is its own or there is none), and its project with whether that project is private and
// whether its proven-sessions setting is on. readIssuePriorityStanding reads it: of the written
// issue and its parent after the write for readPriorityMove, and of a new issue's parent for
// refuseNewChildP0WithoutScope.
type issuePriorityStanding struct {
	own, resolved           *int
	parent, from            *string
	project                 string
	private, provenSessions bool
}

// issuePriorityStandingQuery is refs.IssuePriorityTerms over one key, $1, with the columns of
// issuePriorityStanding: the walk the issue reads resolve a priority by, so a check reads what the
// issue page shows. It reads the chain whole, with no filter of what a writer can see, as J2 says
// the walks that decide whether a write makes work P0 do (refs.IssuePriorityTerms says why): a
// writer who cannot see a P0 issue's project still needs p0 to move or file work under it. The
// text is built once, at start, for the reason issueRowStatement gives.
var issuePriorityStandingQuery = `
	with recursive subjects as (select $1::text as key), ` + refs.IssuePriorityTerms + `
	select i.priority, i.parent_key, ` + refs.IssuePriorityColumns + `,
	       i.project_key, p.visibility = 'private', p.proven_sessions
	from issues i
	join projects p on p.key = i.project_key
	` + refs.IssuePriorityJoin + `
	where i.key = $1`

// readIssuePriorityStanding reads the issue key's issuePriorityStanding through q, and false for a
// key that no issue holds. It takes no lock: a check under a write's locks reads what those locks
// hold still, and a check before them (refuseP0BeforeSettlement, the create's first check) only
// spares work that the check under the locks would refuse.
func readIssuePriorityStanding(ctx context.Context, q queryer, key string) (issuePriorityStanding, bool, error) {
	var standing issuePriorityStanding
	err := q.QueryRow(ctx, issuePriorityStandingQuery, key).Scan(
		&standing.own, &standing.parent, &standing.resolved, &standing.from,
		&standing.project, &standing.private, &standing.provenSessions,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return issuePriorityStanding{}, false, nil
	}
	if err != nil {
		return issuePriorityStanding{}, false, fmt.Errorf("read the priority %s resolves to: %w", key, err)
	}
	return standing, true, nil
}

// refuseResolvedP0WithoutScope is the one rule of a move and a clear (J2 of dispatch://INFRA-5793):
// a write of the issue key that names a parent, or clears key's own priority, from a credential
// that does not hold p0, is 403 OAUTH_SCOPE_REQUIRED naming p0 where it makes the priority key
// resolves to go from 1 to 3, or none, to 0 (priorityMove.makesP0). move is what the write does to
// what key passes down, as readPriorityMove read it.
//
// The rule is decided on key alone. Each issue under key whose own priority is none, with none
// between it and key, resolves to what key passes down, which is key's own priority where key has
// one and else what key resolves to; so each of them goes from not 0 to 0 exactly when key does,
// and an issue under key with a priority of its own takes nothing from key. After the write, key
// resolves to its own priority where it has one, else to what its parent after the write resolves
// to, else to none: a root with no priority. So a move of an issue that keeps a priority of its
// own, and a move that leaves what key resolves to at 0, refuse nothing, and a detach never does
// (movesOrClears).
//
// It reads nothing. writeIssue asks it under the write's locks, right after refuseP0WithoutScope,
// with the move it read under the dependency lock that every write of a priority or a parent
// takes, which holds the chain above key still until the write commits; refuseP0BeforeSettlement
// asks it with a move read on the pool before a close settles the issue's documents.
func refuseResolvedP0WithoutScope(credential *model.Credential, key string, write p0Write, move priorityMove) error {
	if !write.movesOrClears() || !move.makesP0() || credential.Holds(model.ScopeP0) {
		return nil
	}
	if write.namesParent && write.parent != nil {
		return missingScopeError(model.ScopeP0, p0MoveRefusal(key, *write.parent))
	}
	// A clear leaves key no priority of its own, so the P0 after it comes from key's parent, or
	// from the issue that parent takes it from: the refusal names that issue.
	return missingScopeError(model.ScopeP0, p0ClearRefusal(key, *move.after.from))
}

// refuseP0BeforeSettlement is the p0 checks of a PATCH that closes the issue key, asked before the
// close settles the issue's documents (settleIssueBeforeClose), which writes document versions and
// asks: refuseP0WithoutScope for a close that names priority 0, and refuseResolvedP0WithoutScope
// for a close that clears the issue's own priority or moves it under a parent. Where the
// credential does not hold p0, it reads what the close does to what key passes down on the pool,
// with no lock held (readPriorityMove): key's own priority, its parent and the priority it
// resolves to, and, for a close after which key resolves through a parent, that parent's. It
// answers the 403 itself where either rule refuses, so such a close settles nothing. The checks
// under the owner lock decide each write this read lets through: a close whose priorities move
// between the two reads can settle the documents and is then refused there, as a close
// judgeSpecWriter refuses can be. An issue that is not there is left to the lock step's own answer.
func (s *server) refuseP0BeforeSettlement(ctx context.Context, credential *model.Credential, key string, write p0Write) error {
	if credential.Holds(model.ScopeP0) || (!isPriorityZero(write.priority) && !write.movesOrClears()) {
		return nil
	}
	pool, err := s.readyPool()
	if err != nil {
		return err
	}
	move, err := readPriorityMove(ctx, pool, key, write)
	if err != nil || !move.found {
		return err
	}
	if err := refuseP0WithoutScope(credential, write.priority, move.own); err != nil {
		return err
	}
	return refuseResolvedP0WithoutScope(credential, key, write, move)
}

// refuseNewChildP0WithoutScope is the one rule of a new issue with no priority of its own under
// parent (J2 of dispatch://INFRA-5793): such an issue resolves to what parent resolves to, so where
// parent reads P0 the create makes a new P0 issue, and from a credential that does not hold p0 it
// is 403 OAUTH_SCOPE_REQUIRED naming p0, unless the writer's session is parent's architect. That
// is the session of parent's own issue_role_holders row with the role architect, which the Legion
// daemon of parent's project publishes, in whatever project parent is: the architect of an issue
// further above does not count, and neither does a session that holds another role of parent or
// its claim. While parent's project's proven-sessions setting is on, that session counts only on
// a credential that proves it (model.Actor.Proven), as judgeSpecWriter counts an architect; while
// it is off, a credential that proves no session, such as the shared token, names the session it
// acts as. A create that names a priority of its own is not asked: refuseP0WithoutScope judges it
// alone.
//
// createIssue asks it twice. First on the pool, before the near-copy gate writes its
// near_copy_checks row, so a refused create writes nothing: there it answers nothing for a parent
// that no issue holds or that a credential naming one project does not cover
// (access.CredentialCovers), whose refusal the create's own read of its parent answers
// (readCreateParent), so a 403 naming p0 never tells such a caller that a private parent exists.
// The coverage test is of parent alone, never of the issue its priority comes from: the walk reads
// the chain whole. Then in the create's transaction, after the dependency lock and that read of the
// parent, where it decides.
func refuseNewChildP0WithoutScope(ctx context.Context, q queryer, writer issueWriter, parent string) error {
	if writer.credential.Holds(model.ScopeP0) {
		return nil
	}
	standing, found, err := readIssuePriorityStanding(ctx, q, parent)
	if err != nil || !found {
		return err
	}
	if writer.callerProject != "" && !access.CredentialCovers(access.Credential{Listed: []string{writer.callerProject}}, standing.project, standing.private) {
		return nil
	}
	if !isPriorityZero(standing.resolved) {
		return nil
	}
	role, err := heldRole(ctx, q, parent, sessionOf(writer.actor))
	if err != nil {
		return err
	}
	if role == specWriterArchitectRole && (!standing.provenSessions || writer.actor.Proven) {
		return nil
	}
	return missingScopeError(model.ScopeP0, p0NewChildRefusal(parent))
}
