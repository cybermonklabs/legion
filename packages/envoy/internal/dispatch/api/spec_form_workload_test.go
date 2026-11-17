package api

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

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
// The work is the CPU time of the thread that judges (cputest.OfCall), as the form's own table
// measures it (specform.TestTheWorkOfAJudgmentGrowsInProportionToItsText): a wait for a core is
// not in it, and work that allocates nothing is. Each size is judged three times, the two sizes
// in turn, and the least of each is its work. All the documents stand under the root before any is
// judged, since the judgment reads each other covered document under it. Through the request
// route, the broken document of 301 statements is refused, and the clean one opens.
func TestALevelTwoSizedDocumentIsJudgedInWorkInProportionToItsStatements(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	served := &server{deps: tree.deps}
	sizes := []int{301, 4 * 301}
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
			least := map[int]time.Duration{}
			statements := map[int]int{}
			for sample := range 3 {
				for _, count := range sizes {
					documents := []model.AskApprovalDocument{{ArtifactID: issues[count].PrimaryArtifactID, Name: "spec.md", Version: 1}}
					counter := &countingQueryer{q: tree.database.Pool}
					var result specFormResult
					var err error
					runtime.GC()
					took := cputest.OfCall(func() {
						result, err = served.specFormOf(context.Background(), counter, documents, specform.ActRequest)
					})
					if err != nil {
						t.Fatalf("judge %d statements: %v", count, err)
					}
					if found, want := len(result.refusing()), map[bool]int{true: count, false: 0}[test.broken]; found != want {
						t.Fatalf("the judgment of %d statements has %d findings, want %d", count, found, want)
					}
					if sample == 0 || took < least[count] {
						least[count] = took
					}
					statements[count] = counter.statements
				}
			}
			small, large := sizes[0], sizes[1]
			grows := float64(least[large]) / float64(least[small])
			t.Logf("%d statements: %d sent, %v; %d statements: %d sent, %v; %.1f times",
				small, statements[small], least[small], large, statements[large], least[large], grows)
			if statements[small] == 0 {
				t.Fatal("the judgment sent no statement to the database, so this test counts nothing")
			}
			if statements[large] != statements[small] {
				t.Errorf("the judgment sent %d statements for %d statements and %d for %d, want the same count",
					statements[large], large, statements[small], small)
			}
			if grows >= levelTwoWorkGrowth {
				t.Errorf("the work of the judgment grew %.1f times, from %v for %d statements to %v for %d, want less than %d times",
					grows, least[small], small, least[large], large, levelTwoWorkGrowth)
			}
			want := map[bool]int{true: http.StatusConflict, false: http.StatusCreated}[test.broken]
			if response := requestFormApproval(t, tree.handler, issues[small].PrimaryArtifactID, nil); response.Code != want {
				t.Fatalf("request of %d statements: status=%d, want %d", small, response.Code, want)
			}
		})
	}
}
