package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// reparentRan is what goes on, in the words the release before writes into its notice of a
// re-parent seed (record.ReparentSeed.Ran): a change of parent moves nothing of the issue's own
// (R17 of dispatch://INFRA-3320).
const reparentRan = "its run, its phase, its slot and its design-gate steps stay with it"

// tellParentChange queues R18's notice of a change of parent (dispatch://INFRA-3320): fact moves
// the Dispatch parent of issue, an issue in Legion's work, from the parent the record held before
// the event to one it has not been told of (record.Issue.ToldParent), and the engine queues one
// re-parent seed (record.ReparentSeed) and writes the new parent into told_parent in this same
// transaction. It runs before admission records the event (admit's recordObservation), so the
// record's Parent is still the old parent, while the copy already holds the new one (the Mirror
// runs first), which is the chain the seed's notices are routed on.
//
// told_parent is the once-mark: a listing or an event that repeats the same parent tells nobody
// again, and a move back to an earlier parent is told anew. A move to no parent is told whenever
// it is a change, since "" is also the mark of an issue never told. Nothing is told of an issue
// not in Legion's work (record.InLegionsWork): no architect acts on its run.
//
// The engine does no I/O, so the read of whether the new parent opens R1's path waits on the
// seed's executor, whose fact queues the notices (reparentRead).
func (e *Engine) tellParentChange(ctx context.Context, tx pgx.Tx, issue *record.Issue, fact intake.DispatchIssue) error {
	var old string
	if issue.Parent != nil {
		old = *issue.Parent
	}
	if fact.Parent == old || (fact.Parent != "" && fact.Parent == issue.ToldParent) || !record.InLegionsWork(*issue) {
		return nil
	}
	seed, err := e.reparentSeed(ctx, tx, *issue, old, fact.Parent)
	if err != nil {
		return err
	}
	issue.ToldParent = fact.Parent
	if err := e.store.PutIssue(ctx, tx, *issue); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: the Dispatch parent of an issue in Legion's work changed; the new parent is read before the architects are told",
		"issue", issue.Key, "old_parent", old, "new_parent", fact.Parent, "gate_root", seed.Tree, "phase", issue.Phase)
	return e.enqueue(ctx, tx, issue.Key, seed)
}

// reparentSeed is the seed of issue's move from oldParent to newParent as the record holds the
// issue now: its run and the phase entry of the role that works its phase (0 where that role has
// entered none), the phase under way, whether it holds an admission slot, each design-gate step
// it holds itself with its basis, in step order, and the gate root on its chain as the copy holds
// it now (record.Chains.StateTree), which the release before tells.
func (e *Engine) reparentSeed(ctx context.Context, tx pgx.Tx, issue record.Issue, oldParent, newParent string) (record.ReparentSeed, error) {
	seed := record.ReparentSeed{Parent: newParent, Ran: reparentRan, Generation: issue.Generation, OldParent: oldParent, Phase: issue.Phase}
	entries, err := e.store.PhaseEntries(ctx, tx, []string{issue.Key})
	if err != nil {
		return record.ReparentSeed{}, err
	}
	seed.Entry = entries[record.PhaseRole{Issue: issue.Key, Role: phase.RoleFor(record.StandsAt(issue))}].Entry
	slots, err := e.store.Slots(ctx, tx)
	if err != nil {
		return record.ReparentSeed{}, err
	}
	seed.Slot = slices.ContainsFunc(slots, func(slot record.Slot) bool { return slot.Issue == issue.Key })
	gates, err := e.store.GatesOf(ctx, tx, issue.Key)
	if err != nil {
		return record.ReparentSeed{}, err
	}
	for _, step := range classify.RequiredSteps(e.cfg.Steps) {
		gate, held := gates[step]
		if !held {
			continue
		}
		stated := record.ReparentStep{Step: step, Open: classify.DesignGateOpen(gate)}
		if gate.Basis != nil {
			stated.Basis, stated.BasisVersion, stated.Pending = gate.Basis.Issue, gate.Basis.Version, gate.PendingOnBasis()
		}
		seed.Steps = append(seed.Steps, stated)
	}
	chains, err := record.ReadChains(ctx, tx, e.store, issue.Project, []string{issue.Key})
	if err != nil {
		return record.ReparentSeed{}, err
	}
	seed.Tree = chains.StateTree(issue.Key)
	return seed, nil
}

// reparentRead tells what the re-parent seed read of the new parent of an issue whose parent
// changed (intake.ReparentRead, R18 of dispatch://INFRA-3320): one reparented notice
// (record.ReparentedNotice) for each distinct architect among the issue's own, the one above it
// before and the one above it now, each naming the one it tells (record.Notice.To) and each
// reading the same words (reparentedWords). Only a move the record is still told of is told: a
// move under another parent since queued a seed of its own.
//
// The architects are read on the Dispatch chain in this transaction, by the owner walk over the
// claims the store records (record.OwnerWalkOver): the issue's own is told where the store records
// an architect claim of it; the one above it now is the walk from its parent on the chain the copy
// holds now, where an issue above it is in Legion's work; the one above it before is the walk from
// the old parent, where that parent, or an issue above it, is in Legion's work. A walk that meets
// an unknown chain names nobody, and the line the telling writes says so.
//
// A seed of INFRA-4728's shape, which a daemon of that release queued for a root that ran when a
// person made it a child of a live tree and left at this release's first boot, names no run: its
// old parent is the record's, where the record names one other than the seed's new parent, and
// what goes on is read from the record now.
func (e *Engine) reparentRead(ctx context.Context, tx pgx.Tx, fact intake.ReparentRead) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil || issue.ToldParent != fact.Seed.Parent {
		return intake.Result{}, err
	}
	seed := fact.Seed
	if seed.ReleaseBefore() {
		var old string
		if issue.Parent != nil && *issue.Parent != seed.Parent {
			old = *issue.Parent
		}
		if seed, err = e.reparentSeed(ctx, tx, *issue, old, seed.Parent); err != nil {
			return intake.Result{}, err
		}
	}
	version := fact.BasisVersion
	if seed.Parent != "" && version != 0 {
		parent, err := e.store.Issue(ctx, tx, seed.Parent)
		if err != nil {
			return intake.Result{}, err
		}
		if parent != nil && record.Accepted(*parent) {
			version = 0
		}
	}
	architects, err := e.reparentArchitects(ctx, tx, *issue, seed.OldParent)
	if err != nil {
		return intake.Result{}, err
	}
	told := reparentedWords(*issue, seed, architects.now, version)
	for _, to := range architects.distinct() {
		if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: record.ReparentedNotice, Phase: issue.Phase, To: to, Reason: told}); err != nil {
			return intake.Result{}, err
		}
	}
	e.logOnCommit(ctx, "workflow: the architects of an issue whose Dispatch parent changed are told", "issue", issue.Key,
		"old_parent", seed.OldParent, "new_parent", seed.Parent, "own", architects.own, "above_before", architects.before, "above_now", architects.now,
		"basis_version", version)
	return intake.Result{}, nil
}

// reparentArchitects is the issues whose architects a change of parent tells (R18): the issue's
// own, "" where the store records no architect claim of it; the architect above it before, the
// owner walk from oldParent, "" where neither oldParent nor an issue above it is in Legion's work;
// and the architect above it now, the owner walk from the issue's parent, "" where no issue above
// it is in Legion's work. A walk that meets an unknown chain is "" too.
type reparentArchitects struct {
	own, before, now string
}

// distinct is each architect the telling names, once, in the order own, before, now.
func (a reparentArchitects) distinct() []string {
	var told []string
	for _, key := range []string{a.own, a.before, a.now} {
		if key != "" && !slices.Contains(told, key) {
			told = append(told, key)
		}
	}
	return told
}

func (e *Engine) reparentArchitects(ctx context.Context, tx pgx.Tx, issue record.Issue, oldParent string) (reparentArchitects, error) {
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return reparentArchitects{}, err
	}
	keys := []string{issue.Key}
	if oldParent != "" {
		keys = append(keys, oldParent)
	}
	chains, err := record.ReadChains(ctx, tx, e.store, issue.Project, keys)
	if err != nil {
		return reparentArchitects{}, err
	}
	walk, err := record.OwnerWalkOver(ctx, e.store, tx, project, chains, keys)
	if err != nil {
		return reparentArchitects{}, err
	}
	var told reparentArchitects
	own, err := claim.NewToken(project, issue.Key, claim.RoleArchitect)
	if err != nil {
		return reparentArchitects{}, err
	}
	claims, err := e.store.RoleClaims(ctx, tx, []claim.Token{own})
	if err != nil {
		return reparentArchitects{}, err
	}
	if claims[own] != nil {
		told.own = issue.Key
	}
	if oldParent != "" {
		if parent, recorded := chains.Record(oldParent); recorded && record.InLegionsWork(parent) {
			told.before = walk.Owner(oldParent, false)
		} else if len(chains.Above(oldParent)) > 0 {
			told.before = walk.Owner(oldParent, true)
		}
	}
	if len(chains.Above(issue.Key)) > 0 {
		told.now = walk.Owner(issue.Key, true)
	}
	return told, nil
}

// reparentedWords is what each architect a change of parent tells reads (R18 of
// dispatch://INFRA-3320): the old and the new parent, what goes on, which stays (R17): the phase
// under way, the slot it holds, and the design-gate steps it holds with their basis; what the
// architect above it now does with it, where one is (now), as it reads a child's spec; and,
// where version is set, R1's path on the new parent at that version of its primary document.
func reparentedWords(issue record.Issue, seed record.ReparentSeed, now string, version int) string {
	parent := func(key string) string {
		if key == "" {
			return "no parent"
		}
		return key
	}
	var words strings.Builder
	fmt.Fprintf(&words, "The Dispatch parent of %s changed from %s to %s. Its run goes on as it was, in phase %s", issue.Key, parent(seed.OldParent), parent(seed.Parent), seed.Phase)
	if seed.Slot {
		words.WriteString(", holding its admission slot")
	} else {
		words.WriteString(", holding no admission slot")
	}
	if len(seed.Steps) == 0 {
		words.WriteString(", with no design-gate step of its own.")
	} else {
		steps := make([]string, 0, len(seed.Steps))
		for _, step := range seed.Steps {
			stated := string(step.Step)
			switch {
			case step.Pending:
				stated += fmt.Sprintf(" waiting on the approval of version %d of %s's document", step.BasisVersion, step.Basis)
			case step.Basis != "" && step.Open:
				stated += fmt.Sprintf(" open on the approval of version %d of %s's document", step.BasisVersion, step.Basis)
			case step.Open:
				stated += " open"
			default:
				stated += " shut"
			}
			steps = append(steps, stated)
		}
		fmt.Fprintf(&words, ", and its design-gate steps stay as they stand: %s.", strings.Join(steps, "; "))
	}
	if now != "" {
		fmt.Fprintf(&words, " The architect of %s now stands above it: it reads the spec of %s against its own requirements, as it reads a child's spec, and keeps it or takes it out with park_child.", now, issue.Key)
	}
	if version != 0 {
		fmt.Fprintf(&words, " %s carries the legion label and marks requirement blocks in its primary document. "+
			"A person approved version %d of that document, which is its latest version. So the architect of %s can write it as a view on %s. "+
			"Then that architect can register the design gate of %s on that approval, with no approval of its own, "+
			"with register_gate's basis.", seed.Parent, version, issue.Key, seed.Parent, issue.Key)
	}
	return words.String()
}
