package plan

import (
	"fmt"
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
// typed blocks in document order, each id once, and parents the id of the block each task block
// stands directly under, by its id, as FromBlocks reads it (ParentOf); a block with no entry
// stands under no block. A task block whose parent is no task block of blocks is a task at the
// top: no parent, a typed block that is no task block, or an id no block holds, which FromBlocks
// keeps for Validate. A task block under a task with a sub-owner (`owner`) is a part of that task,
// and one under a task with no sub-owner is an element of it. A task block under an element is an
// element of that element's task. So a part of a part with a sub-owner is a part of it, at any
// depth.
//
// It errs where task blocks stand under each other in a cycle, which gives none of them a place,
// naming them from the first in the document's order, and where a task block stands under itself.
// The reason quotes each id (quoted) and lists at most MostReasons of them (listed).
//
// It takes time linear in the blocks: one walk up each chain settles every block it passes
// (cycleOf), and each place is read once (placeOf), so a plan of one long chain of parents, which
// a document of 1 MiB holds to about 14,000 blocks, reads in one pass
// (TestEachParentOfAPlanIsReadInLinearTime).
//
// Before Dispatch serves the tree of an issue, each parent comes from the block's `parent`
// (ParentOf), and the version Dispatch gives holds its typed blocks alone. So a `parent` that
// names a heading of the plan's document names an id no block holds, as a deleted block does, and
// Validate refuses it. That is the limit of the document's read: the tree read gives the node
// each block stands under, and a task block under a heading is then a task at the top.
func Places(blocks []Block, parents map[string]string) (map[string]Place, error) {
	tasks := make(map[string]Block, len(blocks))
	order := make(map[string]int, len(blocks))
	for at, block := range blocks {
		if block.Type == TaskBlockType {
			tasks[block.ID] = block
			order[block.ID] = at
		}
	}
	switch cycle := cycleOf(blocks, tasks, parents, order); {
	case len(cycle) == 1:
		return nil, fmt.Errorf("task block %s stands under itself, so it has no place in the plan", quoted(cycle[0]))
	case cycle != nil:
		names := make([]string, len(cycle))
		for i, id := range cycle {
			names[i] = quoted(id)
		}
		return nil, fmt.Errorf("task blocks %s stand under each other, so none of them has a place in the plan", listed(names))
	}
	places := make(map[string]Place, len(tasks))
	var placeOf func(id string) Place
	placeOf = func(id string) Place {
		if place, ok := places[id]; ok {
			return place
		}
		place := Place{Kind: PlaceTop}
		if above, ok := tasks[parents[id]]; ok {
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

// cycleOf is the task blocks of the cycle of parents whose first block in the document's order
// (order) comes first of every cycle's, in the order of the chain from that block, or nil where no
// chain of parents closes a cycle. tasks is the version's task blocks by id, and parents each
// one's parent (Places). It walks up from each task block in the document's order and stops at a
// block an earlier walk settled, so each block joins one walk: a walk that meets a block of its
// own closes the cycle from that block on, and every block of a walk is settled at its end. So it
// takes time linear in the blocks, where a walk from every block to the top that scanned the chain
// it had walked at each step took time cubic in a chain.
func cycleOf(blocks []Block, tasks map[string]Block, parents map[string]string, order map[string]int) []string {
	const walking, settled = 1, 2
	state := make(map[string]int8, len(tasks))
	at := make(map[string]int, len(tasks))
	var first []string
	var walk []string
	for _, block := range blocks {
		if block.Type != TaskBlockType || state[block.ID] != 0 {
			continue
		}
		walk = walk[:0]
		for id := block.ID; ; {
			if state[id] == settled {
				break
			}
			if state[id] == walking {
				if cycle := fromFirst(walk[at[id]:], order); first == nil || order[cycle[0]] < order[first[0]] {
					first = cycle
				}
				break
			}
			state[id], at[id] = walking, len(walk)
			walk = append(walk, id)
			above, ok := tasks[parents[id]]
			if !ok {
				break
			}
			id = above.ID
		}
		for _, id := range walk {
			state[id] = settled
		}
	}
	return first
}

// fromFirst is cycle, a chain of parents that closes back at its start, begun at its block that
// comes first in the document's order (order), in a slice of its own.
func fromFirst(cycle []string, order map[string]int) []string {
	start := 0
	for i, id := range cycle {
		if order[id] < order[cycle[start]] {
			start = i
		}
	}
	return append(append(make([]string, 0, len(cycle)), cycle[start:]...), cycle[:start]...)
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
		return "", fmt.Errorf("its parent %q is not one dispatch:// reference", quoted(value))
	case !strings.HasPrefix(words[0], document+"#b-"):
		return "", nil
	}
	id := neededID(words[0])
	if id == "" {
		return "", fmt.Errorf("its parent %q names a block of this document by an id that does not decode", quoted(value))
	}
	return id, nil
}
