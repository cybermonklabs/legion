package record

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ErrChainUnknown is an owner walk that met a Dispatch chain the copy cannot show to its top
// before it found an architect (Chain.Known, R16 of dispatch://INFRA-3320): which architect owns
// the issue is not known until the daemon has read Dispatch again, so a notice waits for it as a
// notice for a suspended architect does (decision 4 of dispatch://INFRA-4730).
var ErrChainUnknown = errors.New("the Dispatch chain is unknown")

// OwningArchitect is the architect claim of the issue the owner walk from key ends at
// (OwningIssue). It fails with ErrChainUnknown when the walk meets an unknown chain, and otherwise
// when that issue's key, or project, makes no claim token.
func OwningArchitect(project string, chains *Chains, key string, fromParent bool, runs func(claim.Token) bool) (claim.Token, error) {
	owner := OwningIssue(project, chains, key, fromParent, runs)
	if owner == "" {
		chain := chains.Chain(key)
		return "", fmt.Errorf("%w: the chain of %s is %s at %s", ErrChainUnknown, key, chain.Cause, chain.At)
	}
	return claim.NewToken(project, owner, claim.RoleArchitect)
}

// NoticeStartsAtParent is whether the owner walk for a notice of kind, about role's claim, starts
// at its issue's parent (OwningIssue's fromParent) rather than at the issue itself. Two notices do.
// A child's close or status change: it tells of the child's own run starting, leaving, coming back
// or closing, which is not the child's own architect to judge (a leave suspends that architect and
// a close as done retires it, in workflow's leave), and the architect above it decides what the
// rest of the work under it does. And a worker-died, a worker-stalled or a provider-errors about an
// issue's own architect (role is claim.RoleArchitect): the notice is about that architect, so the
// architect above it must hear it, and a root's own architect, which has none above it, hears its
// own worker-died and worker-stalled (the walk from a root ends at the root, OwningIssue); the
// daemon writes no architect row for a root architect's own provider-errors (daemon's
// providerErrorRunReached), since that wake would reach the session the provider keeps refusing.
// Any of these notices on an issue with no issue above it in Legion's work goes to its gate root,
// never past it to nobody: a failed claim is passed over by the walk anyway, but a stalled one
// still holds its role and would be told of its own stall, and the outbox can publish a child's
// close or status change while the child's own architect still holds its role, before its
// suspension or close lands.
func NoticeStartsAtParent(kind NoticeKind, role claim.Role) bool {
	aboutItsArchitect := role == claim.RoleArchitect &&
		(kind == "worker-died" || kind == "worker-stalled" || kind == ProviderErrorsNotice)
	return aboutItsArchitect || kind == "child-closed" || kind == "child-status"
}

// OwningIssue is the key of the issue whose architect owns key (R18 of dispatch://INFRA-3320), read
// on the Dispatch chain as chains hold it: key's own architect while its claim holds its role
// (runs), else the nearest issue above it in Legion's work (Chains.Above) whose architect claim
// does, whatever the status of the issues between them, else key's gate root (Chains.StateTree:
// for an issue not in Legion's work, the nearest issue above it that the daemon records and that
// holds its own gate, else the issue itself). Nothing of the stored tree is read, so a change of
// parent moves the walk with the chain. The daemon starts a child's architect with the child's
// first phase, and starts a failed one again at the child's next phase start; while a child's
// architect does not hold its role, which supervise.HoldsRole decides (and the daemon's holdsRole
// over the machine's live view), the child's notices go on up to the architect above it. An issue
// whose key makes no claim token has no architect and owns nothing, so the walk passes it.
//
// fromParent starts the walk at the issue above key instead, for a notice the architect above the
// issue must hear (NoticeStartsAtParent): it walks Above alone, and ends at the gate root of
// Above's first issue where none of them holds its role. An issue with nothing above it in
// Legion's work hears its own, at its gate root.
//
// The walk is "" where it reaches the end of what the copy shows of an unknown chain
// (Chain.Known) before an architect that holds its role: the issues above the gap, and so the gate
// root, are not known. An architect found below the gap owns the issue whatever stands above it.
//
// project is the claim project token (claim.ProjectToken). runs is the reader's own view of
// whether an architect's claim holds its role: the daemon's outbox reads the machine's live view,
// which also knows a stop in flight, and a reader in a transaction reads the claims the store
// records (ReadOwnerWalk). An issue the walk asks runs about ends the walk there when project
// makes no claim project token, since it has no claim to ask about; OwningArchitect then fails on
// it.
func OwningIssue(project string, chains *Chains, key string, fromParent bool, runs func(claim.Token) bool) string {
	holds := func(issue string) bool {
		if !claim.IsIssueKey(issue) {
			return false
		}
		token, err := claim.NewToken(project, issue, claim.RoleArchitect)
		return err != nil || runs(token)
	}
	if !fromParent && holds(key) {
		return key
	}
	above := chains.Above(key)
	for _, issue := range above {
		if holds(issue) {
			return issue
		}
	}
	if !chains.Chain(key).Known() {
		return ""
	}
	if fromParent && len(above) > 0 {
		return chains.StateTree(above[0])
	}
	return chains.StateTree(key)
}

// ArchitectActsOn says whether the architect claim architect acts on key (R18 of
// dispatch://INFRA-3320): it is the architect claim of key itself or of an issue above key on its
// Dispatch chain as chains hold it (ArchitectAbove), whatever the status of the issues between
// them. It is the ownership rule an architect's call is held to, and the workflow reads it again
// where the call's fact commits, so a call sent before a change of parent and committed after it
// is judged on the chain it lands on. project is the claim project token (claim.ProjectToken).
// The caller refuses an unknown chain (Chain.Known) before it asks.
func ArchitectActsOn(project string, chains *Chains, architect claim.Token, key string) bool {
	return isArchitectOf(project, architect, key) || ArchitectAbove(project, chains, architect, key)
}

// ArchitectAbove says whether the architect claim architect is the architect claim of an issue
// above key on its Dispatch chain as chains hold it, whatever the status of the issues between
// them: the rule of a call an architect makes only on an issue under its own, rerun_child and
// release_children, which never take the architect's own issue. project is the claim project
// token (claim.ProjectToken). The caller refuses an unknown chain (Chain.Known) before it asks.
func ArchitectAbove(project string, chains *Chains, architect claim.Token, key string) bool {
	return slices.ContainsFunc(chains.Chain(key).Links, func(link string) bool { return isArchitectOf(project, architect, link) })
}

// isArchitectOf says whether architect is the architect claim token of issue in the claim
// project project.
func isArchitectOf(project string, architect claim.Token, issue string) bool {
	token, err := claim.NewToken(project, issue, claim.RoleArchitect)
	return err == nil && token == architect
}

// ArchitectRuns is the owner walk's runs for a reader in a transaction (OwnerWalkOver): it reads
// in tx the architect claims of keys in one statement (Store.RoleClaims), and answers whether a
// token's claim holds its role as the store records it (supervise.HoldsRole, the whole rule for
// such a reader). A token the store holds no claim for holds no role, and neither does one of an
// issue outside keys, which the read did not cover. project is the claim project token
// (claim.ProjectToken); a key that makes no claim token has no claim, so it is left out of the
// read. The daemon's outbox reads the same rule over the machine's live view and also passes over
// a claim whose process a decision in flight is stopping (the daemon's holdsRole), which no record
// shows.
func ArchitectRuns(ctx context.Context, store Store, tx pgx.Tx, project string, keys []string) (func(claim.Token) bool, error) {
	tokens := make([]claim.Token, 0, len(keys))
	for _, key := range keys {
		if token, err := claim.NewToken(project, key, claim.RoleArchitect); err == nil {
			tokens = append(tokens, token)
		}
	}
	held, err := store.RoleClaims(ctx, tx, tokens)
	if err != nil {
		return nil, err
	}
	return func(token claim.Token) bool {
		c := held[token]
		return c != nil && supervise.HoldsRole(c.State, c.SuspendedBy)
	}, nil
}

// OwnerWalk is the owner walk over the Dispatch chains as a transaction reads them (ReadOwnerWalk,
// OwnerWalkOver), for the issues its reader named.
type OwnerWalk struct {
	project string
	chains  *Chains
	runs    func(claim.Token) bool
}

// OwnerWalkOver reads in tx, once, the architect claims the owner walk from each of keys over
// chains weighs: those of keys and of every link of their chains, as the store records them
// (ArchitectRuns). project is the claim project token (claim.ProjectToken), and chains a read that
// started from or reached each of keys (ReadChains).
func OwnerWalkOver(ctx context.Context, store Store, tx pgx.Tx, project string, chains *Chains, keys []string) (OwnerWalk, error) {
	asked := make([]string, 0, len(keys))
	for _, key := range keys {
		asked = append(append(asked, key), chains.Chain(key).Links...)
	}
	runs, err := ArchitectRuns(ctx, store, tx, project, asked)
	if err != nil {
		return OwnerWalk{}, err
	}
	return OwnerWalk{project: project, chains: chains, runs: runs}, nil
}

// ReadOwnerWalk reads in tx, once, what the owner walk from each of keys weighs: the Dispatch
// chains of keys (ReadChainsOf: the walk asks of each key its links, the issues above it in
// Legion's work and, once its chain is known, the state tree of the key or of its first such
// issue, each a question of the key's own chain) and the architect claims on them as the store
// records them (OwnerWalkOver).
func ReadOwnerWalk(ctx context.Context, store Store, tx pgx.Tx, dispatchProject string, keys []string) (OwnerWalk, error) {
	project, err := claim.ProjectToken(dispatchProject)
	if err != nil {
		return OwnerWalk{}, err
	}
	chains, err := ReadChainsOf(ctx, tx, store, dispatchProject, keys)
	if err != nil {
		return OwnerWalk{}, err
	}
	return OwnerWalkOver(ctx, store, tx, project, chains, keys)
}

// Owner is the key of the issue whose architect owns key, one of the keys the walk was read for
// (OwningIssue), "" where the walk meets an unknown chain.
func (w OwnerWalk) Owner(key string, fromParent bool) string {
	return OwningIssue(w.project, w.chains, key, fromParent, w.runs)
}

// Architect is the architect claim that owns key, one of the keys the walk was read for
// (OwningArchitect).
func (w OwnerWalk) Architect(key string, fromParent bool) (claim.Token, error) {
	return OwningArchitect(w.project, w.chains, key, fromParent, w.runs)
}
