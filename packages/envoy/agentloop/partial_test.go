package agentloop

import (
	"encoding/json"
	"strings"
	"testing"

	aisdk "github.com/grafana/ai-sdk"
)

// streamDeltas hands out each text the SDK parses a Final reply at as reply streams in deltas of
// size bytes: the reply so far, after each delta.
func streamDeltas(reply string, size int, parse func(text string)) {
	for end := size; ; end += size {
		parse(reply[:min(end, len(reply))])
		if end >= len(reply) {
			return
		}
	}
}

// countingOutput counts the partial parses it is asked for and the bytes they read.
type countingOutput struct {
	aisdk.Output
	parses, read int
}

func (c *countingOutput) ParsePartial(text string) (any, bool) {
	c.parses++
	c.read += len(text)
	return c.Output.ParsePartial(text)
}

// countedFinal is a Final output of proseSchema whose partial parses are counted.
func countedFinal(t *testing.T) (aisdk.Output, *countingOutput) {
	t.Helper()
	out, err := finalOutput(json.RawMessage(proseSchema))
	if err != nil {
		t.Fatal(err)
	}
	partials, ok := out.(*finalPartials)
	if !ok {
		t.Fatalf("the Final output is a %T, want its partials bounded", out)
	}
	counting := &countingOutput{Output: partials.Output}
	partials.Output = counting
	return out, counting
}

// A Final reply of 4 MiB streamed in 32 KiB deltas, as a service past its bounds sends one, hands
// on snapshots that add up to a few times its length, each valid JSON and each holding more of the
// prose than the one before. Before, every delta was parsed whole and handed on whole, which the
// SDK's own check and the run's OnPartial each read again: 129 snapshots of 275 MB, work in the
// square of the reply that took the command bar's 4 MiB stream past its 30 s under the race
// detector.
func TestAFinalReplysSnapshotsAddUpToAFewTimesItsLength(t *testing.T) {
	t.Parallel()
	out, err := finalOutput(json.RawMessage(proseSchema))
	if err != nil {
		t.Fatal(err)
	}
	reply := `{"prose":"` + strings.Repeat("word ", (4<<20)/5)
	var snapshots, handed, prose int
	streamDeltas(reply, 32<<10, func(text string) {
		value, ok := out.ParsePartial(text)
		if !ok {
			return
		}
		raw, isRaw := value.(json.RawMessage)
		var snapshot struct {
			Prose string `json:"prose"`
		}
		if !isRaw || json.Unmarshal(raw, &snapshot) != nil {
			t.Fatalf("snapshot %d is %T %.80q, want the object as JSON", snapshots+1, value, value)
		}
		if len(snapshot.Prose) <= prose {
			t.Fatalf("snapshot %d holds %d bytes of prose, want more than the %d before it", snapshots+1, len(snapshot.Prose), prose)
		}
		snapshots, handed, prose = snapshots+1, handed+len(raw), len(snapshot.Prose)
	})
	t.Logf("a %d-byte reply in 32 KiB deltas handed on %d snapshots of %d bytes", len(reply), snapshots, handed)
	if snapshots < 2 || handed > 3*len(reply) {
		t.Errorf("a %d-byte reply in 32 KiB deltas handed on %d snapshots of %d bytes, want several adding up to at most %d",
			len(reply), snapshots, handed, 3*len(reply))
	}
}

// A reply whose first byte that is not white space can start no JSON value is parsed at no delta,
// however long it streams: the command bar's 4 MiB of words is never scanned, as it was at each of
// its 130 deltas. A reply that starts as JSON after white space is parsed, and so is the reply of
// the next model call, which starts the count again.
func TestAReplyThatCannotBeJSONIsNeverParsed(t *testing.T) {
	t.Parallel()
	out, counting := countedFinal(t)
	streamDeltas(strings.Repeat("word ", (4<<20)/5), 32<<10, func(text string) { out.ParsePartial(text) })
	if counting.parses != 0 {
		t.Errorf("4 MiB of words was parsed %d times, %d bytes read, want never", counting.parses, counting.read)
	}

	if _, ok := out.ParsePartial("\n  {\"prose\":\"The note"); !ok || counting.parses != 1 {
		t.Errorf("the next call's reply starting as JSON after white space was parsed %d times (snapshot %t), want once into a snapshot", counting.parses, ok)
	}
}

// Every delta of a reply within partialEveryDelta is parsed, so a reply a model writes within its
// output bound reaches OnPartial as it streams, delta by delta, as going's short answer streams
// its words; past it the parses add up to at most three times the reply.
func TestAReplyWithinItsBoundIsParsedAtEveryDelta(t *testing.T) {
	t.Parallel()
	out, counting := countedFinal(t)
	short := `{"prose":"` + strings.Repeat("plum ", 1000) + `"}`
	deltas := 0
	streamDeltas(short, 8, func(text string) {
		deltas++
		out.ParsePartial(text)
	})
	if counting.parses != deltas {
		t.Errorf("a %d-byte reply was parsed at %d of its %d deltas, want every one", len(short), counting.parses, deltas)
	}

	out, counting = countedFinal(t)
	long := `{"prose":"` + strings.Repeat("plum ", (1<<20)/5) + `"}`
	read := 0
	streamDeltas(long, 4<<10, func(text string) {
		if len(text) > partialEveryDelta {
			before := counting.read
			out.ParsePartial(text)
			read += counting.read - before
			return
		}
		out.ParsePartial(text)
	})
	if read > 3*len(long) {
		t.Errorf("past %d bytes a %d-byte reply in 4 KiB deltas was parsed over %d bytes, want at most %d", partialEveryDelta, len(long), read, 3*len(long))
	}
}
