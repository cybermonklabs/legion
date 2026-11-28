//go:build memory

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/logtest"
)

func TestTheConversionPassesOverAnIssuePastTheRoomsBound(t *testing.T) {
	// This serial test holds three large CRDT trees and sets the command's build stamp.
	ctx := t.Context()
	database := conversionCommandDatabase(t)
	const bound = 1 << 20
	var separateItems uint64
	for i, document := range []struct{ slug, role string }{{"spec", ""}, {"verification-md", "verification"}, {"plan-md", "plan"}} {
		var id string
		if err := database.Pool.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
			values ('CUT-1', 'CUT', $1, $1, 'doc', $2, nullif($3, ''), '{"kind":"user","id":"alice"}') returning id::text`,
			document.slug, i == 0, document.role).Scan(&id); err != nil {
			t.Fatal(err)
		}
		// Store a browser-like update directly in the old rooms. The upload budget would refuse
		// this many elements. The old rooms need not have settled these edits into versions yet.
		update, count := conversionHeavyUpdate(t, document.slug)
		if count <= 131_072 || count > bound {
			t.Fatalf("%s's update has %d items, want more than one upload but within the room bound", document.slug, count)
		}
		separateItems += count
		conversionCommandExec(t, database, `insert into doc_updates (artifact_id, version, update, content_changed) values ($1, 1, $2, true)`, id, update)
	}
	if separateItems <= bound {
		t.Fatalf("the three old rooms only hold %d items together", separateItems)
	}
	conversionCommandExec(t, database, `insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CUT-2', 'CUT', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')`)

	oldRooms := func() string {
		t.Helper()
		var state string
		if err := database.Pool.QueryRow(ctx, `
			select coalesce(string_agg(a.slug || ':' || u.version || ':' || md5(u.update), ',' order by a.slug, u.version), '')
			from artifacts a join doc_updates u on u.artifact_id = a.id where a.issue_key = 'CUT-1'`).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := oldRooms()
	checkRows := func(wantSmallTree int) {
		t.Helper()
		var artifacts, trees, rooms, small int
		if err := database.Pool.QueryRow(ctx, `select
			(select count(*) from artifacts where issue_key = 'CUT-1' and kind = 'tree'),
			(select count(*) from issue_trees where issue_key = 'CUT-1'),
			(select count(*) from document_rooms r join artifacts a on a.id = r.artifact_id where a.issue_key = 'CUT-1'),
			(select count(*) from issue_trees where issue_key = 'CUT-2')`).Scan(&artifacts, &trees, &rooms, &small); err != nil {
			t.Fatal(err)
		}
		if artifacts != 0 || trees != 0 || rooms != 0 || small != wantSmallTree || oldRooms() != before {
			t.Fatalf("large issue: %d tree artifacts, %d trees, %d joins, rooms unchanged=%t; small issue trees=%d (want %d)",
				artifacts, trees, rooms, oldRooms() == before, small, wantSmallTree)
		}
	}
	var measured uint64
	pastTheBound := fmt.Sprintf("past the %d", bound)
	for _, dry := range []bool{true, false} {
		args := []string{"--project", "CUT"}
		if dry {
			args = append(args, "--dry-run")
		}
		code, output := runConversionCommand(t, database, args...)
		if code != 0 {
			t.Fatalf("dry=%v: command exited %d: %s", dry, code, output)
		}
		var count uint64
		for line := range strings.Lines(output) {
			if strings.HasPrefix(line, "CUT-1 passed over") && strings.Contains(line, pastTheBound) {
				_, value, _ := strings.Cut(strings.TrimSpace(line), "items=")
				count, _ = strconv.ParseUint(value, 10, 64)
			}
		}
		if count <= bound || count < separateItems || (measured != 0 && count != measured) {
			t.Fatalf("dry=%v: measured %d items after %d in the separate rooms, previously read %d:\n%s", dry, count, separateItems, measured, output)
		}
		measured = count
		if dry {
			if !strings.Contains(output, "CUT-2 would be converted") {
				t.Fatalf("the dry run gave no result for the small issue: %s", output)
			}
			checkRows(0)
		} else {
			checkRows(1)
		}
	}

	logged := new(logtest.Buffer)
	service := docs.New(docs.Deps{Store: database, Events: events.NewBroker()})
	defer service.Shutdown(context.Background())
	if err := docs.NewIssueRoomStep(service, database, slog.New(slog.NewTextHandler(logged, nil))).Tick(ctx); err != nil {
		t.Fatal(err)
	}
	line := logged.Lines("passed over an issue")
	if !strings.Contains(line, "issue=CUT-1 ") || !strings.Contains(line, pastTheBound) || !strings.Contains(line, fmt.Sprintf("items=%d", measured)) {
		t.Fatalf("the step did not report the same reason and item count: %s", logged.String())
	}
	checkRows(1)
}

func conversionHeavyUpdate(t *testing.T, prefix string) ([]byte, uint64) {
	t.Helper()
	const paragraphs = 120_000
	children := make([]*pmdoc.Node, 0, paragraphs)
	for i := range paragraphs {
		children = append(children, &pmdoc.Node{Type: "paragraph",
			Attrs:    pmdoc.Attrs{pmdoc.BlockIDAttr: prefix + "-" + strconv.Itoa(i)},
			Children: []*pmdoc.Node{{Type: "text", Text: "Paragraph."}}})
	}
	doc := crdt.New()
	fragment := doc.GetXmlFragment("prosemirror")
	if err := doc.TransactE(func(tx *crdt.Transaction) error {
		return pmdoc.Update(tx, fragment, &pmdoc.Node{Type: "doc", Children: children})
	}); err != nil {
		t.Fatal(err)
	}
	update := crdt.EncodeStateAsUpdateV1(doc, nil)
	decoder := encoding.NewDecoder(update)
	clients, err := decoder.ReadVarUint()
	if err != nil || clients != 1 {
		t.Fatalf("test update has %d clients, want 1: %v", clients, err)
	}
	items, err := decoder.ReadVarUint()
	if err != nil {
		t.Fatal(err)
	}
	return update, items
}
