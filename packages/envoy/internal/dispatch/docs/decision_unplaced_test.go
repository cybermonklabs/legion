package docs

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The decisions document's address in the tests' issue, DOC-1.
const testDecisionsAddress = "dispatch://DOC-1/artifact/decisions-md"

// headingID is the block id of the heading of the section titled title in document's live tree.
func (issue *restoreIssue) headingID(document, title string) string {
	issue.t.Helper()
	for _, section := range decisionSectionsOf(liveTree(issue.t, issue.service, document)) {
		if section.title == title {
			return section.headingID()
		}
	}
	issue.t.Fatalf("%s holds no section %q", document, title)
	return ""
}

// indexedVersion is the version document's block index stands at, 0 where it owes one.
func (issue *restoreIssue) indexedVersion(document string) int {
	issue.t.Helper()
	var version int
	if err := issue.database.Pool.QueryRow(context.Background(), `
		select coalesce(version, 0) from document_block_versions where artifact_id = $1
	`, document).Scan(&version); err != nil {
		issue.t.Fatalf("read the version the block index of %s stands at: %v", document, err)
	}
	return version
}

// sectionCitation is the citation of the section whose heading is heading in version of the tests'
// decisions document.
func sectionCitation(heading string, version int) string {
	return refs.BlockRef(testDecisionsAddress, heading) + "@v" + strconv.Itoa(version)
}

// requireNoSectionCitation checks that the spec's newest version cites no version of the section
// of the decisions document whose heading is heading: an ask block the section held stands in the
// spec, so no part of the section stays in the decisions document's versions alone.
func (issue *restoreIssue) requireNoSectionCitation(heading string) {
	issue.t.Helper()
	if markdown := issue.latest(issue.spec); strings.Contains(markdown, refs.BlockRef(testDecisionsAddress, heading)+"@v") {
		issue.t.Errorf("the spec's newest version cites the section %s at a version of the decisions document, want no such citation:\n%s", heading, markdown)
	}
}

// value is the newest value of attribute the node of the block at address states.
func (issue *restoreIssue) value(address, attribute string) string {
	issue.t.Helper()
	var value string
	if err := issue.database.Pool.QueryRow(context.Background(), `
		select coalesce((
			select to_id from graph_tuples
			where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and asserted
			order by seq desc limit 1
		), '')
	`, refs.BlockNodeKind, address, attribute, graphstore.KindValue).Scan(&value); err != nil {
		issue.t.Fatalf("read %s of %s: %v", attribute, address, err)
	}
	return value
}

// requireUnplacedDecision checks the decision the conversion made of the section of decisions
// whose heading is heading, a section it could not place under one ask for cause (R9's further
// kinds): the heading's one moved_to reaches the decision, which keeps the heading's block id in
// the spec; the decision's node states uncertainty high and the reason why; its current shaped
// tuples reach the spec blocks shaped names, and it holds no from_ask; the spec's block index
// records a mention of each of asks; and the spec's newest version holds each of words.
func (issue *restoreIssue) requireUnplacedDecision(decisions, heading, cause string, shaped, asks, words []string) {
	t := issue.t
	t.Helper()
	ctx := context.Background()
	decision := issue.refKey(issue.spec) + "#" + heading
	if moved := issue.movedTo(decisions)[issue.refKey(decisions)+"#"+heading]; moved != decision {
		t.Errorf("the heading %s moves to %q, want the decision %s that keeps its block id", heading, moved, decision)
	}
	if uncertainty := issue.value(decision, refs.ValueUncertainty); uncertainty != "high" {
		t.Errorf("the decision %s states uncertainty %q, want high", decision, uncertainty)
	}
	if reason := issue.value(decision, refs.ValueUncertaintyReason); reason != unplacedReason(cause) {
		t.Errorf("the decision %s states uncertainty_reason %q, want %q", decision, reason, unplacedReason(cause))
	}
	tuples, err := graphstore.CurrentOut(ctx, issue.database.Pool, refs.BlockNodeKind, decision, []string{refs.TupleShaped, refs.TupleFromAsk})
	if err != nil {
		t.Fatalf("read the ties of the decision %s: %v", decision, err)
	}
	var gotShaped, fromAsk []string
	for _, tuple := range tuples {
		switch {
		case !tuple.Asserted:
		case tuple.Kind == refs.TupleShaped:
			gotShaped = append(gotShaped, tuple.ToID)
		case tuple.Kind == refs.TupleFromAsk:
			fromAsk = append(fromAsk, tuple.ToID)
		}
	}
	var wantShaped []string
	for _, block := range shaped {
		wantShaped = append(wantShaped, issue.refKey(issue.spec)+"#"+block)
	}
	slices.Sort(gotShaped)
	slices.Sort(wantShaped)
	if !slices.Equal(gotShaped, wantShaped) || len(fromAsk) != 0 {
		t.Errorf("the decision %s shaped %v and is from_ask %v, want it to have shaped %v and to be from no ask", decision, gotShaped, fromAsk, wantShaped)
	}
	rows, err := issue.database.Pool.Query(ctx, `
		select distinct to_id from block_refs where artifact_id = $1 and to_kind = $2 and kind = 'mentions'
	`, issue.spec, graphstore.KindAsk)
	if err != nil {
		t.Fatalf("read the asks the spec mentions: %v", err)
	}
	mentioned := map[string]bool{}
	for rows.Next() {
		var ask string
		if err := rows.Scan(&ask); err != nil {
			t.Fatal(err)
		}
		mentioned[ask] = true
	}
	rows.Close()
	for _, ask := range asks {
		if !mentioned[ask] {
			t.Errorf("the spec's block index records no mention of ask %s, which the decision %s cites: %v", ask, decision, mentioned)
		}
	}
	markdown := issue.latest(issue.spec)
	for _, want := range append([]string{"#" + heading + " "}, words...) {
		if !strings.Contains(markdown, want) {
			t.Errorf("the spec's newest version holds no %q:\n%s", want, markdown)
		}
	}
}

// requireRetiredAndOwedNothing checks that decisions is retired and that, after it, the census
// counts no decisions document and no unconverted section, and the step owes nothing, which
// /healthz answers as decisions_step_owed (DecisionStep.OwedTotal).
func (issue *restoreIssue) requireRetiredAndOwedNothing(decisions string) {
	t := issue.t
	t.Helper()
	ctx := context.Background()
	var retired bool
	if err := issue.database.Pool.QueryRow(ctx, `select retired_at is not null from artifacts where id = $1`, decisions).Scan(&retired); err != nil || !retired {
		t.Errorf("the decisions document reads retired %v (%v), want retired", retired, err)
	}
	if census, err := ReadDecisionsCensus(ctx, issue.database.Pool); err != nil || census.Documents != 0 || census.UnconvertedSections != 0 {
		t.Errorf("the census reads %+v (%v), want no decisions document and no unconverted section", census, err)
	}
	if owed, err := countDecisionStepOwed(ctx, issue.database.Pool); err != nil || owed.Total != 0 {
		t.Errorf("the step owes %+v (%v), want nothing", owed, err)
	}
}

// The three further kinds of section R9 names that the conversion cannot place under one ask (the
// spec's R9): a section that cites two answered asks, one that cites an answered
// ask whose block stands in no document of the issue (an issue-level ask), and one whose body a
// reading cannot hold - it cites one answered ask and holds a list beside its paragraph. Each
// becomes a decision in the spec's `## Decisions` part that keeps the section's heading block id,
// takes the section's links as shaped and no from_ask, keeps the section's citation of each ask
// in its words, which the block index records as mentions, and carries uncertainty high with the
// reason the conversion could not place it. No ask takes the section's links. The heading moves to
// the decision, the decisions document retires, the census counts no unconverted section and the
// step owes nothing.
func TestTheConversionMakesADecisionOfEachSectionItCannotPlaceUnderOneAsk(t *testing.T) {
	const pointer = "The decisions this spec rests on are in dispatch://DOC-1/artifact/decisions-md."
	const spec = "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n" +
		":::ask{#q-store urgency=\"med\" multiple=\"false\"}\nWhich store holds the corpus?\n:::\n\n" +
		":::ask{#q-format urgency=\"low\" multiple=\"false\"}\nWhich format is each line in?\n:::\n\n## Decisions\n\n" + pointer + "\n"
	const title = "The store and its format go together"
	for _, test := range []struct {
		name, cause string
		// section is the section's body, citing the asks asks gives it, which it answers.
		section func(asks []string) string
		asks    func(issue *restoreIssue) []string
		words   []string
	}{
		{
			name: "a section citing two answered asks", cause: unplacedAsks,
			section: func(asks []string) string {
				return "Both rest on one choice, dispatch://DOC-1/ask/" + asks[0] + " and dispatch://DOC-1/ask/" + asks[1] +
					", which shapes dispatch://DOC-1/spec#b-r1.\n"
			},
			asks: func(issue *restoreIssue) []string {
				return []string{
					issue.answer(issue.spec, "q-store", "The seeded database."),
					issue.answer(issue.spec, "q-format", "One line for each measurement."),
				}
			},
			words: []string{"Both rest on one choice"},
		},
		{
			name: "a section citing an issue-level answered ask", cause: unplacedNoBlock,
			section: func(asks []string) string {
				return "The person ruled it in dispatch://DOC-1/ask/" + asks[0] + ", which shapes dispatch://DOC-1/spec#b-r1.\n"
			},
			asks: func(issue *restoreIssue) []string {
				var ask string
				if err := issue.database.Pool.QueryRow(context.Background(), `
					insert into asks (issue_key, author, question, state, answer)
					values ('DOC-1', '{"kind":"user","id":"alice"}', 'Which shelf holds the printed corpus?', 'answered',
						jsonb_build_object('user', 'alice', 'selected', '[]'::jsonb, 'text', 'The top shelf.', 'at', now()))
					returning id::text
				`).Scan(&ask); err != nil {
					issue.t.Fatalf("open the issue-level ask: %v", err)
				}
				return []string{ask}
			},
			words: []string{"The person ruled it in"},
		},
		{
			name: "a section whose body a reading cannot hold", cause: unplacedBody,
			section: func(asks []string) string {
				return "The person ruled it in dispatch://DOC-1/ask/" + asks[0] + ", which shapes dispatch://DOC-1/spec#b-r1.\n\n" +
					"- The database holds every row.\n- No file holds one.\n"
			},
			asks: func(issue *restoreIssue) []string {
				return []string{issue.answer(issue.spec, "q-store", "The seeded database.")}
			},
			words: []string{"The person ruled it in", "- The database holds every row.", "- No file holds one."},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			issue := newRestoreIssue(t, spec)
			asked := test.asks(issue)
			decisions := issue.document("decisions-md", "decisions.md", false, "decisions",
				"# Decisions\n\n## "+title+"\n\n"+test.section(asked))
			heading := issue.headingID(decisions, title)

			result := issue.run(true)
			if result.Decisions != 1 || result.Readings != 0 || result.Dropped != 0 || result.Left != 0 || !result.Retired {
				t.Fatalf("the conversion did %+v, want the section a decision, nothing else, the document retired", result)
			}
			issue.requireUnplacedDecision(decisions, heading, test.cause, []string{"r1"}, asked, test.words)
			for _, ask := range asked {
				tuples, err := graphstore.CurrentOut(ctx, issue.database.Pool, graphstore.KindAsk, ask, []string{refs.TupleShaped})
				if err != nil || slices.ContainsFunc(tuples, func(tuple graphstore.Tuple) bool { return tuple.Asserted }) {
					t.Errorf("ask %s shaped %+v (%v), want nothing: the decision takes the section's links", ask, tuples, err)
				}
			}
			issue.requireRetiredAndOwedNothing(decisions)
		})
	}
}

// A decision the conversion writes waits for a person's review until a Keep or a replacement,
// whoever wrote its section's newest version (the spec's R2 and R9): a person's edit of sections
// an agent wrote makes none of their choices hers, so the decision of such a section - one that
// cites no ask, and one citing two answered asks, which the conversion could not place under one
// ask - names no decided_by and reads waiting_review through the Decisions read
// (decisionread.Decisions), as the decision of the section the agent's session wrote last does,
// which names that session. A decision block alice wrote in the spec herself is hers, and waits
// for nobody.
func TestAConvertedDecisionWaitsForReviewWhereAPersonWroteItsSectionLast(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const pointer = "The decisions this spec rests on are in dispatch://DOC-1/artifact/decisions-md."
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n"+
		":::decision{#p1}\nThe corpus stays on one machine.\n:::\n\n"+
		":::ask{#q-store urgency=\"med\" multiple=\"false\"}\nWhich store holds the corpus?\n:::\n\n"+
		":::ask{#q-format urgency=\"low\" multiple=\"false\"}\nWhich format is each line in?\n:::\n\n## Decisions\n\n"+pointer+"\n")
	asked := []string{
		issue.answer(issue.spec, "q-store", "The seeded database."),
		issue.answer(issue.spec, "q-format", "One line for each measurement."),
	}
	const (
		placed   = "Files stay out of the corpus"
		unplaced = "The store and its format go together"
		agents   = "Readings stay small"
	)
	sections := func(files, both string) string {
		return "# Decisions\n\n## " + placed + "\n\n" + files + " beside the database, which dispatch://DOC-1/spec#b-r1 depends on.\n\n" +
			"## " + unplaced + "\n\n" + both + ", dispatch://DOC-1/ask/" + asked[0] + " and dispatch://DOC-1/ask/" + asked[1] +
			", which shapes dispatch://DOC-1/spec#b-r1.\n\n" +
			"## " + agents + "\n\nEach reading holds one line.\n"
	}
	// The agent's session writes the three sections, and alice's edit then rewords two of them,
	// so her version is the newest of those two and the session's of the third.
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions", "# Decisions\n")
	issue.replace(decisions, sections("We write no file", "Both rest on one choice"))
	issue.replaceAsPerson(decisions, sections("We write no file at all", "Both rest on one shared choice"))
	headings := map[string]string{}
	for _, title := range []string{placed, unplaced, agents} {
		headings[title] = issue.headingID(decisions, title)
	}

	result := issue.run(true)
	if result.Decisions != 3 || result.Readings != 0 || result.Dropped != 0 || result.Left != 0 || !result.Retired {
		t.Fatalf("the conversion did %+v, want the three sections decisions, nothing else, the document retired", result)
	}
	spec := issue.refKey(issue.spec)
	wants := []struct {
		what, block string
		// decider is the decided_by the node states, as kind/id, "" for none.
		decider         string
		unsure, waiting bool
	}{
		{"the decision of the section that cites no ask, which alice's edit wrote last", headings[placed], "", false, true},
		{"the decision of the section the conversion could not place, which alice's edit wrote last", headings[unplaced], "", true, true},
		{"the decision of the section the agent's session wrote last", headings[agents],
			graphstore.KindSession + "/" + agentOfTheReleaseBefore.ID, false, true},
		{"alice's own decision in the spec", "p1", graphstore.KindPerson + "/alice", false, false},
	}
	nodes := make([]decisionread.Node, len(wants))
	for index, want := range wants {
		nodes[index] = decisionread.Node{Kind: refs.BlockNodeKind, ID: spec + "#" + want.block}
	}
	read, err := decisionread.Decisions(ctx, issue.database.Pool, model.Actor{Kind: "user", ID: "alice"}, nodes)
	if err != nil {
		t.Fatalf("read the spec's decisions: %v", err)
	}
	byNode := map[string]model.Decision{}
	for _, decision := range read {
		byNode[decision.NodeID] = decision
	}
	for _, want := range wants {
		address := spec + "#" + want.block
		tuples, err := graphstore.CurrentOut(ctx, issue.database.Pool, refs.BlockNodeKind, address, []string{graphstore.TupleDecidedBy})
		if err != nil {
			t.Fatalf("read the decided_by of %s: %v", address, err)
		}
		var deciders []string
		for _, tuple := range tuples {
			if tuple.Asserted {
				deciders = append(deciders, tuple.ToKind+"/"+tuple.ToID)
			}
		}
		if want.decider == "" && len(deciders) != 0 || want.decider != "" && !slices.Equal(deciders, []string{want.decider}) {
			t.Errorf("%s (%s): the node states the decided_by %v, want %q", want.what, address, deciders, want.decider)
		}
		decision, found := byNode[address]
		switch {
		case !found:
			t.Errorf("%s (%s): the Decisions read answers no decision for it", want.what, address)
		case decision.Kind != "agent" || decision.Unsure != want.unsure || decision.Kept != nil || decision.Replaced:
			t.Errorf("%s (%s): the read answers kind %q, unsure %t, kept %+v, replaced %t, want kind agent, unsure %t, no Keep and not replaced",
				want.what, address, decision.Kind, decision.Unsure, decision.Kept, decision.Replaced, want.unsure)
		case decision.WaitingReview != want.waiting:
			t.Errorf("%s (%s): the read answers waiting_review %t, want %t", want.what, address, decision.WaitingReview, want.waiting)
		}
	}
	issue.requireRetiredAndOwedNothing(decisions)
}

// R9's first further kind (the spec's R9, and R6's second shape): a section whose body
// still holds an ask block no row names - a copy of an ask another document of the issue holds,
// which no work item of the finder moves - has that block moved into the spec first, in the same
// transaction, as a block with no row to rebind: the spec's newest version holds it under its id,
// one moved_to runs from its address in the decisions document to its address in the spec, and no
// row names it. The rest of the section then converts: it is about no answered ask and its words
// are no reading, so it becomes a decision waiting for a person's review. The moved block stands
// in the spec, so the decision cites no version of the section. The document retires and the step
// owes nothing.
func TestTheConversionMovesTheAskBlockASectionStillHoldsAndConvertsTheRest(t *testing.T) {
	ctx := context.Background()
	const shelf = ":::ask{#q-shelf urgency=\"low\" multiple=\"false\"}\nWhich shelf holds the printed corpus?\n:::\n"
	const title = "Which shelf holds the printed corpus"
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n")
	issue.document("notes-md", "notes.md", false, "", "# Notes\n\n"+shelf)
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions",
		"# Decisions\n\n## "+title+"\n\n"+shelf+"\nWe lean towards the top shelf, which dispatch://DOC-1/spec#b-r1 depends on.\n")
	var rows int
	if err := issue.database.Pool.QueryRow(ctx, `select count(*) from asks where block_artifact_id = $1`, decisions).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("%d rows (%v) name a block of the decisions document, want none: its ask block is a copy", rows, err)
	}
	if work, err := graphcensus.ReadRestoreWork(ctx, issue.database.Pool); err != nil || len(work) != 0 {
		t.Fatalf("the finder reads work %+v (%v), want none: no row names the copy", work, err)
	}
	heading := issue.headingID(decisions, title)

	result := issue.run(true)
	from, to := issue.refKey(decisions)+"#q-shelf", issue.refKey(issue.spec)+"#q-shelf"
	if len(result.Restored) != 1 || result.Restored[0].Ask != "" || result.Restored[0].From != from || result.Restored[0].To != to ||
		result.Decisions != 1 || result.Left != 0 || !result.Retired {
		t.Fatalf("the conversion did %+v, want the block no row names moved from %s to %s, the rest a decision, the document retired", result, from, to)
	}
	if moves := issue.movedTo(decisions); moves[from] != to {
		t.Errorf("the moves from the decisions document are %v, want %s to %s", moves, from, to)
	}
	if err := issue.database.Pool.QueryRow(ctx, `select count(*) from asks where block_id = 'q-shelf' and block_artifact_id = $1`, issue.spec).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("%d rows (%v) name the moved block in the spec, want none: the move rebinds no row", rows, err)
	}
	if markdown := issue.latest(issue.spec); !strings.Contains(markdown, ":::ask{#q-shelf ") || !strings.Contains(markdown, "Which shelf holds the printed corpus?") {
		t.Errorf("the spec's newest version holds no moved block q-shelf:\n%s", markdown)
	}
	issue.requireUnplacedDecision(decisions, heading, unplacedBody, []string{"r1"}, nil,
		[]string{"We lean towards the top shelf"})
	issue.requireNoSectionCitation(heading)
	issue.requireRetiredAndOwedNothing(decisions)
}

// A copy no row names of an ask whose block the spec holds (the spec's R6 and R9) joins that
// block: the conversion writes no second block of it into the spec, so the spec's next settlement
// opens no new ask beside the one it copies, and the copy's old address takes its one moved_to to
// the spec's ask block, so a citation of it answers the live ask. The rest of the section
// converts as any other does.
func TestTheConversionJoinsASectionsCopyOfAnAskTheSpecHoldsAndOpensNoSecondAsk(t *testing.T) {
	ctx := context.Background()
	const shelf = ":::ask{#q-shelf urgency=\"low\" multiple=\"false\"}\nWhich shelf holds the printed corpus?\n:::\n"
	const title = "Which shelf holds the printed corpus"
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n"+shelf)
	source := blockAskOn(t, issue.service, issue.spec, "q-shelf").ID
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions",
		"# Decisions\n\n## "+title+"\n\n"+shelf+"\nWe lean towards the top shelf until someone rules.\n")
	var rows int
	if err := issue.database.Pool.QueryRow(ctx, `select count(*) from asks where block_artifact_id = $1`, decisions).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("%d rows (%v) name a block of the decisions document, want none: its ask block is a copy", rows, err)
	}
	heading := issue.headingID(decisions, title)
	asksOfSpec := func() map[string]string {
		t.Helper()
		rows, err := issue.database.Pool.Query(ctx, `select id::text, coalesce(block_id, '') from asks where block_artifact_id = $1`, issue.spec)
		if err != nil {
			t.Fatalf("read the asks of the spec: %v", err)
		}
		defer rows.Close()
		held := map[string]string{}
		for rows.Next() {
			var id, block string
			if err := rows.Scan(&id, &block); err != nil {
				t.Fatal(err)
			}
			held[id] = block
		}
		return held
	}
	before := asksOfSpec()

	result := issue.run(true)
	if len(result.Restored) != 0 || result.Decisions != 1 || result.Left != 0 || !result.Retired {
		t.Errorf("the conversion did %+v, want no block written for the copy, the rest of its section a decision, the document retired", result)
	}
	if markdown := issue.latest(issue.spec); strings.Count(markdown, "Which shelf holds the printed corpus?") != 1 {
		t.Errorf("the spec's newest version asks the copied question %d times, want once, in its own block:\n%s",
			strings.Count(markdown, "Which shelf holds the printed corpus?"), markdown)
	}
	settleCurrentGeneration(t, issue.service, issue.spec)
	var opened []string
	for id, block := range asksOfSpec() {
		if _, held := before[id]; !held {
			opened = append(opened, "ask "+id+" at block "+block)
		}
	}
	if len(opened) != 0 {
		t.Errorf("the spec's settlement after the conversion opened %v beside ask %s, want no second ask", opened, source)
	}
	from, to := issue.refKey(decisions)+"#q-shelf", issue.refKey(issue.spec)+"#q-shelf"
	if moves := issue.movedTo(decisions); moves[from] != to {
		t.Errorf("the moves from the decisions document are %v, want %s to the spec's ask block %s", moves, from, to)
	}
	var block, typ string
	if err := issue.database.Pool.QueryRow(ctx, `
		select b.block_id, b.type from document_blocks b where b.artifact_id = $1 and b.node_id = block_moved_to($2)
	`, issue.spec, from).Scan(&block, &typ); err != nil || block != "q-shelf" || typ != "ask" {
		t.Errorf("the copy's old address %s resolves to block %q of type %q (%v), want the spec's ask block q-shelf", from, block, typ, err)
	}
	if blockID, document := issue.askRow(source); blockID != "q-shelf" || document != issue.spec {
		t.Errorf("ask %s names block %s of %s, want q-shelf of the spec %s", source, blockID, document, issue.spec)
	}
	issue.requireUnplacedDecision(decisions, heading, unplacedBody, nil, nil, []string{"We lean towards the top shelf until someone rules."})
	issue.requireRetiredAndOwedNothing(decisions)
}

// A section whose body holds an ask block the decisions document's block index does not hold yet
// - a live edit no version holds - is the one section a pass leaves (the spec's R9 and
// R6): no move takes that block, and a conversion of the section would take it out of the
// document and write it nowhere. The pass converts the document's other section, leaves that one
// and the document standing, and the step owes the document. The pass's own version of the
// document indexes the block; once its settlement has run, the next pass moves the block into the
// spec, converts the section, retires the document, and the step owes nothing.
func TestTheConversionLeavesASectionHoldingAnUnindexedAskBlockForTheNextPass(t *testing.T) {
	ctx := context.Background()
	const plain = "## Files stay out of the corpus\n\nWe write no file beside the database.\n"
	const title = "Which region goes first"
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n")
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions", "# Decisions\n\n"+plain)
	alice := model.Actor{Kind: "user", ID: "alice"}
	base := liveDocumentToken(t, issue.service, decisions)
	if err := joinedWrite(issue.service, func(ctx context.Context) error {
		_, err := issue.service.ReplaceText(ctx, decisions, "# Decisions\n\n"+plain+"\n## "+title+"\n\n"+
			":::ask{#q-late urgency=\"low\" multiple=\"false\"}\nWhich region goes first?\n:::\n\nWe lean east until someone rules.\n", alice, base)
		return err
	}); err != nil {
		t.Fatalf("write the live edit: %v", err)
	}
	var indexed int
	if err := issue.database.Pool.QueryRow(ctx, `select count(*) from document_blocks where artifact_id = $1 and block_id = 'q-late'`, decisions).Scan(&indexed); err != nil || indexed != 0 {
		t.Fatalf("the block index holds q-late %d times (%v) before the pass, want none", indexed, err)
	}

	first := issue.run(true)
	if first.Decisions != 1 || first.Left != 1 || first.Retired || len(first.Restored) != 0 {
		t.Fatalf("the first pass did %+v, want the plain section a decision, the section holding q-late left, the document standing", first)
	}
	if census, err := ReadDecisionsCensus(ctx, issue.database.Pool); err != nil || census.Documents != 1 || census.UnconvertedSections != 1 {
		t.Errorf("after the first pass the census reads %+v (%v), want one document holding one section", census, err)
	}
	if owed, err := countDecisionStepOwed(ctx, issue.database.Pool); err != nil || owed.DecisionsDocuments != 1 || owed.Total != 1 {
		t.Errorf("after the first pass the step owes %+v (%v), want the one decisions document", owed, err)
	}
	settleCurrentGeneration(t, issue.service, decisions)
	if err := issue.database.Pool.QueryRow(ctx, `select count(*) from document_blocks where artifact_id = $1 and block_id = 'q-late' and type = 'ask'`, decisions).Scan(&indexed); err != nil || indexed != 1 {
		t.Fatalf("the block index holds the ask block q-late %d times (%v) after the first pass, want once", indexed, err)
	}
	heading := issue.headingID(decisions, title)

	second := issue.run(true)
	from, to := issue.refKey(decisions)+"#q-late", issue.refKey(issue.spec)+"#q-late"
	if len(second.Restored) != 1 || second.Restored[0].From != from || second.Restored[0].To != to ||
		second.Decisions != 1 || second.Left != 0 || !second.Retired {
		t.Fatalf("the second pass did %+v, want q-late moved from %s to %s, its section a decision, the document retired", second, from, to)
	}
	issue.requireUnplacedDecision(decisions, heading, unplacedBody, nil, nil,
		[]string{"We lean east until someone rules."})
	issue.requireNoSectionCitation(heading)
	issue.requireRetiredAndOwedNothing(decisions)
}

// A part of a section's body that a decision block cannot hold (the spec's R9) - here a
// reading holding no paragraph, which no decision holds - is left out of the decision the section
// becomes, and the decision ends with a citation of the section in the decisions document's last
// version that held it, the version its block index stands at, where that part's text stays once
// the document retires; the rest of the section's words stand in the decision. Where the index
// holds no version of the section yet, the section is left for the next pass, since no version
// would hold the part the decision leaves out.
func TestTheConversionLeavesOutOfADecisionAPartItCannotHoldAndCitesTheSection(t *testing.T) {
	section := decisionSection{
		heading: &pmdoc.Node{Type: "heading", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "h-census", "level": 2},
			Children: []*pmdoc.Node{{Type: "text", Text: "The census counts in one pass"}}},
		level: 2, title: "The census counts in one pass",
		body: []*pmdoc.Node{
			{Type: "paragraph", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "p-once"}, Children: []*pmdoc.Node{{Type: "text", Text: "The census reads every row once."}}},
			{Type: readingType, Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "r-empty"}},
		},
	}
	restore := func(indexed map[string]refs.BlockChange) (*convertedSection, error) {
		r := &issueRestore{
			issue: "DOC-1", convert: true, spec: issueDocument{address: "dispatch://DOC-1/spec"},
			decisions: &issueDocument{address: testDecisionsAddress}, specBlocks: map[string]bool{},
			indexedVersion: 3, indexedBlocks: indexed,
		}
		converted := &convertedSection{section: section}
		err := r.planDecision(context.Background(), converted, section.body, &unplacedSection{cause: unplacedBody})
		return converted, err
	}

	converted, err := restore(map[string]refs.BlockChange{"h-census": {Version: 2}})
	if err != nil || converted.fate != fateDecision || converted.decisionID != "h-census" {
		t.Fatalf("the section became fate %d under %q (%v), want a decision keeping its heading id", converted.fate, converted.decisionID, err)
	}
	markdown, err := blockMarkdown(converted.decision)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(markdown, "The census reads every row once.") || !strings.Contains(markdown, sectionCitation("h-census", 3)) || strings.Contains(markdown, "r-empty") {
		t.Errorf("the decision reads\n%s\nwant the section's words, no block r-empty, and a citation of the section at version 3", markdown)
	}

	converted, err = restore(map[string]refs.BlockChange{})
	if err != nil || converted.fate != fateLeft {
		t.Errorf("with no version of the section indexed the section became fate %d (%v), want it left for the next pass", converted.fate, err)
	}
}

// askShaped is the far end of each shaped tie ask's node states now.
func (issue *restoreIssue) askShaped(ask string) []string {
	issue.t.Helper()
	tuples, err := graphstore.CurrentOut(context.Background(), issue.database.Pool, graphstore.KindAsk, ask, []string{refs.TupleShaped})
	if err != nil {
		issue.t.Fatalf("read the shaped ties of ask %s: %v", ask, err)
	}
	var shaped []string
	for _, tuple := range tuples {
		if tuple.Asserted {
			shaped = append(shaped, tuple.ToID)
		}
	}
	return shaped
}

// requireReadingWithLinks checks the conversion's work on the section of decisions whose heading is
// heading, a section about ask that links the spec's r1, restated in ask's block block of document:
// document's newest version holds that block with a reading of the section's words and a shaped
// pointer to r1, ask's node states one shaped tie, to r1, and the heading's one moved_to reaches
// the block.
func (issue *restoreIssue) requireReadingWithLinks(decisions, heading, ask, document, block, words string) {
	t := issue.t
	t.Helper()
	markdown := issue.latest(document)
	start := strings.Index(markdown, "#"+block+" ")
	if start < 0 {
		t.Fatalf("the newest version of %s holds no ask block %s:\n%s", document, block, markdown)
	}
	end := strings.Index(markdown[start:], "\n::::\n")
	if end < 0 || !strings.Contains(markdown[start:start+end], ":::reading{#") || !strings.Contains(markdown[start:start+end], words) {
		t.Errorf("the ask block %s of the newest version of %s holds no reading of the section's words %q:\n%s", block, document, words, markdown)
	}
	if opener := openerOf(markdown, block); !strings.Contains(opener, `shaped="dispatch://DOC-1/spec#b-r1"`) {
		t.Errorf("the ask block %s opens %q, want it shaped by the section's link to r1", block, opener)
	}
	if shaped := issue.askShaped(ask); !slices.Equal(shaped, []string{issue.refKey(issue.spec) + "#r1"}) {
		t.Errorf("ask %s's node states shaped %v, want the section's link to r1 alone", ask, shaped)
	}
	if moved := issue.movedTo(decisions)[issue.refKey(decisions)+"#"+heading]; moved != issue.refKey(document)+"#"+block {
		t.Errorf("the heading %s moves to %q, want the ask block %s its reading stands in", heading, moved, block)
	}
}

// A restore alone (restore-answered-asks) gives a section's links to no block it does not write
// (R9): a section that holds an ask block the restore moves, and cites an answered ask whose block
// the spec holds, stays in the decisions document, with its words and its link, once the block it
// held has moved, and the conversion after it writes its words as a reading into that ask's block
// and gives the block the section's link. Were the restore to take the section out as a reading,
// the conversion would no longer see it, and its link would be lost.
//
// DOC-1's spec holds requirement r1 and the answered ask q-store. Its decisions document holds a
// section with an open ask block of its own, q-shelf, and a paragraph citing q-store's ask that
// links r1. The restore alone moves q-shelf into the spec and leaves the section, counted, with
// its paragraph in the decisions document's newest version, no moved_to from its heading, and no
// shaped tie on q-store's ask. The conversion then writes the paragraph as a reading into
// q-store's block, which it shapes by r1, moves the heading to that block, and retires the
// document.
func TestARestoreAloneLeavesASectionAboutAnAskTheSpecHoldsWithItsLinksForTheConversion(t *testing.T) {
	const title = "The corpus lives in the database"
	const words = "The person ruled it in"
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n"+
		":::ask{#q-store urgency=\"med\" multiple=\"false\"}\nWhich store holds the corpus?\n:::\n")
	stored := issue.answer(issue.spec, "q-store", "The seeded database.")
	paragraph := words + " dispatch://DOC-1/ask/" + stored + ", which shapes dispatch://DOC-1/spec#b-r1."
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions", "# Decisions\n\n## "+title+"\n\n"+
		":::ask{#q-shelf urgency=\"low\" multiple=\"false\"}\nWhich shelf holds the printed corpus?\n:::\n\n"+paragraph+"\n")
	heading := issue.headingID(decisions, title)

	restored := issue.run(false)
	from := issue.refKey(decisions) + "#q-shelf"
	if len(restored.Restored) != 1 || restored.Restored[0].From != from || restored.Readings != 0 || restored.Dropped != 0 ||
		restored.Left != 1 || restored.Retired {
		t.Fatalf("the restore alone did %+v, want q-shelf moved from %s, the section left with no reading and the document standing", restored, from)
	}
	if markdown := issue.latest(decisions); !strings.Contains(markdown, "## "+title) || !strings.Contains(markdown, paragraph) || strings.Contains(markdown, "#q-shelf ") {
		t.Errorf("the decisions document's newest version reads:\n%s\nwant the section with its paragraph and link, and no block q-shelf", markdown)
	}
	if moved, stated := issue.movedTo(decisions)[issue.refKey(decisions)+"#"+heading]; stated {
		t.Errorf("the section's heading moves to %s after the restore alone, want it standing in the decisions document", moved)
	}
	if shaped := issue.askShaped(stored); len(shaped) != 0 {
		t.Errorf("ask %s's node states shaped %v after the restore alone, want none yet", stored, shaped)
	}

	converted := issue.run(true)
	if converted.Readings != 1 || converted.Decisions != 0 || converted.Left != 0 || !converted.Retired {
		t.Fatalf("the conversion did %+v, want the section a reading, none left, the document retired", converted)
	}
	issue.requireReadingWithLinks(decisions, heading, stored, issue.spec, "q-store", words)
	issue.requireRetiredAndOwedNothing(decisions)
}

// A pass leaves an issue a document of which owes its block index for a later pass, writing
// nothing of it (restoreIssueDecisions): that document's document_blocks rows are not current, and
// the pass reads from them where each block of the issue stands - the restore's finder leaves a
// spec that owes its index out, and the conversion looks for an answered ask's block in every
// document of the issue (askBlockOf). A pass that read those rows would read an answered ask whose
// block stands in such a document, or left the spec, as standing in no document: it would make the
// section about that ask a decision waiting for a person's review and retire the decisions
// document, and the section's reading and link would reach that ask's block in no later pass.
//
// DOC-1's decisions document holds a section citing an answered ask that links the spec's r1, and
// a document of DOC-1 then owes its block index, as a version past the deferral bound leaves it:
//   - the spec, whose answered ask q-store lost its block to the release before's agent;
//   - its notes, a third document, which hold the answered ask q-notes, and whose block index holds
//     no row of that block yet, as a version whose index its write deferred leaves it.
//
// A pass touches no issue and fails none, the decisions document stands at its one version, and
// neither the spec nor the document that owes its index gains one. Once the block-index
// reconciliation has indexed that document, the next pass writes the section as a reading into the
// ask's block - q-store restored into the spec, or q-notes where the notes hold it - shapes it by
// r1, and retires the decisions document.
func TestAPassLeavesAnIssueADocumentOfWhichOwesItsBlockIndexForALaterPass(t *testing.T) {
	const title = "The corpus lives in the database"
	const words = "The person ruled it in"
	const head = "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n"
	const question = "Which store holds the corpus?"
	for _, test := range []struct {
		name, spec string
		// write writes the answered ask and the document that is to owe its block index, and answers
		// the ask, the document whose block holds it once the later pass has run, and the document
		// that owes its index.
		write func(t *testing.T, issue *restoreIssue) (ask, holder, owing string)
		// restores is whether the later pass restores the ask into the spec.
		restores bool
	}{
		{
			name: "the spec, which the answered ask's block left",
			spec: head + "\n:::ask{#q-store urgency=\"med\" multiple=\"false\"}\n" + question + "\n:::\n",
			write: func(t *testing.T, issue *restoreIssue) (string, string, string) {
				stored := issue.answer(issue.spec, "q-store", "The seeded database.")
				issue.replace(issue.spec, head)
				return stored, issue.spec, issue.spec
			},
			restores: true,
		},
		{
			name: "a third document, which holds the answered ask's block",
			spec: head,
			write: func(t *testing.T, issue *restoreIssue) (string, string, string) {
				notes := issue.document("notes-md", "notes.md", false, "",
					"# Notes\n\n:::ask{#q-notes urgency=\"med\" multiple=\"false\"}\n"+question+"\n:::\n")
				noted := issue.answer(notes, "q-notes", "The seeded database.")
				settleCurrentGeneration(t, issue.service, notes)
				if _, err := issue.database.Pool.Exec(context.Background(), `
					delete from document_blocks where artifact_id = $1 and block_id = 'q-notes'
				`, notes); err != nil {
					t.Fatalf("leave the notes' block index without the ask's block: %v", err)
				}
				return noted, notes, notes
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			issue := newRestoreIssue(t, test.spec)
			ask, holder, owing := test.write(t, issue)
			decisions := issue.document("decisions-md", "decisions.md", false, "decisions", "# Decisions\n\n## "+title+"\n\n"+
				words+" dispatch://DOC-1/ask/"+ask+", which shapes dispatch://DOC-1/spec#b-r1.\n")
			heading := issue.headingID(decisions, title)
			if _, err := issue.database.Pool.Exec(ctx, `update document_block_versions set version = null where artifact_id = $1`, owing); err != nil {
				t.Fatalf("leave %s owing its block index: %v", owing, err)
			}
			specBefore, owingBefore := specVersions(t, issue.database, issue.spec), specVersions(t, issue.database, owing)
			step := NewDecisionStep(issue.database, "", nil)
			step.UseDocuments(issue.service)

			done, failed, err := step.RestoreAndConvert(ctx, "DOC", true, false)
			if err != nil || len(done) != 0 || len(failed) != 0 {
				t.Fatalf("the pass over an issue a document of which owes its block index answered %v, touched %+v and failed %+v, want the issue left alone",
					err, done, failed)
			}
			if readRetired(t, issue.database, decisions) || specVersions(t, issue.database, decisions) != 1 ||
				specVersions(t, issue.database, issue.spec) != specBefore || specVersions(t, issue.database, owing) != owingBefore {
				t.Fatalf("the pass that left DOC-1 wrote it: the decisions document retired %t at %d versions, the spec at %d versions and %s at %d, want the document standing at one, the spec at %d and %s at %d",
					readRetired(t, issue.database, decisions), specVersions(t, issue.database, decisions),
					specVersions(t, issue.database, issue.spec), owing, specVersions(t, issue.database, owing), specBefore, owing, owingBefore)
			}

			drainBlockIndex(t, issue.service, owing+" owed its block index")
			if indexed := issue.indexedVersion(owing); indexed != owingBefore {
				t.Fatalf("the block index of %s stands at version %d after the reconciliation, want its newest, %d", owing, indexed, owingBefore)
			}
			result := issue.run(true)
			restored := 0
			if test.restores {
				restored = 1
			}
			if len(result.Restored) != restored || result.Readings != 1 || result.Decisions != 0 || !result.Retired {
				t.Fatalf("the later pass did %+v, want %d ask restored, the section the ask's reading, no decision, the document retired", result, restored)
			}
			block, _ := issue.askRow(ask)
			issue.requireReadingWithLinks(decisions, heading, ask, holder, block, words)
			issue.requireRetiredAndOwedNothing(decisions)
		})
	}
}
