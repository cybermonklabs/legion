package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cputest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specform"
)

// countingQueryer is q with a count of the statements a judgment sends through it, each statement
// of a batch among them.
type countingQueryer struct {
	q          queryer
	statements int
}

func (c *countingQueryer) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.statements++
	return c.q.QueryRow(ctx, sql, args...)
}

func (c *countingQueryer) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.statements++
	return c.q.Query(ctx, sql, args...)
}

func (c *countingQueryer) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	c.statements += batch.Len()
	return c.q.SendBatch(ctx, batch)
}

// levelTwoWorkGrowth is how many times the work of a judgment may grow when its document grows
// four times: work in proportion to the statements grows about four times, and work for each pair
// of texts about sixteen.
const levelTwoWorkGrowth = 6

// A document of the size of level two, 301 statements that all break the form, and the same
// document with every statement clean, is judged in work in proportion to its statements. The
// judgment (specFormOf) of a document of four times as many statements sends as many statements to
// the database, so it reads the database once for the document and not once for each text, and
// its work grows less than levelTwoWorkGrowth times, so it does no work for each pair of texts.
// The work is the user CPU time of the thread that judges, the work of the judgment's own code,
// measured as the form's own table measures it (cputest.Growth, as
// specform.TestTheWorkOfAJudgmentGrowsInProportionToItsText does): a wait for a core is not in it,
// nor the kernel's work for the thread, nor the work of Postgres, which the count of statements
// bounds; work that allocates nothing is in it. All the documents stand under the root before any
// is judged, since the judgment reads each other covered document under it. Through the request
// route, the broken document of 301 statements is refused, and the clean one opens.
func TestALevelTwoSizedDocumentIsJudgedInWorkInProportionToItsStatements(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	served := &server{deps: tree.deps}
	const scale = 4
	sizes := []int{301, scale * 301}
	for _, test := range []struct {
		name, body string
		broken     bool
	}{
		{name: "broken", body: formFaults[0].broken, broken: true},
		{name: "clean", body: "Dispatch shows the person each question."},
	} {
		t.Run(test.name, func(t *testing.T) {
			issues := map[int]formIssue{}
			for _, count := range sizes {
				var spec strings.Builder
				spec.WriteString("# Level two\n")
				for index := 1; index <= count; index++ {
					fmt.Fprintf(&spec, "\n:::requirement{#statement-%d}\n%s\n:::\n", index, test.body)
				}
				issues[count] = createFormIssue(t, tree.handler, tree.root, fmt.Sprintf("Level two sized, %d statements: %s", count, test.name), spec.String())
				awaitSettled(t, tree.database, issues[count].PrimaryArtifactID)
			}
			statements := map[int]int{}
			judge := func(count int) func() {
				return func() {
					documents := []model.AskApprovalDocument{{ArtifactID: issues[count].PrimaryArtifactID, Name: "spec.md", Version: 1}}
					counter := &countingQueryer{q: tree.database.Pool}
					result, err := served.specFormOf(context.Background(), counter, documents, specform.ActRequest)
					if err != nil {
						t.Fatalf("judge %d statements: %v", count, err)
					}
					if found, want := len(result.refusing()), map[bool]int{true: count, false: 0}[test.broken]; found != want {
						t.Fatalf("the judgment of %d statements has %d findings, want %d", count, found, want)
					}
					statements[count] = counter.statements
				}
			}
			small, large := sizes[0], sizes[1]
			grows, smallWork, largeWork, err := cputest.Growth(3, scale, cputest.Again(judge(small)), cputest.Again(judge(large)))
			t.Logf("%d statements %d times: %d sent each, %v; %d statements once: %d sent, %v; %.1f times",
				small, scale, statements[small], smallWork, large, statements[large], largeWork, grows)
			if err != nil {
				t.Fatal(err)
			}
			if statements[small] == 0 {
				t.Fatal("the judgment sent no statement to the database, so this test counts nothing")
			}
			if statements[large] != statements[small] {
				t.Errorf("the judgment sent %d statements for %d statements and %d for %d, want the same count",
					statements[large], large, statements[small], small)
			}
			if grows >= levelTwoWorkGrowth {
				t.Errorf("the work of the judgment grew %.1f times, from %v for %d runs of %d statements to %v for one run of %d, want less than %d times",
					grows, smallWork, scale, small, largeWork, large, levelTwoWorkGrowth)
			}
			want := map[bool]int{true: http.StatusConflict, false: http.StatusCreated}[test.broken]
			if response := requestFormApproval(t, tree.handler, issues[small].PrimaryArtifactID, nil); response.Code != want {
				t.Fatalf("request of %d statements: status=%d, want %d", small, response.Code, want)
			}
		})
	}
}

// A judgment under a root reads the latest approved version of each other covered document under
// that root, for the names of the system that those versions define, in one statement to the
// database: a judgment under a root of 4 approved documents sends as many statements as one under
// a root of 1, and so does one under 7. Each approved document defines one component, and the
// judgment reads the name of each.
func TestAJudgmentReadsTheApprovedDocumentsUnderItsRootInOneStatement(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	served := &server{deps: tree.deps}
	approved := 0
	approve := func(count int) {
		for ; approved < count; approved++ {
			spec := fmt.Sprintf("# Components\n\n## Part %d (component `part-%d`)\n\nThe server keeps each issue and each document.\n", approved, approved)
			issue := createFormIssue(t, tree.handler, tree.root, fmt.Sprintf("Approved %d", approved), spec)
			awaitSettled(t, tree.database, issue.PrimaryArtifactID)
			if response := reviewFormDocument(t, tree.handler, issue.PrimaryArtifactID, "approved", ""); response.Code != http.StatusCreated {
				t.Fatalf("approve document %d: status=%d body=%s", approved, response.Code, response.Body.String())
			}
		}
	}
	judged := createFormIssue(t, tree.handler, tree.root, "The judged document",
		"# Spec\n\n:::requirement{#judged}\nDispatch shows the person each question.\n:::\n")
	awaitSettled(t, tree.database, judged.PrimaryArtifactID)
	documents := []model.AskApprovalDocument{{ArtifactID: judged.PrimaryArtifactID, Name: "spec.md", Version: 1}}
	sent := map[int]int{}
	for _, count := range []int{1, 4, 7} {
		approve(count)
		read, err := served.approvedSpecTexts(context.Background(), tree.database.Pool, tree.root, map[string]bool{judged.PrimaryArtifactID: true})
		if err != nil {
			t.Fatalf("read the approved documents under a root of %d: %v", count, err)
		}
		if len(read.SystemNames) != count {
			t.Fatalf("under a root of %d approved documents the judgment read the names %q, want %d", count, read.SystemNames, count)
		}
		counter := &countingQueryer{q: tree.database.Pool}
		if _, err := served.specFormOf(context.Background(), counter, documents, specform.ActRequest); err != nil {
			t.Fatalf("judge under a root of %d approved documents: %v", count, err)
		}
		sent[count] = counter.statements
	}
	t.Logf("statements sent for 1, 4 and 7 approved documents: %v", sent)
	if sent[4] != sent[1] || sent[7] != sent[1] {
		t.Errorf("the judgment sent %v statements for 1, 4 and 7 approved documents under its root, want one count for all three", sent)
	}
}
