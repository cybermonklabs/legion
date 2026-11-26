package docs

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// readingSupertagsFromDocs is the supertag the typed-node pass gives each reading of the newest
// version of document, by the reading's block id: reading where the reading stands in a decision,
// or in an ask block whose ask holds an answer, as docs reads it (shownAsks, which is
// readingAnswers over the version's own tree), and block otherwise (refs.ReadingHolders names each
// reading's holder as the pass does).
func readingSupertagsFromDocs(t *testing.T, service *Service, document string) map[string]string {
	t.Helper()
	ctx := context.Background()
	markdown := storedVersionMarkdown(t, service.store, document, latestVersionNumber(t, service, document))
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("read the newest version of %s: %v", document, err)
	}
	holders := refs.ReadingHolders(tree)
	var holding []string
	for _, holder := range holders {
		if holder.Ask && !slices.Contains(holding, holder.ID) {
			holding = append(holding, holder.ID)
		}
	}
	var shown map[string]refs.ShownAsk
	if len(holding) > 0 {
		if err := pgx.BeginFunc(ctx, service.store.Pool, func(tx pgx.Tx) (err error) {
			shown, err = shownAsks(ctx, tx, document, tree, holding)
			return err
		}); err != nil {
			t.Fatalf("read the asks the ask blocks of %s show: %v", document, err)
		}
	}
	supertags := make(map[string]string, len(holders))
	for reading, holder := range holders {
		supertags[reading] = refs.BlockSupertag
		if !holder.Ask || shown[holder.ID].Answered {
			supertags[reading] = refs.ReadingSupertag
		}
	}
	return supertags
}

// readingSupertagsFromSQL is the supertag refs.BlockTypeSupertagSQL gives each reading row of
// document, by the reading's block id.
func readingSupertagsFromSQL(t *testing.T, database *store.Store, document string) map[string]string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select d.block_id, `+refs.BlockTypeSupertagSQL("d")+`
		from document_blocks d where d.artifact_id = $1 and d.type = $2 order by d.block_id
	`, document, refs.ReadingSupertag)
	if err != nil {
		t.Fatalf("read the supertag the census gives each reading of %s: %v", document, err)
	}
	supertags := map[string]string{}
	for rows.Next() {
		var block, supertag string
		if err := rows.Scan(&block, &supertag); err != nil {
			t.Fatalf("scan a reading of %s: %v", document, err)
		}
		supertags[block] = supertag
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the readings of %s: %v", document, err)
	}
	return supertags
}

// TestTheCensusReadsAReadingAsDocsReadingAnswersDoes holds refs.BlockTypeSupertagSQL, the one
// rule the block_type census reads a reading's node by, to docs' reading of the same fact
// (readingAnswers, through shownAsks) over one reading of each case it decides: a reading in a
// decision; in an ask block that its own document's ask answers, whether the ask is answered or
// open, and whether the ask was retracted by settlement while it held an answer and came back with
// its block; in a copy of another document's ask, whether the source is answered or open; and in an
// ask block that no ask row indexes and no source is copied from, before a settlement opens the
// ask. Each reading's row carries reading where docs reads the ask it shows answered and block
// where it does not, and the case fails where either side reads other than the table says, so a
// case that proves nothing, such as a copy that is no copy, is named by its own line.
func TestTheCensusReadsAReadingAsDocsReadingAnswersDoes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, service := blockIndexService(t)

	// A reading is added to its ask the way a browser adds one, and the version holding it is
	// written through the real version write, which a write that takes markdown cannot do for a
	// reading in an ask that holds no answer (refuseReadingsWithoutAnswers).
	withReading := func(document string) {
		t.Helper()
		editLiveTree(t, service, document, appendToAsk(readingNode()))
		if _, err := blockTypeSupertagWrite(ctx, service, document, nil, "A version with its reading"); err != nil {
			t.Fatalf("write the version of %s that holds its reading: %v", document, err)
		}
	}
	// ask seeds a document of issue number with a ship ask of its own, which settlement indexes.
	ask := func(number int, markdown string) string {
		t.Helper()
		document := createIssueDocument(t, database, number, "# Document")
		seedServiceText(t, service, document, markdown)
		settleCurrentGeneration(t, service, document)
		return document
	}

	// An answered ask of its own, and a copy of it on another document of its issue.
	answeredOwn := ask(1, shipAsk("ship"))
	answerBlockAsk(t, service, answeredOwn)
	withReading(answeredOwn)
	answeredCopy := ask(1, "Record\n\n"+shipAsk("ship"))
	withReading(answeredCopy)

	// An open ask of its own, and a copy of it on another document of its issue.
	openOwn := ask(2, "Record\n\n"+shipAsk("ship"))
	withReading(openOwn)
	openCopy := ask(2, "Record\n\n"+shipAsk("ship"))
	withReading(openCopy)

	// A reading in a decision.
	decision := createIssueDocument(t, database, 3, "# Decision")
	seedServiceText(t, service, decision, orderDecisionReading)
	if _, err := blockTypeSupertagWrite(ctx, service, decision, nil, "A version with its decision"); err != nil {
		t.Fatalf("write the version of %s that holds its decision: %v", decision, err)
	}

	// An ask block no ask row indexes, which no settlement has opened, in a document of an issue
	// whose other documents hold no such ask: no source to be a copy of.
	unindexed, _ := blockTypeSupertagDocument(t, database, service, "CORE-1", "CORE", "unindexed", false, "", "Record\n\n"+shipAsk("ship"),
		appendToAsk(readingNode()), false)

	// An ask settlement retracted while it held an answer, which came back with its block and a
	// reading, and which the settlement then restored with the answer (ask_readings_test.go).
	retracted := createIssueDocument(t, database, 4, "# Retracted")
	seedServiceText(t, service, retracted, shipAsk("ship"))
	settleCurrentGeneration(t, service, retracted)
	answerAsRecorded(t, service, retracted, "ship", []string{"Ship"})
	editLiveTree(t, service, retracted, func(*pmdoc.Node) *pmdoc.Node {
		return &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "Context"}}}}}
	})
	settleCurrentGeneration(t, service, retracted)
	retractAnswered(t, service, retracted, "ship")
	editLiveTree(t, service, retracted, appendBlocks(t, shipAskWithReading("ship")))
	settleCurrentGeneration(t, service, retracted)

	for _, test := range []struct {
		name     string
		document string
		reading  string
		want     string
	}{
		{"a reading in a decision", decision, "d1-r1", refs.ReadingSupertag},
		{"an ask of its own document that holds an answer", answeredOwn, "r1", refs.ReadingSupertag},
		{"an ask of its own document that is open", openOwn, "r1", refs.BlockSupertag},
		{"an ask settlement retracted while it was answered, restored", retracted, "r1", refs.ReadingSupertag},
		{"a copy of an answered ask of another document of its issue", answeredCopy, "r1", refs.ReadingSupertag},
		{"a copy of an open ask of another document of its issue", openCopy, "r1", refs.BlockSupertag},
		{"an ask block no ask row indexes and no source holds", unindexed, "r1", refs.BlockSupertag},
	} {
		fromDocs := readingSupertagsFromDocs(t, service, test.document)
		fromSQL := readingSupertagsFromSQL(t, database, test.document)
		if len(fromDocs) != 1 || len(fromSQL) != 1 {
			t.Errorf("%s: document %s holds the readings %v by docs' read of its newest version and %v by its rows, want its one reading %s in both: this case proves nothing",
				test.name, test.document, fromDocs, fromSQL, test.reading)
			continue
		}
		if fromDocs[test.reading] != test.want {
			t.Errorf("%s: docs reads the reading %s as %q, want %q: this case's fixture is not the case it names", test.name, test.reading, fromDocs[test.reading], test.want)
		}
		if fromSQL[test.reading] != fromDocs[test.reading] {
			t.Errorf("%s: refs.BlockTypeSupertagSQL gives the reading %s the supertag %q where docs' readingAnswers reads %q", test.name, test.reading, fromSQL[test.reading], fromDocs[test.reading])
		}
	}
}
