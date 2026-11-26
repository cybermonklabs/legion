package refs

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The plan of the identity mentions out of a node reserved for its author names no person but the
// author its made_by proved, by the rule graphstore.ToPerson refuses every other person by
// (graphstore.AuthorIsPerson), so the index never plans a mention, or the retraction of one, that
// ToPerson would refuse. A node made by a session alice owns is alice's: its text's mention of
// alice is stated, and retracted once the text no longer names her. A node made by bob's actor
// carrying alice as its owner is alice's as well, and hidden from bob: its text's mention of bob is
// neither stated nor retracted.
func TestAMentionOutOfANodeReservedForItsAuthorNamesOnlyTheAuthorItsMakerProved(t *testing.T) {
	at := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	alice := "alice"
	for _, row := range []struct {
		name   string
		maker  model.Actor
		person string
		states bool
	}{
		{"a session alice owns made it, and its text names alice", model.Actor{Kind: "session", ID: "s1", Owner: &alice}, "alice", true},
		{"bob's actor carrying alice as its owner made it, and its text names bob", model.Actor{Kind: "user", ID: "bob", Owner: &alice}, "bob", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			target := Target{Kind: graphstore.KindPerson, ID: row.person}
			_, stated := identityMentions(graphstore.KindComment, "c1", graphstore.VisibilityAuthor, row.maker, at,
				[]Target{target}, map[Target]struct{}{})
			_, retracted := unnamedMentions(graphstore.KindComment, "c1", graphstore.VisibilityAuthor, row.maker, at,
				nil, map[Target]struct{}{target: {}}, map[Target]struct{}{})
			for _, plan := range []struct {
				what     string
				people   []graphstore.Tuple
				asserted bool
			}{{"the mention", stated, true}, {"the retraction", retracted, false}} {
				named := len(plan.people) == 1 && plan.people[0].ToKind == graphstore.KindPerson &&
					plan.people[0].ToID == row.person && plan.people[0].Asserted == plan.asserted
				switch {
				case row.states && !named:
					t.Errorf("the plan of %s of %s out of the node is %+v, want the one tuple to %s", plan.what, row.person, plan.people, row.person)
				case !row.states && len(plan.people) != 0:
					t.Errorf("the plan of %s of %s out of the node is %+v, want none: the node is hidden from %s", plan.what, row.person, plan.people, row.person)
				}
			}
		})
	}
}
