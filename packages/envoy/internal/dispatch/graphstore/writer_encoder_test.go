package graphstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A run of rows one writer wrote shares one encoding of that writer (writerEncoder): a write of a
// version's blocks names one writer for every node and tuple of it, and an encoding per row held as
// many copies of the same bytes as the version has blocks. Every builder of a write's arguments
// that binds a writer - a node's made_by (nodeParams), a tuple's who (tupleParams, and
// tupleParamsOf, which the append binds), a claim's who (claimParams) and the made_by the append
// states a node's base attributes with (appendedNodeParams) - hands each row of a run the bytes the
// run's first row took, and a row whose writer differs from the row before it bytes of its own. A
// run is read on the actor as a row records it: one carrying the request's credential is the same
// writer as one without it, and no row's bytes name the credential.
func TestARunOfRowsByOneWriterSharesOneEncodingOfIt(t *testing.T) {
	at := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	alice := model.Actor{Kind: "user", ID: "alice"}
	aliceWithCredential := alice
	aliceWithCredential.Credential = &model.Credential{Scopes: []string{model.ScopePerson}}
	bob := model.Actor{Kind: "session", ID: "bob"}
	writers := []model.Actor{alice, alice, aliceWithCredential, alice, bob, bob, alice}

	nodes := make([]Node, len(writers))
	tuples := make([]Tuple, len(writers))
	claims := make([]Claim, len(writers))
	for index, writer := range writers {
		id := fmt.Sprintf("b-%d", index)
		nodes[index] = Node{Kind: "block", ID: id, Supertag: "paragraph", Visibility: "project", State: "live", MadeAt: at, MadeBy: writer}
		tuples[index] = Tuple{FromKind: "block", FromID: id, Kind: "part_of", ToKind: "artifact", ToID: "a-1", At: at, Who: writer, Asserted: true}
		claims[index] = Claim{FromKind: "block", FromID: id, Kind: "held_by", ToKind: "person", ToID: "alice", At: at, Who: writer}
	}
	type binding struct {
		name    string
		encoded [][]byte
	}
	writerColumn := func(name string, args []any, err error, column int) binding {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return binding{name, args[column].([][]byte)}
	}
	nodeArgs, nodeErr := nodeParams(nodes)
	tupleArgs, tupleErr := tupleParams(tuples)
	segmentArgs, segmentErr := tupleParamsOf(len(tuples), func(index int) Tuple { return tuples[index] })
	claimArgs, claimErr := claimParams(claims)
	appendedArgs, appendedErr := appendedNodeParams(nodes, nil, nil)
	for _, builder := range []binding{
		writerColumn("nodeParams", nodeArgs, nodeErr, 7),
		writerColumn("tupleParams", tupleArgs, tupleErr, 6),
		writerColumn("tupleParamsOf", segmentArgs, segmentErr, 6),
		writerColumn("claimParams", claimArgs, claimErr, 6),
		writerColumn("appendedNodeParams", appendedArgs, appendedErr, 7),
	} {
		name, encoded := builder.name, builder.encoded
		if len(encoded) != len(writers) {
			t.Fatalf("%s binds %d writers, want %d", name, len(encoded), len(writers))
		}
		for index, writer := range writers {
			want, err := json.Marshal(writer.Recorded())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(encoded[index], want) {
				t.Errorf("%s binds row %d's writer as %s, want %s", name, index, encoded[index], want)
			}
			if index == 0 {
				continue
			}
			run := writer.Recorded() == writers[index-1].Recorded()
			if shared := &encoded[index][0] == &encoded[index-1][0]; shared != run {
				t.Errorf("%s: row %d shares row %d's encoding of its writer %t, want %t (one encoding per run of one writer)",
					name, index, index-1, shared, run)
			}
		}
	}
}
