package store

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/listenerwire"
)

func TestMergeTopics(t *testing.T) {
	item := Interest{Topics: []string{"notifications.github.>", "notifications.slack.*.*.mention"}}
	next := Merge(item, []string{"notifications.slack.*.*.mention", "notifications.agent.s1"})
	if len(next.Topics) != 3 {
		t.Fatalf("expected 3 topics, got %d", len(next.Topics))
	}
}

func TestRemoveTopics(t *testing.T) {
	item := Interest{Topics: []string{"a", "b", "c"}}
	next := Remove(item, []string{"b"})
	if len(next.Topics) != 2 {
		t.Fatalf("expected 2 topics, got %d", len(next.Topics))
	}
	if next.Topics[0] != "a" || next.Topics[1] != "c" {
		t.Fatalf("unexpected topics: %#v", next.Topics)
	}
	clear := Remove(item, nil)
	if len(clear.Topics) != 0 {
		t.Fatalf("expected full removal, got %#v", clear.Topics)
	}
}

// The listener answers a stored interest as the wire package declares it (Interest.Wire), so the
// two types name the same JSON keys, and the answer carries every field of the record it was
// decoded from but the keys only a newer build names, which the record still writes back.
func TestAnInterestAnswersEveryKeyTheWireDeclares(t *testing.T) {
	stored, wire := JSONKeys(reflect.TypeFor[Interest]()), JSONKeys(reflect.TypeFor[listenerwire.Interest]())
	if !slices.Equal(stored, wire) {
		t.Fatalf("store.Interest names the keys %v and listenerwire.Interest %v: an answer would drop or invent a field", stored, wire)
	}
	const known = `{"session_id":"s1","machine_id":"m1","dir":"/d","topics":["notifications.team.x"],"updated_at":42`
	item, err := decodeInterestValue([]byte(known + `,"future":{"k":1}}`))
	if err != nil {
		t.Fatalf("decode a newer build's interest: %v", err)
	}
	answer, err := json.Marshal(item.Wire())
	if err != nil {
		t.Fatalf("encode the answer: %v", err)
	}
	if string(answer) != known+"}" {
		t.Fatalf("the answer is %s, want %s}", answer, known)
	}
	rewritten, err := encodeInterest(item)
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	if !strings.Contains(string(rewritten), `"future":{"k":1}`) {
		t.Fatalf("the record is written back as %s, without the newer build's key", rewritten)
	}
}
