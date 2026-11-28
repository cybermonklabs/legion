package docs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// TestTheDocsReconciliationsReadServedAndFormerLinks: the document service's own readers of a
// Dispatch link read one at the canonical address, at a served address (DISPATCH_SERVED_ORIGINS)
// and at a former address (DISPATCH_FORMER_ORIGINS) as a reference to the issue its page names, and
// one at any other address as none, given the server's list of origins as main builds it
// (text.ServerOrigins). Each reader is a row, driven with one link at each of the four addresses:
//   - the move of an approval request to a document's new version (approval_ask.go), which indexes
//     the request's reworded question: the request cites the issues its summary's links name;
//   - the excerpt reconciliation (reference_excerpts.go), which reads a document's text again to
//     cut each mention's excerpt: a mention it reads gets the block holding it, and an edge it reads
//     no mention for gets none.
func TestTheDocsReconciliationsReadServedAndFormerLinks(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	// The issues each link names, CORE-1 to CORE-4, by the address it is written at, and whether it
	// is a reference; CORE-5 holds the documents.
	links := []struct {
		address, origin, issue string
		reference              bool
	}{
		{"the canonical address", "https://a.example.test", "CORE-1", true},
		{"a served address", "https://b.example.test", "CORE-2", true},
		{"a former address", "https://old.example.test", "CORE-3", true},
		{"an unrelated address", "https://other.example.test", "CORE-4", false},
	}
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice')`); err != nil {
		t.Fatalf("seed project CORE: %v", err)
	}
	for number := 1; number <= 5; number++ {
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-' || $1::int, 'CORE', $1, 'An issue', '{"kind":"user","id":"alice"}', 'U')
		`, number); err != nil {
			t.Fatalf("seed issue CORE-%d: %v", number, err)
		}
	}
	var pages []string
	for _, link := range links {
		pages = append(pages, link.origin+"/issues/"+link.issue)
	}
	linked := "The plan is at " + strings.Join(pages, " and ") + " today."

	service := New(Deps{
		Store: database, Events: events.NewBroker(), Identity: headerIdentity(database),
		Origins: text.ServerOrigins(links[0].origin, []string{links[1].origin}, []string{links[2].origin}),
		// No timer settles a document while a row reads what its reader wrote.
		Settle: time.Hour,
	})
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = service.Shutdown(stop)
	})

	for _, reader := range []struct {
		name string
		// read is, for each link's address, whether the reader read the link as a reference.
		read func(t *testing.T) map[string]bool
	}{
		{"the move of an approval request to a document's new version (approval_ask.go)", func(t *testing.T) map[string]bool {
			plan := createOwedDocument(t, database, service, "CORE-5", "", "plan", "# Plan\n\nShip it.\n", "")
			settle(t, database, plan)
			// An open approval request over version 1 whose summary holds the links, as the approval
			// route stores one.
			documents := []model.AskApprovalDocument{{ArtifactID: plan, Name: "plan.md", Version: 1, RequestedVersion: 1}}
			approval, err := EncodeApproval(&model.AskApproval{Documents: documents})
			if err != nil {
				t.Fatal(err)
			}
			var ask string
			if err := database.Pool.QueryRow(ctx, `
				insert into asks (issue_key, author, question, options, urgency, kind, approval)
				values ('CORE-5', '{"kind":"session","id":"asker"}', $1, '[{"label":"Approve"},{"label":"Request changes"}]', 'high', 'approval', $2)
				returning id::text
			`, ApprovalQuestion(documents, linked), approval).Scan(&ask); err != nil {
				t.Fatalf("store the approval request: %v", err)
			}
			if _, err := joinedReplaceText(service, plan, "# Plan\n\nShip it on Monday.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
				t.Fatalf("revise the plan: %v", err)
			}
			written, _ := writeVersionThroughTheRealPath(t, service, plan, "Revised")
			var question string
			if err := database.Pool.QueryRow(ctx, `select question from asks where id = $1`, ask).Scan(&question); err != nil {
				t.Fatalf("read the approval request: %v", err)
			}
			if moved := fmt.Sprintf("(version %d)", written.Version.Number); written.Version.Number != 2 || !strings.Contains(question, moved) {
				t.Fatalf("the approval request asks %q after version %d, want it moved to version 2", question, written.Version.Number)
			}
			read := map[string]bool{}
			for _, link := range links {
				var cited bool
				if err := database.Pool.QueryRow(ctx, `
					select exists (select 1 from refs where from_kind = 'ask' and from_id = $1 and to_kind = 'issue' and to_id = $2)
				`, ask, link.issue).Scan(&cited); err != nil {
					t.Fatalf("read the approval request's reference to %s: %v", link.issue, err)
				}
				read[link.address] = cited
			}
			return read
		}},
		{"the excerpt reconciliation's reading of a document's version (reference_excerpts.go)", func(t *testing.T) map[string]bool {
			notes := createOwedDocument(t, database, service, "CORE-5", "", "notes", linked+"\n", "")
			settle(t, database, notes)
			// The index holds an edge from the document to each link's issue, each owed an excerpt,
			// as rebuild-refs leaves the edges it inserts: what the reconciliation's own reading of
			// the text finds a mention of is what gets one.
			for _, link := range links {
				if _, err := database.Pool.Exec(ctx, `
					insert into refs (from_kind, from_id, to_kind, to_id) values ('artifact', $1, 'issue', $2)
				`, notes, link.issue); err != nil {
					t.Fatalf("index the document's reference to %s: %v", link.issue, err)
				}
			}
			if _, _, err := service.ReconcileReferenceExcerpts(ctx); err != nil {
				t.Fatalf("reconcile the reference excerpts: %v", err)
			}
			if isOwed(t, database, notes) {
				t.Fatal("the document's mentions are still owed an excerpt after the reconciliation")
			}
			read := map[string]bool{}
			for _, link := range links {
				var excerpt *string
				if err := database.Pool.QueryRow(ctx, `
					select excerpt_text from refs where from_kind = 'artifact' and from_id = $1 and to_kind = 'issue' and to_id = $2
				`, notes, link.issue).Scan(&excerpt); err != nil {
					t.Fatalf("read the excerpt of the document's reference to %s: %v", link.issue, err)
				}
				read[link.address] = excerpt != nil && strings.Contains(*excerpt, link.origin+"/issues/"+link.issue)
			}
			return read
		}},
	} {
		t.Run(reader.name, func(t *testing.T) {
			read := reader.read(t)
			for _, link := range links {
				t.Run(link.address, func(t *testing.T) {
					if read[link.address] != link.reference {
						t.Errorf("%s reads the link at %s (%s) as a reference: %t, want %t", reader.name, link.address, link.origin, read[link.address], link.reference)
					}
				})
			}
		})
	}
}
