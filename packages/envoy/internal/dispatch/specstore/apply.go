package specstore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specchange"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The refusals of an approval of a change request (D4), each a 409 of its code.
const (
	// RefusalStale is a node whose base is not the approved spec's current version: the approved
	// spec changed it after the owner wrote the request (R22), and the owner is told.
	RefusalStale = "SPEC_REQUEST_STALE"
	// RefusalParentMissing is a statement under a parent neither the approved spec nor the
	// request holds (R22; a parent the request proposes is taken).
	RefusalParentMissing = "SPEC_PARENT_MISSING"
	// RefusalLoop is a loop across the parent and support relations of the approved spec and the
	// request together (R5).
	RefusalLoop = "SPEC_LOOP"
	// RefusalChecksNotIncluded is a request that adds, changes or removes a check whose approval
	// request does not name its Verification document.
	RefusalChecksNotIncluded = "SPEC_CHECKS_NOT_INCLUDED"
	// RefusalNodeInvalid is a node the approved spec cannot hold: a block that reads as no node,
	// an id that is no id, or that two blocks hold, a removed id written again, a relation to a node
	// of no kind it may name, a node short of a field its kind needs, or a removal of a node the
	// approved spec never held.
	RefusalNodeInvalid = "SPEC_NODE_INVALID"
)

// Refusal is an approval the approved spec refuses: its code, its words, and what it names - the
// stale nodes of a stale request, or the problems of the others.
type Refusal struct {
	Code     string
	Detail   string
	Stale    []specchange.Stale
	Problems []specgraph.Problem
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Detail }

// invalidRules are the rules of specgraph.Validate a node of the approved spec must keep, beside
// no-parent and loop, which refuse with codes of their own. The others - a name of code in a
// text, a leaf or a supported statement with no check, a structure statement about nothing, a
// check with no failing example - are rules of the form of The Spec, which the reviewer of R25 and
// the form's own gate judge, and which an approved level held before any check was written.
var invalidRules = []string{
	"bad-id", "doubled-id", "retired-id", "no-text", "bad-kind", "missing-target",
	"check-serves", "no-claim", "no-procedure", "bad-subject", "bad-procedure", "no-test", "no-job",
	"test-on-non-program", "bad-selector", "bad-component", "bad-interface",
}

// Input is one approval of a change request of the issue Issue, of the repository Repo of
// Project: the request as the approved versions read it, whether the approval request named the
// issue's Verification document, the act's provenance, and who wrote it.
type Input struct {
	Project, Repo, Issue string
	Request              specchange.Request
	ChecksIncluded       bool
	Provenance           Provenance
	Who                  model.Actor
}

// Written is one fact an act wrote: a node's new version, or its removal.
type Written struct {
	ID      string
	Kind    string
	Version int
	Removed bool
	Act     string
}

// Applied is what an act wrote, at the one time it wrote it.
type Applied struct {
	At      time.Time
	Written []Written
}

// Queue queues one write step on the caller's write phase (docs.Ledger.Queue), or runs it.
type Queue func(step func(*store.WriteBatch) error) error

// RunNow is a Queue that runs each step at once in tx.
func RunNow(ctx context.Context, tx pgx.Tx) Queue {
	return func(step func(*store.WriteBatch) error) error { return store.RunWrite(ctx, tx, step) }
}

// Apply writes one approval of a change request into the approved spec of the repository, in the
// caller's transaction: under the repository's lock (Lock), it reads the approved spec, compares
// the request with it (specchange.Classify), and refuses (a *Refusal) a stale node, a node the
// approved spec cannot hold, a check the approval request left out, a statement under a missing
// parent and a loop, in that order, writing nothing. Otherwise it queues, after whatever the caller
// queued before, one write of every node the request adds, changes or removes, each with in's
// provenance and one time, and of each check a statement's earlier text brings back (R25); a node
// the request holds as context writes nothing. A node it writes whose statement an import held
// clears that hold.
func Apply(ctx context.Context, tx pgx.Tx, in Input, queue Queue) (Applied, error) {
	if err := Lock(ctx, tx, in.Project, in.Repo); err != nil {
		return Applied{}, err
	}
	state, err := Read(ctx, tx, in.Project, in.Repo)
	if err != nil {
		return Applied{}, err
	}
	approved, err := state.Approved()
	if err != nil {
		return Applied{}, err
	}
	// The issue's own acceptance checks are no part of the change request: the approval neither
	// writes them nor is refused for them (specchange.OfTheSpec, the one rule of it).
	in.Request = specchange.OfTheSpec(in.Request, approved)
	changes, stale, problems, err := specchange.Classify(in.Request, approved)
	if err != nil {
		return Applied{}, err
	}
	if len(stale) > 0 {
		specchange.SortStale(stale)
		return Applied{}, &Refusal{Code: RefusalStale, Detail: staleDetail(stale), Stale: stale}
	}
	problems = append(problems, nodeLosses(in.Request.Problems)...)
	problems = append(problems, doubledIDs(in.Request)...)
	if len(problems) > 0 {
		return Applied{}, &Refusal{Code: RefusalNodeInvalid, Detail: problemDetail(problems), Problems: problems}
	}
	if !in.ChecksIncluded {
		var checks []string
		for _, change := range changes {
			if change.IsCheck() && change.Writes() {
				checks = append(checks, change.ID)
			}
		}
		if len(checks) > 0 {
			return Applied{}, &Refusal{Code: RefusalChecksNotIncluded, Detail: fmt.Sprintf(
				"the request adds, changes or removes the checks %s, and its approval request does not name the issue's Verification document; open the request again with the Verification document beside the spec",
				strings.Join(checks, ", "))}
		}
	}
	plan := planChanges(state, changes)
	if refusal := judgeMerged(state, plan); refusal != nil {
		return Applied{}, refusal
	}
	if len(plan) == 0 {
		return Applied{}, nil
	}
	var at time.Time
	if err := tx.QueryRow(ctx, `select clock_timestamp()`).Scan(&at); err != nil {
		return Applied{}, fmt.Errorf("read the time of the approval: %w", err)
	}
	at = at.UTC()
	nodes, tuples, written, err := writesOf(state, plan, in.Provenance, in.Who, at)
	if err != nil {
		return Applied{}, err
	}
	holds, err := openHolds(ctx, tx, in.Project, in.Repo, written)
	if err != nil {
		return Applied{}, err
	}
	for _, hold := range holds {
		nodes = append(nodes, clearedHold(hold, in.Project, in.Repo, in.Who, at))
	}
	if err := queue(func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, nodes, tuples)
		return nil
	}); err != nil {
		return Applied{}, err
	}
	return Applied{At: at, Written: written}, nil
}

// planned is one node an act writes: its new fields, or its removal, and the act that wrote it -
// the request's, or the bring-back of a check with the statement whose earlier text brought it.
type planned struct {
	id              string
	node            any
	remove          bool
	broughtBackWith string
}

// planChanges is every write of changes, then each check the request's statements bring back with
// an earlier text of theirs (R25: "A change request that brings back an earlier approved text of a
// statement also brings back the checks of that text"): the checks that served the statement while
// that text stood, each at the text it held then, and the removal of each check that serves it now
// and did not then. A check the request itself writes or holds is left as the request has it.
func planChanges(state State, changes []specchange.Change) []planned {
	var plan []planned
	touched := map[string]bool{}
	for _, change := range changes {
		touched[change.ID] = true
		if change.Writes() {
			plan = append(plan, planned{id: change.ID, node: withoutPosition(change.Node), remove: change.What == specchange.Removed})
		}
	}
	for _, change := range changes {
		statement, ok := change.Node.(specgraph.Statement)
		if !ok || change.What != specchange.Changed {
			continue
		}
		history := state.Nodes[change.ID]
		earlier := -1
		for index := len(history.Versions) - 2; index >= 0; index-- {
			if node, err := DecodeNode(history.Versions[index].Kind, history.Versions[index].Node); err == nil && specchange.Digest(node) == specchange.Digest(withoutPosition(statement)) {
				earlier = index
				break
			}
		}
		if earlier < 0 {
			continue
		}
		// The checks of that text are those the approved spec held just before the act that
		// replaced it.
		replaced := history.Versions[earlier+1].At
		then := map[string]any{}
		for _, id := range state.sortedIDs() {
			if version := state.Nodes[id].AsOf(replaced); version != nil && version.Kind == specchange.KindCheck {
				if node, err := DecodeNode(version.Kind, version.Node); err == nil && node.(specgraph.Check).Serves == change.ID {
					then[id] = node
				}
			}
		}
		for _, id := range state.sortedIDs() {
			if touched[id] {
				continue
			}
			current := state.Nodes[id].Current()
			earlierCheck, served := then[id]
			switch {
			case served && (current == nil || !sameNode(current, earlierCheck)):
				plan = append(plan, planned{id: id, node: earlierCheck, broughtBackWith: change.ID})
				touched[id] = true
			case !served && current != nil && current.Kind == specchange.KindCheck:
				if node, err := DecodeNode(current.Kind, current.Node); err == nil && node.(specgraph.Check).Serves == change.ID {
					plan = append(plan, planned{id: id, remove: true, broughtBackWith: change.ID})
					touched[id] = true
				}
			}
		}
	}
	return plan
}

// sameNode is whether version holds node's fields.
func sameNode(version *Version, node any) bool {
	held, err := DecodeNode(version.Kind, version.Node)
	return err == nil && specchange.Digest(held) == specchange.Digest(node)
}

// judgeMerged judges the approved spec as the plan would leave it against the approved spec as it
// stands: each fault of specgraph.Validate the plan adds - a statement under no parent of the two,
// a loop, a fault of a node - refuses the approval, and a fault the approved spec already held does
// not. A removed id stays retired for every write but one: the check a statement's restored text
// brings back (R25), which takes that id's approved history up again rather than reusing it.
func judgeMerged(state State, plan []planned) *Refusal {
	current, retired := map[string]any{}, []string{}
	for _, id := range state.sortedIDs() {
		history := state.Nodes[id]
		if version := history.Current(); version != nil {
			if node, err := DecodeNode(version.Kind, version.Node); err == nil {
				current[id] = node
			}
		} else if history.Removed() {
			retired = append(retired, id)
		}
	}
	merged := make(map[string]any, len(current)+len(plan))
	for id, node := range current {
		merged[id] = node
	}
	// A node written again over its own removal stays among the retired ids, and Validate refuses
	// it as retired-id rather than take it, unless the plan brings it back with an earlier text of
	// its statement.
	broughtBack := map[string]bool{}
	for _, write := range plan {
		if write.remove {
			delete(merged, write.id)
			continue
		}
		merged[write.id] = write.node
		if write.broughtBackWith != "" {
			broughtBack[write.id] = true
		}
	}
	stillRetired := slices.DeleteFunc(slices.Clone(retired), func(id string) bool { return broughtBack[id] })
	before := map[string]bool{}
	for _, problem := range specgraph.Validate(treeOf(current), specgraph.Options{Retired: retired}) {
		before[problem.Node+"\x00"+problem.Detail] = true
	}
	var missing, loops, invalid []specgraph.Problem
	for _, problem := range specgraph.Validate(treeOf(merged), specgraph.Options{Retired: stillRetired}) {
		if before[problem.Node+"\x00"+problem.Detail] {
			continue
		}
		switch {
		case problem.Rule == "no-parent":
			missing = append(missing, problem)
		case problem.Rule == "loop":
			loops = append(loops, problem)
		case slices.Contains(invalidRules, problem.Rule):
			invalid = append(invalid, problem)
		}
	}
	switch {
	case len(invalid) > 0:
		return &Refusal{Code: RefusalNodeInvalid, Detail: problemDetail(invalid), Problems: invalid}
	case len(missing) > 0:
		return &Refusal{Code: RefusalParentMissing, Detail: problemDetail(missing) +
			"; propose the parent in the same request, or put the statement under a parent the approved spec holds", Problems: missing}
	case len(loops) > 0:
		return &Refusal{Code: RefusalLoop, Detail: problemDetail(loops), Problems: loops}
	}
	return nil
}

// treeOf is the tree of nodes, by id, as specgraph holds one, each at the position of its id.
func treeOf(nodes map[string]any) specgraph.Tree {
	var tree specgraph.Tree
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		pos := specgraph.Position{File: id, Line: 1}
		switch n := nodes[id].(type) {
		case specgraph.Statement:
			n.Pos = pos
			tree.Statements = append(tree.Statements, n)
		case specgraph.Check:
			n.Pos = pos
			tree.Checks = append(tree.Checks, n)
		case specgraph.Component:
			n.Pos = pos
			tree.Components = append(tree.Components, n)
		}
	}
	return tree
}

// writesOf is the nodes and facts of plan, at one time, with provenance, and what each wrote: a
// version numbered after the node's last, or the removal of its current version.
func writesOf(state State, plan []planned, provenance Provenance, who model.Actor, at time.Time) ([]graphstore.Node, []graphstore.Tuple, []Written, error) {
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	var written []Written
	for _, write := range plan {
		address := Address(state.Project, state.Repo, write.id)
		if !graphstore.AddressFits(address) {
			return nil, nil, nil, &Refusal{Code: RefusalNodeInvalid, Detail: fmt.Sprintf("the node %s is longer than an address of the store", write.id)}
		}
		act := provenance
		if write.broughtBackWith != "" {
			act.Act, act.BroughtBackWith = ActBroughtBack, write.broughtBackWith
		}
		history := state.Nodes[write.id]
		last := 0
		if history != nil && len(history.Versions) > 0 {
			last = history.Versions[len(history.Versions)-1].Version
		}
		var value any
		var attribute string
		entry := Written{ID: write.id, Act: act.Act}
		if write.remove {
			attribute, value = AttributeRemoved, Removal{Version: last, At: at, Provenance: act}
			entry.Removed, entry.Version, entry.Kind = true, last, history.Current().Kind
		} else {
			kind, fields, err := EncodeNode(write.node)
			if err != nil {
				return nil, nil, nil, err
			}
			attribute, value = AttributeVersion, Version{Version: last + 1, At: at, Kind: kind, Node: fields, Provenance: act}
			entry.Version, entry.Kind = last+1, kind
		}
		text, err := marshalValue(value)
		if err != nil {
			return nil, nil, nil, err
		}
		nodes = append(nodes, specNode(KindNode, address, state.Project, who, at))
		tuples = append(tuples, graphstore.Tuple{
			FromKind: KindNode, FromID: address, Kind: attribute, ToKind: graphstore.KindValue, ToID: text,
			At: at, Who: who, SourceEvent: graphstore.NoEvent, Asserted: true, Changes: true,
		})
		written = append(written, entry)
	}
	return nodes, tuples, written, nil
}

// specNode is the node of the approved spec, or of a hold, at address: project-visible and live,
// in its project, restated, so a write of a node already there moves nothing of it.
func specNode(kind, address, project string, who model.Actor, at time.Time) graphstore.Node {
	return graphstore.Node{
		Kind: kind, ID: address, Supertag: kind, Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
		Project: project, MadeAt: at, MadeBy: who, SourceEvent: graphstore.NoEvent, Restates: true, Changed: true,
	}
}

// nodeLosses is each problem of a request's blocks that loses the block's node, or names a base
// that is no version: a problem that keeps each node (a claim of two sentences) is the form's.
func nodeLosses(problems []specgraph.Problem) []specgraph.Problem {
	var losses []specgraph.Problem
	for _, problem := range problems {
		if problem.Effect != specgraph.KeepsEachNode {
			losses = append(losses, problem)
		}
	}
	return losses
}

// doubledIDs is a problem for each id two blocks of the request hold.
func doubledIDs(request specchange.Request) []specgraph.Problem {
	seen := map[string]string{}
	var problems []specgraph.Problem
	for _, proposal := range request.Proposals {
		if first, held := seen[proposal.ID]; held {
			problems = append(problems, specgraph.Problem{Pos: specgraph.Position{File: proposal.Block, Line: 1}, Node: proposal.ID,
				Rule: "doubled-id", Detail: "doubled-id: the request holds " + proposal.ID + " in two blocks, " + first + " and " + proposal.Block})
			continue
		}
		seen[proposal.ID] = proposal.Block
	}
	return problems
}

// staleDetail is the words of a stale refusal.
func staleDetail(stale []specchange.Stale) string {
	parts := make([]string, 0, len(stale))
	for _, node := range stale {
		switch {
		case node.RemovedNode:
			parts = append(parts, fmt.Sprintf("%s (written against version %d; the approved spec removed it)", node.ID, node.Base))
		case node.Base == 0:
			parts = append(parts, fmt.Sprintf("%s (written against no version; the approved spec holds version %d)", node.ID, node.Current))
		case node.Current == 0:
			parts = append(parts, fmt.Sprintf("%s (written against version %d; the approved spec holds none)", node.ID, node.Base))
		default:
			parts = append(parts, fmt.Sprintf("%s (written against version %d; the approved spec holds version %d)", node.ID, node.Base, node.Current))
		}
	}
	return "the approved spec changed after the request was written: " + strings.Join(parts, ", ") +
		"; write the request again against the approved spec and open a new approval request"
}

// problemDetail is the words of a refusal for its problems.
func problemDetail(problems []specgraph.Problem) string {
	parts := make([]string, 0, len(problems))
	for _, problem := range problems {
		parts = append(parts, problem.Node+": "+problem.Detail)
	}
	return strings.Join(parts, "; ")
}
