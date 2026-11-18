package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A tuple holds the write that caused it (R1 of dispatch://INFRA-1149), and the two base
// attributes every node carries - `visibility` and `state` - are attribute tuples under that
// rule (R8). So a comment's `visibility` and `state` must name the event the same transaction
// appended, exactly as its `authored_by` does.
//
// This replaces the graphstore test of the same name, with the tester's consent, and it is
// written to three conditions the replacement is read against:
//
//   - it drives the real comment create path, through the request handler, rather than calling
//     graphstore.Write directly: the defect it holds closed was an ordering of that path's own
//     writes, so a test that writes through the store cannot see it come back;
//   - it reads every tuple the write appended with no `to_kind` filter, the attribute tuples
//     included. graphstore.CurrentOut binds `to_kind <> 'value'`, because it answers the far
//     ends of a node and a value is no node, and that filter is what hid this defect: the test
//     beside this one reads the write through CurrentOut and passed while both base attributes
//     carried the sentinel, because neither was in what it read;
//   - it names the event by reading `events`, never the node row's `source_event` column. The
//     row's column is written by the same request, so a test that read the cause from there
//     would compare the write with itself and hold nothing.
func TestEveryBaseAttributeTupleCarriesTheWriteThatCausedIt(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A paragraph of the spec.\n")
	const writer = "019a1c0b-3333-7000-8000-000000000003"

	response := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{
			"body":  "A comment whose every stated fact names the write that stored it.",
			"actor": map[string]string{"kind": "session", "id": writer},
		}, "agent-token")
	if response.Code != http.StatusCreated {
		t.Fatalf("write the comment: status=%d body=%s", response.Code, response.Body.String())
	}
	written := decodeBody[model.Comment](t, response)
	event := commentCreatedEvent(t, database, written.ID)

	rows, err := database.Pool.Query(ctx, `
		select kind, to_kind, to_id, source_event
		from graph_tuples
		where from_kind = 'comment' and from_id = $1
		order by seq
	`, written.ID)
	if err != nil {
		t.Fatalf("read the tuples the write appended: %v", err)
	}
	defer rows.Close()
	values := map[string]string{}
	appended := 0
	for rows.Next() {
		var kind, toKind, toID string
		var cause int64
		if err := rows.Scan(&kind, &toKind, &toID, &cause); err != nil {
			t.Fatalf("read the tuples the write appended: %v", err)
		}
		appended++
		if cause != event {
			t.Errorf("the %s tuple (%s %q) carries cause %d, want the event %d the same transaction appended",
				kind, toKind, toID, cause, event)
		}
		if toKind == graphstore.KindValue {
			values[kind] = toID
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the tuples the write appended: %v", err)
	}
	if appended == 0 {
		t.Fatal("the comment create path appended no tuple at all")
	}

	// The two base attributes are among them, each holding what the node row holds: the row is
	// the projection of these tuples, so a value in one and not the other is a row no appended
	// fact explains, and no tuple may be updated to mend it afterwards.
	node, held, err := graphstore.ReadNode(ctx, database.Pool, "comment", written.ID)
	if err != nil || !held {
		t.Fatalf("read the comment's node: held=%t err=%v", held, err)
	}
	for attribute, row := range map[string]string{
		"visibility": node.Visibility,
		"state":      node.State,
	} {
		stated, ok := values[attribute]
		if !ok {
			t.Errorf("the write appended no %s tuple, while the node row reads %q", attribute, row)
			continue
		}
		if stated != row {
			t.Errorf("the %s tuple states %q and the node row reads %q", attribute, stated, row)
		}
	}
}
