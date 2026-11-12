package legion

// tree.go holds the one step every tree walk takes (TreeAbove) and the one rule that places an
// issue in a tree (TreeChain and TreeRoot, dispatch://INFRA-1077). The Legion view and the four
// metrics both place an issue in a tree — the view to tell a root from a child and to count a
// root's unstarted children, the metrics to scope a filing and a root's wait on its children —
// and one rule stated twice is two rules: a board that puts an issue in one tree while the
// metrics put it in another is a view and a card that disagree about the same issue.
//
// A walk that states which rows a computation must read takes the same step under a stopping
// rule of its own, written in that walk's own doc. That is a second rule with a name and a
// reason of its own, and not a copy of this one.

// TreeNode is what the walk reads of one issue: the tree root the daemon named for it, the parent
// Dispatch records, and whether the walk may climb through it from a child below.
//
// Tree is the daemon's own `tree` for the issue, which names its top issue outright. It is a fact
// of the project and not of any window, so a population answers it for every issue the daemon
// named one for, not for the issues one report happens to read. It is empty where no read carried
// one, which is every daemon before dispatch://INFRA-980.
//
// Legion is one rule, the same for every reader: an issue is a Legion issue when the daemon
// reports it — it is in the daemon's state, or Dispatch observed a phase of it — or when it
// carries Legion phase work, a worker event or a handoff (dispatch://INFRA-1077's spec, "The top
// issue of a tree"). A population answers it for the issues it holds and false for the rest. It
// is not the rule that picks which finished issues a metric counts, which needs phase work.
// TreeChain is the one walk that reads this field, and where the reading makes that walk stop is
// stated once, in TreeChain's own words.
//
// Both halves are facts of the project and not of any window, so both readers read them
// project-wide: the daemon's reports from the whole of what the sampler recorded, and the phase
// work from one read of every handoff and every Legion role session's event (PhaseWorkKeys).
// A window narrows which issues a metric measures, never which issues are Legion's.
type TreeNode struct {
	Tree   string
	Parent string
	Legion bool
}

// TreeRoot is the issue's tree root: the issue the walk below reaches last. Each step of that
// walk is TreeAbove, and where the walk stops, which is what makes one issue the root and
// another a child, is stated once in TreeChain's doc. Nothing here reads a slot: a slot holder
// is not a root.
//
// lookup answers what the population holds for a key, and false for a key it does not hold.
func TreeRoot(key string, lookup func(key string) (TreeNode, bool)) string {
	chain := TreeChain(key, lookup)
	return chain[len(chain)-1]
}

// TreeAbove is the one step every tree walk takes: the issue above key, and "" where no issue
// stands above it. The daemon's own `tree` names the top issue of the tree outright, so the step
// reads it before any parent link, and an issue the daemon reports as its own tree has nothing
// above it. Where no read carried a `tree`, the step takes the parent Dispatch records.
//
// fromParent is whether the step took the parent link, and it is false where the step took a
// reported `tree` or found nothing above the issue. A walk that treats the two links
// differently — TreeChain stops below a parent that is not a Legion issue, and takes a
// reported `tree` whatever the issue it names — branches on this answer and never reads the
// fields a second time, so the order the step reads them in is stated here alone.
//
// The step carries no stopping rule beyond "no issue stands above this one". Where a walk stops
// is the walk's own rule, because the two walks over this step stop at different issues and for
// different reasons:
//
//   - TreeChain places an issue in one tree, and it stops below a parent that is not a Legion
//     issue. The rule and the reason for it are in TreeChain's doc below.
//   - The scope walks state which rows a computation must read, and they climb through a parent
//     that is not a Legion issue. The rule and the reason for it are in the doc of each walk
//     that holds it, in legion/metrics and in legionfacts.
//
// Each of the two rules is written once, where it holds. A caller that needs another stopping
// rule writes a third rule with a name and a reason of its own, and never a copy of one of these
// two.
func TreeAbove(key string, node TreeNode) (above string, fromParent bool) {
	if node.Tree != "" {
		if node.Tree == key {
			return "", false
		}
		return node.Tree, false
	}
	if node.Parent == "" {
		return "", false
	}
	return node.Parent, true
}

// TreeChain is the walk TreeRoot makes, kept: the issue itself, then every issue the walk climbs
// through, ending at the tree root, which is why TreeRoot is its last entry. A reader that must
// place something on the nearest issue above another — the Legion view counts an unstarted child
// on the nearest issue of its tree that carries a card — reads the chain rather than stating the
// climb a second time.
//
// Each step is TreeAbove, the one step every tree walk takes. Where this walk stops is this
// walk's own rule, and the four stops below are the whole of it:
//
//  1. At an issue lookup does not hold. Nothing is known of what stands above such an issue.
//  2. At an issue TreeAbove names nothing above, which is an issue with no `tree` and no parent,
//     and an issue the daemon reports as its own tree.
//  3. Below a parent that lookup does not hold or that is not a Legion issue, where the step
//     answered that it took the parent link. This is the placement stop, and placement is what
//     it is for: a chain that climbed through such a parent would place the issue in the tree
//     of the tracking issues above Legion's work, and a board card and a metric's row would
//     then name a root no Legion worker ever worked. A `tree` the daemon reported is taken
//     whether or not the issue it names is a Legion issue, because the daemon named the top
//     issue of its own tree and the placement stop has nothing to add to it.
//  4. At a key the walk has already visited, so a parent chain that loops and a `tree` that
//     points back into the chain both stop rather than running to a depth cap.
//
// A walk that states a scope rather than places an issue stops by another rule, which is
// written in that walk's own doc.
func TreeChain(key string, lookup func(key string) (TreeNode, bool)) []string {
	chain := []string{key}
	seen := map[string]bool{}
	for {
		current := chain[len(chain)-1]
		if seen[current] {
			return chain
		}
		seen[current] = true
		node, known := lookup(current)
		if !known {
			return chain
		}
		above, fromParent := TreeAbove(current, node)
		if above == "" {
			return chain
		}
		if fromParent {
			// The step took the parent link, so the placement stop reads the parent it named.
			if up, known := lookup(above); !known || !up.Legion {
				return chain
			}
		}
		chain = append(chain, above)
	}
}
