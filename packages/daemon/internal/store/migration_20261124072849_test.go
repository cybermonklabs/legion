package store

import (
	"context"
	"strings"
	"testing"
)

// answerToolCallsToolVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const answerToolCallsToolVersion = 20261124072849

// 20261124072849 gives answer_tool_calls the tool each call ran with: a gh call recorded before
// it reads gh, a dispatch call recorded after it reads dispatch, a web_search one is taken, and a
// tool the loop does not have is refused. The negative control is the dispatch call recorded
// before the migration, which the table has no column for. The row before it is written with SQL
// that names the columns of the schema the migration finds, as the release before writes it.
func TestMigrationAnswerToolCallsTool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, answerToolCallsToolVersion-1)
	dispatchCall := AnswerToolCall{Question: "dispatch:m-1", Seq: 2, Tool: ToolDispatch, Args: []string{"read", "LEGION-9"}, StartedAt: at(2)}
	if err := store.StartAnswerToolCall(ctx, dispatchCall); err == nil || !strings.Contains(err.Error(), `column "tool"`) {
		t.Fatalf("a dispatch call recorded before the migration: %v; want the table to have no tool to record", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into answer_tool_calls (question, seq, args, app, owner, started_at)
		values ('dispatch:m-1', 1, '["pr","view","7"]', 'review', 'acme', $1)`, at(1)); err != nil {
		t.Fatalf("record the gh call of the release before: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the tool column: %v", err)
	}

	if err := store.StartAnswerToolCall(ctx, dispatchCall); err != nil {
		t.Fatalf("a dispatch call recorded after the migration: %v", err)
	}
	if err := store.StartAnswerToolCall(ctx, AnswerToolCall{Question: "dispatch:m-1", Seq: 3, Tool: "web_search", Args: []string{"plum"}, StartedAt: at(3)}); err != nil {
		t.Fatalf("a web_search call recorded after the migration: %v", err)
	}
	calls, err := store.AnswerToolCalls(ctx, "dispatch:m-1")
	if err != nil {
		t.Fatal(err)
	}
	var tools []string
	for _, call := range calls {
		tools = append(tools, call.Tool)
	}
	if strings.Join(tools, " ") != "gh dispatch web_search" || calls[1].App != "" || calls[1].Owner != "" {
		t.Fatalf("the calls after the migration read %+v; want the earlier gh, the dispatch call with no app or owner, and the search", calls)
	}
	if got, ok, err := store.AnswerToolCall(ctx, "dispatch:m-1", 2); err != nil || !ok || got.Tool != ToolDispatch {
		t.Errorf("the dispatch call read alone = %+v, %t, %v; want its tool", got, ok, err)
	}
	if err := store.StartAnswerToolCall(ctx, AnswerToolCall{Question: "dispatch:m-1", Seq: 4, Tool: "shell", Args: []string{"ls"}, StartedAt: at(4)}); err == nil ||
		!strings.Contains(err.Error(), "answer_tool_calls_tool_check") {
		t.Errorf("a call of the tool shell: %v; want the tool check to refuse a tool the loop does not have", err)
	}
}
