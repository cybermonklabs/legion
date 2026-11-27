package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The section reader cuts a decisions document as the Decisions read of the release before cut
// it: each `##` heading outside `## Replaced` and each `###` heading under it opens a section,
// which holds every top-level block up to the next heading of its level or above; a `###` under
// an ordinary section is a block of that section; the document's title and `## Replaced` open
// none. A section's `Unsure:` paragraph is read with the rest of its words.
func TestTheSectionReaderCutsADecisionsDocumentIntoItsSections(t *testing.T) {
	tree, err := pmdoc.Parse("# Decisions\n\nAn introduction no section holds.\n\n" +
		"## Readings come from the seeded database\n\nThe person ruled it.\n\n### A detail\n\nIt stays small.\n\n" +
		"## Files stay out of the corpus\n\nWe write no file.\n\nUnsure: a reader who wants a file export would change this.\n\n" +
		"## Replaced\n\n### Files held the corpus once\n\nWe first kept the corpus in files.\n\n" +
		"### A second replaced one\n\nIt went too.\n")
	if err != nil {
		t.Fatal(err)
	}
	sections := decisionSectionsOf(tree)
	type read struct {
		title    string
		replaced bool
		body     int
		unsure   string
	}
	var got []read
	for _, section := range sections {
		_, reason := section.unsure()
		got = append(got, read{section.title, section.replaced, len(section.body), reason})
	}
	want := []read{
		{"Readings come from the seeded database", false, 3, ""},
		{"Files stay out of the corpus", false, 2, "a reader who wants a file export would change this."},
		{"Files held the corpus once", true, 1, ""},
		{"A second replaced one", true, 1, ""},
	}
	if len(got) != len(want) {
		t.Fatalf("the reader cut %d sections %+v, want %d: %+v", len(got), got, len(want), want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("section %d reads %+v, want %+v", index, got[index], want[index])
		}
	}
}

// restoreIssue is an issue of a test database, DOC-1 unless another names it, its spec and the
// documents the decisions step writes, each written through the document service and settled as
// the release before left it.
type restoreIssue struct {
	t        *testing.T
	database *store.Store
	service  *Service
	key      string
	spec     string
}

// newRestoreIssue creates project DOC, issue DOC-1 and its spec holding markdown, settled, so each
// ask block of it holds its ask.
func newRestoreIssue(t *testing.T, markdown string) *restoreIssue {
	t.Helper()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee) values ('DOC', 'Documents', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('DOC-1', 'DOC', 1, 'Document', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("create the issue: %v", err)
	}
	service := newTestServiceOn(t, database)
	service.settle = time.Hour
	issue := &restoreIssue{t: t, database: database, service: service, key: "DOC-1"}
	issue.spec = issue.document("spec", "spec.md", true, "", markdown)
	return issue
}

// another creates issue number of project DOC, DOC-<number>, after issue, on its database and
// document service, and its spec holding markdown, settled as newRestoreIssue settles DOC-1's.
func (issue *restoreIssue) another(number int, markdown string) *restoreIssue {
	t := issue.t
	t.Helper()
	key := "DOC-" + strconv.Itoa(number)
	if _, err := issue.database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, 'DOC', $2, 'Document', '{"kind":"user","id":"alice"}', $3)
	`, key, number, "V"+strconv.Itoa(number)); err != nil {
		t.Fatalf("create the issue %s: %v", key, err)
	}
	other := &restoreIssue{t: t, database: issue.database, service: issue.service, key: key}
	other.spec = other.document("spec", "spec.md", true, "", markdown)
	return other
}

// document creates a document of the issue holding markdown as its version 1, written by alice
// and indexed as an upload of a new document is, and settles it, so each ask block holds its ask.
func (issue *restoreIssue) document(slug, name string, primary bool, role, markdown string) string {
	t := issue.t
	t.Helper()
	var id string
	var rolePointer *string
	if role != "" {
		rolePointer = &role
	}
	if err := issue.database.Pool.QueryRow(context.Background(), `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
		values ($5, 'DOC', $1, $2, 'doc', $3, $4, '{"kind":"user","id":"alice"}')
		returning id::text
	`, slug, name, primary, rolePointer, issue.key).Scan(&id); err != nil {
		t.Fatalf("create document %s: %v", slug, err)
	}
	if err := joinedWrite(issue.service, func(ctx context.Context) error {
		written, err := issue.service.SeedText(ctx, id, markdown, model.Actor{Kind: "user", ID: "alice"})
		if err != nil {
			return err
		}
		tx, _ := txFromContext(ctx)
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
		`, id, written); err != nil {
			return err
		}
		_, write, err := issue.service.IndexDocumentReferences(ctx, id, 1, written)
		if err != nil {
			return err
		}
		return causeInWritePhase(ctx, tx, write, graphstore.NoEvent)
	}); err != nil {
		t.Fatalf("write document %s: %v", slug, err)
	}
	settleCurrentGeneration(t, issue.service, id)
	return id
}

// agentOfTheReleaseBefore is the session the fixtures' writes of the release before are made as:
// that release refused no write that dropped an answered ask's block, and recorded a bearer's
// write as the session it named, with no act of a person.
var agentOfTheReleaseBefore = model.Actor{Kind: "session", ID: "session-of-the-release-before"}

// replace writes markdown over document and its version as the release before's agent wrote one
// (agentOfTheReleaseBefore), and settles it: the drop of an answered ask's block the restore
// exists to undo. The write is made through this build's upload with the person scope's mark, the
// one way it still takes such a drop, and the agent names no person, so it records no removal
// (dropperOf) and its version names no person (versionPerson).
func (issue *restoreIssue) replace(document, markdown string) {
	issue.t.Helper()
	issue.write(document, markdown, agentOfTheReleaseBefore, WithPersonScope)
}

// replaceAsPerson writes markdown over document as alice, a person, and its version, and settles
// it.
func (issue *restoreIssue) replaceAsPerson(document, markdown string) {
	issue.t.Helper()
	issue.write(document, markdown, model.Actor{Kind: "user", ID: "alice"}, func(ctx context.Context) context.Context { return ctx })
}

// write writes markdown over document as actor, in a context mark gives, with its version, whose
// facts it appends with no event, as a version of the fixtures is recorded, and settles it. The
// upload names the token the document stands at, read before the write as a caller reads it.
func (issue *restoreIssue) write(document, markdown string, actor model.Actor, mark func(context.Context) context.Context) {
	t := issue.t
	t.Helper()
	base := liveDocumentToken(t, issue.service, document)
	if err := joinedWrite(issue.service, func(ctx context.Context) error {
		ctx = mark(ctx)
		if _, err := issue.service.ReplaceText(ctx, document, markdown, actor, base); err != nil {
			return err
		}
		written, err := issue.service.SnapshotVersion(ctx, document, actor)
		if err != nil {
			return err
		}
		tx, _ := txFromContext(ctx)
		return causeInWritePhase(ctx, tx, written.Blocks, graphstore.NoEvent)
	}); err != nil {
		t.Fatalf("replace %s: %v", document, err)
	}
	settleCurrentGeneration(t, issue.service, document)
}

// answer has alice answer the ask of block blockID of document in text, and answers its id.
func (issue *restoreIssue) answer(document, blockID, text string) string {
	t := issue.t
	t.Helper()
	ask := blockAskOn(t, issue.service, document, blockID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	at := time.Now().UTC()
	change := asks.Answered(model.AskAnswer{User: "alice", Selected: []string{}, Text: &text, At: at})
	change.Who, change.At = alice, at
	settleAskAs(t, issue.service, document, blockID, ask.ID, alice, change)
	return ask.ID
}

// run runs the decisions step's third phase over DOC, converting where convert, and answers what
// it did to DOC-1.
func (issue *restoreIssue) run(convert bool) DecisionIssue {
	t := issue.t
	t.Helper()
	step := NewDecisionStep(issue.database, "", nil)
	step.UseDocuments(issue.service)
	pass, err := step.RestoreAndConvert(context.Background(), "DOC", convert, false)
	done, failed := pass.Done, pass.Failed
	if err != nil || len(failed) > 0 {
		t.Fatalf("the third phase answered %v with failures %+v", err, failed)
	}
	if len(done) != 1 || done[0].Issue != "DOC-1" {
		t.Fatalf("the third phase touched %+v, want DOC-1 alone", done)
	}
	return done[0]
}

// latest is the markdown of document's newest version.
func (issue *restoreIssue) latest(document string) string {
	issue.t.Helper()
	var markdown string
	if err := issue.database.Pool.QueryRow(context.Background(), `
		select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, document).Scan(&markdown); err != nil {
		issue.t.Fatalf("read the newest version of %s: %v", document, err)
	}
	return markdown
}

// refKey is document's ref_key.
func (issue *restoreIssue) refKey(document string) string {
	issue.t.Helper()
	return refKeyOf(issue.t, issue.database, document)
}

// movedTo is each moved_to tuple the store states, from address to address, from document's
// blocks.
func (issue *restoreIssue) movedTo(document string) map[string]string {
	issue.t.Helper()
	rows, err := issue.database.Pool.Query(context.Background(), `
		select from_id, to_id from graph_tuples
		where kind = $1 and from_kind = $2 and starts_with(from_id, $3) and asserted
	`, graphstore.TupleMovedTo, refs.BlockNodeKind, issue.refKey(document)+"#")
	if err != nil {
		issue.t.Fatalf("read the moves: %v", err)
	}
	defer rows.Close()
	moves := map[string]string{}
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			issue.t.Fatal(err)
		}
		moves[from] = to
	}
	return moves
}

// askRow is the block and the document the row of ask names.
func (issue *restoreIssue) askRow(ask string) (string, string) {
	issue.t.Helper()
	var block, document string
	if err := issue.database.Pool.QueryRow(context.Background(), `
		select block_id, block_artifact_id::text from asks where id = $1
	`, ask).Scan(&block, &document); err != nil {
		issue.t.Fatalf("read the row of ask %s: %v", ask, err)
	}
	return block, document
}

// The conversion of a decisions document, after the restore of the
// answered asks its sections restate (R6): a section that cites an answered ask gives that ask's
// restored block its links as `shaped` and its body as a reading, whose author is the author of
// the section's newest version; one whose body is the ask's answer and nothing more, in other case
// and spacing, is dropped and writes no reading; one that cites no ask becomes a decision in the
// spec's `## Decisions` part, its `Unsure:` paragraph uncertainty="high" with the rest of the
// paragraph as uncertainty_reason; a section under `## Replaced` becomes a decision with
// replaced="true". The blocks stand in place of the part's pointer line. Each section leaves one
// moved_to tuple from its heading's address, and the document, holding no section any more, is
// retired.
func TestTheConversionTurnsEachSectionIntoANodeOrARuleDeletesIt(t *testing.T) {
	const pointer = "The decisions this spec rests on are in dispatch://DOC-1/artifact/decisions-md."
	const asked = "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n" +
		":::ask{#q-store urgency=\"high\" multiple=\"false\"}\nWhere do the readings come from?\n:::\n\n" +
		":::ask{#q-format urgency=\"med\" multiple=\"false\"}\nHow is a reading written?\n:::\n\n## Decisions\n\n" + pointer + "\n"
	issue := newRestoreIssue(t, asked)
	store := issue.answer(issue.spec, "q-store", "Read the seeded test database.")
	format := issue.answer(issue.spec, "q-format", "One line for each measurement.")
	issue.replace(issue.spec, "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n## Decisions\n\n"+pointer+"\n")
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions", "# Decisions\n\n"+
		"## Readings come from the seeded database\n\nThe person ruled the test database, and we add that it stays small. "+
		"This shapes dispatch://DOC-1/spec#b-r1. Decided in dispatch://DOC-1/ask/"+store+".\n\n"+
		"## How a reading is written: dispatch://DOC-1/ask/"+format+"\n\none line  for each MEASUREMENT.\n\n"+
		"## Files stay out of the corpus\n\nWe write no file beside the database, which dispatch://DOC-1/spec#b-r1 depends on.\n\n"+
		"Unsure: a reader who wants a file export would change this.\n\n"+
		"## Replaced\n\n### Files held the corpus once\n\nWe first kept the corpus in files.\n")

	result := issue.run(true)
	if len(result.Restored) != 2 || result.Readings != 1 || result.Dropped != 1 || result.Decisions != 2 || result.Left != 0 || !result.Retired {
		t.Fatalf("the conversion did %+v, want two asks restored, one reading, one section dropped, two decisions, none left, the document retired", result)
	}
	spec := issue.latest(issue.spec)
	part := spec[strings.Index(spec, "## Decisions"):]
	// Each restored ask is answered, so its opener keeps what a version keeps of it on a fleet of
	// one build - its state, who answered and when (keptAskState) - and none of the answer's words.
	for _, want := range []string{
		`#q-store urgency="high" multiple="false" state="answered" answered_by="alice" answered_at="`,
		`" shaped="dispatch://DOC-1/spec#b-r1"}`,
		"we add that it stays small",
		`#q-format urgency="med" multiple="false" state="answered" answered_by="`,
		`uncertainty="high" uncertainty_reason="a reader who wants a file export would change this."`,
		"We write no file beside the database",
		`replaced="true"`,
		"Files held the corpus once",
	} {
		if !strings.Contains(part, want) {
			t.Errorf("the spec's Decisions part holds no %q:\n%s", want, part)
		}
	}
	for _, gone := range []string{pointer, "Unsure:", "one line  for each", "selected=", ` answer="`} {
		if strings.Contains(spec, gone) {
			t.Errorf("the converted spec still holds %q:\n%s", gone, spec)
		}
	}
	if strings.Index(part, "q-store") > strings.Index(part, "q-format") {
		t.Errorf("the restored asks stand out of answer-time order:\n%s", part)
	}
	for _, ask := range []string{store, format} {
		if _, document := issue.askRow(ask); document != issue.spec {
			t.Errorf("ask %s's row names document %s, want the spec %s", ask, document, issue.spec)
		}
	}
	if moves := issue.movedTo(decisions); len(moves) != 4 {
		t.Errorf("the conversion stated %d moves %v from the decisions document's headings, want one for each of its four sections", len(moves), moves)
	}
	var retired bool
	var role *string
	if err := issue.database.Pool.QueryRow(context.Background(), `select retired_at is not null, role from artifacts where id = $1`, decisions).Scan(&retired, &role); err != nil || !retired || role != nil {
		t.Errorf("the decisions document reads retired %v role %v (%v), want retired with no role", retired, role, err)
	}
	// The readings' alone: the block index states an authored_by of every authored node too (the
	// requirement r1 alice wrote, the issue tree's branch), which is no reading.
	var authored int
	if err := issue.database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples t
		where t.kind = $1 and t.from_kind = $2 and starts_with(t.from_id, $3) and t.to_kind = $4 and t.to_id = 'alice' and t.asserted
		  and exists (select 1 from graph_nodes n where n.kind = t.from_kind and n.id = t.from_id and n.supertag = 'reading')
	`, graphstore.TupleAuthoredBy, refs.BlockNodeKind, issue.refKey(issue.spec)+"#", graphstore.KindPerson).Scan(&authored); err != nil || authored != 1 {
		t.Errorf("the spec's readings hold %d authored_by tuples to alice (%v), want the one reading's: the author of its section's newest version", authored, err)
	}
	if work, err := graphcensus.ReadRestoreWork(context.Background(), issue.database.Pool); err != nil || len(work) != 0 {
		t.Errorf("the restore leaves work %+v (%v), want none", work, err)
	}
}

// A section under `## Replaced` becomes a decision with replaced="true" whichever ask it is about
// (the spec's R9): that rule comes before the reading rule. A section under it that cites a
// person's answered ask - whose block the spec holds, or whose block left the spec and the
// restore returns - becomes a decision in the spec's `## Decisions` part that keeps the section's
// heading block id, takes the section's links as shaped and the ask as its one from_ask; no
// reading stands under the ask, the ask takes none of the section's links, and the heading's
// moved_to reaches the decision, not the ask's block; a person's approval of the spec carries to
// the conversion's version, whose one change is that decision under the heading it adds. One that
// holds an open ask block and nothing else, which moves into the spec, becomes a decision too,
// settling no ask, rather than being dropped onto that block. Each time the document retires and
// the step owes nothing.
func TestTheConversionMakesASectionUnderReplacedADecisionWhicheverAskItIsAbout(t *testing.T) {
	const title = "microVM pods with a cgroup per command"
	const words = "We first ran each command in a microVM pod with a cgroup of its own."
	const asked = "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n" +
		":::ask{#q-pods urgency=\"high\" multiple=\"false\"}\nWhere does each command run?\n:::\n"
	replacedCiting := func(ask string) string {
		return "# Decisions\n\n## Replaced\n\n### " + title + "\n\n" + words + " The person ruled it in dispatch://DOC-1/ask/" + ask +
			", which shapes dispatch://DOC-1/spec#b-r1.\n"
	}
	for _, test := range []struct {
		name string
		// prepare writes the decisions document and answers its id and the ask its section is
		// about: the answered ask it cites, or the open ask whose block it holds.
		prepare func(issue *restoreIssue) (string, string)
		// settles is whether the decision settles that ask (from_ask), shaped the spec blocks it
		// takes the section's links as, words what the spec's newest version holds of the section,
		// restored how many asks the restore writes into the spec, and approval what becomes of
		// the spec's approval ("" for a spec nobody approved).
		settles  bool
		shaped   []string
		words    []string
		restored int
		approval string
	}{
		{
			name: "a section citing an answered ask whose block the spec holds",
			prepare: func(issue *restoreIssue) (string, string) {
				ask := issue.answer(issue.spec, "q-pods", "Each command runs in a microVM pod.")
				issue.approveLatest(issue.spec)
				return issue.document("decisions-md", "decisions.md", false, "decisions", replacedCiting(ask)), ask
			},
			settles: true, shaped: []string{"r1"}, words: []string{words}, approval: "carried",
		},
		{
			name: "a section citing an answered ask whose block left the spec",
			prepare: func(issue *restoreIssue) (string, string) {
				ask := issue.answer(issue.spec, "q-pods", "Each command runs in a microVM pod.")
				issue.replace(issue.spec, "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n")
				return issue.document("decisions-md", "decisions.md", false, "decisions", replacedCiting(ask)), ask
			},
			settles: true, shaped: []string{"r1"}, words: []string{words}, restored: 1,
		},
		{
			name: "a section holding an open ask block and nothing else",
			prepare: func(issue *restoreIssue) (string, string) {
				decisions := issue.document("decisions-md", "decisions.md", false, "decisions", "# Decisions\n\n## Replaced\n\n### "+title+"\n\n"+
					":::ask{#q-region urgency=\"low\" multiple=\"false\"}\nWhich region went first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n")
				return decisions, blockAskOn(issue.t, issue.service, decisions, "q-region").ID
			},
			words: []string{title, "#q-region "}, restored: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			issue := newRestoreIssue(t, asked)
			decisions, ask := test.prepare(issue)
			heading := issue.headingID(decisions, title)

			result := issue.run(true)
			markdown := issue.latest(issue.spec)
			tree, err := pmdoc.ParseRendering(markdown)
			if err != nil {
				t.Fatalf("read the spec's newest version: %v", err)
			}
			pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
				if node.Type == readingType {
					t.Errorf("the spec's newest version holds the reading %v %q, want none: the section under `## Replaced` is a decision, not a reading of ask %s",
						node.Attrs[pmdoc.BlockIDAttr], pmdoc.TextContent(node), ask)
				}
				return true
			})
			if result.Decisions != 1 || result.Readings != 0 || result.Dropped != 0 || result.Removed != 0 || result.Left != 0 ||
				len(result.Restored) != test.restored || !result.Retired || result.SpecApproval != test.approval {
				t.Errorf("the conversion did %+v, want the section a decision, %d asks restored, nothing else, the spec's approval %q, the document retired",
					result, test.restored, test.approval)
			}
			decision := issue.refKey(issue.spec) + "#" + heading
			if moved := issue.movedTo(decisions)[issue.refKey(decisions)+"#"+heading]; moved != decision {
				t.Errorf("the heading %s moves to %q, want the decision %s that keeps its block id", heading, moved, decision)
			}
			if replaced := issue.value(decision, refs.ValueReplaced); replaced != "true" {
				t.Errorf("the decision %s states replaced %q, want true", decision, replaced)
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
			var wantShaped, wantFrom []string
			for _, block := range test.shaped {
				wantShaped = append(wantShaped, issue.refKey(issue.spec)+"#"+block)
			}
			if test.settles {
				wantFrom = []string{ask}
			}
			if !slices.Equal(gotShaped, wantShaped) || !slices.Equal(fromAsk, wantFrom) {
				t.Errorf("the decision %s shaped %v and is from_ask %v, want shaped %v and from_ask %v", decision, gotShaped, fromAsk, wantShaped, wantFrom)
			}
			if tuples, err := graphstore.CurrentOut(ctx, issue.database.Pool, graphstore.KindAsk, ask, []string{refs.TupleShaped}); err != nil ||
				slices.ContainsFunc(tuples, func(tuple graphstore.Tuple) bool { return tuple.Asserted }) {
				t.Errorf("ask %s shaped %+v (%v), want nothing: the decision takes the section's links", ask, tuples, err)
			}
			fromAskAttribute := `from_ask="dispatch://DOC-1/ask/` + ask + `"`
			for _, want := range append([]string{"#" + heading + " ", `replaced="true"`}, test.words...) {
				if !strings.Contains(markdown, want) {
					t.Errorf("the spec's newest version holds no %q:\n%s", want, markdown)
				}
			}
			if strings.Contains(markdown, fromAskAttribute) != test.settles {
				t.Errorf("the spec's newest version holding %q is %v, want %v:\n%s", fromAskAttribute, !test.settles, test.settles, markdown)
			}
			issue.requireRetiredAndOwedNothing(decisions)
		})
	}
}

// A standing Keep the conversion carries onto a decision's node is
// written by the actor the Keep's own decision.kept event recorded, so the kept_by and the
// kept_text_sum it becomes keep the Proven mark the Keep's credential gave it: a row derived from
// an earlier write carries that write's actor, and so its mark (model.Actor.Proven). A Keep whose
// row no decision.kept names is the person alone, with no mark, since nothing the step reads says
// a credential proved them.
func TestTheConversionWritesAKeepAsTheActorItsEventRecorded(t *testing.T) {
	ctx := context.Background()
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n")
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions", "# Decisions\n\n"+
		"## Files stay out of the corpus\n\nWe write no file beside the database.\n\n"+
		"## The census runs first\n\nThe census reads the corpus before any view of it.\n")
	headings := map[string]string{}
	for _, section := range decisionSectionsOf(liveTree(t, issue.service, decisions)) {
		headings[section.title] = section.headingID()
	}
	proven, unnamed := headings["Files stay out of the corpus"], headings["The census runs first"]
	if proven == "" || unnamed == "" {
		t.Fatalf("the decisions document's sections are %v, want the two headings", headings)
	}
	alice := model.Actor{Kind: "user", ID: "alice", Proven: true,
		Act: &model.ActorAct{Session: "session-alice", SessionProven: true, Scopes: []string{model.ScopePerson}}}
	recorded, err := json.Marshal(alice)
	if err != nil {
		t.Fatal(err)
	}
	keep := func(block, login string) {
		t.Helper()
		section, found, err := issue.service.DecisionHeading(ctx, issue.database.Pool, decisions, block)
		if err != nil || !found {
			t.Fatalf("the heading %s reads no section (%t, %v)", block, found, err)
		}
		if _, err := issue.database.Pool.Exec(ctx, `
			insert into decision_keeps (artifact_id, block_id, text_sha256, changed_version, kept_by)
			values ($1::uuid, $2, $3, 1, $4)
		`, decisions, block, section.TextSHA256, login); err != nil {
			t.Fatalf("write %s's Keep of %s: %v", login, block, err)
		}
	}
	keep(proven, "alice")
	keep(unnamed, "bob")
	// alice's Keep appended its decision.kept beside the row, as Keep does (api's
	// keepDecisionHeading); bob's row has none.
	if _, err := issue.database.Pool.Exec(ctx, `
		with bumped as (update issues set last_seq = last_seq + 1 where key = 'DOC-1' returning last_seq)
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'DOC-1', bumped.last_seq, 'decision.kept', $1::jsonb,
		       jsonb_build_object('artifact_id', $2::text, 'block_id', $3::text, 'changed_version', 1), false
		from bumped
	`, string(recorded), decisions, proven); err != nil {
		t.Fatalf("append alice's decision.kept: %v", err)
	}

	if result := issue.run(true); result.Decisions != 2 || !result.Retired {
		t.Fatalf("the conversion did %+v, want both sections decisions and the document retired", result)
	}
	moves := issue.movedTo(decisions)
	for _, test := range []struct {
		heading string
		want    model.Actor
	}{
		{proven, alice},
		{unnamed, model.Actor{Kind: "user", ID: "bob"}},
	} {
		address := moves[issue.refKey(decisions)+"#"+test.heading]
		if address == "" {
			t.Fatalf("the conversion stated no move from the heading %s: %v", test.heading, moves)
		}
		keptBy, err := graphstore.History(ctx, issue.database.Pool, refs.BlockNodeKind, address, refs.TupleKeptBy)
		if err != nil || len(keptBy) != 1 || keptBy[0].ToID != test.want.ID || !reflect.DeepEqual(keptBy[0].Who, test.want) {
			t.Errorf("the decision %s holds the kept_by tuples %+v (%v), want one to %s written by %+v", address, keptBy, err, test.want.ID, test.want)
		}
		// Each Keep was of the section as it stands, so each decision holds a kept_text_sum too.
		keptSum, err := graphstore.History(ctx, issue.database.Pool, refs.BlockNodeKind, address, refs.ValueKeptTextSum)
		if err != nil || len(keptSum) != 1 || !reflect.DeepEqual(keptSum[0].Who, test.want) {
			t.Errorf("the decision %s holds the kept_text_sum values %+v (%v), want one written by %+v", address, keptSum, err, test.want)
		}
	}
}

// A Keep recorded on a section before a renderer change summed the section in the spelling of the
// version it names, and the conversion carries it to the section's decision as the release
// before's Decisions read kept it (keptAcrossRenderers). The version the Keep names is stored in
// another spelling of the section's loose list (respelledLooseList), and the Keep holds the sum of
// the section's text in that spelling, so its sum is not the section's as this build renders it.
// Stamped as another renderer version stored it, the version holds the kept text, which this build
// reads as the section's text now: the decision holds alice's kept_by and a kept_text_sum. Stamped
// with this renderer version, the same version and Keep are compared by their sum alone, as a
// version this renderer version stored is: the decision holds her kept_by and no kept_text_sum,
// and waits for review.
func TestTheConversionCarriesAKeepOfASectionAnotherRendererVersionSpelled(t *testing.T) {
	for _, test := range []struct {
		name    string
		stamp   int
		carries bool
	}{
		{"another renderer version stored the version the Keep names", pmdoc.RendererVersion - 1, true},
		{"this renderer version stamps the version the Keep names", pmdoc.RendererVersion, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n")
			decisions := issue.document("decisions-md", "decisions.md", false, "decisions",
				"# Decisions\n\n## The corpus lists three sources\n\nThe sources are these:\n\n"+looseList)
			sections := decisionSectionsOf(liveTree(t, issue.service, decisions))
			if len(sections) != 1 {
				t.Fatalf("the decisions document's sections are %+v, want one", sections)
			}
			heading := sections[0].headingID()
			list, respelledList := strings.TrimSuffix(looseList, "\n"), strings.TrimSuffix(respelledLooseList, "\n")
			section, found, err := issue.service.DecisionHeading(ctx, issue.database.Pool, decisions, heading)
			if err != nil || !found || !strings.Contains(section.Text, list) {
				t.Fatalf("the heading %s reads the section %+v (%t, %v), want one holding its loose list", heading, section, found, err)
			}
			var number int
			var stored string
			if err := issue.database.Pool.QueryRow(ctx, `
				select number, markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
			`, decisions).Scan(&number, &stored); err != nil {
				t.Fatalf("read the decisions document's latest version: %v", err)
			}
			respelled := strings.Replace(stored, looseList, respelledLooseList, 1)
			if respelled == stored {
				t.Fatalf("the decisions document's version %d holds no loose list to respell:\n%s", number, stored)
			}
			if _, err := issue.database.Pool.Exec(ctx, `
				update artifact_versions set markdown = $3, renderer_version = $4 where artifact_id = $1 and number = $2
			`, decisions, number, respelled, test.stamp); err != nil {
				t.Fatalf("store version %d in another spelling of its loose list: %v", number, err)
			}
			kept := strings.Replace(section.Text, list, respelledList, 1)
			if decisionread.TextSum(kept) == section.TextSHA256 {
				t.Fatalf("the kept text %q sums as the section does now: the Keep must hold another sum", kept)
			}
			if _, err := issue.database.Pool.Exec(ctx, `
				insert into decision_keeps (artifact_id, block_id, text_sha256, changed_version, kept_by)
				values ($1::uuid, $2, $3, $4, 'alice')
			`, decisions, heading, decisionread.TextSum(kept), number); err != nil {
				t.Fatalf("write alice's Keep of %s: %v", heading, err)
			}

			if result := issue.run(true); result.Decisions != 1 || !result.Retired {
				t.Fatalf("the conversion did %+v, want the section a decision and the document retired", result)
			}
			address := issue.movedTo(decisions)[issue.refKey(decisions)+"#"+heading]
			if address == "" {
				t.Fatalf("the conversion stated no move from the heading %s", heading)
			}
			keptBy, err := graphstore.History(ctx, issue.database.Pool, refs.BlockNodeKind, address, refs.TupleKeptBy)
			if err != nil || len(keptBy) != 1 || keptBy[0].ToID != "alice" {
				t.Errorf("the decision %s holds the kept_by tuples %+v (%v), want one to alice", address, keptBy, err)
			}
			keptSum, err := graphstore.History(ctx, issue.database.Pool, refs.BlockNodeKind, address, refs.ValueKeptTextSum)
			if want := map[bool]int{true: 1, false: 0}[test.carries]; err != nil || len(keptSum) != want {
				t.Errorf("the decision %s holds the kept_text_sum values %+v (%v), want %d", address, keptSum, err, want)
			}
		})
	}
}

// The collision of each shape of the restore: an answered ask whose
// block left the spec, where a callout of the spec now holds its block id, and an answered ask
// block of the decisions document whose id a callout of the spec holds too, each take a block id
// no document of the issue holds, recorded on the ask's row. The main shape writes no moved_to;
// the second writes one, from the block's address in the decisions document to its new one.
func TestTheRestoreGivesACollidingAskAFreshBlockIDInEachShape(t *testing.T) {
	const callouts = ":::callout{#q-left kind=\"note\" title=\"\"}\nThe readings name their database.\n:::\n\n" +
		":::callout{#q-doc kind=\"note\" title=\"\"}\nA moved block keeps a line.\n:::\n"
	issue := newRestoreIssue(t, "# Spec\n\n:::ask{#q-left urgency=\"high\" multiple=\"false\"}\nWhere do the readings come from?\n:::\n")
	left := issue.answer(issue.spec, "q-left", "The test database.")
	issue.replace(issue.spec, "# Spec\n\n"+callouts)
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions",
		"# Decisions\n\n## Which line names a moved block\n\n:::ask{#q-doc urgency=\"low\" multiple=\"false\"}\nWhich line names a block that moved?\n:::\n")
	moved := issue.answer(decisions, "q-doc", "The census names each move.")

	result := issue.run(false)
	if len(result.Restored) != 2 {
		t.Fatalf("the restore returned %+v, want both asks", result.Restored)
	}
	for _, restored := range result.Restored {
		if !restored.NewID {
			t.Errorf("ask %s kept its block id though the spec holds it", restored.Ask)
		}
	}
	leftBlock, leftDocument := issue.askRow(left)
	movedBlock, movedDocument := issue.askRow(moved)
	if leftBlock == "q-left" || movedBlock == "q-doc" || leftDocument != issue.spec || movedDocument != issue.spec {
		t.Fatalf("the rows name %s of %s and %s of %s, want fresh ids in the spec %s", leftBlock, leftDocument, movedBlock, movedDocument, issue.spec)
	}
	spec := issue.latest(issue.spec)
	for _, want := range []string{"#q-left kind=", "#q-doc kind=", "#" + leftBlock + " urgency=", "#" + movedBlock + " urgency="} {
		if !strings.Contains(spec, want) {
			t.Errorf("the restored spec holds no %q:\n%s", want, spec)
		}
	}
	if moves := issue.movedTo(issue.spec); len(moves) != 0 {
		t.Errorf("the main shape stated moves %v, want none", moves)
	}
	want := map[string]string{issue.refKey(decisions) + "#q-doc": issue.refKey(issue.spec) + "#" + movedBlock}
	moves := issue.movedTo(decisions)
	if len(moves) != 2 || moves[issue.refKey(decisions)+"#q-doc"] != want[issue.refKey(decisions)+"#q-doc"] {
		t.Errorf("the second shape stated moves %v, want %v and the one of the section that held nothing but the block", moves, want)
	}
}

// A reading in a copy of an answered ask, on another document of the issue, hangs on the source's
// node (refs.TypedSources.Shown), while its row names the copy's own block as its parent. The
// census reads such a part_of as the block the reading stands in, the copy's, and not as the block
// of the source's id in the reading's document (graphcensus' askBlockID): once the restore gives
// the source a fresh block id (R6's collision rule), the copy keeps the id it was copied under.
//
// DOC-1's spec holds the answered ask q-shelf, and its notes hold a copy of it holding a reading.
// The release before's agent takes q-shelf's block out of the spec and writes a callout under its
// id there, and the restore returns the ask under a fresh id, while the copy in the notes keeps
// q-shelf. The census reads no difference of the block kind or of reading_parent.
func TestTheCensusReadsAReadingInACopyUnderTheCopyOnceTheRestoreRenamedItsSource(t *testing.T) {
	ctx := context.Background()
	const shelf = "Which shelf holds the printed corpus?"
	issue := newRestoreIssue(t, "# Spec\n\n:::ask{#q-shelf urgency=\"low\" multiple=\"false\"}\n"+shelf+"\n:::\n")
	source := issue.answer(issue.spec, "q-shelf", "The top shelf.")
	notes := issue.document("notes-md", "notes.md", false, "", "# Notes\n\n::::ask{#q-shelf urgency=\"low\" multiple=\"false\"}\n"+shelf+"\n\n"+
		":::reading{#g-shelf}\nThe agent reads the top shelf as the one by the door.\n:::\n::::\n")
	reading := issue.refKey(notes) + "#g-shelf"
	parents, err := graphstore.CurrentOut(ctx, issue.database.Pool, refs.BlockNodeKind, reading, []string{graphstore.TuplePartOf})
	if err != nil || len(parents) != 1 || parents[0].ToKind != graphstore.KindAsk || parents[0].ToID != source {
		t.Fatalf("the reading in the copy states part_of %+v (%v), want the source ask %s's node", parents, err, source)
	}
	issue.replace(issue.spec, "# Spec\n\n:::callout{#q-shelf kind=\"note\" title=\"\"}\nThe shelf is labelled.\n:::\n")

	result := issue.run(false)
	if len(result.Restored) != 1 || !result.Restored[0].NewID {
		t.Fatalf("the restore did %+v, want the source ask returned under a fresh block id", result)
	}
	if block, _ := issue.askRow(source); block == "q-shelf" {
		t.Fatalf("the source ask's row names block %s after the restore, want a fresh id", block)
	}
	requireNoCensusDifference(t, takeStoreCensus(t, issue.database), refs.BlockNodeKind, "reading_parent")
}

// A spec that holds no `## Decisions` part: the restore adds the
// heading, at the end of the spec, and the restored ask block under it.
func TestTheRestoreAddsADecisionsPartToASpecThatHoldsNone(t *testing.T) {
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe corpus is written once.\n:::\n\n:::ask{#q-b urgency=\"med\" multiple=\"false\"}\nWhich build writes the corpus?\n:::\n")
	issue.answer(issue.spec, "q-b", "The base release.")
	issue.replace(issue.spec, "# Spec\n\n:::requirement{#r1}\nThe corpus is written once.\n:::\n")
	result := issue.run(false)
	if len(result.Restored) != 1 || result.SpecVersion == 0 {
		t.Fatalf("the restore did %+v, want one ask restored in a version of the spec", result)
	}
	spec := issue.latest(issue.spec)
	heading := strings.Index(spec, "## Decisions\n")
	if heading < 0 || !strings.Contains(spec[heading:], "#q-b urgency=") || strings.Index(spec, "#r1") > heading {
		t.Fatalf("the restored spec holds no Decisions part after its requirement with the ask under it:\n%s", spec)
	}
}

// A closed issue's spec: the restore writes it as the version a
// settlement of a closed issue's document is written, through SnapshotVersion under the
// owner-verified mark, and the issue stays closed.
func TestTheRestoreWritesAClosedIssuesSpec(t *testing.T) {
	issue := newRestoreIssue(t, "# Spec\n\n:::ask{#q-c urgency=\"med\" multiple=\"false\"}\nWhich build writes the corpus?\n:::\n")
	issue.answer(issue.spec, "q-c", "The base release.")
	issue.replace(issue.spec, "# Spec\n\nThe corpus.\n")
	writeIssueClosed(t, issue.database, "DOC-1", true)
	result := issue.run(false)
	if len(result.Restored) != 1 || result.SpecVersion == 0 {
		t.Fatalf("the restore of the closed issue did %+v, want one ask restored in a version of the spec", result)
	}
	if spec := issue.latest(issue.spec); !strings.Contains(spec, "#q-c urgency=") {
		t.Fatalf("the closed issue's spec holds no restored ask:\n%s", spec)
	}
	var closed bool
	if err := issue.database.Pool.QueryRow(context.Background(), `select closed_at is not null from issues where key = 'DOC-1'`).Scan(&closed); err != nil || !closed {
		t.Fatalf("the issue reads closed %v (%v) after the restore, want it closed still", closed, err)
	}
}

// A block that has no ask row and is no copy of an ask holds none of the eight server-owned
// attributes (askServerAttributeNames) in any version the restore writes, whatever its live tree
// gives it (R10). No release's API makes such a block: alice's browser, connected to the document's
// room while DOC-1 is open, writes it there, and the block opens no ask and copies none. In each
// case the restore runs on a fleet of one build and writes a version of the document holding the
// block, and every version it writes holds that block, if it holds it, with none of the eight:
//   - settled on a fleet of one build: alice writes the block into the spec after DOC-1 closed in
//     the database alone, a minute ahead, so the store's close margin admits it
//     (observedBeforeClose), and the closed issue's settlement saves it with no row and no state.
//     The spec's answered ask lost its block to the release before's agent, so the restore writes
//     a version of the spec.
//   - settled resolved while the release before served: the same, but the release before still
//     serves when the closed issue's settlement saves the block, which it writes state="resolved"
//     for that release's pages (resolveUnindexedAskBlock); the fleet is then one build.
//   - left in the decisions document, resolved by the close's sweep while the release before
//     served: alice writes the block into the decisions document beside an answered ask block,
//     which the restore moves into the spec, while DOC-1 is open and the release before serves;
//     no settlement runs, so the document's block index never holds the block, and the close's
//     final sweep writes it state="resolved" (WriteIssueAskStates, markUnindexed). The fleet is
//     then one build, and the restore, which moves only the blocks the index holds, leaves the
//     block in the decisions document, whose version it writes.
func TestEveryVersionTheRestoreWritesHoldsNoneOfTheEightOnABlockWithNoAskRowThatIsNoCopy(t *testing.T) {
	const head = "# Spec\n\n:::requirement{#r1}\nThe corpus is written once.\n:::\n"
	const rowless = "q-closed"
	const block = "\n:::ask{#" + rowless + " urgency=\"med\" multiple=\"false\"}\nWas this asked after the close?\n:::\n"
	const resolved = `state="resolved"`
	alice := model.Actor{Kind: "user", ID: "alice"}
	closeAMinuteAhead := func(t *testing.T, issue *restoreIssue) {
		t.Helper()
		if _, err := issue.database.Pool.Exec(context.Background(), `update issues set closed_at = now() + interval '1 minute' where key = 'DOC-1'`); err != nil {
			t.Fatalf("close DOC-1 in the database alone: %v", err)
		}
	}
	// serveOneBuild takes the two processes serveTwoBuilds registered out of the fleet.
	serveOneBuild := func(t *testing.T, issue *restoreIssue) {
		t.Helper()
		if _, err := issue.database.Pool.Exec(context.Background(), `delete from server_processes where commit in ('the-release-before', 'this-release')`); err != nil {
			t.Fatalf("leave a fleet of one build: %v", err)
		}
	}
	// droppedSpec is DOC-1 whose spec's answered ask lost its block to the release before's agent,
	// with alice's browser connected to the spec's room.
	droppedSpec := func(t *testing.T) (*restoreIssue, *docstest.Peer) {
		t.Helper()
		issue := newRestoreIssue(t, head+"\n:::ask{#q-left urgency=\"med\" multiple=\"false\"}\nWhich build writes the corpus?\n:::\n")
		issue.answer(issue.spec, "q-left", "The base release.")
		issue.replace(issue.spec, head)
		return issue, connectBrowser(t, newTestServer(t, issue.service).URL, issue.spec, "alice")
	}
	for _, test := range []struct {
		name string
		// write builds DOC-1 and alice's block, and answers the issue and the document holding it.
		write func(t *testing.T) (*restoreIssue, string)
	}{
		{"settled on a fleet of one build", func(t *testing.T) (*restoreIssue, string) {
			issue, browser := droppedSpec(t)
			closeAMinuteAhead(t, issue)
			editAsBrowser(t, issue.service, issue.spec, browser, head+block)
			settleCurrentGeneration(t, issue.service, issue.spec)
			if settled := openerOf(issue.latest(issue.spec), rowless); settled == "" || strings.Contains(settled, "state=") {
				t.Fatalf("the closed issue's settlement saved alice's block as %q, want it held with no state", settled)
			}
			return issue, issue.spec
		}},
		{"settled resolved while the release before served", func(t *testing.T) (*restoreIssue, string) {
			issue, browser := droppedSpec(t)
			closeAMinuteAhead(t, issue)
			serveTwoBuilds(t, issue.database)
			editAsBrowser(t, issue.service, issue.spec, browser, head+block)
			settleCurrentGeneration(t, issue.service, issue.spec)
			if settled := openerOf(issue.latest(issue.spec), rowless); !strings.Contains(settled, resolved) {
				t.Fatalf("the closed issue's settlement saved alice's block as %q while the release before served, want it %s", settled, resolved)
			}
			serveOneBuild(t, issue)
			return issue, issue.spec
		}},
		{"left in the decisions document, resolved by the close's sweep while the release before served", func(t *testing.T) (*restoreIssue, string) {
			const section = "\n## Which line names a moved block\n\n:::ask{#q-doc urgency=\"low\" multiple=\"false\"}\nWhich line names a block that moved?\n:::\n"
			issue := newRestoreIssue(t, head)
			decisions := issue.document("decisions-md", "decisions.md", false, "decisions", "# Decisions\n"+section)
			issue.answer(decisions, "q-doc", "The census names each move.")
			browser := connectBrowser(t, newTestServer(t, issue.service).URL, decisions, "alice")
			serveTwoBuilds(t, issue.database)
			editAsBrowser(t, issue.service, decisions, browser, "# Decisions\n"+block+section)
			waitForLandedAppends(t, issue.service, decisions)
			closeAMinuteAhead(t, issue)
			if err := joinedWrite(issue.service, func(ctx context.Context) error {
				tx, _ := txFromContext(ctx)
				if _, _, err := lockArtifactOwner(ctx, tx, decisions); err != nil {
					return err
				}
				_, err := issue.service.WriteIssueAskStates(ctx, "DOC-1", nil, alice, true)
				return err
			}); err != nil {
				t.Fatalf("run the close's final sweep: %v", err)
			}
			if live := openerOf(residentRoomMarkdown(t, issue.service, decisions), rowless); !strings.Contains(live, resolved) {
				t.Fatalf("the close's sweep left alice's block as %q in the decisions document's room, want it %s", live, resolved)
			}
			serveOneBuild(t, issue)
			return issue, decisions
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			issue, document := test.write(t)
			if rows := askRowCount(t, issue.service, document, rowless); rows != 0 {
				t.Fatalf("alice's block has %d ask rows, want none", rows)
			}
			var copies int
			if err := issue.database.Pool.QueryRow(ctx, `
				select count(*) from document_blocks d
				join graph_tuples t on t.from_kind = d.node_kind and t.from_id = d.node_address
				where d.artifact_id = $1 and d.block_id = $2 and t.kind = $3 and t.asserted
			`, document, rowless, graphstore.TupleCopiedFrom).Scan(&copies); err != nil {
				t.Fatalf("read the copied_from of alice's block: %v", err)
			}
			if copies != 0 {
				t.Fatalf("alice's block holds %d copied_from tuples, want none: it copies no ask", copies)
			}
			// The versions the restore writes are every version of DOC-1's documents past the
			// newest each held before it, and each is one the restore reports.
			before := map[string]int{}
			slugs := map[string]string{}
			documents, err := issue.database.Pool.Query(ctx, `
				select a.id::text, a.slug, coalesce(max(v.number), 0) from artifacts a
				left join artifact_versions v on v.artifact_id = a.id
				where a.issue_key = 'DOC-1' group by a.id, a.slug
			`)
			if err != nil {
				t.Fatalf("read the documents of DOC-1: %v", err)
			}
			for documents.Next() {
				var id, slug string
				var newest int
				if err := documents.Scan(&id, &slug, &newest); err != nil {
					t.Fatal(err)
				}
				before[id], slugs[id] = newest, slug
			}
			documents.Close()
			if err := documents.Err(); err != nil {
				t.Fatal(err)
			}
			result := issue.run(false)
			reported := map[string]int{issue.spec: result.SpecVersion}
			if document != issue.spec {
				reported[document] = result.DecisionsVersion
			}
			if len(result.Restored) != 1 || result.OtherVersions != 0 || reported[document] <= before[document] {
				t.Fatalf("the restore did %+v, want one ask restored, a version of the document holding alice's block past %d and none of another document", result, before[document])
			}
			held := 0
			for id, newest := range before {
				rows, err := issue.database.Pool.Query(ctx, `
					select number, markdown from artifact_versions where artifact_id = $1 and number > $2 order by number
				`, id, newest)
				if err != nil {
					t.Fatalf("read the versions the restore wrote of %s: %v", slugs[id], err)
				}
				var numbers []int
				for rows.Next() {
					var number int
					var markdown string
					if err := rows.Scan(&number, &markdown); err != nil {
						t.Fatal(err)
					}
					numbers = append(numbers, number)
					held += blocksHoldingNoneOfTheEight(t, fmt.Sprintf("version %d of %s the restore wrote", number, slugs[id]), markdown, rowless)
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				var want []int
				if number := reported[id]; number > 0 {
					want = []int{number}
				}
				if !slices.Equal(numbers, want) {
					t.Fatalf("%s holds versions %v past %d, want the restore's versions %v alone", slugs[id], numbers, newest, want)
				}
			}
			if held != 1 {
				t.Fatalf("the versions the restore wrote hold %d blocks %s, want alice's one in the version of %s", held, rowless, slugs[document])
			}
		})
	}
}

// blocksHoldingNoneOfTheEight answers how many ask blocks id markdown holds, and fails t for each
// of the eight server-owned attributes (askServerAttributeNames) one of them holds, naming what
// markdown is and printing the block's opening line and markdown: a block that has no ask row and
// is no copy of an ask holds none of the eight (R10).
func blocksHoldingNoneOfTheEight(t *testing.T, what, markdown, id string) int {
	t.Helper()
	held := 0
	for _, line := range strings.Split(markdown, "\n") {
		kind, attrs, opens := pmdoc.ParseTypedDirectiveOpen(strings.TrimLeft(line, " \t>"))
		if !opens || kind != "ask" || attrs[pmdoc.BlockIDAttr] != id {
			continue
		}
		held++
		for _, name := range askServerAttributeNames {
			if value, holds := attrs[name]; holds {
				t.Errorf("%s holds %s=%v on the block with no ask row that is no copy:\n%s\nwant none of the eight; it reads:\n%s",
					what, name, value, line, markdown)
			}
		}
	}
	return held
}

// A version written without settling its document first, once the fleet is one build, holds none
// of the eight server-owned attributes on a block that has no ask row and is no copy of an ask
// (R10, storableVersionTree), though the document holds the state="resolved" a closed issue's
// settlement wrote on that block while the release before served. alice's browser, connected to
// the spec's room while DOC-1 is open, writes the block after DOC-1 closed in the database alone, a
// minute ahead, so the store's close margin admits it (observedBeforeClose), and the closed
// issue's settlement saves it with no row, resolved for the release before's pages
// (resolveUnindexedAskBlock). The fleet is then one build. alice types over the spec's title,
// which the margin admits too, and bob comments on the closed issue: his comment's anchor pins a
// version (WithCommentMarks, SnapshotVersion), the write of a version a closed issue still takes,
// which takes the stored document, alice's keystroke in it, without settling it. That version
// holds alice's block once, with none of the eight.
func TestAVersionWrittenOnAFleetOfOneBuildHoldsNoneOfTheEightOnABlockWithNoAskRowThatIsNoCopy(t *testing.T) {
	ctx := context.Background()
	const head = "# Spec\n\n:::requirement{#r1}\nThe corpus is written once.\n:::\n"
	const rowless = "q-closed"
	const retitled = "Spec, retitled once the fleet is one build"
	issue := newRestoreIssue(t, head)
	alice := connectBrowser(t, newTestServer(t, issue.service).URL, issue.spec, "alice")
	if _, err := issue.database.Pool.Exec(ctx, `update issues set closed_at = now() + interval '1 minute' where key = 'DOC-1'`); err != nil {
		t.Fatalf("close DOC-1 in the database alone: %v", err)
	}
	serveTwoBuilds(t, issue.database)
	editAsBrowser(t, issue.service, issue.spec, alice, head+"\n:::ask{#"+rowless+" urgency=\"med\" multiple=\"false\"}\nWas this asked after the close?\n:::\n")
	settleCurrentGeneration(t, issue.service, issue.spec)
	if settled := openerOf(issue.latest(issue.spec), rowless); !strings.Contains(settled, `state="resolved"`) {
		t.Fatalf("the closed issue's settlement saved alice's block as %q while the release before served, want it resolved", settled)
	}
	if rows := askRowCount(t, issue.service, issue.spec, rowless); rows != 0 {
		t.Fatalf("alice's block has %d ask rows, want none", rows)
	}
	if _, err := issue.database.Pool.Exec(ctx, `delete from server_processes where commit in ('the-release-before', 'this-release')`); err != nil {
		t.Fatalf("leave a fleet of one build: %v", err)
	}
	typeOver(t, alice, "Spec", retitled)
	waitFor(t, roomWait(t), "the spec's room to take alice's keystroke", func() bool {
		return strings.Contains(residentRoomMarkdown(t, issue.service, issue.spec), retitled)
	})
	waitForLandedAppends(t, issue.service, issue.spec)

	before := latestVersionNumber(t, issue.service, issue.spec)
	if _, err := commentAnchor(issue.service, issue.spec, "c1", "The corpus is written once.", model.Actor{Kind: "user", ID: "bob"}); err != nil {
		t.Fatalf("bob's comment on the closed issue: %v", err)
	}
	rows, err := issue.database.Pool.Query(ctx, `
		select number, markdown from artifact_versions where artifact_id = $1 and number > $2 order by number
	`, issue.spec, before)
	if err != nil {
		t.Fatalf("read the versions of the spec past %d: %v", before, err)
	}
	written := 0
	for rows.Next() {
		var number int
		var markdown string
		if err := rows.Scan(&number, &markdown); err != nil {
			t.Fatal(err)
		}
		written++
		if !strings.Contains(markdown, retitled) {
			t.Errorf("version %d of the spec reads:\n%s\nwant alice's keystroke in it, which the store holds", number, markdown)
		}
		if held := blocksHoldingNoneOfTheEight(t, fmt.Sprintf("version %d of the spec bob's comment pinned", number), markdown, rowless); held != 1 {
			t.Errorf("version %d of the spec holds %d blocks %s, want alice's one; it reads:\n%s", number, held, rowless, markdown)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if written != 1 {
		t.Fatalf("bob's comment left %d versions of the spec past %d, want the one its anchor pins", written, before)
	}
}

// partOf is the address of the document the current part_of tuple of ask's node names, "" for
// none.
func (issue *restoreIssue) partOf(ask string) string {
	issue.t.Helper()
	tuples, err := graphstore.CurrentOut(context.Background(), issue.database.Pool, graphstore.KindAsk, ask, []string{graphstore.TuplePartOf})
	if err != nil {
		issue.t.Fatalf("read the part_of of ask %s: %v", ask, err)
	}
	for _, tuple := range tuples {
		if tuple.Asserted {
			return tuple.ToID
		}
	}
	return ""
}

// restoreWith runs the third phase on DOC-1 alone over work, the finder's read of an earlier
// moment, as a pass that read its work before the issue's lock does, converting where convert, and
// answers what it did.
func (issue *restoreIssue) restoreWith(work []graphcensus.RestoreWork, convert bool) DecisionIssue {
	t := issue.t
	t.Helper()
	step := NewDecisionStep(issue.database, "", nil)
	step.UseDocuments(issue.service)
	result, err := step.restoreIssue(context.Background(), "DOC-1", work, convert, false)
	if err != nil {
		t.Fatalf("the third phase on DOC-1 answered %v", err)
	}
	return result
}

// Every ask block a decisions document holds moves into the spec in the second shape of the
// restore, whatever its ask's state: in that shape an ask block present in decisions.md moves,
// with one moved_to tuple, and the section that held it converts (R9). An
// open ask, a declined one and one the release before answered on its row alone - whose node no
// catch-up has stated answered yet - each move: the finder reads it as the restore's work, its row
// names its block in the spec, its node's part_of names the spec, a moved_to goes from its block's
// address in the decisions document to its address in the spec, the spec's newest version holds
// the block and the decisions document's no longer does. The section that held it, whose words no
// reading can take since its ask holds no answer as a reading reads one, becomes a decision that
// waits for a person's review (R9's fourth further kind): uncertainty high with the reason, its
// words and a citation of the ask it held, and no citation of the section at a version of the
// decisions document, since the block it held stands in the spec. The document's other section,
// which cites no ask, becomes a decision, and the document, holding nothing once the pass has run,
// is retired, as is one that holds an open ask block outside every section beside such a plain
// section.
func TestEveryAskBlockOfADecisionsDocumentMovesAndTheSectionThatHeldItConverts(t *testing.T) {
	const spec = "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n"
	const plain = "## Files stay out of the corpus\n\nWe write no file beside the database.\n\n"
	const lean = "\nWe lean east until someone rules.\n"
	ask := func(id string) string {
		return ":::ask{#" + id + " urgency=\"low\" multiple=\"false\"}\nWhich region goes first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	for _, test := range []struct {
		name, markdown, block string
		// settle is what happens to the ask before the pass, on its row and its node.
		settle func(issue *restoreIssue, decisions, ask string)
		// held is whether a section held the ask block, and becomes a decision of its own.
		held bool
		// state is the state the restored block's stored opener keeps, as its node holds it
		// (keptAskState): none for an ask its node holds open, the release before's answer on
		// its row alone among them.
		state string
	}{
		{"a section holding an open ask", "# Decisions\n\n" + plain + "## Which region goes first\n\n" + ask("q-open") + lean, "q-open",
			func(*restoreIssue, string, string) {}, true, ""},
		{"a section holding a declined ask", "# Decisions\n\n" + plain + "## Which region goes first\n\n" + ask("q-declined") + lean, "q-declined",
			func(issue *restoreIssue, decisions, held string) {
				at := time.Now().UTC()
				change := asks.Resolved(model.AskResolution{Kind: model.AskResolutionDeclined, Reason: "Not ours to rule.", Actor: alice, At: at})
				change.Who, change.At = alice, at
				settleAskAs(issue.t, issue.service, decisions, "q-declined", held, alice, change)
			}, true, asks.StateResolved},
		{"a section holding an ask the release before answered on its row alone", "# Decisions\n\n" + plain + "## Which region goes first\n\n" + ask("q-skipped") + lean, "q-skipped",
			func(issue *restoreIssue, _, held string) {
				answerAsThePreviousBuild(issue.t, issue.database, "DOC-1", held, "East")
			}, true, ""},
		{"an open ask block outside every section", "# Decisions\n\n" + ask("q-loose") + "\n" + plain, "q-loose",
			func(*restoreIssue, string, string) {}, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			issue := newRestoreIssue(t, spec)
			decisions := issue.document("decisions-md", "decisions.md", false, "decisions", test.markdown)
			held := blockAskOn(t, issue.service, decisions, test.block).ID
			test.settle(issue, decisions, held)
			work, err := graphcensus.ReadRestoreWork(ctx, issue.database.Pool)
			if err != nil || len(work) != 1 || work[0].Shape != graphcensus.RestoreMoves || work[0].Ask != held {
				t.Fatalf("the finder reads work %+v (%v), want the ask %s in the second shape", work, err, held)
			}
			var heading string
			if test.held {
				heading = issue.headingID(decisions, "Which region goes first")
			}

			result := issue.run(true)
			decided := 1
			if test.held {
				decided = 2
			}
			if !result.Retired || result.Left != 0 || result.Decisions != decided || len(result.Restored) != 1 {
				t.Fatalf("the conversion did %+v, want the ask moved, %d decisions, none left, the document retired", result, decided)
			}
			if block, document := issue.askRow(held); block != test.block || document != issue.spec {
				t.Errorf("the ask's row names block %s of %s, want %s of the spec %s", block, document, test.block, issue.spec)
			}
			if part := issue.partOf(held); part != issue.refKey(issue.spec) {
				t.Errorf("the ask's node is part_of %q, want the spec %s", part, issue.refKey(issue.spec))
			}
			from, to := issue.refKey(decisions)+"#"+test.block, issue.refKey(issue.spec)+"#"+test.block
			if moves := issue.movedTo(decisions); moves[from] != to {
				t.Errorf("the moves from the decisions document are %v, want %s to %s", moves, from, to)
			}
			if markdown := issue.latest(issue.spec); !strings.Contains(markdown, "#"+test.block+" ") {
				t.Errorf("the spec's newest version holds no ask block %s:\n%s", test.block, markdown)
			} else if opener := openerOf(markdown, test.block); test.state == "" && strings.Contains(opener, " state=") ||
				test.state != "" && !strings.Contains(opener, ` state="`+test.state+`"`) || strings.Contains(opener, "answered_by=") {
				t.Errorf("the restored block of %s opens %q, want it keeping the state %q its node holds and no answerer", test.block, opener, test.state)
			}
			if markdown := issue.latest(decisions); strings.Contains(markdown, "#"+test.block+" ") {
				t.Errorf("the decisions document's newest version still holds the ask block %s:\n%s", test.block, markdown)
			}
			if test.held {
				issue.requireUnplacedDecision(decisions, heading, unplacedBody, nil, []string{held},
					[]string{"We lean east until someone rules."})
				issue.requireNoSectionCitation(heading)
			}
			issue.requireRetiredAndOwedNothing(decisions)
		})
	}
}

// A section converted into a reading of an ask block the spec held already, which is not the
// spec's first ask block: the restore alone gives the answered ask whose
// block left the spec a fresh block id, since a callout holds its old one, and writes it into the
// `## Decisions` part after the spec's other answered ask; the conversion then turns the section
// that cites it into a reading of that block and another section into a decision in one version.
// That version holds the reading inside the ask's block, and the reading's node and its
// document_blocks row exist, so the moved_to from the section's heading names a block that holds
// the section's words. The restore that gives the returning ask its fresh id points the ask's
// asked_on at the fresh block and retracts the old one in its own write, so the store census's
// asked_on kind reads no difference in either direction right after it, with no backfill between.
func TestTheConversionWritesAReadingIntoAPresentAskBlockThatIsNotTheSpecsFirst(t *testing.T) {
	ctx := context.Background()
	const pointer = "The decisions this spec rests on are in dispatch://DOC-1/artifact/decisions-md."
	issue := newRestoreIssue(t, "# Spec\n\n:::ask{#q-first urgency=\"low\" multiple=\"false\"}\nWhich build seeds the corpus?\n:::\n\n"+
		":::ask{#q-left urgency=\"high\" multiple=\"false\"}\nWhere do the readings come from?\n:::\n\n## Decisions\n\n"+pointer+"\n")
	issue.answer(issue.spec, "q-first", "The base release.")
	left := issue.answer(issue.spec, "q-left", "Read the seeded test database.")
	issue.replace(issue.spec, "# Spec\n\n:::ask{#q-first urgency=\"low\" multiple=\"false\"}\nWhich build seeds the corpus?\n:::\n\n"+
		":::callout{#q-left kind=\"note\" title=\"\"}\nThe readings name their database.\n:::\n\n## Decisions\n\n"+pointer+"\n")
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions", "# Decisions\n\n"+
		"## Readings come from the seeded database\n\nThe person ruled the test database, and we add that it stays small. "+
		"Decided in dispatch://DOC-1/ask/"+left+".\n\n"+
		"## Files stay out of the corpus\n\nWe write no file beside the database.\n")
	askedOn := func() []string {
		t.Helper()
		held, err := graphstore.CurrentOut(ctx, issue.database.Pool, graphstore.KindAsk, left, []string{graphstore.TupleAskedOn})
		if err != nil {
			t.Fatalf("read the asked_on of ask %s: %v", left, err)
		}
		ends := make([]string, len(held))
		for index, tuple := range held {
			ends[index] = tuple.ToKind + ":" + tuple.ToID
		}
		return ends
	}
	if ends, want := askedOn(), refs.BlockNodeKind+":"+issue.refKey(issue.spec)+"#q-left"; !slices.Equal(ends, []string{want}) {
		t.Fatalf("before the restore ask %s states asked_on %v, want the one to %s: this case proves nothing", left, ends, want)
	}

	restored := issue.run(false)
	if len(restored.Restored) != 1 || !restored.Restored[0].NewID {
		t.Fatalf("the restore did %+v, want the ask restored under a fresh block id", restored)
	}
	block, _ := issue.askRow(left)
	if spec := issue.latest(issue.spec); strings.Index(spec, "#q-first ") > strings.Index(spec, "#"+block+" ") {
		t.Fatalf("the restored ask block %s does not stand after the spec's first ask block:\n%s", block, spec)
	}
	if ends, want := askedOn(), refs.BlockNodeKind+":"+issue.refKey(issue.spec)+"#"+block; !slices.Equal(ends, []string{want}) {
		t.Errorf("after the restore ask %s states asked_on %v, want only the one to its fresh block %s", left, ends, want)
	}
	if kind := censusKind(t, takeStoreCensus(t, issue.database), graphstore.TupleAskedOn); kind.Rows != 2 || kind.Nodes != 2 || kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
		t.Errorf("after the restore the asked_on census reads %d rows, %d tuples, %d rows no tuple states and %d tuples no row projects (%+v, %+v), want the two asks' rows and tuples and no difference",
			kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes)
	}

	result := issue.run(true)
	if result.Readings != 1 || result.Decisions != 1 || result.Left != 0 || !result.Retired {
		t.Fatalf("the conversion did %+v, want one reading, one decision, none left, the document retired", result)
	}
	spec := issue.latest(issue.spec)
	start := strings.Index(spec, "#"+block+" ")
	if start < 0 {
		t.Fatalf("the spec's newest version holds no ask block %s:\n%s", block, spec)
	}
	end := strings.Index(spec[start:], "\n::::\n")
	if end < 0 || !strings.Contains(spec[start:start+end], ":::reading{#") || !strings.Contains(spec[start:start+end], "we add that it stays small") {
		t.Fatalf("the ask block %s of the spec's newest version holds no reading of the section's words:\n%s", block, spec)
	}
	reading := spec[start+strings.Index(spec[start:], ":::reading{#")+len(":::reading{#"):]
	reading = reading[:strings.IndexAny(reading, " }")]
	var rows, nodes int
	if err := issue.database.Pool.QueryRow(ctx, `
		select (select count(*) from document_blocks where artifact_id = $1 and block_id = $2 and type = 'reading'),
		       (select count(*) from graph_nodes where kind = $3 and id = $4)
	`, issue.spec, reading, refs.BlockNodeKind, issue.refKey(issue.spec)+"#"+reading).Scan(&rows, &nodes); err != nil || rows != 1 || nodes != 1 {
		t.Fatalf("the reading %s has %d document_blocks rows and %d nodes (%v), want one of each", reading, rows, nodes, err)
	}
	heading := ""
	for from, to := range issue.movedTo(decisions) {
		if to == issue.refKey(issue.spec)+"#"+block {
			heading = from
		}
	}
	if heading == "" {
		t.Errorf("no moved_to names the ask block %s the section's reading stands in: %v", block, issue.movedTo(decisions))
	}
}

// A section the pass converts into a reading of an ask block a document holds already is taken
// out of the decisions document only where the pass wrote that reading (refuseUnwrittenReadings):
// a reading no write holds fails the issue's pass, whose transaction then writes nothing, so no
// moved_to names a block that holds none of the section's words; the same reading written passes.
func TestThePassRefusesASectionWhoseReadingItWroteIntoNoBlock(t *testing.T) {
	reading := &pmdoc.Node{Type: readingType, Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "r9"}}
	spec := issueDocument{id: "spec-id", address: "dispatch://DOC-1/spec"}
	r := &issueRestore{
		issue: "DOC-1", decisions: &issueDocument{address: "dispatch://DOC-1/artifact/decisions-md"},
		readingsWritten: map[string]map[string]bool{spec.id: {}},
		sections: []*convertedSection{{
			section: decisionSection{title: "Readings come from the seeded database"},
			fate:    fateReading, ask: "a1", reading: reading, askDocument: spec, askBlock: "q-left",
		}},
	}
	err := r.refuseUnwrittenReadings()
	if err == nil || !strings.Contains(err.Error(), "Readings come from the seeded database") || !strings.Contains(err.Error(), "q-left") {
		t.Fatalf("the pass answered %v for a reading it wrote into no block, want a refusal naming the section and the ask block", err)
	}
	r.readingsWritten[spec.id]["r9"] = true
	if err := r.refuseUnwrittenReadings(); err != nil {
		t.Fatalf("the pass answered %v for a reading it wrote, want none", err)
	}
}

// The restore replaces the spec's one pointer line to the decisions document: the first
// paragraph of the `## Decisions` part that cites the document itself. A
// paragraph of the part that cites a section of the document is the person's prose, which the
// restore keeps, so the restore that changed nothing else carries the person's approval, which
// names the restore's case.
func TestTheRestoreKeepsAParagraphCitingASectionOfTheDecisionsDocument(t *testing.T) {
	ctx := context.Background()
	const pointer = "The decisions this spec rests on are in dispatch://DOC-1/artifact/decisions-md."
	const cite = "We chose the base release because its corpus is the one we measured (dispatch://DOC-1/artifact/decisions-md#b-h1)."
	const head = "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n"
	const tail = "## Decisions\n\n" + pointer + "\n\n" + cite + "\n"
	issue := newRestoreIssue(t, head+":::ask{#q1 urgency=\"med\" multiple=\"false\"}\nWhich build writes the corpus?\n:::\n\n"+tail)
	issue.answer(issue.spec, "q1", "The base release.")
	var approved int
	if err := issue.database.Pool.QueryRow(ctx, `select max(number) from artifact_versions where artifact_id = $1`, issue.spec).Scan(&approved); err != nil {
		t.Fatalf("read the spec's newest version: %v", err)
	}
	approveStoredVersion(t, issue.database, issue.spec, approved, "alice")
	issue.replace(issue.spec, head+tail)
	issue.document("decisions-md", "decisions.md", false, "decisions", "# Decisions\n\n## The base release writes the corpus\n\nIts corpus is the one we measured.\n")

	result := issue.run(false)
	if len(result.Restored) != 1 || result.SpecApproval != "carried" {
		t.Fatalf("the restore did %+v, want the ask restored and the approval of version %d carried", result, approved)
	}
	spec := issue.latest(issue.spec)
	part := spec[strings.Index(spec, "## Decisions"):]
	if !strings.Contains(part, cite) || !strings.Contains(part, "#q1 urgency=") || strings.Contains(spec, pointer) {
		t.Fatalf("the restored spec's Decisions part reads\n%s\nwant the ask in place of the pointer line and the paragraph citing a section kept", part)
	}
	if strings.Index(part, "#q1 urgency=") > strings.Index(part, cite) {
		t.Errorf("the restored ask stands after the kept paragraph, want it in place of the pointer line:\n%s", part)
	}
	reason := approvedReasonAt(t, issue.service, issue.spec, result.SpecVersion)
	if !strings.HasPrefix(reason, "Carried forward from the approval of version ") || !strings.Contains(reason, restoreCase.change) {
		t.Errorf("the carried approval reads %q, want it naming the restore's case", reason)
	}
}

// The restore reads the finder's work again under the issue's lock:
// a returning ask whose block a person put back in the spec since the finder read it is left as
// it stands, and no second block of it is written, so the person's block keeps the ask's row.
func TestTheRestoreLeavesAReturningAskWhoseBlockItsSpecHoldsAgain(t *testing.T) {
	const block = ":::ask{#q-back urgency=\"med\" multiple=\"false\"}\nWhich build writes the corpus?\n:::\n"
	issue := newRestoreIssue(t, "# Spec\n\n"+block)
	ask := issue.answer(issue.spec, "q-back", "The base release.")
	issue.replace(issue.spec, "# Spec\n\nThe corpus.\n")
	work, err := graphcensus.ReadRestoreWork(context.Background(), issue.database.Pool)
	if err != nil || len(work) != 1 || work[0].Shape != graphcensus.RestoreReturns || work[0].Ask != ask {
		t.Fatalf("the finder reads %+v (%v), want the returning ask %s", work, err, ask)
	}
	issue.replaceAsPerson(issue.spec, "# Spec\n\nThe corpus.\n\n"+block)

	result := issue.restoreWith(work, false)
	if len(result.Restored) != 0 || result.SpecVersion != 0 {
		t.Fatalf("the restore over the finder's earlier read did %+v, want nothing: the spec holds the ask's block again", result)
	}
	if spec := issue.latest(issue.spec); strings.Count(spec, "urgency=\"med\"") != 1 {
		t.Fatalf("the spec holds the ask's block %d times, want once:\n%s", strings.Count(spec, "urgency=\"med\""), spec)
	}
	if blockID, document := issue.askRow(ask); blockID != "q-back" || document != issue.spec {
		t.Fatalf("the ask's row names block %s of %s, want q-back of the spec %s", blockID, document, issue.spec)
	}
}

// An answered ask the restore moves out of the decisions document moves from the address the
// finder read, whether or not the document still holds the block
// once the issue is locked: the release before's agent, which took the block out of the document
// since and recorded no removal, leaves the ask's row naming it, and the restore writes the ask
// into the spec, its row, one moved_to from the old address to the new one, and its node's
// relations, which name the spec. A person's deletion of the block is no such drop: the next test.
func TestTheRestoreMovesAnAskWhoseBlockLeftTheDecisionsDocumentSinceItWasRead(t *testing.T) {
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe corpus is written once.\n:::\n")
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions",
		"# Decisions\n\n## Which line names a moved block\n\n:::ask{#q-doc urgency=\"low\" multiple=\"false\"}\nWhich line names a block that moved?\n:::\n")
	moved := issue.answer(decisions, "q-doc", "The census names each move.")
	work, err := graphcensus.ReadRestoreWork(context.Background(), issue.database.Pool)
	if err != nil || len(work) != 1 || work[0].Shape != graphcensus.RestoreMoves || work[0].Ask != moved {
		t.Fatalf("the finder reads %+v (%v), want the moving ask %s", work, err, moved)
	}
	issue.replace(decisions, "# Decisions\n\n## Which line names a moved block\n\nThe census names each move.\n")
	if blockID, document := issue.askRow(moved); blockID != "q-doc" || document != decisions {
		t.Fatalf("after the agent took the block out the ask's row names %s of %s, want q-doc of the decisions document", blockID, document)
	}
	if removals := heldRemovalsOf(t, issue, moved); len(removals) != 0 {
		t.Fatalf("after the agent took the block out the ask holds the removals %+v, want none: no person removed it", removals)
	}

	result := issue.restoreWith(work, false)
	from, to := issue.refKey(decisions)+"#q-doc", issue.refKey(issue.spec)+"#q-doc"
	if len(result.Restored) != 1 || result.Restored[0].From != from || result.Restored[0].To != to {
		t.Fatalf("the restore did %+v, want the ask moved from %s to %s", result.Restored, from, to)
	}
	if blockID, document := issue.askRow(moved); blockID != "q-doc" || document != issue.spec {
		t.Errorf("the ask's row names %s of %s, want q-doc of the spec %s", blockID, document, issue.spec)
	}
	if moves := issue.movedTo(decisions); moves[from] != to {
		t.Errorf("the restore stated moves %v, want one from %s to %s", moves, from, to)
	}
	if part := issue.partOf(moved); part != issue.refKey(issue.spec) {
		t.Errorf("the ask's node is part_of %q, want the spec %s", part, issue.refKey(issue.spec))
	}
	if spec := issue.latest(issue.spec); !strings.Contains(spec, "#q-doc urgency=") {
		t.Errorf("the restored spec holds no block of the ask:\n%s", spec)
	}
}

// A person who deletes an answered ask's block from the decisions document while the step runs
// does not find it written into the spec. Alice's browser deletes the block after the finder read
// the ask as work of the second shape, and the settlement records her removal on the ask's node,
// under the event of the version of the document it writes. The restore over the finder's earlier
// read passes the ask over and declines it as it declines a return a person removed: it writes no
// block of it and no version of the spec, leaves its row naming the block it left, states no move
// of it, reports alice and that version, and leaves the one removal her settlement stated.
func TestTheRestoreDeclinesAnAnsweredAskAPersonDeletedFromTheDecisionsDocumentSinceItWasRead(t *testing.T) {
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe corpus is written once.\n:::\n")
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions",
		"# Decisions\n\n## Which line names a moved block\n\n:::ask{#q-doc urgency=\"low\" multiple=\"false\"}\nWhich line names a block that moved?\n:::\n")
	deleted := issue.answer(decisions, "q-doc", "The census names each move.")
	work, err := graphcensus.ReadRestoreWork(context.Background(), issue.database.Pool)
	if err != nil || len(work) != 1 || work[0].Shape != graphcensus.RestoreMoves || work[0].Ask != deleted {
		t.Fatalf("the finder reads %+v (%v), want the moving ask %s", work, err, deleted)
	}
	browserReplaceText(t, issue.service, decisions, "# Decisions\n\n## Which line names a moved block\n\nThe census names each move.\n",
		model.Actor{Kind: "user", ID: "alice"})
	settleCurrentGeneration(t, issue.service, decisions)
	version := latestVersionNumber(t, issue.service, decisions)
	removals := heldRemovalsOf(t, issue, deleted)
	if len(removals) != 1 || !removals[0].Asserted || removals[0].ToID != "alice" || removals[0].SourceEvent == graphstore.NoEvent {
		t.Fatalf("after alice's browser deleted its block the ask %s holds the removals %+v, want one asserted to alice under an event", deleted, removals)
	}
	if blockID, document := issue.askRow(deleted); blockID != "q-doc" || document != decisions {
		t.Fatalf("after alice's browser deleted its block the ask's row names %s of %s, want q-doc of the decisions document", blockID, document)
	}

	result := issue.restoreWith(work, false)
	if len(result.Restored) != 0 || result.SpecVersion != 0 ||
		len(result.Declined) != 1 || result.Declined[0] != (DeclinedAsk{Ask: deleted, Person: "alice", Version: version}) {
		t.Fatalf("the restore wrote %+v at spec version %d and declined %+v, want the ask %s (q-doc), which alice deleted from the decisions document since the finder read it, written nowhere and declined to alice at version %d of that document",
			result.Restored, result.SpecVersion, result.Declined, deleted, version)
	}
	if spec := issue.latest(issue.spec); strings.Contains(spec, "#q-doc ") {
		t.Errorf("the spec holds a block of the ask %s, which alice deleted:\n%s", deleted, spec)
	}
	if held := heldRemovalsOf(t, issue, deleted); len(held) != 1 || held[0].Seq != removals[0].Seq {
		t.Errorf("after the restore the ask %s holds the removals %+v, want the one alice's settlement stated, %+v", deleted, held, removals[0])
	}
	if blockID, document := issue.askRow(deleted); blockID != "q-doc" || document != decisions {
		t.Errorf("after the restore the ask's row names %s of %s, want q-doc of the decisions document %s", blockID, document, decisions)
	}
	from := issue.refKey(decisions) + "#q-doc"
	if to, moved := issue.movedTo(decisions)[from]; moved {
		t.Errorf("the restore stated a move of the ask's block from %s to %s, want none", from, to)
	}
}

// A person's browser deletion of an answered ask's block from the decisions document is that
// person's removal whether or not its settlement has run when the decisions step reads the
// document. On a fleet of this build, alice's browser deletes q-doc from the standing
// decisions.md and the room's settlement has not run yet (the settle delay runs from her last
// update), so no version of the document records her removal; the step's pass then reads the
// finder's work, which takes q-doc for an ask of the second shape, and restores. The pass writes
// no block of q-doc into the spec and writes the ask nowhere, as it does once that settlement has
// run (TestTheRestoreDeclinesAnAnsweredAskAPersonDeletedFromTheDecisionsDocumentSinceItWasRead):
// the room the pass could read holds no block of q-doc, and the step retires the document, so
// her deletion would reach no store at all.
func TestTheRestoreWritesBackNoAnsweredAskAPersonsBrowserDeletedFromTheDecisionsDocumentBeforeItSettled(t *testing.T) {
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe corpus is written once.\n:::\n")
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions",
		"# Decisions\n\n## Which line names a moved block\n\n:::ask{#q-doc urgency=\"low\" multiple=\"false\"}\nWhich line names a block that moved?\n:::\n")
	deleted := issue.answer(decisions, "q-doc", "The census names each move.")
	before := latestVersionNumber(t, issue.service, decisions)
	browserReplaceText(t, issue.service, decisions, "# Decisions\n\n## Which line names a moved block\n\nThe census names each move.\n",
		model.Actor{Kind: "user", ID: "alice"})
	if version := latestVersionNumber(t, issue.service, decisions); version != before {
		t.Fatalf("the decisions document stands at version %d after alice's browser deletion, want %d: its settlement has not run", version, before)
	}
	work, err := graphcensus.ReadRestoreWork(context.Background(), issue.database.Pool)
	if err != nil {
		t.Fatalf("read the restore's work: %v", err)
	}

	result := issue.restoreWith(work, false)
	for _, restored := range result.Restored {
		if restored.Ask == deleted {
			t.Errorf("the restore wrote %+v, the ask %s (q-doc), which alice's browser deleted from the decisions document before its settlement ran, want it written nowhere", restored, deleted)
		}
	}
	if spec := issue.latest(issue.spec); strings.Contains(spec, "#q-doc ") {
		t.Errorf("the spec holds a block of the ask %s, which alice's browser deleted from the decisions document:\n%s", deleted, spec)
	}
}

// A conversion that retires the decisions document first writes the version that states a
// person's browser deletion no settlement has versioned yet. Alice's browser deletes q-doc, the
// document's last ask block and all it held but its title, and the room's settlement has not run.
// The pass converts: the document then holds no section and no ask block, so the pass retires it.
// Before the retirement it writes a version of the document that holds no block of q-doc, and
// that version states her removal: the ask's node holds one removed_by, to alice, under that
// version's event. The result declines the ask to alice at that version, the ask's row keeps
// naming the block it left, and the spec holds no block of it.
func TestAConversionThatRetiresTheDecisionsDocumentFirstVersionsAPersonsUnsettledDeletionOfItsLastAsk(t *testing.T) {
	ctx := context.Background()
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe corpus is written once.\n:::\n")
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions",
		"# Decisions\n\n:::ask{#q-doc urgency=\"low\" multiple=\"false\"}\nWhich line names a block that moved?\n:::\n")
	deleted := issue.answer(decisions, "q-doc", "The census names each move.")
	before := latestVersionNumber(t, issue.service, decisions)
	browserReplaceText(t, issue.service, decisions, "# Decisions\n", model.Actor{Kind: "user", ID: "alice"})
	if version := latestVersionNumber(t, issue.service, decisions); version != before {
		t.Fatalf("the decisions document stands at version %d after alice's browser deletion, want %d: its settlement has not run", version, before)
	}
	if removals := heldRemovalsOf(t, issue, deleted); len(removals) != 0 {
		t.Fatalf("before the pass the ask %s holds the removals %+v, want none: no version records alice's deletion", deleted, removals)
	}
	work, err := graphcensus.ReadRestoreWork(ctx, issue.database.Pool)
	if err != nil {
		t.Fatalf("read the restore's work: %v", err)
	}

	result := issue.restoreWith(work, true)
	if !result.Retired || result.DecisionsVersion <= before {
		t.Fatalf("the conversion retired the decisions document %t and wrote its version %d, want it retired after a version above %d", result.Retired, result.DecisionsVersion, before)
	}
	if len(result.Restored) != 0 || len(result.Declined) != 1 ||
		result.Declined[0] != (DeclinedAsk{Ask: deleted, Person: "alice", Version: result.DecisionsVersion}) {
		t.Fatalf("the conversion wrote %+v and declined %+v, want the ask %s (q-doc) written nowhere and declined to alice at version %d of the decisions document",
			result.Restored, result.Declined, deleted, result.DecisionsVersion)
	}
	written := versionMarkdown(t, issue.database, decisions, result.DecisionsVersion)
	if strings.Contains(written, "#q-doc ") {
		t.Errorf("version %d of the decisions document holds a block of q-doc, which alice's browser deleted:\n%s", result.DecisionsVersion, written)
	}
	var event int64
	if err := issue.database.Pool.QueryRow(ctx, `
		select id from events
		where type = 'artifact.version' and payload->>'artifact_id' = $1 and (payload->'version'->>'number')::int = $2
	`, decisions, result.DecisionsVersion).Scan(&event); err != nil {
		t.Fatalf("read the event of version %d of the decisions document: %v", result.DecisionsVersion, err)
	}
	if held := heldRemovalsOf(t, issue, deleted); len(held) != 1 || !held[0].Asserted || held[0].ToID != "alice" || held[0].SourceEvent != event {
		t.Errorf("after the conversion the ask %s holds the removals %+v, want one asserted to alice under the event %d of version %d of the decisions document",
			deleted, held, event, result.DecisionsVersion)
	}
	if blockID, document := issue.askRow(deleted); blockID != "q-doc" || document != decisions {
		t.Errorf("after the conversion the ask's row names %s of %s, want q-doc of the decisions document %s", blockID, document, decisions)
	}
	if spec := issue.latest(issue.spec); strings.Contains(spec, "#q-doc ") {
		t.Errorf("the spec holds a block of the ask %s, which alice's browser deleted from the decisions document:\n%s", deleted, spec)
	}
}

// openerOf is the opening line of the ask block id that markdown holds, "" where it holds none.
func openerOf(markdown, id string) string {
	start := strings.Index(markdown, "ask{#"+id+" ")
	if start < 0 {
		return ""
	}
	end := strings.Index(markdown[start:], "\n")
	if end < 0 {
		return markdown[start:]
	}
	return markdown[start : start+end]
}

// approveLatest records alice's approval of document's newest version, and answers its number.
func (issue *restoreIssue) approveLatest(document string) int {
	issue.t.Helper()
	approved := latestVersionNumber(issue.t, issue.service, document)
	approveStoredVersion(issue.t, issue.database, document, approved, "alice")
	return approved
}

// The restore writes back the ask block it moves byte for byte (R7): an answered ask whose block
// a person approved with a link and a code span in its question and a sub-list under an option,
// which an agent then removed from the spec, comes back holding every one of them, and the
// restore's version reads approved, since it changed where the block stands and not its bytes.
// Where an agent changed the block's link after the approval and then removed the block, the
// restore writes the block back as the spec last held it: its words are the approved ones and
// its bytes are not, so the restore supersedes the approval and reports it with the ask.
func TestTheRestoreWritesBackTheBytesOfTheAskBlockAPersonApproved(t *testing.T) {
	const head = "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n"
	const approvedLink = "https://example.com/archive"
	block := func(link string) string {
		return ":::ask{#q-bytes urgency=\"med\" multiple=\"false\"}\nWhich store holds the corpus, [the archive](" + link + ") or `pg_dump` files?\n\n" +
			"- Archive: The cold tier.\n  - Its restores take a day.\n- Files: A nightly dump.\n:::\n"
	}
	for _, test := range []struct {
		name string
		// link is the block's link as the agent left it before removing the block.
		link     string
		approval string
	}{
		{"a block unchanged since the approval", approvedLink, "carried"},
		{"a block whose link an agent changed since the approval", "https://example.com/elsewhere", "superseded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := newRestoreIssue(t, head+block(approvedLink))
			ask := blockAskOn(t, issue.service, issue.spec, "q-bytes")
			alice := model.Actor{Kind: "user", ID: "alice"}
			at := time.Now().UTC()
			change := asks.Answered(model.AskAnswer{User: "alice", Selected: []string{"Archive"}, At: at})
			change.Who, change.At = alice, at
			settleAskAs(t, issue.service, issue.spec, "q-bytes", ask.ID, alice, change)
			approved := issue.approveLatest(issue.spec)
			if test.link != approvedLink {
				issue.replace(issue.spec, head+block(test.link))
			}
			issue.replace(issue.spec, head)

			result := issue.run(false)
			if len(result.Restored) != 1 || result.SpecApproval != test.approval {
				t.Fatalf("the restore did %+v, want the ask restored and the approval of version %d %s", result, approved, test.approval)
			}
			spec := issue.latest(issue.spec)
			heading := strings.Index(spec, "## Decisions\n")
			if heading < 0 {
				t.Fatalf("the restored spec holds no Decisions part:\n%s", spec)
			}
			part := spec[heading:]
			for _, want := range []string{"[the archive](" + test.link + ")", "`pg_dump` files", "Its restores take a day."} {
				if !strings.Contains(part, want) {
					t.Errorf("the restored block holds no %q, which the spec last held:\n%s", want, part)
				}
			}
			if test.approval == "superseded" {
				reported := false
				for _, superseded := range result.Superseded {
					reported = reported || superseded.Approval.ApprovedVersion == approved && slices.Contains(superseded.Asks, "dispatch://DOC-1/ask/"+ask.ID)
				}
				if !reported {
					t.Errorf("the restore reported %+v, want the approval of version %d superseded with the ask %s", result.Superseded, approved, ask.ID)
				}
			}
		})
	}
}

// The restore weighs a restored ask block's `shaped` pointers as the person approved them, and
// leaves out of its comparison only what the step's own write did to them (restoredShape). An
// answered ask a person approved with no pointer, which an agent then gave a `shaped` pointer and
// later removed from the spec in a version no person wrote, comes back holding that pointer, which
// the approved block did not hold: the restore supersedes the approval and reports it with the
// ask. Where the block held the pointer when the person approved it, and where the step's own
// write alone gives the block its pointer - the link of the decisions section that restates its
// ask - the approval carries.
func TestTheRestoreSupersedesAnApprovalOverAShapedPointerAnAgentGaveTheBlock(t *testing.T) {
	const head = "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n"
	const pointer = "dispatch://DOC-1/spec#b-r1"
	tied := ` shaped="` + pointer + `"`
	block := func(shaped string) string {
		return ":::ask{#q-tie urgency=\"med\" multiple=\"false\"" + shaped + "}\nWhich store holds the corpus?\n:::\n"
	}
	for _, test := range []struct {
		name string
		// approved and given are the block's pointer as the person approved it and as an agent
		// left it before it removed the block; restated is whether the decisions document holds a
		// section that restates the ask and links r1, which the conversion writes into its block.
		approved, given string
		restated        bool
		approval        string
	}{
		{"a pointer an agent gave the block after the approval", "", tied, false, "superseded"},
		{"a pointer the block held when the person approved it", tied, tied, false, "carried"},
		{"a pointer the step's own write gives the block from the section that restates its ask", "", "", true, "carried"},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := newRestoreIssue(t, head+block(test.approved))
			ask := issue.answer(issue.spec, "q-tie", "The database we run.")
			approved := issue.approveLatest(issue.spec)
			if test.given != test.approved {
				issue.replace(issue.spec, head+block(test.given))
			}
			issue.replace(issue.spec, head)
			if test.restated {
				issue.document("decisions-md", "decisions.md", false, "decisions", "# Decisions\n\n"+
					"## The store holds the corpus\n\nThe database we run holds every word of it. This shapes "+pointer+". "+
					"Decided in dispatch://DOC-1/ask/"+ask+".\n")
			}

			result := issue.run(test.restated)
			if len(result.Restored) != 1 || result.SpecApproval != test.approval {
				t.Fatalf("the restore did %+v, want the ask restored and the approval of version %d %s", result, approved, test.approval)
			}
			if opener := openerOf(issue.latest(issue.spec), "q-tie"); !strings.Contains(opener, tied) {
				t.Fatalf("the restored block of q-tie opens %q, want it holding%s", opener, tied)
			}
			if test.approval == "superseded" {
				reported := false
				for _, superseded := range result.Superseded {
					reported = reported || superseded.Approval.ApprovedVersion == approved && slices.Contains(superseded.Asks, "dispatch://DOC-1/ask/"+ask)
				}
				if !reported {
					t.Errorf("the restore reported %+v, want the approval of version %d superseded with the ask %s", result.Superseded, approved, ask)
				}
			}
		})
	}
}

// An ask block moved out of a standing decisions document keeps its `shaped` ties (R6: "its
// `shaped` tuples stay intact"): the block of q-a, which shaped the spec's r1 and the decisions
// document's q-b, moves into the spec with q-b, and holds a pointer to r1 and one to q-b where q-b
// now stands, the spec; its ask's node states both ties, and none to the decisions document.
func TestAnAskBlockMovedOutOfTheDecisionsDocumentKeepsItsShapedTies(t *testing.T) {
	ctx := context.Background()
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n")
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions", "# Decisions\n\n"+
		"## Which store holds the corpus\n\n:::ask{#q-a urgency=\"med\" multiple=\"false\" "+
		"shaped=\"dispatch://DOC-1/spec#b-r1 dispatch://DOC-1/artifact/decisions-md#b-q-b\"}\nWhich store holds the corpus?\n:::\n\n"+
		"## Which reader reads it first\n\n:::ask{#q-b urgency=\"low\" multiple=\"false\"}\nWhich reader reads it first?\n:::\n")
	askA := blockAskOn(t, issue.service, decisions, "q-a").ID
	if ties, err := graphstore.CurrentOut(ctx, issue.database.Pool, graphstore.KindAsk, askA, []string{refs.TupleShaped}); err != nil || len(ties) != 2 {
		t.Fatalf("before the restore q-a's node holds the shaped ties %+v (%v), want two", ties, err)
	}

	result := issue.run(false)
	if len(result.Restored) != 2 {
		t.Fatalf("the restore did %+v, want both ask blocks moved", result.Restored)
	}
	const want = `shaped="dispatch://DOC-1/spec#b-r1 dispatch://DOC-1/spec#b-q-b"`
	if opener := openerOf(issue.latest(issue.spec), "q-a"); !strings.Contains(opener, want) {
		t.Fatalf("the restored block of q-a opens %q, want it holding %s", opener, want)
	}
	ties, err := graphstore.CurrentOut(ctx, issue.database.Pool, graphstore.KindAsk, askA, []string{refs.TupleShaped})
	if err != nil {
		t.Fatalf("read q-a's shaped ties: %v", err)
	}
	held := map[string]bool{}
	for _, tie := range ties {
		if tie.Asserted {
			held[tie.ToID] = true
		}
	}
	spec := issue.refKey(issue.spec)
	if len(held) != 2 || !held[refs.BlockAddress(spec, "r1")] || !held[refs.BlockAddress(spec, "q-b")] {
		t.Fatalf("after the restore q-a's node holds the shaped ties %+v, want one to %s and one to %s", ties,
			refs.BlockAddress(spec, "r1"), refs.BlockAddress(spec, "q-b"))
	}
}

// An ask block of the decisions document that no row names and is a copy of an ask another
// document of the issue holds moves into the spec, on a fleet of one build, holding the form a
// version keeps of the ask it copies (keptAskState), as a version of the spec stores a copy
// (storableVersionTree): alice answers the ask of notes.md, whose copy the decisions document's
// settlement shows answered and names as copied, and the step's version of the spec opens the
// moved copy with that state, answerer and time and its copied_from and copied_from_document, so
// the release before's CLI reads it decided, and holds none of the answer's words.
func TestAnAskBlockNoRowNamesThatIsACopyMovesIntoTheSpecKeepingWhatAVersionKeepsOfItsSource(t *testing.T) {
	const shelf = ":::ask{#q-shelf urgency=\"low\" multiple=\"false\"}\nWhich shelf holds the printed corpus?\n:::\n"
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n")
	notes := issue.document("notes-md", "notes.md", false, "", "# Notes\n\n"+shelf)
	source := issue.answer(notes, "q-shelf", "The top shelf.")
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions",
		"# Decisions\n\n## Which shelf holds the printed corpus\n\n"+shelf+"\nWe lean towards the top shelf until someone rules.\n")
	kept := []string{`state="answered"`, `answered_by="alice"`, `answered_at="`, `copied_from="` + source + `"`,
		`copied_from_document="` + documentAddress(t, issue.service, notes) + `"`}
	copied := openerOf(residentRoomMarkdown(t, issue.service, decisions), "q-shelf")
	for _, want := range kept {
		if !strings.Contains(copied, want) {
			t.Fatalf("the decisions document's copy of q-shelf opens %q, want it holding %s", copied, want)
		}
	}

	result := issue.run(true)
	from, to := issue.refKey(decisions)+"#q-shelf", issue.refKey(issue.spec)+"#q-shelf"
	if len(result.Restored) != 1 || result.Restored[0].Ask != "" || result.Restored[0].From != from || result.Restored[0].To != to ||
		result.SpecVersion == 0 || latestVersionNumber(t, issue.service, issue.spec) != result.SpecVersion {
		t.Fatalf("the step did %+v, want the block no row names moved from %s to %s in the spec's newest version", result, from, to)
	}
	opener := openerOf(issue.latest(issue.spec), "q-shelf")
	for _, want := range kept {
		if !strings.Contains(opener, want) {
			t.Errorf("the step's version %d of the spec opens the moved copy %q, want it holding %s", result.SpecVersion, opener, want)
		}
	}
	for _, name := range answerWords {
		if strings.Contains(opener, " "+name+"=") {
			t.Errorf("the step's version %d of the spec opens the moved copy %q, want none of the answer's words, %s among them", result.SpecVersion, opener, name)
		}
	}
}

// A copy that moves takes into the spec the state its source holds when the step runs, not the
// state the copy showed at its own last settlement. The decisions document's copy of the ask of
// notes.md settles while that ask is open. Then alice answers the ask as the answer route does
// (answerArmingCopies): the copy's settlement again is armed, and under the issue's settle delay of
// an hour it has not run when the step runs, so the copy still shows the ask open. The step's
// version of the spec opens the moved copy answered, by alice, at the time of her answer, with its
// copied_from and copied_from_document.
func TestAMovedCopyTakesItsSourcesStateWhereTheCopysOwnSettlementHasNotRun(t *testing.T) {
	const shelf = ":::ask{#q-shelf urgency=\"low\" multiple=\"false\"}\nWhich shelf holds the printed corpus?\n:::\n"
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n")
	notes := issue.document("notes-md", "notes.md", false, "", "# Notes\n\n"+shelf)
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions",
		"# Decisions\n\n## Which shelf holds the printed corpus\n\n"+shelf+"\nWe lean towards the top shelf until someone rules.\n")
	source, answeredAt := issue.answerArmingCopies(notes, "q-shelf", "The top shelf.")
	if copied := openerOf(residentRoomMarkdown(t, issue.service, decisions), "q-shelf"); !strings.Contains(copied, `copied_from="`+source+`"`) ||
		strings.Contains(copied, "state=") || strings.Contains(copied, "answered_by=") {
		t.Fatalf("after the answer the decisions document's copy of q-shelf opens %q, want it naming its source and showing it open, as its last settlement left it", copied)
	}

	result := issue.run(true)
	if len(result.Restored) != 1 || result.Restored[0].Ask != "" || result.SpecVersion == 0 {
		t.Fatalf("the step did %+v, want the copy no row names moved into a version of the spec", result)
	}
	opener := openerOf(issue.latest(issue.spec), "q-shelf")
	for _, want := range []string{`state="answered"`, `answered_by="alice"`, `answered_at="` + asks.AnsweredAtValue(answeredAt) + `"`,
		`copied_from="` + source + `"`, `copied_from_document="` + documentAddress(t, issue.service, notes) + `"`} {
		if !strings.Contains(opener, want) {
			t.Errorf("the step's version %d of the spec opens the moved copy %q, want it holding %s", result.SpecVersion, opener, want)
		}
	}
}

// answerArmingCopies has alice answer the ask of block blockID of document in text as the answer
// route does: on the ask's node and row (asks.Settle), with the settlement of each copy of it
// marked owed and armed for when the write commits (SettleCopiesOf), and into the ask's own block
// (KeepAskBlockInStep). It answers the ask's id and the time of the answer.
func (issue *restoreIssue) answerArmingCopies(document, blockID, text string) (string, time.Time) {
	t := issue.t
	t.Helper()
	ask := blockAskOn(t, issue.service, document, blockID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	at := time.Now().UTC()
	change := asks.Answered(model.AskAnswer{User: "alice", Selected: []string{}, Text: &text, At: at})
	change.Who, change.At = alice, at
	if err := joinedWrite(issue.service, func(ctx context.Context) error {
		tx, _ := txFromContext(ctx)
		answered, err := asks.Settle(ctx, tx, ask.ID, change)
		if err != nil {
			return err
		}
		answered.BlockID, answered.BlockArtifactID = &blockID, &document
		if err := issue.service.SettleCopiesOf(ctx, answered); err != nil {
			return err
		}
		return issue.service.KeepAskBlockInStep(ctx, answered, alice)
	}); err != nil {
		t.Fatalf("answer the ask of block %s: %v", blockID, err)
	}
	return ask.ID, at
}

// The restore takes out of the spec's `## Decisions` part only its pointer line to the decisions
// document, a paragraph whose one content is the citation of that document (isPointerLine). A
// person's paragraph that cites the document among other words is the person's prose, which the
// restore keeps wherever it stands in the part: before the pointer line, the ask takes the
// pointer line's place after it; with no pointer line, the ask stands after the part's last
// block. So is a paragraph that names the decisions and says they are in the document with words
// of its own between, which no pointer line's lead holds (pointerLineLead). Each restore changed
// nothing else, so the approval carries, and no carry takes the person's paragraph out of the
// version they approved.
func TestTheRestoreKeepsAPersonsParagraphCitingTheDecisionsDocumentAmongOtherWords(t *testing.T) {
	const pointer = "The decisions this spec rests on are in dispatch://DOC-1/artifact/decisions-md."
	const prose = "We chose the base release because backups are cheap; the trade-offs are in dispatch://DOC-1/artifact/decisions-md."
	const qualified = "The decisions the board rejected twice are in dispatch://DOC-1/artifact/decisions-md."
	const head = "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n"
	const block = ":::ask{#q1 urgency=\"med\" multiple=\"false\"}\nWhich build writes the corpus?\n:::\n\n"
	for _, test := range []struct {
		name, prose, part string
	}{
		{"before the pointer line", prose, prose + "\n\n" + pointer + "\n"},
		{"with no pointer line", prose, prose + "\n"},
		{"naming the decisions with words of its own before `are in`, with no pointer line", qualified, qualified + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := newRestoreIssue(t, head+block+"## Decisions\n\n"+test.part)
			issue.answer(issue.spec, "q1", "The base release.")
			approved := issue.approveLatest(issue.spec)
			issue.replace(issue.spec, head+"## Decisions\n\n"+test.part)
			issue.document("decisions-md", "decisions.md", false, "decisions", "# Decisions\n\n## The base release writes the corpus\n\nIts corpus is the one we measured.\n")

			result := issue.run(false)
			if len(result.Restored) != 1 || result.SpecApproval != "carried" {
				t.Fatalf("the restore did %+v, want the ask restored and the approval of version %d carried", result, approved)
			}
			spec := issue.latest(issue.spec)
			part := spec[strings.Index(spec, "## Decisions"):]
			if !strings.Contains(part, test.prose) {
				t.Fatalf("the restored spec's Decisions part reads\n%s\nwant the person's paragraph citing the decisions document kept", part)
			}
			if strings.Contains(spec, pointer) || strings.Index(part, "#q1 urgency=") < strings.Index(part, test.prose) {
				t.Errorf("the restored spec's Decisions part reads\n%s\nwant the ask after the person's paragraph, in place of any pointer line", part)
			}
		})
	}
}

// The `## Decisions` heading is no change to what a person approved where the decisions step made
// it, and an empty heading the person approved is theirs. A spec approved with no Decisions part,
// whose answered ask an agent then removed, reads approved at the restore's version, which returns
// the ask under a heading it adds. A spec approved with an empty Decisions heading, whose answered
// ask a decisions section restates, reads approved at the conversion's version, which writes the
// section's links and words into the ask and keeps the heading; where an agent deleted that empty
// heading after the approval, the conversion's version, which holds no heading, supersedes it.
func TestTheStepCarriesAnApprovalAcrossOnlyTheDecisionsHeadingItMade(t *testing.T) {
	const head = "# Spec\n\n:::requirement{#r1}\nThe corpus is written once.\n:::\n\n"
	const block = ":::ask{#q-h urgency=\"med\" multiple=\"false\"}\nWhich build writes the corpus?\n:::\n"
	t.Run("a spec approved with no Decisions part, its ask returned under the heading the restore adds", func(t *testing.T) {
		issue := newRestoreIssue(t, head+block)
		issue.answer(issue.spec, "q-h", "The base release.")
		approved := issue.approveLatest(issue.spec)
		issue.replace(issue.spec, head)
		result := issue.run(false)
		if len(result.Restored) != 1 || result.SpecApproval != "carried" {
			t.Fatalf("the restore did %+v, want the ask restored and the approval of version %d carried", result, approved)
		}
		if spec := issue.latest(issue.spec); strings.Count(spec, "## Decisions\n") != 1 || !strings.Contains(spec[strings.Index(spec, "## Decisions\n"):], "#q-h urgency=") {
			t.Fatalf("the restored spec holds no one Decisions part with the ask under it:\n%s", spec)
		}
	})
	for _, test := range []struct {
		name, written, approval string
	}{
		{"a spec approved with an empty Decisions heading, which the conversion's version keeps", head + block + "\n## Decisions\n", "carried"},
		{"a spec approved with an empty Decisions heading an agent deleted, which the conversion's version lacks", head + block, "superseded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := newRestoreIssue(t, head+block+"\n## Decisions\n")
			ask := issue.answer(issue.spec, "q-h", "The base release.")
			approved := issue.approveLatest(issue.spec)
			if test.written != head+block+"\n## Decisions\n" {
				issue.replace(issue.spec, test.written)
			}
			issue.document("decisions-md", "decisions.md", false, "decisions", "# Decisions\n\n"+
				"## The base release writes the corpus\n\nIts corpus is the one we measured. This shapes dispatch://DOC-1/spec#b-r1. "+
				"Decided in dispatch://DOC-1/ask/"+ask+".\n")

			result := issue.run(true)
			if result.Readings != 1 || result.SpecVersion == 0 || result.SpecApproval != test.approval {
				t.Fatalf("the conversion did %+v, want one reading written into the spec and the approval of version %d %s", result, approved, test.approval)
			}
			spec := issue.latest(issue.spec)
			if opener := openerOf(spec, "q-h"); !strings.Contains(opener, `shaped="dispatch://DOC-1/spec#b-r1"`) {
				t.Fatalf("the converted spec's ask opens %q, want the section's link as its shaped pointer:\n%s", opener, spec)
			}
			if held := strings.Contains(spec, "## Decisions"); held != (test.approval == "carried") {
				t.Fatalf("the converted spec holds a Decisions heading %t:\n%s", held, spec)
			}
		})
	}
}
