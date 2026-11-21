package docs

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
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

// restoreIssue is issue DOC-1 of a test database, its spec and the documents the decisions step
// writes, each written through the document service and settled as the release before left it.
type restoreIssue struct {
	t        *testing.T
	database *store.Store
	service  *Service
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
	issue := &restoreIssue{t: t, database: database, service: service}
	issue.spec = issue.document("spec", "spec.md", true, "", markdown)
	return issue
}

// document creates a document of DOC-1 holding markdown as its version 1, written by alice and
// indexed as an upload of a new document is, and settles it, so each ask block holds its ask.
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
		values ('DOC-1', 'DOC', $1, $2, 'doc', $3, $4, '{"kind":"user","id":"alice"}')
		returning id::text
	`, slug, name, primary, rolePointer).Scan(&id); err != nil {
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
	done, failed, err := step.RestoreAndConvert(context.Background(), "DOC", convert, false)
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
	var refKey string
	if err := issue.database.Pool.QueryRow(context.Background(), `select ref_key from artifacts where id = $1`, document).Scan(&refKey); err != nil {
		issue.t.Fatalf("read the ref_key of %s: %v", document, err)
	}
	return refKey
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
	var authored int
	if err := issue.database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples
		where kind = $1 and from_kind = $2 and starts_with(from_id, $3) and to_kind = $4 and to_id = 'alice' and asserted
	`, graphstore.TupleAuthoredBy, refs.BlockNodeKind, issue.refKey(issue.spec)+"#", graphstore.KindPerson).Scan(&authored); err != nil || authored != 1 {
		t.Errorf("the spec's readings hold %d authored_by tuples to alice (%v), want the one reading's: the author of its section's newest version", authored, err)
	}
	if work, err := graphcensus.ReadRestoreWork(context.Background(), issue.database.Pool); err != nil || len(work) != 0 {
		t.Errorf("the restore leaves work %+v (%v), want none", work, err)
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
// the section's words.
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

	restored := issue.run(false)
	if len(restored.Restored) != 1 || !restored.Restored[0].NewID {
		t.Fatalf("the restore did %+v, want the ask restored under a fresh block id", restored)
	}
	block, _ := issue.askRow(left)
	if spec := issue.latest(issue.spec); strings.Index(spec, "#q-first ") > strings.Index(spec, "#"+block+" ") {
		t.Fatalf("the restored ask block %s does not stand after the spec's first ask block:\n%s", block, spec)
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
// once the issue is locked: a person who took the block out of the document since leaves the
// ask's row naming it, and the restore writes the ask into the spec, its row, one moved_to from
// the old address to the new one, and its node's relations, which name the spec.
func TestTheRestoreMovesAnAskWhoseBlockLeftTheDecisionsDocumentSinceItWasRead(t *testing.T) {
	issue := newRestoreIssue(t, "# Spec\n\n:::requirement{#r1}\nThe corpus is written once.\n:::\n")
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions",
		"# Decisions\n\n## Which line names a moved block\n\n:::ask{#q-doc urgency=\"low\" multiple=\"false\"}\nWhich line names a block that moved?\n:::\n")
	moved := issue.answer(decisions, "q-doc", "The census names each move.")
	work, err := graphcensus.ReadRestoreWork(context.Background(), issue.database.Pool)
	if err != nil || len(work) != 1 || work[0].Shape != graphcensus.RestoreMoves || work[0].Ask != moved {
		t.Fatalf("the finder reads %+v (%v), want the moving ask %s", work, err, moved)
	}
	issue.replaceAsPerson(decisions, "# Decisions\n\n## Which line names a moved block\n\nThe census names each move.\n")
	if blockID, document := issue.askRow(moved); blockID != "q-doc" || document != decisions {
		t.Fatalf("after the person took the block out the ask's row names %s of %s, want q-doc of the decisions document", blockID, document)
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

// The restore takes out of the spec's `## Decisions` part only its pointer line to the decisions
// document, a paragraph whose one content is the citation of that document (isPointerLine). A
// person's paragraph that cites the document among other words is the person's prose, which the
// restore keeps wherever it stands in the part: before the pointer line, the ask takes the
// pointer line's place after it; with no pointer line, the ask stands after the part's last
// block. Each restore changed nothing else, so the approval carries, and no carry takes the
// person's paragraph out of the version they approved.
func TestTheRestoreKeepsAPersonsParagraphCitingTheDecisionsDocumentAmongOtherWords(t *testing.T) {
	const pointer = "The decisions this spec rests on are in dispatch://DOC-1/artifact/decisions-md."
	const prose = "We chose the base release because backups are cheap; the trade-offs are in dispatch://DOC-1/artifact/decisions-md."
	const head = "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n"
	const block = ":::ask{#q1 urgency=\"med\" multiple=\"false\"}\nWhich build writes the corpus?\n:::\n\n"
	for _, test := range []struct {
		name, part string
	}{
		{"before the pointer line", prose + "\n\n" + pointer + "\n"},
		{"with no pointer line", prose + "\n"},
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
			if !strings.Contains(part, prose) {
				t.Fatalf("the restored spec's Decisions part reads\n%s\nwant the person's paragraph citing the decisions document kept", part)
			}
			if strings.Contains(spec, pointer) || strings.Index(part, "#q1 urgency=") < strings.Index(part, prose) {
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
