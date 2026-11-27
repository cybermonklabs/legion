package graphstore

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestOnlyASessionTheServerProvedEarnsAnAlias holds AuthorFacts to R14's rule on what ties a
// session to a person: "a session the server itself proved theirs" does, and "a session an actor
// merely claims proves nothing". A person's act names a session two ways. A broker-issued token
// proves the session it is bound to (api.actingPerson copies it onto the act), and that session is
// an alias of the person. A personal token proves none, and the session its act names is the one
// its caller claimed in `?session=` or a body actor (api.actingPerson, api.actSessionFromBody):
// that session earns no alias, so a delivery to it reaches nobody's page. A session author the
// broker proved a person's is the other way round: the session wrote the thing, the alias ties it
// to its owner, and the owner is a node of the store from that write on.
func TestOnlyASessionTheServerProvedEarnsAnAlias(t *testing.T) {
	at := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	owner := "Carol"
	for _, test := range []struct {
		name    string
		actor   model.Actor
		aliases []string
		nodes   []string
	}{
		{
			name:    "a broker-issued token's session, acting as its person",
			actor:   model.Actor{Kind: "user", ID: "carol", Act: &model.ActorAct{Session: "s-proved", Scopes: model.Scopes}},
			aliases: []string{"s-proved>carol"},
			nodes:   []string{"person/carol"},
		},
		{
			name: "a personal token's claimed session, acting as its person",
			actor: model.Actor{Kind: "user", ID: "carol", Act: &model.ActorAct{
				Session: "s-claimed", TokenID: "token-1", TokenName: "laptop", Scopes: model.Scopes,
			}},
			nodes: []string{"person/carol"},
		},
		{
			name: "a session the broker proved a person's, writing as itself",
			actor: model.Actor{Kind: "session", ID: "s-own", Owner: &owner,
				Act: &model.ActorAct{Session: "s-own", Scopes: model.Scopes}},
			aliases: []string{"s-own>carol"},
			nodes:   []string{"session/s-own", "person/carol"},
		},
		{
			name:  "a session a body named, on a token that names its owner",
			actor: model.Actor{Kind: "session", ID: "s-named", Owner: &owner},
			nodes: []string{"session/s-named"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			nodes, tuples, named := AuthorFacts("comment", "c-1", test.actor, at, 9420001)
			if !named {
				t.Fatalf("AuthorFacts names nobody for %+v", test.actor)
			}
			var gotNodes, gotAliases []string
			for _, node := range nodes {
				gotNodes = append(gotNodes, node.Kind+"/"+node.ID)
			}
			for _, tuple := range tuples {
				if tuple.Kind == TupleAliasOf {
					gotAliases = append(gotAliases, tuple.FromID+">"+tuple.ToID)
				}
			}
			if !slices.Equal(gotNodes, test.nodes) {
				t.Errorf("the write's nodes are %v, want %v", gotNodes, test.nodes)
			}
			if !slices.Equal(gotAliases, test.aliases) {
				t.Errorf("the write's aliases are %v, want %v", gotAliases, test.aliases)
			}
		})
	}
}
