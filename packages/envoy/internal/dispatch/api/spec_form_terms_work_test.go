package api

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/cputest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specform"
)

// The walks of the list of terms of a root (R50) do work in proportion to what they read, measured
// as the CPU time of the thread that walks (cputest.OnThread), of n units and of 4n, the two sizes
// in turn, the least of the samples of each; the form's own table
// (specform.TestTheWorkOfAJudgmentGrowsInProportionToItsText) counts the statements of each rule's
// judgment instead. The work of the larger grows less than levelTwoWorkGrowth times. A walk that
// reads the record in memory works on maps, whose cost for each entry grows once they no longer
// fit the cache of a core, so the sizes of such a row keep both maps within it, and a window runs
// the walk as many times as it takes to last.

// walkSamples is how many times each size is timed. A walk of the record in memory takes less than
// a millisecond, and the other thread of its core, busy on a loaded machine, can double the CPU
// time of one window, so the least of seven samples is its work.
const walkSamples = 7

// leastWork is the CPU time of one call of each of calls, each window timed on its thread
// (cputest.MeasureThread), the least of walkSamples samples, the calls in turn, so that a change
// of the machine's load reaches each.
func leastWork(calls ...func()) []time.Duration {
	least := make([]time.Duration, len(calls))
	for sample := range walkSamples {
		for index, call := range calls {
			runtime.GC()
			took := cputest.MeasureThread(func(runs int) func() {
				return func() {
					for range runs {
						call()
					}
				}
			})
			if sample == 0 || took < least[index] {
				least[index] = took
			}
		}
	}
	return least
}

// requireLinearWork fails t when the work of the larger of two sizes grows levelTwoWorkGrowth
// times or more over the work of the smaller.
func requireLinearWork(t *testing.T, sizes []int, least []time.Duration) {
	t.Helper()
	grows := float64(least[1]) / float64(least[0])
	t.Logf("%d units: %v; %d units: %v; %.1f times", sizes[0], least[0], sizes[1], least[1], grows)
	if grows >= levelTwoWorkGrowth {
		t.Errorf("the work grew %.1f times, from %v for %d units to %v for %d, want less than %d times",
			grows, least[0], sizes[0], least[1], sizes[1], levelTwoWorkGrowth)
	}
}

// noRows is a queryer whose every read finds no row, so that the work of a walk that reads the
// database is its own work alone.
type noRows struct{}

func (noRows) QueryRow(context.Context, string, ...any) pgx.Row { return emptyRows{} }

func (noRows) Query(context.Context, string, ...any) (pgx.Rows, error) { return emptyRows{}, nil }

func (noRows) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return nil }

// emptyRows is the answer of a read that finds no row. It has the methods that pgx.ForEachRow and
// pgx.CollectRows call; any other one is the nil pgx.Rows it holds, which panics.
type emptyRows struct{ pgx.Rows }

func (emptyRows) Close()                        {}
func (emptyRows) Err() error                    { return nil }
func (emptyRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (emptyRows) Next() bool                    { return false }
func (emptyRows) Scan(...any) error             { return pgx.ErrNoRows }

// Each walk of the list that reads the record in memory, row by row: the unread list of a
// judgment, which each refusal and each answer gives each version of once; the documents of many
// definitions of one term, whose join order reads the moves of their issues; the parents of many
// issues, which the next step up of the history reads; and one issue that moved many times inside
// the root, whose arrival walks back each move. Each walk answers what it found, and a row whose
// walk finds nothing at either size reads nothing, so it fails.
func TestTheWalksOfTheListGrowInProportionToWhatTheyRead(t *testing.T) {
	const root = "ACME-1"
	roots := []string{root}
	for _, row := range []struct {
		name string
		n    int
		walk func(n int) func() int
	}{
		{"the unread list of a judgment", 1_000, func(n int) func() int {
			issue, rooted := "ACME-2", root
			document := specFormDocument{
				artifact: model.Artifact{ID: "judged", Name: "spec.md", IssueKey: &issue}, version: 1,
				specScope: specScope{scope: specScopeCovered, root: &rooted},
				findings: []specFormFinding{{Document: "spec.md", ArtifactID: "judged", Version: 1,
					Finding: specform.Finding{Node: "s", Kind: specform.KindStatement, Where: "spec.md version 1", Rule: "sentences"}}},
			}
			for index := range n {
				unread := fmt.Sprintf("ACME-%d", index+3)
				document.list.Unread = append(document.list.Unread, specFormUnread{
					Document: "spec.md", ArtifactID: fmt.Sprintf("unread-%d", index), Issue: unread, Version: 1,
					Line: specform.UnreadLine("spec.md", 1, unread),
				})
			}
			result := specFormResult{documents: []specFormDocument{document}}
			// The refusal holds the line of each version (its notes); the walk that the row times is
			// the one that gives each version once, which the refusal and each answer share.
			if body := result.refusal(specform.ActRequest).body(); !strings.Contains(body.Error, document.list.Unread[n-1].Line) {
				t.Fatalf("the refusal of %d unread versions does not name the last: %.300s", n, body.Error)
			}
			return func() int { return len(result.summary("judged").Unread) }
		}},
		{"the documents of many definitions of one term", 1_000, func(n int) func() int {
			documents := make([]approvedSpecDocument, n)
			contested := map[int][]string{}
			for index := range documents {
				documents[index] = approvedSpecDocument{
					id: fmt.Sprintf("document-%d", index), name: "spec.md", issue: fmt.Sprintf("ACME-%d", index+2),
					version: 1, terms: []specform.Term{{Name: "lease", Definition: "The hold of one session on one work unit."}},
				}
				contested[index] = []string{"lease"}
			}
			return func() int {
				joins, _, err := definitionJoins(context.Background(), noRows{}, root, roots, documents, contested)
				if err != nil {
					t.Fatal(err)
				}
				return len(joins)
			}
		}},
		{"the parents of many issues", 1_000, func(n int) func() int {
			history := parentHistory{}
			keys := make([]string, n)
			for index := range keys {
				keys[index] = fmt.Sprintf("ACME-%d", index+2)
				history[keys[index]] = []parentChange{{event: int64(index + 1), parent: fmt.Sprintf("ACME-%d", n+index+2)}}
			}
			return func() int { return len(history.above(keys)) }
		}},
		{"one issue that moved many times inside the root", 4_000, func(n int) func() int {
			// ACME-2 stands outside the root; ACME-4 and ACME-5 stand under it. ACME-3 was made under
			// ACME-2, came under the root at event 11, and then moved between ACME-4 and ACME-5.
			history := parentHistory{
				"ACME-4": {{event: 1, parent: root}},
				"ACME-5": {{event: 2, parent: root}},
				"ACME-3": {{event: 10, parent: "ACME-2"}, {event: 11, parent: "ACME-4"}},
			}
			for index := range n {
				parent := map[bool]string{true: "ACME-5", false: "ACME-4"}[index%2 == 0]
				history["ACME-3"] = append(history["ACME-3"], parentChange{event: int64(index + 12), parent: parent})
			}
			return func() int {
				if event, arrived := history.arrival("ACME-3", root, roots); arrived {
					return int(event)
				}
				return 0
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			sizes := []int{row.n, 4 * row.n}
			walks := []func() int{row.walk(sizes[0]), row.walk(sizes[1])}
			found := make([]int, len(walks))
			least := leastWork(func() { found[0] = walks[0]() }, func() { found[1] = walks[1]() })
			if found[0] == 0 || found[1] == 0 {
				t.Fatalf("the walk found %v at %v units, so this row reads nothing", found, sizes)
			}
			requireLinearWork(t, sizes, least)
		})
	}
}

// The list of a root that two documents make, each defining the same terms, is made in work in
// proportion to its terms: the read of the approved documents, the definitions of each term, the
// order in which each definition joined the list (definitionJoins) and the join order of the
// definitions of each term (orderDefinitions). Each size has a root of its own, outside the roots
// of the server's setting, so its approvals are not judged, and the walk reads that root alone.
func TestTheListOfTwoDocumentsThatDefineTheSameTermsIsMadeInWorkInProportionToItsTerms(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	sizes := []int{250, 4 * 250}
	form := tree.deps.SpecForm
	form.roots = []string{tree.root}
	reads := make([]func() int, len(sizes))
	for index, size := range sizes {
		root := createFormIssue(t, tree.handler, "", fmt.Sprintf("A root of %d terms", size), "# The Spec\n\nThe design of the written account.\n")
		form.roots = append(form.roots, root.Key)
		var spec strings.Builder
		spec.WriteString("# Spec\n\n## Terms\n\n")
		for term := range size {
			fmt.Fprintf(&spec, "- **term %d**: Dispatch keeps record %d.\n", term+1, term+1)
		}
		for _, name := range []string{"first", "second"} {
			document := createFormIssue(t, tree.handler, root.Key, fmt.Sprintf("The %s document of %d terms", name, size), spec.String())
			awaitSettled(t, tree.database, document.PrimaryArtifactID)
			approveFormDocument(t, tree.handler, document.PrimaryArtifactID)
		}
		reads[index] = func() int {
			approved, err := form.approvedSpecTexts(context.Background(), tree.database.Pool, root.Key)
			if err != nil {
				t.Fatal(err)
			}
			return len(approved.definitions)
		}
	}
	listed := make([]int, len(reads))
	least := leastWork(func() { listed[0] = reads[0]() }, func() { listed[1] = reads[1]() })
	if listed[0] != sizes[0] || listed[1] != sizes[1] {
		t.Fatalf("the lists hold %v terms, want %v", listed, sizes)
	}
	requireLinearWork(t, sizes, least)
}
