package plan

import (
	"fmt"
	"slices"
	"strings"
)

// PlaceKind says where a task block stands in its plan.
type PlaceKind string

const (
	PlaceTop     PlaceKind = "top"     // a task at the top: Legion carries it out
	PlacePart    PlaceKind = "part"    // a part of a task with a sub-owner: Legion carries it out
	PlaceElement PlaceKind = "element" // an element of a task's plan: Legion does not carry it out
)

// Place is where one task block stands: Kind, and Task, the task it is a part or an element of.
// Task is "" at the top. For a part it is the task with a sub-owner that the block stands
// directly under. For an element it is the nearest block above it that Legion carries out.
type Place struct {
	Kind PlaceKind
	Task string
}

// Places is where each task block of one version stands, by its id. blocks is the version's
// typed blocks in document order, each id once, each task block's Parent set by its reader. A
// task block whose Parent is no task block of blocks is a task at the top: no Parent, a typed
// block that is no task block, or an id no block holds, which FromBlocks keeps for Validate. A
// task block under a task with a sub-owner (`owner`) is a part of that task, and one under a task
// with no sub-owner is an element of it. A task block under an element is an element of that
// element's task. So a part of a part with a sub-owner is a part of it, at any depth.
//
// It errs where task blocks stand under each other in a cycle, which gives none of them a place,
// naming them from the first in the document's order, and where a task block stands under itself.
//
// Before Dispatch serves the tree of an issue, each Parent comes from the block's `parent`
// (ParentOf), and the version Dispatch gives holds its typed blocks alone. So a `parent` that
// names a heading of the plan's document names an id no block holds, as a deleted block does, and
// Validate refuses it. That is the limit of the document's read: the tree read gives the node
// each block stands under, and a task block under a heading is then a task at the top.
func Places(blocks []Block) (map[string]Place, error) {
	tasks := make(map[string]Block, len(blocks))
	for _, block := range blocks {
		if block.Type == TaskBlockType {
			tasks[block.ID] = block
		}
	}
	switch cycle := firstParentCycle(blocks, tasks); {
	case len(cycle) == 1:
		return nil, fmt.Errorf("task block %s stands under itself, so it has no place in the plan", cycle[0])
	case cycle != nil:
		return nil, fmt.Errorf("task blocks %s stand under each other, so none of them has a place in the plan", strings.Join(cycle, ", "))
	}
	places := make(map[string]Place, len(tasks))
	var placeOf func(id string) Place
	placeOf = func(id string) Place {
		if place, ok := places[id]; ok {
			return place
		}
		place := Place{Kind: PlaceTop}
		if above, ok := tasks[tasks[id].Parent]; ok {
			switch upper := placeOf(above.ID); {
			case upper.Kind == PlaceElement:
				place = Place{Kind: PlaceElement, Task: upper.Task}
			case above.Attributes["owner"] != "":
				place = Place{Kind: PlacePart, Task: above.ID}
			default:
				place = Place{Kind: PlaceElement, Task: above.ID}
			}
		}
		places[id] = place
		return place
	}
	for id := range tasks {
		placeOf(id)
	}
	return places, nil
}

// firstParentCycle is the task blocks of the cycle of parents through the first block of blocks,
// in the document's order, that stands on one, in the order of the chain from that block, or nil
// where no task block's chain of parents closes a cycle. tasks is the version's task blocks by id.
// A chain that runs into a cycle it is not on is no cycle of its own. Each block is walked once:
// a walk up from a block stops at a block an earlier walk passed, so blocks nested as deep as the
// document holds cost one pass, and a block's parent chains meet at most one cycle, since each
// block has one parent. A cycle a walk closes is named from its first block in the document.
func firstParentCycle(blocks []Block, tasks map[string]Block) []string {
	order := make(map[string]int, len(blocks))
	for i, block := range blocks {
		if _, seen := order[block.ID]; !seen {
			order[block.ID] = i
		}
	}
	const (
		unwalked = iota
		walking
		walked
	)
	state := make(map[string]int, len(tasks))
	var first []string
	for _, block := range blocks {
		if block.Type != TaskBlockType || state[block.ID] != unwalked {
			continue
		}
		var chain []string
		at, ok := block.ID, true
		for ok && state[at] == unwalked {
			state[at] = walking
			chain = append(chain, at)
			var above Block
			above, ok = tasks[tasks[at].Parent]
			at = above.ID
		}
		if ok && state[at] == walking {
			cycle := chain[slices.Index(chain, at):]
			start := 0
			for i, id := range cycle {
				if order[id] < order[cycle[start]] {
					start = i
				}
			}
			cycle = append(slices.Clone(cycle[start:]), cycle[:start]...)
			if first == nil || order[cycle[0]] < order[first[0]] {
				first = cycle
			}
		}
		for _, id := range chain {
			state[id] = walked
		}
	}
	return first
}

// ParentOf reads a task block's `parent` value before Dispatch serves the tree: the id of the
// block of document it names, and "" where it is empty or names anything else. document is the
// plan's document as a reference to one of its blocks names it before its `#b-`, as Dispatch
// writes one (FormatBlockRef in packages/envoy/internal/dispatch/text): `dispatch://<issue>/spec`
// for the issue's primary document, `dispatch://<issue>/artifact/<slug>` for any other. So each
// form names its own document alone, and a block of another document or another issue, or an ask,
// a comment or a message, names no block of document, whatever its id. The id is read as a need's
// is (neededID): after the last `#b-`, with a pin left out, percent-decoded. It errs on a value
// that is not one `dispatch://` reference, and on a reference to a block of document whose id does
// not decode.
func ParentOf(value, document string) (string, error) {
	words := strings.Fields(value)
	switch {
	case len(words) == 0:
		return "", nil
	case len(words) > 1 || !strings.HasPrefix(words[0], "dispatch://"):
		return "", fmt.Errorf("its parent %q is not one dispatch:// reference", value)
	case !strings.HasPrefix(words[0], document+"#b-"):
		return "", nil
	}
	id := neededID(words[0])
	if id == "" {
		return "", fmt.Errorf("its parent %q names a block of this document by an id that does not decode", value)
	}
	return id, nil
}
