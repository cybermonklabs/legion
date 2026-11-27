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

// p0ScopeRefusal is the message of the 403 OAUTH_SCOPE_REQUIRED a write that makes an issue's own
// priority 0 (P0) is refused with, where its credential does not hold the p0 scope. It names the
// scope and says how to get it, since the command prints it as it is.
const p0ScopeRefusal = "this request's credential does not hold the p0 scope, which a priority of 0 needs: " +
	"a person sets P0 at a browser, or gives p0 to a personal token in Settings, Agent tokens"

// p0MoveRefusal is the message of the 403 OAUTH_SCOPE_REQUIRED that refuses a move of the issue key
// under parent that makes key, and each issue under it that takes its priority from it, P0
// (refuseResolvedP0WithoutScope). Like p0ScopeRefusal it names the scope and says how to proceed,
// and it names the issue the move is under, since that issue is where the P0 comes from.
func p0MoveRefusal(key, parent string) string {
	return fmt.Sprintf("this request's credential does not hold the p0 scope, which a move that makes work P0 needs: "+
		"under %s, which reads P0, %s would take priority 0, and so would each issue under it that takes its priority from it. "+
		"Give %s a priority from 1 to 3 before the move, or ask a person with p0: a person moves it at a browser, "+
		"or gives p0 to a personal token in Settings, Agent tokens", parent, key, key)
}

// p0ClearRefusal is the message of the 403 OAUTH_SCOPE_REQUIRED that refuses a clear of the issue
// key's own priority after which key takes P0 from the issue from, and so does each issue under key
// that takes its priority from it (refuseResolvedP0WithoutScope).
func p0ClearRefusal(key, from string) string {
	return fmt.Sprintf("this request's credential does not hold the p0 scope, which a clear that makes work P0 needs: "+
		"with no priority of its own, %s would take priority 0 from %s, and so would each issue under it that takes its priority from it. "+
		"Set a priority from 1 to 3 in place of the clear, or ask a person with p0: a person clears it at a browser, "+
		"or gives p0 to a personal token in Settings, Agent tokens", key, from)
}

// p0NewChildRefusal is the message of the 403 OAUTH_SCOPE_REQUIRED that refuses a new issue with no
// priority of its own under parent, which reads P0, from a writer that is not parent's architect
// (refuseNewChildP0WithoutScope). It says the three ways to proceed.
func p0NewChildRefusal(parent string) string {
	return fmt.Sprintf("this request's credential does not hold the p0 scope, which a new issue with no priority under a P0 issue needs: "+
		"%s reads P0, so the new issue would take priority 0 from it. "+
		"Give it a priority from 1 to 3, file it as the architect of %s, or ask a person with p0: a person files it at a browser, "+
		"or gives p0 to a personal token in Settings, Agent tokens", parent, parent)
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

// issuePriorityStanding is what the p0 checks read of one issue: its own priority, its parent, the
// priority it resolves to (refs.IssuePriorityTerms) and the issue that priority comes from (nil
// where it is its own or there is none), and its project with whether that project is private and
// whether its proven-sessions setting is on. writeIssue fills the first three from the issue it
// read under its owner lock (loadIssue answers them); readIssuePriorityStanding reads all of them.
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
// resolves to go from 1 to 3, or none, to 0. before is key as it stands before the write: its own
// priority, its parent and the priority it resolves to.
//
// The rule is decided on key alone. Each issue under key whose own priority is none, with none
// between it and key, resolves to what key passes down, which is key's own priority where key has
// one and else what key resolves to; so each of them goes from not 0 to 0 exactly when key does,
// and an issue under key with a priority of its own takes nothing from key. After the write, key
// resolves to its own priority where it has one, else to what its parent after the write resolves
// to (the parent the write names, or the one it has where the write names none), read through q
// by the walk every issue read resolves a priority by, else to none: a root with no priority. So a
// move of an issue that keeps a priority of its own, and a move that leaves what key resolves to
// at 0, refuse nothing, and a detach never does (movesOrClears). writeIssue asks it under the
// write's locks, where the dependency lock that every write of a priority or a parent takes holds
// the chain above key still, right after refuseP0WithoutScope; refuseP0BeforeSettlement asks it on
// the pool before a close settles the issue's documents.
func refuseResolvedP0WithoutScope(ctx context.Context, q queryer, credential *model.Credential, key string, write p0Write, before issuePriorityStanding) error {
	if !write.movesOrClears() || isPriorityZero(before.resolved) || credential.Holds(model.ScopeP0) {
		return nil
	}
	// from is the issue the priority after the write comes from, which a refused clear names: a
	// clear leaves key no priority of its own, so it is the parent's or the one the parent takes.
	after, from := before.own, ""
	if write.namesPriority {
		after = write.priority
	}
	parent := before.parent
	if write.namesParent {
		parent = write.parent
	}
	if after == nil && parent != nil {
		above, found, err := readIssuePriorityStanding(ctx, q, *parent)
		if err != nil {
			return err
		}
		// A parent that no issue holds resolves no priority: the write's own read of its parent
		// refuses it (validateIssueParent).
		if found {
			after, from = above.resolved, *parent
			if above.from != nil {
				from = *above.from
			}
		}
	}
	if !isPriorityZero(after) {
		return nil
	}
	if write.namesParent && write.parent != nil {
		return missingScopeError(model.ScopeP0, p0MoveRefusal(key, *write.parent))
	}
	return missingScopeError(model.ScopeP0, p0ClearRefusal(key, from))
}

// refuseP0BeforeSettlement is the p0 checks of a PATCH that closes the issue key, asked before the
// close settles the issue's documents (settleIssueBeforeClose), which writes document versions and
// asks: refuseP0WithoutScope for a close that names priority 0, and refuseResolvedP0WithoutScope
// for a close that clears the issue's own priority or moves it under a parent. Where the
// credential does not hold p0, it reads key's own priority, its parent and the priority it resolves
// to on the pool, with no lock held, and, for a close that then resolves through a parent, that
// parent's, and answers the 403 itself where either rule refuses, so such a close settles nothing.
// The checks under the owner lock decide each write this read lets through: a close whose
// priorities move between the two reads can settle the documents and is then refused there, as a
// close judgeSpecWriter refuses can be. An issue that is not there is left to the lock step's own
// answer.
func (s *server) refuseP0BeforeSettlement(ctx context.Context, credential *model.Credential, key string, write p0Write) error {
	if credential.Holds(model.ScopeP0) || (!isPriorityZero(write.priority) && !write.movesOrClears()) {
		return nil
	}
	pool, err := s.readyPool()
	if err != nil {
		return err
	}
	stored, found, err := readIssuePriorityStanding(ctx, pool, key)
	if err != nil || !found {
		return err
	}
	if err := refuseP0WithoutScope(credential, write.priority, stored.own); err != nil {
		return err
	}
	return refuseResolvedP0WithoutScope(ctx, pool, credential, key, write, stored)
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
