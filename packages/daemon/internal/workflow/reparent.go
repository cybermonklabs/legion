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
// Before any of that, a change of parent that moves the issue's gate root forgets what its holder
// took of the approved design (forgetToldAcrossGateRoots), in or out of Legion's work, since that
// record names the documents of the gate it left.
//
// The engine does no I/O, so the read of whether the new parent opens R1's path waits on the
// seed's executor, whose fact queues the notices (reparentRead).
func (e *Engine) tellParentChange(ctx context.Context, tx pgx.Tx, issue *record.Issue, fact intake.DispatchIssue) error {
	var old string
	if issue.Parent != nil {
		old = *issue.Parent
	}
	if fact.Parent == old {
		return nil
	}
	if err := e.forgetToldAcrossGateRoots(ctx, tx, *issue, old); err != nil {
		return err
	}
	if (fact.Parent != "" && fact.Parent == issue.ToldParent) || !record.InLegionsWork(*issue) {
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

// forgetToldAcrossGateRoots forgets what issue's holder took of the approved design
// (record.Store.ForgetTold) where the change of its Dispatch parent from oldParent, which the copy
// already holds (the Mirror runs first), moves its gate root (record.Chains.GateRoot): the holder
// row's versions are of the documents of the gate the issue stood under, so under another gate
// root they name nothing of its documents, as main's move of an issue into another tree cleared
// the issue's generation. The gate root before is the issue itself where it holds its own gate in
// Legion's work, which no change of parent moves, else oldParent's gate root on the same reading,
// else, with no old parent, the issue itself. An old parent of another project was no issue above
// the issue (record.InProjectParent, the chain's project boundary for a link the copy no longer
// holds), so the issue stood as a root under it, and its gate root before is read as with no old
// parent. Where it forgets, an issue in Legion's work whose phase holder is now owed the new gate's
// approved versions has the reconcile of that tree's news queued (queueMovedNews), so its holder
// is told them. A move under the same gate root forgets nothing.
func (e *Engine) forgetToldAcrossGateRoots(ctx context.Context, tx pgx.Tx, issue record.Issue, oldParent string) error {
	above := record.InProjectParent(issue.Key, oldParent)
	keys := []string{issue.Key}
	if above != "" {
		keys = append(keys, above)
	}
	chains, err := record.ReadChains(ctx, tx, e.store, issue.Project, keys)
	if err != nil {
		return err
	}
	before := issue.Key
	if !(chains.HoldsOwnGate(issue.Key) && record.InLegionsWork(issue)) && above != "" {
		before = chains.GateRoot(above)
	}
	after := chains.GateRoot(issue.Key)
	if before == after {
		return nil
	}
	if err := e.store.ForgetTold(ctx, tx, issue.Key); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a change of parent moved an issue's gate root; what its holder took of the approved design is forgotten",
		"issue", issue.Key, "old_parent", oldParent, "gate_root_before", before, "gate_root_after", after)
	if !record.InLegionsWork(issue) {
		return nil
	}
	return e.queueMovedNews(ctx, tx, issue)
}

// TellParentAtFirstBoot is the first boot's notice of a root under a parent at work (step 11 of
// the conversion, decision 21 of dispatch://INFRA-4730; R21 of dispatch://INFRA-3320): the
// release before ran issue as a root of its own, and the copy its boot filled shows it under
// parent, an issue in Legion's work. R18's notice is told through
// the path a change of parent takes: one re-parent seed naming parent as the new parent and no
// old parent, queued as the parent is written into told_parent, so the issue's own architect and
// the one above it are told once, and a later listing or event that repeats the parent tells
// nobody again (tellParentChange). Nothing is told of an issue not in Legion's work. It answers
// whether it queued a seed.
func (e *Engine) TellParentAtFirstBoot(ctx context.Context, tx pgx.Tx, issue record.Issue, parent string) (bool, error) {
	if parent == "" || !record.InLegionsWork(issue) {
		return false, nil
	}
	seed, err := e.reparentSeed(ctx, tx, issue, "", parent)
	if err != nil {
		return false, err
	}
	issue.ToldParent = parent
	if err := e.store.PutIssue(ctx, tx, issue); err != nil {
		return false, err
	}
	e.logOnCommit(ctx, "workflow: the first boot tells the architects of a root the release before ran under a parent in Legion's work",
		"issue", issue.Key, "parent", parent, "gate_root", seed.Tree, "phase", issue.Phase)
	return true, e.enqueue(ctx, tx, issue.Key, seed)
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
	for _, step := range (record.TreeGate{Steps: e.cfg.Steps, Rows: gates}).Required() {
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
// reading the same words (reparentedWords). The seed's parents, not the issue's current parent,
// identify the two owner walks, so a later move does not erase the telling of an earlier one.
// When the latest move's new chain is unknown, no notices are queued until the chain is known
// (R16), whatever the issue's phase meanwhile: a stop does not end the telling a move made in
// Legion's work owes, and once the chain is known each architect is told in the seed's words, as
// for an issue in work (the architect's ruling,
// dispatch://INFRA-4730/comment/000d0b0f-743a-4130-8ba2-3c95b97d1ff7). Deferring them together
// keeps the words and the deduplication across recipients intact.
//
// A seed of INFRA-4728's shape, which a daemon of that release queued for a root that ran when a
// person made it a child of a live tree and left at this release's first boot, names no run: its
// old parent is the record's, where the record names one other than the seed's new parent, and
// what goes on is read from the record now. A later move makes that reconstruction unreliable,
// so only a seed of this older shape still requires its parent to match ToldParent.
func (e *Engine) reparentRead(ctx context.Context, tx pgx.Tx, fact intake.ReparentRead) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	seed := fact.Seed
	if seed.ReleaseBefore() {
		if issue.ToldParent != seed.Parent {
			return intake.Result{}, nil
		}
		var old string
		if issue.Parent != nil && *issue.Parent != seed.Parent {
			old = *issue.Parent
		}
		if seed, err = e.reparentSeed(ctx, tx, *issue, old, seed.Parent); err != nil {
			return intake.Result{}, err
		}
	} else if seed.OldParent == seed.Parent {
		// A seed that names no change has nothing to tell.
		return intake.Result{}, nil
	}
	architects, err := e.reparentArchitects(ctx, tx, *issue, seed)
	if err != nil {
		return intake.Result{}, err
	}
	if architects.waits {
		// A fact applied while the chain is still unknown must leave the telling owed.
		// The outbox waits on the replacement row until R16's re-read fills the copy.
		return intake.Result{}, e.enqueue(ctx, tx, issue.Key, seed)
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
	now := architects.now
	if issue.ToldParent != seed.Parent {
		// Still tell the recipients of that move, but don't ask them to act on it as if
		// the issue were still under that parent.
		now, version = "", 0
	}
	told := reparentedWords(*issue, seed, now, version)
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

// reparentWaitParent is the parent whose chain the seed waits for: the seed's new parent while the
// issue has been told of no later one (a later move supersedes the seed, and its own seed tells
// it) and while it is of the issue's project (record.InProjectParent: a parent of another project
// ends the chain, which is then known), "" for none. The wait holds whatever the issue's phase:
// R16 of dispatch://INFRA-3320 reads Dispatch again for every recorded issue whose chain is
// unknown, in Legion's work or out of it, until the chain is known (admit's noteUnknownChains, the
// controller tick's re-read), so the telling owed since the move waits for that read, out of work
// as in it.
func reparentWaitParent(issue record.Issue, seed record.ReparentSeed) string {
	if issue.ToldParent != seed.Parent {
		return ""
	}
	return record.InProjectParent(issue.Key, seed.Parent)
}

// ReparentChainPending lets the outbox keep the same seed on backoff while R16's re-read
// repairs the copy, rather than running the Dispatch read and queuing a replacement every time.
func ReparentChainPending(ctx context.Context, tx pgx.Tx, store record.Store, key string, seed record.ReparentSeed) (bool, error) {
	issue, err := store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return false, err
	}
	parent := reparentWaitParent(*issue, seed)
	if parent == "" {
		return false, nil
	}
	chains, err := record.ReadChainsOf(ctx, tx, store, issue.Project, []string{parent})
	if err != nil {
		return false, err
	}
	return reparentChainUnknown(*issue, seed, chains), nil
}

// reparentChainUnknown says whether seed waits on the chain of its wait parent
// (reparentWaitParent), which chains, read over that parent, shows still unknown.
func reparentChainUnknown(issue record.Issue, seed record.ReparentSeed, chains *record.Chains) bool {
	parent := reparentWaitParent(issue, seed)
	return parent != "" && !chains.Chain(parent).Known()
}

// reparentArchitects names the issue's own architect claim and the owners reached from the
// seed's two parents. Parents in another project are not part of the walk. waits says the chain
// of the seed's wait parent is still unknown (reparentChainUnknown), and then it names no one:
// the telling stays owed until R16's re-read fills the copy.
type reparentArchitects struct {
	own, before, now string
	waits            bool
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

func (e *Engine) reparentArchitects(ctx context.Context, tx pgx.Tx, issue record.Issue, seed record.ReparentSeed) (reparentArchitects, error) {
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return reparentArchitects{}, err
	}
	before := record.InProjectParent(issue.Key, seed.OldParent)
	now := record.InProjectParent(issue.Key, seed.Parent)
	var keys []string
	for _, key := range []string{before, now} {
		if key != "" {
			keys = append(keys, key)
		}
	}
	chains, err := record.ReadChainsOf(ctx, tx, e.store, issue.Project, keys)
	if err != nil {
		return reparentArchitects{}, err
	}
	if reparentChainUnknown(issue, seed, chains) {
		return reparentArchitects{waits: true}, nil
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
	ownerFrom := func(parent string) string {
		if parent != "" {
			if rec, recorded := chains.Record(parent); recorded && record.InLegionsWork(rec) {
				return walk.Owner(parent, false)
			}
			if len(chains.Above(parent)) > 0 {
				return walk.Owner(parent, true)
			}
		}
		return ""
	}
	told.before, told.now = ownerFrom(before), ownerFrom(now)
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
		fmt.Fprintf(&words, " The architect of %s now stands above it: it reads the spec of %s against its own requirements, as it reads a child's spec, and keeps it or ends it with its own status write in Dispatch.", now, issue.Key)
	}
	if version != 0 {
		fmt.Fprintf(&words, " %s carries the legion label and marks requirement blocks in its primary document. "+
			"A person approved version %d of that document, which is its latest version. So the architect of %s can write it as a view on %s. "+
			"Then that architect can register the design gate of %s on that approval, with no approval of its own, "+
			"with register_gate's basis.", seed.Parent, version, issue.Key, seed.Parent, issue.Key)
	}
	return words.String()
}
