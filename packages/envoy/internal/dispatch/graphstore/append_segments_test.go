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

// An append of more tuples and nodes than one statement binds (appendRows) is several statements
// (appendSegments), and its rows land in the order one statement gives them: the caller's
// relations, its attributes, and the base attributes node by node, visibility before state, at
// the place the caller's baseAt names; every node moves onto the cause. Both writers are cut
// mid-relations and mid-nodes, the held one with the base attributes after the caller's own
// attributes and the one in hand with them before.
func TestAnAppendPastOneStatementLandsInTheOrderOfOne(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	at := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	const cause = int64(9800001)
	count := appendRows + 300

	write := func(document string, event int64) ([]Node, []Tuple, []Tuple) {
		nodes := make([]Node, count)
		relations := make([]Tuple, count)
		for index := range nodes {
			id := fmt.Sprintf("%s#b%05d", document, index)
			nodes[index] = Node{
				Kind: "block", ID: id, Supertag: "block",
				Visibility: VisibilityProject, State: StateLive,
				MadeAt: at, MadeBy: who, SourceEvent: event, Restates: true,
			}
			relations[index] = Tuple{
				FromKind: "block", FromID: id, Kind: "in_document", ToKind: "artifact", ToID: document,
				At: at, Who: who, SourceEvent: event, Asserted: true,
			}
		}
		values := []Tuple{
			{FromKind: "artifact", FromID: document, Kind: "title", ToKind: KindValue, ToID: "first", At: at, Who: who, SourceEvent: event, Asserted: true},
			{FromKind: "artifact", FromID: document, Kind: "title", ToKind: KindValue, ToID: "second", At: at, Who: who, SourceEvent: event, Asserted: true},
		}
		return nodes, relations, values
	}
	stated := func(tuples []Tuple) []string {
		rows := make([]string, len(tuples))
		for index, tuple := range tuples {
			rows[index] = tuple.FromID + " " + tuple.Kind + " " + tuple.ToID
		}
		return rows
	}
	based := func(nodes []Node) []string {
		rows := make([]string, 0, 2*len(nodes))
		for _, node := range nodes {
			rows = append(rows, node.ID+" "+AttributeVisibility+" "+node.Visibility, node.ID+" "+AttributeState+" "+node.State)
		}
		return rows
	}

	for _, writer := range []struct {
		name     string
		document string
		write    func(document string) (func(*store.WriteBatch) error, []string)
	}{
		{
			name:     "a write held for its cause, as the block index writes a version",
			document: "ORDER-1/spec",
			write: func(document string) (func(*store.WriteBatch) error, []string) {
				nodes, relations, values := write(document, NoEvent)
				held := PlanUncaused(nodes, slices.Concat(relations, values))
				return func(b *store.WriteBatch) error {
					QueueNodes(b, held)
					QueueCaused(b, held, cause)
					return nil
				}, slices.Concat(stated(relations), stated(values), based(nodes))
			},
		},
		{
			name:     "a write with its cause in hand, as the backfill and the seed write",
			document: "ORDER-2/spec",
			write: func(document string) (func(*store.WriteBatch) error, []string) {
				nodes, relations, values := write(document, cause)
				return func(b *store.WriteBatch) error {
					QueueWrite(b, nodes, slices.Concat(relations, values))
					return nil
				}, slices.Concat(stated(relations), based(nodes), stated(values))
			},
		},
	} {
		t.Run(writer.name, func(t *testing.T) {
			document := writer.document
			queue, want := writer.write(document)
			appends := 0
			for _, statement := range queuedBy(t, queue) {
				if strings.Contains(statement.SQL, "insert into graph_tuples") {
					appends++
				}
			}
			if appends < 2 {
				t.Fatalf("the append of %d nodes and %d tuples is %d statement(s), want it cut at %d rows", count, count+2, appends, appendRows)
			}
			runInOneTransaction(t, database, writer.name, func(ctx context.Context, tx pgx.Tx) error {
				return store.RunWrite(ctx, tx, queue)
			})

			rows, err := database.Pool.Query(ctx, `
				select from_id || ' ' || kind || ' ' || to_id from graph_tuples
				where source_event = $1 and (from_id = $2 or from_id like $2 || '#%')
				order by seq
			`, cause, document)
			if err != nil {
				t.Fatalf("read the appended tuples: %v", err)
			}
			got, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatalf("read the appended tuples: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("the append wrote %d tuples, want %d", len(got), len(want))
			}
			for index := range want {
				if got[index] != want[index] {
					t.Fatalf("appended tuple %d is %q, want %q: the statements broke the order of one", index, got[index], want[index])
				}
			}
			var moved int
			if err := database.Pool.QueryRow(ctx, `
				select count(*) from graph_nodes where id like $1 || '#%' and source_event = $2
			`, document, cause).Scan(&moved); err != nil {
				t.Fatalf("count the nodes on the cause: %v", err)
			}
			if moved != count {
				t.Errorf("%d of the %d nodes stand on the cause %d", moved, count, cause)
			}
		})
	}
}
