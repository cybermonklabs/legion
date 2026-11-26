package refs

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// R5: the backfill's catch-up of an older build's edit or delete retracts what the item's text no
// longer names as the item index retracts it (ItemUnnamedMentions, unnamedMentions): of the
// identities the item's node mentions, each the text does not name, in (kind, id) order, made by
// the node's maker at the write's moment and stated under its event, which each carries as its
// source_event and its legacy_source_seq. A delivery route of a comment is kept though no text names
// it, an identity the text spells only in code is retracted, and one the text names stands. A
// session's and a role's retraction is planned beside the node's other facts, and a person's for
// graphstore.ToPerson. A delete's empty text names nobody, so it retracts every mention but the
// route's.
func TestTheRetractionOfWhatAnItemsTextNoLongerNamesKeepsItsRoutes(t *testing.T) {
	at := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	maker := model.Actor{Kind: "user", ID: "alice"}
	const (
		event  = int64(42)
		named  = "019a1c0b-5555-7000-8000-000000000044"
		routed = "019a1c0b-6666-7000-8000-000000000045"
	)
	standing := []Target{
		{Kind: graphstore.KindPerson, ID: "dave@example.com"},
		{Kind: graphstore.KindPerson, ID: "erin@example.com"},
		{Kind: graphstore.KindRole, ID: "reviewer"},
		{Kind: graphstore.KindSession, ID: named},
		{Kind: graphstore.KindSession, ID: routed},
	}
	kept := []Target{{Kind: graphstore.KindSession, ID: routed}}
	// planned is each tuple of a plan as its far end, whether it asserts, its maker, its moment and
	// the event it is stated under.
	planned := func(tuples []graphstore.Tuple) []string {
		var read []string
		for _, tuple := range tuples {
			legacy := int64(-1)
			if tuple.LegacySourceSeq != nil {
				legacy = *tuple.LegacySourceSeq
			}
			read = append(read, fmt.Sprintf("%s/%s -%s-> %s/%s asserted=%t by=%s at=%s event=%d legacy=%d",
				tuple.FromKind, tuple.FromID, tuple.Kind, tuple.ToKind, tuple.ToID, tuple.Asserted, tuple.Who.ID,
				tuple.At.Format(time.RFC3339), tuple.SourceEvent, legacy))
		}
		return read
	}
	retraction := func(kind, id string) string {
		return fmt.Sprintf("%s/c1 -%s-> %s/%s asserted=false by=alice at=%s event=%d legacy=%d",
			graphstore.KindComment, graphstore.TupleMentions, kind, id, at.Format(time.RFC3339), event, event)
	}
	for _, write := range []struct {
		name, body         string
		identities, people []string
	}{{
		name:       "an edit that keeps dave and the named session and spells the role in code",
		body:       "Over to dispatch://person/dave@example.com and dispatch://agent/" + named + " for the copy; `dispatch://role/reviewer` is code.",
		identities: []string{retraction(graphstore.KindRole, "reviewer")},
		people:     []string{retraction(graphstore.KindPerson, "erin@example.com")},
	}, {
		name:       "a delete, whose text is empty",
		identities: []string{retraction(graphstore.KindRole, "reviewer"), retraction(graphstore.KindSession, named)},
		people:     []string{retraction(graphstore.KindPerson, "dave@example.com"), retraction(graphstore.KindPerson, "erin@example.com")},
	}} {
		t.Run(write.name, func(t *testing.T) {
			identities, people := ItemUnnamedMentions(ItemComment, "c1", graphstore.VisibilityProject, maker, at, event, write.body, standing, kept)
			if got := planned(identities); !slices.Equal(got, write.identities) {
				t.Errorf("the retraction of the sessions and roles is\n  %v\nwant\n  %v", got, write.identities)
			}
			if got := planned(people); !slices.Equal(got, write.people) {
				t.Errorf("the retraction of the people is\n  %v\nwant\n  %v", got, write.people)
			}
		})
	}
	if identities, people := ItemUnnamedMentions(ItemComment, "c1", graphstore.VisibilityProject, maker, at, event, "", nil, kept); len(identities)+len(people) != 0 {
		t.Errorf("a node that mentions nobody plans the retractions %v and %v, want none", planned(identities), planned(people))
	}
}
