package admit

import (
	"slices"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// covers is what the cap lends under a slot rule whose start may take a cover (record.TakeCover,
// record.SlotRule.Take): an issue of the project that holds one of the cap's slots and waits on
// another issue lends its slot to one issue it waits on, which then starts on that slot whatever
// the cap says, and the row it takes counts against none of the cap's own, since the slot it
// works on is the holder's. A holder lends to one issue at most, and an issue works on one
// holder's slot at most.
//
// What a holder waits on is read from the mirror and the children-first judgement the pass
// already holds — its unfinished Dispatch blockers (blockers.Unfinished) and the open children
// its first phase waits for (record.LineFacts.ChildWaits) — and not from the release for a wait
// (blockers.waitsOf): a holder lends exactly where it keeps its slot through the wait, which is
// a root whose architect writes its design, or any holder under a rule whose wait gives no slot
// back, and those are the waits waitsOf leaves out.
//
// It is read again on every judgement of the line from the pass's own slots, rows and facts, and
// stored nowhere: a cover stands while its holder holds its slot and waits on the issue and that
// issue holds a slot of its own, and each of those is a record the next pass reads. Under every
// other take it is the zero value, which lends nothing, so capLets reads the cap alone, as the
// daemon's own rule does.
type covers struct {
	// offered is, for each issue a holder waits on that holds no slot, the holders that wait on
	// it and lend to nothing yet, in slot order.
	offered map[string][]string
	// lenders is the holders whose slot an issue works on now, by key.
	lenders map[string]struct{}
	// lent is the number of those holders: the rows the cap counts that stand on a holder's slot
	// rather than on one of the cap's own (capLets takes them off the rows held).
	lent int
}

// coversOf is the covers of own, the project's slots in slot order, under the rule facts carry:
// the zero covers, which lend nothing, under any take but record.TakeCover. Each holder in turn
// lends to the first issue it waits on that holds a slot and neither works on another holder's
// nor lends its own, which is a cover already in use; a holder with no such issue offers its
// slot to each issue it waits on that holds none, for the promotion to start on it
// (covers.take). No issue both lends its slot and works on another's, so no slot is lent twice
// and no row is taken off the count twice.
func coversOf(own []record.Slot, rows map[string]blockers.Row, facts record.LineFacts) covers {
	if facts.Rule.OrMain().Take != record.TakeCover || len(own) == 0 {
		return covers{}
	}
	holding := make(map[string]struct{}, len(own))
	for _, slot := range own {
		holding[slot.Issue] = struct{}{}
	}
	waitsOn := make(map[string][]string, len(own))
	for _, slot := range own {
		waits := blockers.Unfinished(rows, slot.Issue)
		for _, child := range facts.ChildWaits[slot.Issue].Open {
			if !slices.Contains(waits, child.Key) {
				waits = append(waits, child.Key)
			}
		}
		waitsOn[slot.Issue] = waits
	}
	c := covers{offered: make(map[string][]string), lenders: make(map[string]struct{})}
	covered := make(map[string]struct{}, len(own))
	for _, slot := range own {
		if _, works := covered[slot.Issue]; works {
			continue
		}
		for _, key := range waitsOn[slot.Issue] {
			_, holds := holding[key]
			_, taken := covered[key]
			_, lends := c.lenders[key]
			if !holds || taken || lends || key == slot.Issue {
				continue
			}
			covered[key] = struct{}{}
			c.lenders[slot.Issue] = struct{}{}
			c.lent++
			break
		}
	}
	for _, slot := range own {
		_, lends := c.lenders[slot.Issue]
		_, works := covered[slot.Issue]
		if lends || works {
			continue
		}
		for _, key := range waitsOn[slot.Issue] {
			if _, holds := holding[key]; !holds {
				c.offered[key] = append(c.offered[key], slot.Issue)
			}
		}
	}
	return c
}

// offers says whether a holder offers its slot to key: some holder waits on key and lends to
// nothing yet.
func (c covers) offers(key string) bool {
	_, found := c.holderFor(key)
	return found
}

// take starts key on the slot of the first holder that offers one to it, if any does: that
// holder lends to key from now on and offers its slot to nothing else, and the row key takes is
// one more row standing on a holder's slot (lent). The promotion calls it on every row it grants,
// whichever half of the cap let the candidate through, so a candidate a holder waits on never
// costs the cap a row of its own while a holder offers one.
func (c *covers) take(key string) {
	holder, found := c.holderFor(key)
	if !found {
		return
	}
	c.lenders[holder] = struct{}{}
	c.lent++
}

// holderFor is the first holder, in slot order, that waits on key and lends to nothing yet.
func (c covers) holderFor(key string) (string, bool) {
	for _, holder := range c.offered[key] {
		if _, lends := c.lenders[holder]; !lends {
			return holder, true
		}
	}
	return "", false
}
