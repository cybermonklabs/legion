package graphstore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// factRun is a TupleRun over facts it builds from build at each index, counting how many it built.
type factRun struct {
	count int
	build func(index int) Tuple
	built int
}

func (r *factRun) Len() int { return r.count }

func (r *factRun) At(index int) UncausedTuple {
	r.built++
	return UncausedTuple{Tuple: r.build(index)}
}

// facts is every fact write holds, as "from kind to" rows, in order.
func facts(write Uncaused) []string {
	var rows []string
	for held := range write.Facts() {
		rows = append(rows, held.FromID+" "+held.Kind+" "+held.ToID)
	}
	return rows
}

// A write that holds a run of facts (HoldRun) appends the rows the same facts held as tuples
// append, in the same order of seq: the run's relations among the caller's relations and its
// attributes among the caller's attributes, each in its place, across an append of more rows than
// one statement binds. The append builds the run's facts as it binds them.
func TestARunOfFactsAppendsTheRowsItsTuplesWould(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	at := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	const cause = int64(9800101)
	count := appendRows + 300

	// Each write's blocks, their in_document before the run, a rank and a text_parent of each
	// block in the run, and a title of the document after it.
	plan := func(document string) ([]Node, []UncausedTuple, *factRun, []UncausedTuple) {
		nodes := make([]Node, count)
		before := make([]UncausedTuple, count)
		for index := range nodes {
			id := fmt.Sprintf("%s#b%05d", document, index)
			nodes[index] = Node{
				Kind: "block", ID: id, Supertag: "block",
				Visibility: VisibilityProject, State: StateLive, MadeAt: at, MadeBy: who, Restates: true,
			}
			before[index] = UncausedTuple{Tuple: Tuple{
				FromKind: "block", FromID: id, Kind: "in_document", ToKind: "artifact", ToID: document,
				At: at, Who: who, Asserted: true,
			}}
		}
		run := &factRun{count: 2 * count, build: func(index int) Tuple {
			id := fmt.Sprintf("%s#b%05d", document, index/2)
			if index%2 == 0 {
				return Tuple{FromKind: "block", FromID: id, Kind: "rank", ToKind: KindValue, ToID: fmt.Sprintf("r%05d", index/2), At: at, Who: who, Asserted: true}
			}
			return Tuple{FromKind: "block", FromID: id, Kind: "text_parent", ToKind: "artifact", ToID: document, At: at, Who: who, Asserted: true}
		}}
		after := []UncausedTuple{{Tuple: Tuple{
			FromKind: "artifact", FromID: document, Kind: "title", ToKind: KindValue, ToID: "last", At: at, Who: who, Asserted: true,
		}}}
		return nodes, before, run, after
	}
	appended := func(document string) []string {
		rows, err := database.Pool.Query(ctx, `
			select replace(from_id || ' ' || kind || ' ' || to_id, $2, 'DOC') from graph_tuples
			where source_event = $1 and (from_id = $2 or from_id like $2 || '#%')
			order by seq
		`, cause, document)
		if err != nil {
			t.Fatalf("read the appended tuples of %s: %v", document, err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the appended tuples of %s: %v", document, err)
		}
		return got
	}

	nodes, before, run, after := plan("RUN-1/spec")
	held := ArmlessFresh(PlanUncaused(nodes, nil), "block")
	held.State(before...)
	held.HoldRun(run)
	held.State(after...)
	if held.Len() != 3*count+1 {
		t.Fatalf("the write holds %d facts, want %d", held.Len(), 3*count+1)
	}
	runInOneTransaction(t, database, "the write holding a run", func(ctx context.Context, tx pgx.Tx) error {
		return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			QueueNodes(b, held)
			QueueCaused(b, held, cause)
			return nil
		})
	})
	if run.built == 0 {
		t.Fatal("the append built none of the run's facts")
	}

	nodes, before, tuples, after := plan("RUN-2/spec")
	whole := ArmlessFresh(PlanUncaused(nodes, nil), "block")
	whole.State(before...)
	for index := range tuples.Len() {
		whole.State(tuples.At(index))
	}
	whole.State(after...)
	runInOneTransaction(t, database, "the write holding the same facts as tuples", func(ctx context.Context, tx pgx.Tx) error {
		return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			QueueNodes(b, whole)
			QueueCaused(b, whole, cause)
			return nil
		})
	})

	got, want := appended("RUN-1/spec"), appended("RUN-2/spec")
	if len(want) != 3*count+1+2*count {
		t.Fatalf("the tuples' write appended %d rows, want its %d facts and two base attributes of each of its %d nodes", len(want), 3*count+1, count)
	}
	if !slices.Equal(got, want) {
		for index := range min(len(got), len(want)) {
			if got[index] != want[index] {
				t.Fatalf("the run's write appended %d rows and the tuples' %d; the first that differs is row %d: %q, want %q", len(got), len(want), index, got[index], want[index])
			}
		}
		t.Fatalf("the run's write appended %d rows, the tuples' %d", len(got), len(want))
	}
	if !strings.HasPrefix(got[count], "DOC#b00000 text_parent DOC") {
		t.Errorf("row %d is %q, want the run's first relation after the in_document of every block", count, got[count])
	}
}

// The facts of a write that holds a run are its tuples and the run's in their places, and a reader
// of them reads them only through Len and Facts: Joined keeps each run in its place after the
// writes before it, the facts a writer adds after joining follow every part's (split), Parted gives
// each part the run's facts of its own nodes in their places as a run, StateFactsOf adds a write's
// run as a run, and HeldHeads names each near end once.
func TestTheFactsOfARunStandInTheirPlaceThroughEveryReaderOfTheWrite(t *testing.T) {
	tuple := func(from, kind, to string) UncausedTuple {
		return UncausedTuple{Tuple: Tuple{FromKind: "block", FromID: from, Kind: kind, ToKind: KindValue, ToID: to, Asserted: true}}
	}
	run := &factRun{count: 4, build: func(index int) Tuple {
		return tuple([]string{"x", "y"}[index%2], "rank", fmt.Sprint("r", index)).Tuple
	}}
	var first Uncaused
	first.State(tuple("x", "title", "a"))
	first.HoldRun(run)
	first.State(tuple("y", "title", "b"))
	if want := []string{"x title a", "x rank r0", "y rank r1", "x rank r2", "y rank r3", "y title b"}; !slices.Equal(facts(first), want) {
		t.Fatalf("the write's facts are %q, want %q", facts(first), want)
	}
	if first.Len() != 6 {
		t.Errorf("Len is %d, want 6", first.Len())
	}
	for index, want := range facts(first) {
		if held := first.fact(index); held.FromID+" "+held.Kind+" "+held.ToID != want {
			t.Errorf("fact %d is %+v, want %q", index, held.Tuple, want)
		}
	}

	second := Holding(tuple("z", "title", "c"))
	joined := Joined(second, first)
	joined.State(tuple("z", "title", "d"))
	want := []string{"z title c", "x title a", "x rank r0", "y rank r1", "x rank r2", "y rank r3", "y title b", "z title d"}
	if !slices.Equal(facts(joined), want) {
		t.Fatalf("the joined write's facts are %q, want %q", facts(joined), want)
	}
	var parts []string
	for _, part := range joined.split() {
		parts = append(parts, facts(part)...)
	}
	if !slices.Equal(parts, want) {
		t.Errorf("the joined write's parts hold %q, want %q", parts, want)
	}

	parted := Parted(first, func(_, id string) string { return id })
	if got, want := facts(parted["x"]), []string{"x title a", "x rank r0", "x rank r2"}; !slices.Equal(got, want) {
		t.Errorf("the part of x holds %q, want %q", got, want)
	}
	if got, want := facts(parted["y"]), []string{"y rank r1", "y rank r3", "y title b"}; !slices.Equal(got, want) {
		t.Errorf("the part of y holds %q, want %q", got, want)
	}

	var copied Uncaused
	copied.State(tuple("w", "title", "e"))
	copied.StateFactsOf(first)
	if got, want := facts(copied), append([]string{"w title e"}, facts(first)...); !slices.Equal(got, want) {
		t.Errorf("the facts StateFactsOf added are %q, want %q", got, want)
	}
	if len(copied.runs) != 1 {
		t.Errorf("StateFactsOf added the run as %d runs, want it added as one, unbuilt", len(copied.runs))
	}

	heads := HeldHeads(joined)
	if want := []NodeKey{{Kind: "block", ID: "z"}, {Kind: "block", ID: "x"}, {Kind: "block", ID: "y"}}; !slices.Equal(heads, want) {
		t.Errorf("HeldHeads is %v, want %v", heads, want)
	}
}
