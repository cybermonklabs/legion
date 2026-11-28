package docs

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The two marking rules this test sets side by side: the build of rule 1 that INFRA-1149's parent
// fill ships, which the test stands in for with a Service that applies rule 1
// (Service.markingRule), and the build of rule 2 this release ships (refs.BlockIndexMarkingRule).
const (
	markingRuleOne = 1
	markingRuleTwo = 2
)

// statedBy is the pass that last stated document, as its document_block_versions row records it:
// the version it stated and the marking rule it applied, "" for either that the row holds null.
func statedBy(t *testing.T, database *store.Store, document string) (version, rule string) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(stated_version::text, ''), coalesce(stated_rule::text, '')
		from document_block_versions where artifact_id = $1
	`, document).Scan(&version, &rule); err != nil {
		t.Fatalf("read the pass that stated %s: %v", document, err)
	}
	return version, rule
}

// cursorRule is the marking rule the block_index_event_cursors row of the build of commit records,
// and the event id it stands at.
func cursorRule(t *testing.T, database *store.Store, commit string) (rule int, event int64) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select marking_rule, event_id from block_index_event_cursors where build_commit = $1
	`, commit).Scan(&rule, &event); err != nil {
		t.Fatalf("read the event cursor of build %q: %v", commit, err)
	}
	return rule, event
}

// ruleOneBuild is the stand-in for a build of marking rule 1 on database: the document service
// blockIndexService serves, applying rule 1 where it indexes, marks and clears. A pass of it writes
// the facts this build's pass writes; each case below puts them back as a build of rule 1 left
// them where it needs the absence of one.
func ruleOneBuild(t *testing.T) (*store.Store, *Service) {
	t.Helper()
	database, service := blockIndexService(t)
	service.markingRule = markingRuleOne
	return database, service
}

// TestRule2MarksWhatRule1IndexedAndClearsOnlyItsOwnMarks holds the build of marking rule 2 to the
// four things R13 asks of it beside a build of rule 1, each case on a database of its own:
//
//   - A version a build of rule 1 indexes itself leaves the document's store_version at its version
//     and no pass of rule 2 has stated it (stated_version behind it): the next tick of rule 2 marks
//     the version from its event, and the pass that drains the mark gives the version its document
//     version node and each block its rank, its text and the supertag of its type, which a build of
//     rule 1 wrote none of.
//   - A pass of rule 1 over a document that carries a mark of rule 2 leaves the mark, store_owed_rule
//     2, whatever it indexes, and clears the mark of rule 1 beside it, which is its own: rule 2's
//     build then drains the one it left.
//   - A mark of rule 1 that the backfill of rule 2 finds is raised to rule 2, with its time kept.
//   - The first tick of a build of rule 2 inserts its cursor row with marking_rule 2, at the horizon
//     the backfill of rule 2 recorded, and not at the cursor of the build of rule 1, which stands
//     above that horizon.
//
// The stand-in for a build of rule 1 is a Service that applies rule 1 (Service.markingRule). Its
// pass writes every fact this build's pass does, so the first case deletes the facts rule 2 owes
// before the tick, as asTheReleaseBeforeLeftIt does for the ranks and texts, and the case fails
// where one of them is still absent after rule 2's drain.
func TestRule2MarksWhatRule1IndexedAndClearsOnlyItsOwnMarks(t *testing.T) {
	t.Parallel()
	t.Run("a version a build of rule 1 indexed", func(t *testing.T) {
		ctx := context.Background()
		database, one := ruleOneBuild(t)
		two := markingRuleBuild(t, database, nextBuildCommit, markingRuleTwo)
		appendIssueEvent(t, database)
		const slug = "indexed"
		document := createOwedDocument(t, database, one, "CORE-1", "", slug, nestedSpec(slug), "")
		settle(t, database, document)
		drainBlockIndex(t, one, "the build of rule 1")
		refKey := refKeyOf(t, database, document)
		if row := blockRowOf(t, database, document); !row.standsAt(1) || row.marked {
			t.Fatalf("the build of rule 1 left %s at %s, want it indexed at version 1 with no mark: this test proves nothing", document, row)
		}
		if version, rule := statedBy(t, database, document); version != "1" || rule != "1" {
			t.Fatalf("the build of rule 1 states %s at version %q by rule %q, want version 1 by rule 1", document, version, rule)
		}
		requirePresentVersionNode(t, database, refKey, 1, "after the build of rule 1 indexed the version")
		// A tick of the build of rule 2 reads every event written so far, so the tick below reads
		// the one version event the test writes next.
		awaitedBlockIndexTick(t, two)

		// What a build of rule 1 leaves of the version: store_version at the version, no pass of rule
		// 2 stated it (stated_version and stated_rule null), none of the ranks, texts and placements
		// the pass of rule 2 writes, every typed block's node carrying block, and no version node.
		asTheReleaseBeforeLeftIt(t, database, document, "CORE-1")
		if _, err := database.Pool.Exec(ctx, `
			update graph_nodes set supertag = $3 where kind = $1 and starts_with(id, $2) and supertag <> $3
		`, refs.BlockNodeKind, refKey+"#", refs.BlockSupertag); err != nil {
			t.Fatalf("put the supertag block on the nodes of %s: %v", refKey, err)
		}
		if _, err := database.Pool.Exec(ctx, `
			with tuples as (delete from graph_tuples where (from_kind = $1 and from_id = $2) or (to_kind = $1 and to_id = $2))
			delete from graph_nodes where kind = $1 and id = $2
		`, graphstore.KindVersion, graphstore.VersionID(refKey, 1)); err != nil {
			t.Fatalf("take the node of version 1 of %s away: %v", refKey, err)
		}
		blocks := blockTypeSupertagNodes(t, database, refKey)
		typed := 0
		for _, block := range blocks {
			if block.typ == "callout" && block.supertag == refs.BlockSupertag {
				typed++
			}
			if _, ranked := treeValue(t, database, block.address, treeRank); ranked {
				t.Fatalf("block %s still holds a rank after the build of rule 1 left it: this test proves nothing", block.address)
			}
			if _, texted := treeValue(t, database, block.address, treeText); texted {
				t.Fatalf("block %s still holds a text after the build of rule 1 left it: this test proves nothing", block.address)
			}
		}
		if typed == 0 {
			t.Fatalf("no callout of %s carries the supertag block after the build of rule 1 left it: this test proves nothing: %v", slug, blocks)
		}
		if _, stands, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindVersion, graphstore.VersionID(refKey, 1)); err != nil || stands {
			t.Fatalf("version 1 of %s still holds its node (%t, %v) after the build of rule 1 left it: this test proves nothing", slug, stands, err)
		}
		if row := blockRowOf(t, database, document); !row.standsAt(1) || row.marked {
			t.Fatalf("the build of rule 1 left %s at %s, want its store_version at its version and no mark", document, row)
		}
		if version, rule := statedBy(t, database, document); version != "" || rule != "" {
			t.Fatalf("%s is stated at version %q by rule %q, want no pass of rule 2 to have stated it", document, version, rule)
		}

		payload, err := json.Marshal(map[string]any{"artifact_id": document, "version": map[string]any{"number": 1}})
		if err != nil {
			t.Fatalf("encode the payload of the version's event of %s: %v", document, err)
		}
		if _, err := recordIssueEvent(ctx, database.Pool, "CORE-1", "artifact.version", aliceActor, payload); err != nil {
			t.Fatalf("append the version's event of %s: %v", document, err)
		}
		tick := awaitedBlockIndexTick(t, two)
		if tick.Marked != 1 {
			t.Errorf("the tick of the build of rule %d marked %d document(s), want the one version a build of rule %d indexed and no pass of rule %d stated",
				markingRuleTwo, tick.Marked, markingRuleOne, markingRuleTwo)
		}
		drainBlockIndex(t, two, "the build of rule 2")
		if marked := markedDocuments(t, database); marked != 0 {
			t.Errorf("%d document(s) still hold a mark after the build of rule 2 drained, want none", marked)
		}
		if version, rule := statedBy(t, database, document); version != "1" || rule != "2" {
			t.Errorf("after rule 2's drain %s is stated at version %q by rule %q, want version 1 by rule 2", document, version, rule)
		}
		requirePresentVersionNode(t, database, refKey, 1, "after the pass of rule 2")
		for _, block := range blockTypeSupertagNodes(t, database, refKey) {
			if _, ranked := treeValue(t, database, block.address, treeRank); !ranked {
				t.Errorf("block %s holds no rank after the pass of rule 2", block.address)
			}
			if _, texted := treeValue(t, database, block.address, treeText); !texted {
				t.Errorf("block %s holds no text after the pass of rule 2", block.address)
			}
		}
		blockTypeSupertagRequire(t, blockTypeSupertagNodes(t, database, refKey))
	})

	t.Run("a pass of rule 1 over a mark of rule 2", func(t *testing.T) {
		ctx := context.Background()
		database, one := ruleOneBuild(t)
		two := markingRuleBuild(t, database, nextBuildCommit, markingRuleTwo)
		documents := map[string]string{}
		for _, slug := range []string{"higher", "lower"} {
			documents[slug] = createOwedDocument(t, database, one, "CORE-1", "", slug, nestedSpec(slug), "")
			settle(t, database, documents[slug])
		}
		drainBlockIndex(t, one, "seed")
		// The release before this one indexes each document again, so each one's stand is below its
		// version and the pass of rule 1 has work of its own there: it indexes the version, wins the
		// fence at it and states it, whatever mark the row holds.
		versions := map[string]int{}
		for slug, document := range documents {
			versions[slug] = restateAsThePreviousRelease(t, database, one, document)
		}
		for slug, rule := range map[string]int{"higher": markingRuleTwo, "lower": markingRuleOne} {
			if _, err := database.Pool.Exec(ctx, graphcensus.BlockBatchMarkStatement, []string{documents[slug]}, rule); err != nil {
				t.Fatalf("mark %s at marking rule %d: %v", slug, rule, err)
			}
			if row := blockRowOf(t, database, documents[slug]); !row.marked || row.rule != rule || row.standsAt(versions[slug]) {
				t.Fatalf("the mark of rule %d left %s at %s, want it marked by rule %d with its stand below version %d: this test proves nothing",
					rule, slug, row, rule, versions[slug])
			}
		}

		reconcileBlockIndex(t, one, "")
		lower := blockRowOf(t, database, documents["lower"])
		if !lower.clearedAt(versions["lower"]) {
			t.Errorf("a pass of rule %d left the document holding its own mark of rule %d at %s, want the mark cleared with the version %d stated",
				markingRuleOne, markingRuleOne, lower, versions["lower"])
		}
		higher := blockRowOf(t, database, documents["higher"])
		if !higher.marked || higher.rule != markingRuleTwo {
			t.Errorf("a pass of rule %d over a mark of rule %d left %s, want the mark left, store_owed_rule %d", markingRuleOne, markingRuleTwo, higher, markingRuleTwo)
		}
		if higher.version == nil || !higher.standsAt(versions["higher"]) {
			t.Errorf("a pass of rule %d over a mark of rule %d left %s, want it to have stated the version %d it indexed",
				markingRuleOne, markingRuleTwo, higher, versions["higher"])
		}
		// The build of rule 2 then drains the mark it was left and states the version by its rule.
		if _, written := tickWrites(t, two); !written[documents["higher"]] {
			t.Errorf("a tick of the build of rule %d wrote no version of %s after the pass of rule %d left its mark", markingRuleTwo, documents["higher"], markingRuleOne)
		}
		if row := blockRowOf(t, database, documents["higher"]); !row.clearedAt(versions["higher"]) {
			t.Errorf("after the build of rule %d drained, %s stands at %s, want its mark cleared at version %d", markingRuleTwo, documents["higher"], row, versions["higher"])
		}
		if _, rule := statedBy(t, database, documents["higher"]); rule != "2" {
			t.Errorf("after the build of rule %d drained, %s is stated by rule %q, want rule %d", markingRuleTwo, documents["higher"], rule, markingRuleTwo)
		}
	})

	t.Run("a mark of rule 1 the backfill of rule 2 finds", func(t *testing.T) {
		ctx := context.Background()
		database, one := ruleOneBuild(t)
		document := createOwedDocument(t, database, one, "CORE-1", "", "marked", nestedSpec("marked"), "")
		settle(t, database, document)
		drainBlockIndex(t, one, "the build of rule 1")
		// The mark is dated well before the backfill runs, so a mark written again would read a later
		// time. The pass of rule 1 stated the document by rule 1, which is below rule 2.
		wrote := time.Date(2026, 11, 1, 12, 30, 0, 0, time.UTC)
		if _, err := database.Pool.Exec(ctx, `
			update document_block_versions set store_owed_at = $2, store_owed_rule = $3 where artifact_id = $1
		`, document, wrote, markingRuleOne); err != nil {
			t.Fatalf("mark %s at marking rule %d: %v", document, markingRuleOne, err)
		}
		if row := blockRowOf(t, database, document); !row.marked || row.rule != markingRuleOne {
			t.Fatalf("the mark left %s at %s, want a mark of rule %d: this test proves nothing", document, row, markingRuleOne)
		}
		if _, rule := statedBy(t, database, document); rule != "1" {
			t.Fatalf("%s is stated by rule %q, want rule 1, which is below the rule the backfill marks at: this test proves nothing", document, rule)
		}

		if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
			t.Fatalf("backfill the store at marking rule %d: %v", refs.BlockIndexMarkingRule, err)
		}
		if row := blockRowOf(t, database, document); !row.marked || row.rule != markingRuleTwo {
			t.Errorf("the backfill of rule %d left %s at %s, want its mark of rule %d raised to rule %d",
				markingRuleTwo, document, row, markingRuleOne, markingRuleTwo)
		}
		if at, _ := backfillMark(t, database, document); at == nil || !at.Equal(wrote) {
			t.Errorf("the backfill of rule %d left the mark of %s dated %v, want the time of the mark it raised, %s",
				markingRuleTwo, document, at, wrote.Format(time.RFC3339Nano))
		}
	})

	t.Run("the first tick of rule 2", func(t *testing.T) {
		ctx := context.Background()
		database, one := ruleOneBuild(t)
		appendIssueEvent(t, database)
		document := createOwedDocument(t, database, one, "CORE-1", "", "cursor", nestedSpec("cursor"), "")
		settle(t, database, document)
		drainBlockIndex(t, one, "the build of rule 1")
		awaitedBlockIndexTick(t, one)

		// The backfill of rule 2 records the horizon it began at, and the build of rule 1 then reads
		// events past it and moves its cursor above that horizon.
		if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
			t.Fatalf("backfill the store at marking rule %d: %v", refs.BlockIndexMarkingRule, err)
		}
		horizon := recordedBackfillHorizon(t, database, markingRuleTwo)
		appendIssueEvent(t, database)
		appendIssueEvent(t, database)
		awaitedBlockIndexTick(t, one)
		ruleOne, cursor := cursorRule(t, database, "")
		if ruleOne != markingRuleOne || cursor <= horizon {
			t.Fatalf("the build of rule 1 holds a cursor row of rule %d at %d, want rule %d above the backfill's horizon %d: this test proves nothing",
				ruleOne, cursor, markingRuleOne, horizon)
		}

		two := markingRuleBuild(t, database, nextBuildCommit, markingRuleTwo)
		var rows int
		if err := database.Pool.QueryRow(ctx, `select count(*) from block_index_event_cursors where marking_rule = $1`, markingRuleTwo).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("%d cursor row(s) of rule %d stand before its first tick (%v), want none: this test proves nothing", rows, markingRuleTwo, err)
		}
		tick := awaitedBlockIndexTick(t, two)
		if started := tickStart(t, database, nextBuildCommit, tick); started != horizon {
			t.Errorf("the first tick of the build of rule %d started at %d, want the horizon %d the backfill of rule %d recorded, and not the cursor %d of the build of rule %d",
				markingRuleTwo, started, horizon, markingRuleTwo, cursor, markingRuleOne)
		}
		rule, event := cursorRule(t, database, nextBuildCommit)
		if rule != markingRuleTwo {
			t.Errorf("the first tick of the build of rule %d inserted its cursor row with marking_rule %d, want %d", markingRuleTwo, rule, markingRuleTwo)
		}
		if event < horizon {
			t.Errorf("the cursor row of rule %d stands at %d, below the backfill's horizon %d", markingRuleTwo, event, horizon)
		}
		if rule, now := cursorRule(t, database, ""); rule != markingRuleOne || now != cursor {
			t.Errorf("the cursor row of the build of rule %d reads rule %d at %d after rule %d's first tick, want it left at rule %d, %d",
				markingRuleOne, rule, now, markingRuleTwo, markingRuleOne, cursor)
		}
	})
}
