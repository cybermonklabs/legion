package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/wordrecords"
)

// The fixtures below are written for these tests: every text is invented, and the owner and the
// sessions are stand-ins.
const (
	fixtureOwner  = "owner@example.com"
	fixtureNaming = "NAME-1"
	fixtureSeed   = 3231
	fixtureEnd    = "2026-11-01T00:00:00Z"
)

var (
	fixtureAgent  = map[string]any{"kind": "session", "id": "01a1aaaa-0000-7000-8000-000000000001"}
	fixtureOther  = map[string]any{"kind": "session", "id": "01a1aaaa-0000-7000-8000-000000000002"}
	fixturePerson = map[string]any{"kind": "user", "id": fixtureOwner}
)

// dsFixture builds a snapshot of hand-written events and the verdicts of its owner texts.
type dsFixture struct {
	events   []map[string]any
	versions []snapLine
	labels   []snapLine
	verdicts map[string]string
	next     int64
}

func newDSFixture() *dsFixture { return &dsFixture{verdicts: map[string]string{}, next: 100} }

// event adds one event and answers its id.
func (f *dsFixture) event(typ string, actor map[string]any, issue string, payload map[string]any) int64 {
	f.next++
	project := ""
	if issue != "" {
		project = strings.SplitN(issue, "-", 2)[0]
	}
	ev := map[string]any{
		"id": f.next, "type": typ, "project": project, "actor": actor, "payload": payload,
		"created_at": time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC).Add(time.Duration(f.next) * time.Minute).Format(time.RFC3339Nano),
	}
	if issue != "" {
		ev["issue_key"] = issue
	}
	f.events = append(f.events, ev)
	return f.next
}

// issue opens an issue with its primary document, whose first version is markdown.
func (f *dsFixture) issue(key, parent, title, markdown string) {
	payload := map[string]any{"key": key, "project": strings.SplitN(key, "-", 2)[0], "title": title, "parent": nil, "primary_artifact_id": "art-" + key}
	if parent != "" {
		payload["parent"] = parent
	}
	f.event("issue.created", fixtureAgent, key, payload)
	text := markdown
	f.versions = append(f.versions, snapLine{Kind: "version", ArtifactID: "art-" + key, Number: 1, Text: &text})
}

// comment adds a comment; reply names the comment it answers. It answers the comment's id.
func (f *dsFixture) comment(id, issue string, actor map[string]any, body, reply string) string {
	payload := map[string]any{"id": id, "body": body, "issue_key": issue}
	if reply != "" {
		payload["reply_to"], payload["answers"] = reply, reply
	}
	f.event("comment.created", actor, issue, payload)
	return id
}

// owner adds an owner comment with its verdict.
func (f *dsFixture) owner(id, issue, body, reply, verdict string) {
	f.comment(id, issue, fixturePerson, body, reply)
	f.verdicts["comment:"+id+"@"+fmt.Sprint(f.next)] = verdict
}

func (f *dsFixture) write(t *testing.T, dir string) (snapshot, verdicts string) {
	t.Helper()
	snapshot, verdicts = filepath.Join(dir, "snapshot.jsonl.gz"), filepath.Join(dir, "verdicts.jsonl")
	var buf bytes.Buffer
	gz := dsGzip(&buf)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(dsJSONLine(gz, snapLine{Kind: "header", Format: dsFormat, End: fixtureEnd, Owner: fixtureOwner,
		NamingIssue: fixtureNaming, Events: len(f.events), Versions: len(f.versions), Labels: len(f.labels)}))
	for _, ev := range f.events {
		raw, err := json.Marshal(ev)
		must(err)
		must(dsJSONLine(gz, snapLine{Kind: "event", Event: raw}))
	}
	for _, v := range f.versions {
		must(dsJSONLine(gz, v))
	}
	for _, l := range f.labels {
		must(dsJSONLine(gz, l))
	}
	must(gz.Close())
	must(os.WriteFile(snapshot, buf.Bytes(), 0o644))
	f.writeVerdicts(t, verdicts)
	return snapshot, verdicts
}

func (f *dsFixture) writeVerdicts(t *testing.T, path string) {
	t.Helper()
	var lines []dsVerdict
	for _, k := range sortedKeys(f.verdicts) {
		lines = append(lines, dsVerdict{Text: k, Verdict: f.verdicts[k], Words: []string{}, Model: "claude-standin", PromptSHA256: sha256Hex([]byte(wordrecords.SelectPrompt))})
	}
	if _, err := writeJSONL(path, lines); err != nil {
		t.Fatal(err)
	}
}

// fixtureWords is n words drawn from a vocabulary by rng, so texts of two trees share no run of
// five words.
func fixtureWords(rng *rand.Rand, n int) string {
	vocab := strings.Fields("amber basin cedar delta ember fjord granite harbor iris juniper kestrel lagoon " +
		"meadow nectar orchid pebble quartz raven saffron thistle umber velvet willow yarrow zephyr " +
		"anchor beacon canyon dune estuary falcon glacier heron inlet jasper")
	words := make([]string, n)
	for i := range words {
		words[i] = vocab[rng.IntN(len(vocab))]
	}
	return strings.Join(words, " ")
}

// standardFixture is trees enough for the split to have work to do: in each tree an agent
// comment the owner asks about, answers plainly, or asks a question that an agent answers, and
// some trees with a child issue, a document version the owner reviews, an ask he answers, a
// phrase he corrects, a labelled phrase, near copies across two trees and a link on the naming
// issue.
func standardFixture() *dsFixture {
	f := newDSFixture()
	rng := rand.New(rand.NewPCG(7, 11))
	f.issue(fixtureNaming, "", "Collect the examples", "The examples go here.")
	for i := range 40 {
		root := fmt.Sprintf("TREE-%d", i)
		child := fmt.Sprintf("TREE-%d01", i)
		f.issue(root, "", "Root "+fixtureWords(rng, 4), fixtureWords(rng, 30))
		f.issue(child, root, "Child "+fixtureWords(rng, 4), fixtureWords(rng, 30))
		agent := f.comment(fmt.Sprintf("c-%d", i), child, fixtureAgent, fixtureWords(rng, 40), "")
		switch i % 4 {
		case 0:
			f.owner(fixtureOwnerID(i), child, "What does "+fixtureWords(rng, 2)+" mean here?", agent, wordrecords.WordMeaning)
		case 1:
			f.owner(fixtureOwnerID(i), child, "Looks right, go ahead.", agent, wordrecords.None)
		case 2:
			f.owner(fixtureOwnerID(i), child, "Why did the "+fixtureWords(rng, 2)+" stop?", agent, wordrecords.AgentQuestion)
			f.comment(fmt.Sprintf("r-%d", i), child, fixtureOther, "It stopped because "+fixtureWords(rng, 20), fixtureOwnerID(i))
		case 3:
			f.owner(fixtureOwnerID(i), root, "Thanks, that is clear.", "", wordrecords.None)
			f.comment(fmt.Sprintf("p-%d", i), root, fixtureAgent, fixtureWords(rng, 25), "")
			f.owner(fmt.Sprintf("q-%d", i), root, "Fine by me.", fmt.Sprintf("p-%d", i), wordrecords.None)
		}
	}
	// Near copies: two trees whose agent texts repeat one long text with one word changed.
	long := fixtureWords(rng, 120)
	f.issue("NEAR-1", "", "Near one", "One.")
	f.issue("NEAR-2", "", "Near two", "Two.")
	f.comment("near-a", "NEAR-1", fixtureAgent, long+" alpha", "")
	f.owner("near-oa", "NEAR-1", "I cannot follow this at all.", "near-a", wordrecords.CannotUnderstand)
	f.comment("near-b", "NEAR-2", fixtureAgent, long+" omega", "")
	f.owner("near-ob", "NEAR-2", "What is this list for?", "near-b", wordrecords.WordMeaning)
	// A document version the owner asks about in his review.
	f.event("artifact.version", fixtureAgent, "TREE-101", map[string]any{"artifact_id": "art-TREE-101", "name": "spec.md", "version": map[string]any{"number": 2}})
	second := *firstVersion(f, "art-TREE-101") + "\n\nThe warm path skips the index on a hit."
	f.versions = append(f.versions, snapLine{Kind: "version", ArtifactID: "art-TREE-101", Number: 2, Text: &second})
	f.event("artifact.changes_requested", fixturePerson, "TREE-101", map[string]any{"artifact_id": "art-TREE-101", "version": 2, "reason": "What is the warm path?"})
	f.verdicts["review:"+fmt.Sprint(f.next)+"@"+fmt.Sprint(f.next)] = wordrecords.WordMeaning
	// An ask the owner answers by choosing an option only, and one he answers with words.
	f.event("ask.opened", fixtureAgent, "TREE-5", map[string]any{"id": "ask-1", "question": "Which mirror goes first?", "options": []map[string]any{{"label": "East", "description": "the east mirror"}, {"label": "West"}}})
	f.event("ask.answered", fixturePerson, "TREE-5", map[string]any{"id": "ask-1", "answer": map[string]any{"selected": []string{"East"}}})
	f.event("ask.opened", fixtureAgent, "TREE-9", map[string]any{"id": "ask-2", "question": "Do I rotate the keys now or at the window?"})
	f.event("ask.answered", fixturePerson, "TREE-9", map[string]any{"id": "ask-2", "answer": map[string]any{"text": "What is the window?"}})
	f.verdicts["answer:ask-2:"+fmt.Sprint(f.next)+"@"+fmt.Sprint(f.next)] = wordrecords.WordMeaning
	// A phrase the owner corrects, and a labelled phrase.
	f.comment("phrase-a", "TREE-13", fixtureAgent, "This names the owner of the queue.", "")
	f.owner("phrase-o", "TREE-13", `Do not write "names the" when you mean who owns it.`, "phrase-a", wordrecords.JargonComplaint)
	f.comment("phrase-b", "TREE-21", fixtureAgent, "The report shows the cost of each run.", "")
	f.owner("phrase-ok", "TREE-21", `Good, "shows the cost" is what I wanted.`, "phrase-b", wordrecords.None)
	f.comment("label-a", "TREE-17", fixtureAgent, "The table shows the gap between the two runs.", "")
	f.labels = append(f.labels, snapLine{Kind: "label", EventID: f.next, SourceID: "label-a", Family: "comment", Sets: []string{"t09-dev-sample"}, Shapes: []string{"shows"}})
	// The owner links one of his questions on the naming issue.
	f.owner("link-o", fixtureNaming, "An example: dispatch://TREE-2/comment/"+fixtureOwnerID(2), "", wordrecords.None)
	return f
}

// fixtureOwnerID is the id of tree i's owner comment, shaped as Dispatch's ids are, so a link
// can name it.
func fixtureOwnerID(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

func firstVersion(f *dsFixture, artifact string) *string {
	for _, v := range f.versions {
		if v.ArtifactID == artifact && v.Number == 1 {
			return v.Text
		}
	}
	return nil
}

// readGzipJSONL reads each line of a gzipped JSONL file into a T.
func readGzipJSONL[T any](t *testing.T, path string) []T {
	t.Helper()
	var out []T
	if err := eachGzipLine(path, func(line []byte) error {
		var v T
		if err := json.Unmarshal(line, &v); err != nil {
			return err
		}
		out = append(out, v)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func readDatasetCases(t *testing.T, dir string) []dsCase {
	t.Helper()
	return readGzipJSONL[dsCase](t, filepath.Join(dir, "cases.jsonl.gz"))
}

// The same snapshot, verdicts and seed build the same bytes, and each of the four groups and
// the owner's link are in them.
func TestDatasetBuildIsDeterministic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snapshot, verdicts := standardFixture().write(t, dir)
	in := dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed}
	first, err := buildDataset(in, filepath.Join(dir, "first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildDataset(in, filepath.Join(dir, "second"))
	if err != nil {
		t.Fatal(err)
	}
	if first.casesSHA != second.casesSHA || first.documentSHA != second.documentSHA || first.sessionsSHA != second.sessionsSHA {
		t.Fatalf("two builds of the same inputs differ: %+v and %+v", first, second)
	}
	a, _ := os.ReadFile(filepath.Join(dir, "first", "counts.json"))
	b, _ := os.ReadFile(filepath.Join(dir, "second", "counts.json"))
	if !bytes.Equal(a, b) {
		t.Fatalf("the two builds' counts differ:\n%s\n%s", a, b)
	}
	// The session list is export-sessions.sh's input: one line per writer session, cut at the
	// latest write of its cases, in UTC.
	list, _ := os.ReadFile(filepath.Join(dir, "first", "sessions.tsv"))
	lines := strings.Split(strings.TrimSuffix(string(list), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], fixtureAgent["id"].(string)+"\t") || !strings.HasPrefix(lines[1], fixtureOther["id"].(string)+"\t") {
		t.Fatalf("sessions.tsv is %q, want one line for each of the two writer sessions", list)
	}
	for _, line := range lines {
		if _, cut, _ := strings.Cut(line, "\t"); !strings.HasSuffix(cut, "Z") {
			t.Errorf("session line %q has no UTC cut time", line)
		}
	}
	cases := readDatasetCases(t, filepath.Join(dir, "first"))
	groups := map[string]int{}
	var named, document, phraseCorrection, phraseLabelled, uneditedSpec bool
	for _, c := range cases {
		groups[c.Group]++
		named = named || c.NamedByOwner && c.Group == dsGroupAnswer && c.Question != nil && c.Question.SourceID == fixtureOwnerID(2)
		document = document || c.Group == dsGroupQuestion && c.Kind == dsKindDocument && c.Agent.Text == "The warm path skips the index on a hit."
		if c.Phrase != nil {
			phraseCorrection = phraseCorrection || c.Agent.SourceID == "phrase-a" && strings.Join(c.Phrase.Shapes, ",") == "name"
			phraseLabelled = phraseLabelled || c.Agent.SourceID == "label-a" && strings.Join(c.Phrase.Sources, ",") == "labelled:t09-dev-sample"
			if c.Agent.SourceID == "phrase-b" {
				t.Errorf("an agent text the owner quoted with no complaint is phrase case %s", c.ID)
			}
		}
		if c.Record.Issue == nil || c.Record.Issue.Root == "" || c.WriteTime == "" || c.Agent.SHA256 == "" {
			t.Errorf("case %s lacks its record, its issue's root, its write time or its digest: %+v", c.ID, c)
		}
		// A record names each of its documents. Every document of the fixture is an issue's primary
		// document, and only TREE-101's has a later version, whose event names it.
		for _, d := range c.Record.Documents {
			if d.Name != "spec.md" {
				t.Errorf("case %s: document %s of its record is named %q, want spec.md", c.ID, d.ArtifactID, d.Name)
			}
			uneditedSpec = uneditedSpec || d.ArtifactID != "art-TREE-101"
		}
	}
	for _, g := range dsGroups {
		if groups[g] == 0 {
			t.Errorf("no %s cases among %v", g, groups)
		}
	}
	if groups[dsGroupPlain] != groups[dsGroupQuestion] {
		t.Errorf("%d plain cases, want as many as the %d question cases", groups[dsGroupPlain], groups[dsGroupQuestion])
	}
	if !named || !document || !phraseCorrection || !phraseLabelled || !uneditedSpec {
		t.Errorf("named answer %t, document question %t, corrected phrase %t, labelled phrase %t, a record with a spec no later version names %t; want each",
			named, document, phraseCorrection, phraseLabelled, uneditedSpec)
	}
}

// Flipping one stored verdict changes the dataset's digest: the negative control of the
// determinism above.
func TestDatasetFlippedVerdictChangesTheDigest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := standardFixture()
	snapshot, verdicts := f.write(t, dir)
	before, err := buildDataset(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed}, filepath.Join(dir, "before"))
	if err != nil {
		t.Fatal(err)
	}
	flipped := 0
	for k, v := range f.verdicts {
		if strings.HasPrefix(k, "comment:"+fixtureOwnerID(1)+"@") && v == wordrecords.None {
			f.verdicts[k] = wordrecords.JargonComplaint
			flipped++
		}
	}
	if flipped != 1 {
		t.Fatalf("flipped %d verdicts, want 1", flipped)
	}
	f.writeVerdicts(t, verdicts)
	after, err := buildDataset(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed}, filepath.Join(dir, "after"))
	if err != nil {
		t.Fatal(err)
	}
	if before.casesSHA == after.casesSHA {
		t.Fatalf("one flipped verdict left the digest at %s", before.casesSHA)
	}
	if after.counts.Groups[dsGroupQuestion].Cases != before.counts.Groups[dsGroupQuestion].Cases+1 {
		t.Errorf("question cases %d before and %d after the flip, want one more", before.counts.Groups[dsGroupQuestion].Cases, after.counts.Groups[dsGroupQuestion].Cases)
	}
}

// The split puts each issue tree whole in one part, holds out about 40% of each case group, and
// both parts hold cases.
func TestDatasetSplitKeepsEachTreeInOnePart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snapshot, verdicts := standardFixture().write(t, dir)
	res, err := buildDataset(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed}, filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.counts.NoTreeInBothParts {
		t.Fatal("the counts say a tree is in both parts")
	}
	partOf := map[string]string{}
	trees := map[string]map[string]bool{}
	for _, c := range readDatasetCases(t, filepath.Join(dir, "out")) {
		if p, ok := partOf[c.Tree]; ok && p != c.Part {
			t.Errorf("tree %s is in %s and %s", c.Tree, p, c.Part)
		}
		partOf[c.Tree] = c.Part
		if trees[c.Part] == nil {
			trees[c.Part] = map[string]bool{}
		}
		trees[c.Part][c.Tree] = true
		if !strings.HasPrefix(c.Tree, "TREE-") && !strings.HasPrefix(c.Tree, "NEAR-") && c.Tree != fixtureNaming {
			t.Errorf("case %s has tree %s, want its issue's root", c.ID, c.Tree)
		}
	}
	if len(trees[dsHeldOut]) == 0 || len(trees[dsDevelopment]) == 0 {
		t.Fatalf("trees by part %v, want trees in both", trees)
	}
	for _, g := range dsGroups {
		gc := res.counts.Groups[g]
		if gc.Cases < 5 {
			continue
		}
		if share := float64(gc.HeldOut) / float64(gc.Cases); share < 0.25 || share > 0.55 {
			t.Errorf("%s: %d of %d cases held out (%.2f), want about %.2f", g, gc.HeldOut, gc.Cases, share, dsHeldOutShare)
		}
	}
	// A child issue's cases go with its root: TREE-0's comment is on TREE-001.
	if partOf["TREE-001"] != "" {
		t.Errorf("a child issue is a tree of its own: %v", partOf)
	}
}

// A case whose text nearly repeats one on another tree joins that tree's split group, so it is in
// the same part; a text that repeats nothing keeps a group of its own.
func TestDatasetNearCopyMovesWithItsGroup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snapshot, verdicts := standardFixture().write(t, dir)
	if _, err := buildDataset(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed}, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	byTree := map[string]dsCase{}
	for _, c := range readDatasetCases(t, filepath.Join(dir, "out")) {
		if c.Tree == "NEAR-1" || c.Tree == "NEAR-2" || c.Tree == "TREE-0" {
			byTree[c.Tree] = c
		}
	}
	a, b, other := byTree["NEAR-1"], byTree["NEAR-2"], byTree["TREE-0"]
	if a.ID == "" || b.ID == "" || other.ID == "" {
		t.Fatalf("cases on the near-copy trees and TREE-0: %v", byTree)
	}
	if a.SplitGroup != b.SplitGroup || a.Part != b.Part {
		t.Errorf("near copies in split groups %s (%s) and %s (%s), want one group and one part", a.SplitGroup, a.Part, b.SplitGroup, b.Part)
	}
	if a.SplitGroup != "NEAR-1" {
		t.Errorf("the joined group's key is %s, want its least tree NEAR-1", a.SplitGroup)
	}
	if other.SplitGroup != "TREE-0" {
		t.Errorf("TREE-0's case is in split group %s, want one of its own", other.SplitGroup)
	}
}

// writeToDevelopment writes a --to-development list of keys, with a comment line and a blank line,
// and reads it with its own SHA-256 as the one the repository commits. It answers the list and
// that SHA-256.
func writeToDevelopment(t *testing.T, path string, keys ...string) (*dsToDevelopment, string) {
	t.Helper()
	data := []byte("# split groups for the test\n\n" + strings.Join(keys, "\n") + "\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := readToDevelopment(path, sha256Hex(data))
	if err != nil {
		t.Fatal(err)
	}
	return l, sha256Hex(data)
}

// build --to-development puts every case of a listed split group in the development part, and
// every other case keeps the part the split gives it: no group goes to the held-out part in place
// of the ones moved. counts.json holds the list's SHA-256 and what moved, and no key. The build
// without the list writes no to_development, and a list of groups the split already puts in
// development changes no file but counts.json.
func TestDatasetBuildToDevelopmentMovesOnlyTheListedGroups(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snapshot, verdicts := standardFixture().write(t, dir)
	in := dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed}
	plain, err := buildDataset(in, filepath.Join(dir, "plain"))
	if err != nil {
		t.Fatal(err)
	}
	partOf, groupPart, casesOf := map[string]string{}, map[string]string{}, map[string]int{}
	for _, c := range readDatasetCases(t, filepath.Join(dir, "plain")) {
		partOf[c.ID], groupPart[c.SplitGroup] = c.Part, c.Part
		casesOf[c.SplitGroup]++
	}
	var held, developed []string
	for _, g := range sortedKeys(groupPart) {
		if groupPart[g] == dsHeldOut {
			held = append(held, g)
		} else {
			developed = append(developed, g)
		}
	}
	if len(held) < 2 || len(developed) < 1 {
		t.Fatalf("split groups held out %v and in development %v, want two and one", held, developed)
	}
	listed := []string{held[0], held[1], developed[0]}
	list, sum := writeToDevelopment(t, filepath.Join(dir, "list.txt"), listed...)
	in.ToDevelopment = list
	moved, err := buildDataset(in, filepath.Join(dir, "moved"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range readDatasetCases(t, filepath.Join(dir, "moved")) {
		want := partOf[c.ID]
		if slices.Contains(listed, c.SplitGroup) {
			want = dsDevelopment
		}
		if c.Part != want {
			t.Errorf("case %s of split group %s is in the %s part, want %s", c.ID, c.SplitGroup, c.Part, want)
		}
	}
	td := moved.counts.ToDevelopment
	if td == nil || td.ListSHA256 != sum || td.SplitGroups != 3 || td.GroupsMoved != 2 || td.CasesMoved != casesOf[held[0]]+casesOf[held[1]] {
		t.Fatalf("to_development = %+v, want the list's SHA-256 %s, 3 groups listed, 2 moved with %d cases", td, sum, casesOf[held[0]]+casesOf[held[1]])
	}
	for _, g := range dsGroups {
		if was, is := plain.counts.Groups[g].HeldOut, moved.counts.Groups[g].HeldOut; was-is != td.CasesMovedByGroup[g] {
			t.Errorf("%s: %d cases held out by the split and %d after the list, and the list says it moved %d", g, was, is, td.CasesMovedByGroup[g])
		}
	}
	counts, err := os.ReadFile(filepath.Join(dir, "moved", "counts.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range listed {
		if bytes.Contains(counts, []byte(`"`+key+`"`)) {
			t.Errorf("counts.json holds the listed key %s", key)
		}
	}
	if plainCounts, _ := os.ReadFile(filepath.Join(dir, "plain", "counts.json")); bytes.Contains(plainCounts, []byte("to_development")) || !bytes.Contains(counts, []byte(`"to_development"`)) {
		t.Errorf("to_development is in the counts.json of the build without the list, or not in the one with it")
	}

	in.ToDevelopment, _ = writeToDevelopment(t, filepath.Join(dir, "developed.txt"), developed[0])
	same, err := buildDataset(in, filepath.Join(dir, "same"))
	if err != nil {
		t.Fatal(err)
	}
	if same.casesSHA != plain.casesSHA || same.documentSHA != plain.documentSHA || same.sessionsSHA != plain.sessionsSHA ||
		same.counts.ToDevelopment.GroupsMoved != 0 || same.counts.ToDevelopment.CasesMoved != 0 {
		t.Errorf("a list of a group already in development wrote cases %s (without it %s) and moved %+v", same.casesSHA, plain.casesSHA, same.counts.ToDevelopment)
	}
}

// build refuses a --to-development file whose SHA-256 is not the one the repository commits, and
// says both, before it writes a file. It refuses a list with a key that is no split group of the
// build, and says how many keys are, not which.
func TestDatasetBuildToDevelopmentRefusesAnotherList(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snapshot, verdicts := standardFixture().write(t, dir)
	path := filepath.Join(dir, "list.txt")
	data := []byte("NEAR-1\nTREE-1\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	out := filepath.Join(dir, "cli")
	code := run(t.Context(), []string{"dataset", "build", "--snapshot", snapshot, "--verdicts", verdicts, "--seed", strconv.Itoa(fixtureSeed),
		"--to-development", path, "--out", out}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), sha256Hex(data)) || !strings.Contains(stderr.String(), dsToDevelopmentSHA256) {
		t.Errorf("build of a list the repository does not commit: exit %d, stderr %q; want 1 and both SHA-256s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(out, "cases.jsonl.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused build wrote cases.jsonl.gz (%v)", err)
	}
	other := strings.Repeat("0", 64)
	if _, err := readToDevelopment(path, other); err == nil || !strings.Contains(err.Error(), sha256Hex(data)) || !strings.Contains(err.Error(), other) {
		t.Errorf("a list read against another committed SHA-256: %v, want a refusal that says both", err)
	}

	list, _ := writeToDevelopment(t, filepath.Join(dir, "unknown.txt"), "NEAR-1", "TREE-NONE", "TREE-NADA")
	_, err := buildDataset(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed, ToDevelopment: list}, filepath.Join(dir, "unknown"))
	if err == nil || !strings.Contains(err.Error(), "2 of the 3 keys") || strings.Contains(err.Error(), "TREE-N") {
		t.Errorf("a list with two keys that are no split group: %v, want a refusal that counts 2 of 3 and says no key", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "unknown", "cases.jsonl.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused build wrote cases.jsonl.gz (%v)", err)
	}
}

// apply moves a listed split group to the development part with its cases, whatever the split
// gave it, and counts them by case group; every other group keeps the split's part.
func TestToDevelopmentApplyMovesTheListedGroupsAndCountsTheirCases(t *testing.T) {
	t.Parallel()
	cases := []*dsCase{
		{ID: "question/comment:a@1", Group: dsGroupQuestion, SplitGroup: "TREE-1", Tree: "TREE-1"},
		{ID: "plain/comment:b@2", Group: dsGroupPlain, SplitGroup: "TREE-1", Tree: "TREE-3"},
		{ID: "answer/comment:c@3", Group: dsGroupAnswer, SplitGroup: "TREE-2", Tree: "TREE-2"},
	}
	list := &dsToDevelopment{sha256: sha256Hex([]byte("TREE-1\n")), keys: map[string]bool{"TREE-1": true}}
	parts := map[string]string{"TREE-1": dsHeldOut, "TREE-2": dsHeldOut}
	moved, err := list.apply(cases, parts)
	if err != nil {
		t.Fatal(err)
	}
	if moved.GroupsMoved != 1 || moved.CasesMoved != 2 || moved.CasesMovedByGroup[dsGroupQuestion] != 1 || moved.CasesMovedByGroup[dsGroupPlain] != 1 ||
		parts["TREE-1"] != dsDevelopment || parts["TREE-2"] != dsHeldOut {
		t.Fatalf("the list moved %+v and left the parts %v, want TREE-1 alone in development with its 2 cases", moved, parts)
	}
}

// The reply-chain walk goes up through the owner's own replies to the first agent text, an
// answer on an ask finds the ask, and the first agent reply to a question is found.
func TestDatasetReplyChainFindsTheAgentText(t *testing.T) {
	t.Parallel()
	f := newDSFixture()
	f.issue("WALK-1", "", "Walk", "Spec.")
	f.comment("agent-1", "WALK-1", fixtureAgent, "The relay drains the queue before each window.", "")
	f.comment("person-1", "WALK-1", fixturePerson, "Which window?", "agent-1")
	f.comment("person-2", "WALK-1", fixturePerson, "And what is the relay?", "person-1")
	f.comment("agent-2", "WALK-1", fixtureOther, "The relay is the job that copies events out.", "person-2")
	f.comment("person-3", "WALK-1", fixturePerson, "A note with no reply above it.", "")
	f.event("ask.opened", fixtureAgent, "WALK-1", map[string]any{"id": "ask-9", "question": "Ship it?"})
	f.event("ask.answered", fixturePerson, "WALK-1", map[string]any{"id": "ask-9", "answer": map[string]any{"text": "What does shipping mean here?"}})
	dir := t.TempDir()
	snapshot, _ := f.write(t, dir)
	ix, err := loadIndex(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := func(key string) *dsText {
		ts := ix.texts[key]
		if len(ts) == 0 {
			t.Fatalf("no text %s", key)
		}
		return ts[0]
	}
	if a, how := ix.answered(text("comment:person-2")); a == nil || a.Key != "comment:agent-1" || how != dsFoundReplyChain {
		t.Errorf("the owner's reply to his own reply answers %v (%s), want comment:agent-1 by the reply chain", a, how)
	}
	if r, how := ix.firstReply(text("comment:person-2")); r == nil || r.Key != "comment:agent-2" || how != dsFoundDirectReply {
		t.Errorf("the first agent reply is %v (%s), want comment:agent-2", r, how)
	}
	var answer *dsText
	for _, ts := range ix.texts {
		if ts[0].Kind == dsKindAnswer {
			answer = ts[0]
		}
	}
	if a, how := ix.answered(answer); a == nil || a.Key != "ask:ask-9" || how != dsFoundReplyChain {
		t.Errorf("the answer's text answers %v (%s), want ask:ask-9", a, how)
	}
	// A top-level note answers the last agent text on its issue before it.
	if a, how := ix.answered(text("comment:person-3")); a == nil || a.Key != "comment:agent-2" || how != dsFoundIssue {
		t.Errorf("the top-level note answers %v (%s), want comment:agent-2 by its issue", a, how)
	}
}

// A record names each document as Dispatch named it just before the write. An issue's primary
// document is spec.md though no event after issue.created names it. A document a person renamed
// keeps its old name in a record written before the rename, and has the new one after it.
func TestDatasetRecordNamesEachDocumentAsItWasNamedThen(t *testing.T) {
	t.Parallel()
	f := newDSFixture()
	f.issue("DOC-1", "", "Launch", "Spec.")
	f.event("artifact.created", fixtureAgent, "DOC-1", map[string]any{"artifact": map[string]any{
		"id": "art-notes", "kind": "doc", "name": "Draft notes", "issue_key": "DOC-1", "versions": []map[string]any{{"number": 1}}}})
	f.comment("before", "DOC-1", fixtureAgent, "The launch is on Monday.", "")
	f.event("artifact.renamed", fixturePerson, "DOC-1", map[string]any{
		"artifact": map[string]any{"id": "art-notes", "kind": "doc", "name": "Launch notes"}, "previous_name": "Draft notes"})
	f.comment("after", "DOC-1", fixtureAgent, "The launch moved to Tuesday.", "")
	snapshot, _ := f.write(t, t.TempDir())
	ix, err := loadIndex(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"comment:before": "Draft notes", "comment:after": "Launch notes"} {
		ts := ix.texts[key]
		if len(ts) == 0 {
			t.Fatalf("no text %s", key)
		}
		names := map[string]string{}
		for _, d := range ix.record(ts[0]).Documents {
			names[d.ArtifactID] = d.Name
		}
		if len(names) != 2 || names["art-DOC-1"] != "spec.md" || names["art-notes"] != want {
			t.Errorf("the record of %s names its documents %v, want spec.md and %q", key, names, want)
		}
	}
}

// Each copy of a broadcast stands in its own recipient's conversation, so a record there holds it
// as the thread's first text. The broadcast is still one write of the owner: his texts and the
// people's texts hold its first copy alone, a later copy reached by its key answers the first
// copy, and the build reads no verdict for a later copy.
func TestDatasetBroadcastCopyIsItsConversationsRoot(t *testing.T) {
	t.Parallel()
	f := newDSFixture()
	message := func(typ, id string, actor map[string]any, session, body, reply, broadcast string) {
		payload := map[string]any{"id": id, "body": body, "target": "session:" + session, "author": actor}
		if reply != "" {
			payload["in_reply_to"] = reply
		}
		if broadcast != "" {
			payload["broadcast_id"] = broadcast
		}
		f.event(typ, actor, "", payload)
	}
	first, second := fixtureAgent["id"].(string), fixtureOther["id"].(string)
	const body = "Stop the deploys until the window opens."
	message("message.created", "copy-1", fixturePerson, first, body, "", "bc-1")
	f.verdicts["message:copy-1@"+strconv.FormatInt(f.next, 10)] = wordrecords.None
	message("message.created", "copy-2", fixturePerson, second, body, "", "bc-1")
	message("message.answered", "reply-2", fixtureOther, second, "The deploys wait for the window.", "copy-2", "")
	message("message.answered", "owner-2", fixturePerson, second, "What is the window?", "reply-2", "")
	f.verdicts["message:owner-2@"+strconv.FormatInt(f.next, 10)] = wordrecords.WordMeaning
	dir := t.TempDir()
	snapshot, verdicts := f.write(t, dir)
	ix, err := loadIndex(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	var owners []string
	for _, o := range ix.ownerTexts() {
		owners = append(owners, o.Key)
	}
	if !slices.Equal(owners, []string{"message:copy-1", "message:owner-2"}) || len(ix.personTexts()) != 2 {
		t.Errorf("the owner's texts are %v of %d people's texts, want the broadcast once and his question", owners, len(ix.personTexts()))
	}
	if got := ix.at("message:copy-2", f.next+1); got == nil || got.Key != "message:copy-1" {
		t.Errorf("the later copy's key answers %v, want the first copy", got)
	}

	out := filepath.Join(dir, "out")
	res, err := buildDataset(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed}, out)
	if err != nil {
		t.Fatal(err)
	}
	if res.counts.OwnerTexts != 2 || res.counts.PersonTexts != 2 {
		t.Errorf("counts hold %d owner texts of %d people's texts, want 2 of 2", res.counts.OwnerTexts, res.counts.PersonTexts)
	}
	var question *dsCase
	for _, c := range readDatasetCases(t, out) {
		if c.Agent.Key == "message:reply-2" && c.Group == dsGroupQuestion {
			question = &c
		}
	}
	if question == nil {
		t.Fatal("no question case on the second recipient's reply")
	}
	thread := question.Record.Thread
	if len(thread) != 1 || thread[0].Key != "message:copy-2" || thread[0].Text != body || thread[0].ActorKind != "user" || thread[0].Actor != fixtureOwner {
		t.Errorf("the second recipient's record holds %+v, want the owner's copy of the broadcast", thread)
	}
}

// owner-texts writes each text the owner wrote himself, in event order, with its time: no
// agent's text, no other person's, and no text a session wrote for him. The snapshot of a bundle
// writes the same bytes.
func TestDatasetOwnerTextsAreHisOwnWithTheirTimes(t *testing.T) {
	t.Parallel()
	f := newDSFixture()
	f.issue("OWN-1", "", "Own", "Spec.")
	f.comment("agent-1", "OWN-1", fixtureAgent, "The relay drains the queue before each window.", "")
	f.comment("owner-1", "OWN-1", fixturePerson, "What is the relay?", "agent-1")
	asked := f.next
	f.comment("other-1", "OWN-1", map[string]any{"kind": "user", "id": "other@example.com"}, "I ask the same.", "agent-1")
	f.comment("acted-1", "OWN-1", map[string]any{"kind": "user", "id": fixtureOwner, "act": map[string]any{"session": fixtureAgent["id"]}},
		"A session wrote this for him.", "")
	f.event("ask.opened", fixtureAgent, "OWN-1", map[string]any{"id": "ask-7", "question": "Ship it?"})
	f.event("ask.answered", fixturePerson, "OWN-1", map[string]any{"id": "ask-7", "answer": map[string]any{"text": "Ship it after the window."}})
	answered := f.next
	dir := t.TempDir()
	snapshot, verdicts := f.write(t, dir)

	var stdout, stderr bytes.Buffer
	out := filepath.Join(dir, "out")
	if code := run(t.Context(), []string{"dataset", "owner-texts", "--snapshot", snapshot, "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("owner-texts: exit %d: %s", code, stderr.String())
	}
	path := filepath.Join(out, dsOwnerTextsFile)
	var texts []dsCaseText
	if err := eachGzipLine(path, func(line []byte) error {
		var text dsCaseText
		if err := json.Unmarshal(line, &text); err != nil {
			return err
		}
		texts = append(texts, text)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	at := func(event int64) string {
		return time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC).Add(time.Duration(event) * time.Minute).Format(time.RFC3339Nano)
	}
	want := []dsCaseText{{Text: "What is the relay?", Time: at(asked)}, {Text: "Ship it after the window.", Time: at(answered)}}
	if len(texts) != len(want) {
		t.Fatalf("owner texts = %+v, want his comment and his answer", texts)
	}
	for i, text := range texts {
		if text.Text != want[i].Text || text.Time != want[i].Time || text.Actor != fixtureOwner || text.ActorKind != "user" || text.Session != "" {
			t.Errorf("owner text %d = %+v, want %q at %s by %s", i, text, want[i].Text, want[i].Time, fixtureOwner)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if printed := "owner texts: 2\n" + dsOwnerTextsFile + " sha256: " + sha256Hex(data) + "\n"; stdout.String() != printed {
		t.Errorf("owner-texts printed %q, want %q", stdout.String(), printed)
	}

	bundle := filepath.Join(dir, "bundle.tar.gz")
	if _, _, err := packBundle(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed}, bundle, dsPartSize); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	fromBundle := filepath.Join(dir, "from-bundle")
	if code := run(t.Context(), []string{"dataset", "owner-texts", "--from", bundle, "--out", fromBundle}, &stdout, &stderr); code != 0 {
		t.Fatalf("owner-texts --from: exit %d: %s", code, stderr.String())
	}
	if again, err := os.ReadFile(filepath.Join(fromBundle, dsOwnerTextsFile)); err != nil || !bytes.Equal(again, data) {
		t.Errorf("owner-texts --from the bundle wrote other bytes (%v)", err)
	}
	if left, _ := filepath.Glob(filepath.Join(fromBundle, ".bundle-*")); len(left) != 0 {
		t.Errorf("owner-texts --from left %v", left)
	}
	for _, args := range [][]string{{"--out", out}, {"--snapshot", snapshot, "--from", bundle, "--out", out}, {"--snapshot", snapshot}} {
		if code := run(t.Context(), append([]string{"dataset", "owner-texts"}, args...), &stdout, &stderr); code != 2 {
			t.Errorf("owner-texts %v: exit %d, want 2", args, code)
		}
	}
}

// build --from the bundle pack wrote, whole or as parts, prints the digest the direct build
// prints, and the SHA-256 of the snapshot and the verdicts it read with the header's end and the
// SHA-256 of its owner; its counts carry the confirmation read the bundle holds, whose items are
// the draw. The read leaves the cases as they were, and one that names a case that is not a
// question case is refused.
func TestDatasetBuildFromPackPrintsTheSameDigest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snapshot, verdicts := confirmedFixture().write(t, dir)
	plain, err := buildDataset(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed}, filepath.Join(dir, "plain"))
	if err != nil {
		t.Fatal(err)
	}
	var questions []string
	for _, c := range readDatasetCases(t, filepath.Join(dir, "plain")) {
		if c.Group == dsGroupQuestion {
			questions = append(questions, c.ID)
		}
	}
	confirm := func(name string, ids ...string) string {
		t.Helper()
		items := []map[string]any{}
		for i, id := range ids {
			items = append(items, map[string]any{"n": i + 1, "case_id": id, "about_words": i != 1, "why": "invented"})
		}
		data, _ := json.Marshal(map[string]any{"reader": "StandIn", "confirmed": len(ids) - 1, "total": len(ids), "items": items})
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	sorted := slices.Sorted(slices.Values(questions))
	var drawn []string
	for _, j := range newPyRandom(77).sample(len(sorted), dsConfirmedSize) {
		drawn = append(drawn, sorted[j])
	}
	in := dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed, Confirmed: confirm("confirm.json", drawn...), ConfirmedSeed: 77}
	direct, err := buildDataset(in, filepath.Join(dir, "direct"))
	if err != nil {
		t.Fatal(err)
	}
	if s := direct.counts.ConfirmedSample; direct.casesSHA != plain.casesSHA || s == nil || s.Confirmed != dsConfirmedSize-1 || s.Total != dsConfirmedSize || s.Seed != 77 || s.Reader != "StandIn" || s.ResultSHA256 == "" ||
		s.Draw != "python3 random.Random(77).sample(sorted question cases by id, 50)" {
		t.Fatalf("with the confirmation read: cases %s (without it %s), sample %+v", direct.casesSHA, plain.casesSHA, direct.counts.ConfirmedSample)
	}
	bad := in
	bad.Confirmed = confirm("bad.json", questions[0], "plain/comment:not-a-question@1")
	if _, err := buildDataset(bad, filepath.Join(dir, "bad-confirm")); err == nil || !strings.Contains(err.Error(), "not a question case") {
		t.Fatalf("a confirmation read of a case that is not a question case: %v", err)
	}
	bundle := filepath.Join(dir, "bundle", "dataset.tar.gz")
	if err := os.MkdirAll(filepath.Dir(bundle), 0o755); err != nil {
		t.Fatal(err)
	}
	sum, parts, err := packBundle(in, bundle, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) < 3 {
		t.Fatalf("a bundle over the part size was written as %v, want parts and a manifest", parts)
	}
	again, _, err := packBundle(in, filepath.Join(dir, "again.tar.gz"), dsPartSize)
	if err != nil || again != sum {
		t.Fatalf("a second pack of the same inputs: %s, %v; want %s", again, err, sum)
	}
	directCounts, _ := os.ReadFile(filepath.Join(dir, "direct", "counts.json"))
	snapshotSHA, err := fileSHA256(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	verdictsSHA, err := fileSHA256(verdicts)
	if err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{bundle, bundle + ".manifest.json"} {
		var stdout, stderr bytes.Buffer
		out := filepath.Join(dir, "from-"+filepath.Base(from))
		if code := run(t.Context(), []string{"dataset", "build", "--from", from, "--out", out}, &stdout, &stderr); code != 0 {
			t.Fatalf("build --from %s: exit %d: %s", from, code, stderr.String())
		}
		for _, want := range []string{"\nsnapshot sha256: " + snapshotSHA + "\n", "\nverdicts sha256: " + verdictsSHA + "\n",
			"\nend: " + fixtureEnd + "\n", "\nowner_sha256: " + sha256Hex([]byte(fixtureOwner)) + "\n", "\ncases.jsonl.gz sha256: " + direct.casesSHA + "\n"} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("build --from %s printed:\n%s\nwant %q", from, stdout.String(), want)
			}
		}
		if counts, _ := os.ReadFile(filepath.Join(out, "counts.json")); !bytes.Equal(counts, directCounts) {
			t.Errorf("build --from %s wrote counts\n%s\nwant the direct build's\n%s", from, counts, directCounts)
		}
	}
	// A part whose bytes changed is refused.
	part := parts[0]
	data, _ := os.ReadFile(part)
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(part, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"dataset", "build", "--from", bundle + ".manifest.json", "--out", filepath.Join(dir, "bad")}, &stdout, &stderr); code == 0 {
		t.Fatalf("build --from a manifest whose part changed answered 0: %s", stdout.String())
	}
}

// confirmedFixture is standardFixture with 50 trees more, each with an agent comment the owner
// asks the meaning of: question cases enough for the draw of R1's confirmation read.
func confirmedFixture() *dsFixture {
	f := standardFixture()
	rng := rand.New(rand.NewPCG(13, 17))
	for i := range dsConfirmedSize {
		issue := fmt.Sprintf("MORE-%d", i)
		f.issue(issue, "", "More "+fixtureWords(rng, 4), fixtureWords(rng, 30))
		agent := f.comment(fmt.Sprintf("m-%d", i), issue, fixtureAgent, fixtureWords(rng, 40), "")
		f.owner(fmt.Sprintf("mo-%d", i), issue, "What does "+fixtureWords(rng, 2)+" mean here?", agent, wordrecords.WordMeaning)
	}
	return f
}

// A build refused on its confirmation read writes none of its files: a reused --out keeps the
// files of the build before it, counts.json included, and a fresh one is not left behind, by a
// build of the inputs or of a bundle of them (--from, whose files are joined under --out).
func TestDatasetBuildRefusedOnTheConfirmationReadLeavesOutAsItWas(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snapshot, verdicts := confirmedFixture().write(t, dir)
	plain := filepath.Join(dir, "plain")
	if _, err := buildDataset(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed}, plain); err != nil {
		t.Fatal(err)
	}
	var questions []string
	for _, c := range readDatasetCases(t, plain) {
		if c.Group == dsGroupQuestion {
			questions = append(questions, c.ID)
		}
	}
	sorted := slices.Sorted(slices.Values(questions))
	var items []map[string]any
	for i, j := range newPyRandom(77).sample(len(sorted), dsConfirmedSize) {
		items = append(items, map[string]any{"n": i + 1, "case_id": sorted[j], "about_words": true})
	}
	// Two items in the other order: every case is a question case of the draw, and the order is
	// not the draw's.
	items[3], items[4] = items[4], items[3]
	data, _ := json.Marshal(map[string]any{"reader": "StandIn", "confirmed": len(items), "total": len(items), "items": items})
	swapped := filepath.Join(dir, "swapped.json")
	if err := os.WriteFile(swapped, data, 0o644); err != nil {
		t.Fatal(err)
	}
	reused := filepath.Join(dir, "reused")
	if err := os.MkdirAll(reused, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cases.jsonl.gz", "documents.jsonl.gz", "sessions.tsv", "counts.json"} {
		if err := os.WriteFile(filepath.Join(reused, name), []byte("the build before: "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := dirFiles(t, reused)
	bundle := filepath.Join(dir, "bundle", "dataset.tar.gz")
	if err := os.MkdirAll(filepath.Dir(bundle), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := packBundle(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed, Confirmed: swapped, ConfirmedSeed: 77}, bundle, dsPartSize); err != nil {
		t.Fatal(err)
	}
	direct := []string{"--snapshot", snapshot, "--verdicts", verdicts, "--seed", strconv.Itoa(fixtureSeed), "--confirmed", swapped, "--confirmed-seed", "77"}
	fresh, freshFrom := filepath.Join(dir, "fresh"), filepath.Join(dir, "fresh-from")
	for _, build := range []struct {
		out    string
		inputs []string
	}{{reused, direct}, {fresh, direct}, {freshFrom, []string{"--from", bundle}}} {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), append(append([]string{"dataset", "build"}, build.inputs...), "--out", build.out), &stdout, &stderr)
		if says := ": item 4 is " + items[3]["case_id"].(string) + ", and the draw python3 random.Random(77).sample(sorted question cases by id, 50) gives "; code != 1 || !strings.Contains(stderr.String(), says) {
			t.Errorf("build --out %s with two items of the draw swapped: exit %d: %s", build.out, code, stderr.String())
		}
	}
	if after := dirFiles(t, reused); !maps.Equal(after, before) {
		t.Errorf("the refused build changed the reused --out: %v, want %v", slices.Sorted(maps.Keys(after)), slices.Sorted(maps.Keys(before)))
	}
	for _, out := range []string{fresh, freshFrom} {
		if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the refused build left the fresh --out %s (%v)", out, err)
		}
	}
}

// dirFiles is each entry of dir by name, with a file's bytes.
func dirFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		files[e.Name()] = string(data)
	}
	return files
}

// build reads a snapshot whose last event is created at its header's end, and refuses one that
// holds an event created after it, naming the first such event's id and time; owner-texts and
// followups, which read the snapshot build reads, refuse it too (as classify does: each reads it
// by loadIndex).
func TestDatasetBuildRefusesAnEventAfterTheEnd(t *testing.T) {
	t.Parallel()
	f := standardFixture()
	f.events[len(f.events)-1]["created_at"] = fixtureEnd
	snapshot, verdicts := f.write(t, t.TempDir())
	built := filepath.Join(t.TempDir(), "out")
	if _, err := buildDataset(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed}, built); err != nil {
		t.Fatalf("a snapshot whose last event is created at its end: %v", err)
	}
	f.comment("late-1", "TREE-1", fixtureAgent, "Written half a second after the end.", "")
	late := f.next
	f.events[len(f.events)-1]["created_at"] = "2026-11-01T00:00:00.5Z"
	f.comment("late-2", "TREE-1", fixtureAgent, "Written a day after the end.", "")
	f.events[len(f.events)-1]["created_at"] = "2026-11-02T00:00:00Z"
	dir := t.TempDir()
	snapshot, verdicts = f.write(t, dir)
	_, err := buildDataset(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed}, filepath.Join(t.TempDir(), "out"))
	want := fmt.Sprintf("holds event %d, created at 2026-11-01T00:00:00.5Z, after its end %s", late, fixtureEnd)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a snapshot with events after its end: %v, want a refusal saying %q", err, want)
	}
	bundle := filepath.Join(dir, "late.tar.gz")
	if _, _, err := packBundle(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed}, bundle, dsPartSize); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"owner-texts", "--snapshot", snapshot, "--out", filepath.Join(dir, "owner-texts")},
		{"followups", "--from", bundle, "--cases", built, "--out", filepath.Join(dir, "followups")},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), append([]string{"dataset"}, args...), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), want) {
			t.Errorf("dataset %s over a snapshot with events after its end: exit %d, %s; want 1 saying %q", args[0], code, stderr.String(), want)
		}
	}
}

// build reads a --verdicts path that names no file as a verdicts file that holds no verdict, so
// it says which of the owner's texts have none, as for any verdicts file that lacks them.
func TestDatasetBuildSaysTheTextsWithNoVerdictForAMissingVerdictsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snapshot, _ := standardFixture().write(t, dir)
	missing := filepath.Join(dir, "no-verdicts.jsonl")
	_, err := buildDataset(dsInputs{Snapshot: snapshot, Verdicts: missing, Seed: fixtureSeed}, filepath.Join(dir, "out"))
	if want := " texts have no verdict in " + missing + ": run classify again"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a --verdicts path that names no file: %v, want a refusal saying %q", err, want)
	}
}

// readConfirmed takes the items python3 drew, random.Random(3231051).sample of the 729 question
// cases sorted by id, 50 (confirmedDraw), whatever order the build lists the cases in. It refuses
// those items with one case swapped for a question case the draw does not hold, and with two
// cases in the other order, naming the first item that differs, and a draw of 3 cases or of none,
// each the draw of its own size, since R1's read is of 50.
func TestReadConfirmedRefusesItemsThatAreNotTheDraw(t *testing.T) {
	t.Parallel()
	const seed = 3231051
	ids := make([]string, 729)
	for i := range ids {
		ids[i] = fmt.Sprintf("question/comment:%04d@1", i)
	}
	questions := slices.Clone(ids)
	slices.Reverse(questions)
	dir := t.TempDir()
	write := func(name string, draw []int) string {
		t.Helper()
		items := make([]map[string]any, len(draw))
		for i, j := range draw {
			items[i] = map[string]any{"n": i + 1, "case_id": ids[j], "about_words": true}
		}
		data, _ := json.Marshal(map[string]any{"reader": "StandIn", "confirmed": len(draw), "total": len(draw), "items": items})
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if s, err := readConfirmed(write("drawn.json", confirmedDraw), seed, questions); err != nil || s.Total != 50 || s.Confirmed != 50 {
		t.Fatalf("the items python3 drew: %+v, %v", s, err)
	}
	notDrawn := 0
	for slices.Contains(confirmedDraw, notDrawn) {
		notDrawn++
	}
	swapped := slices.Clone(confirmedDraw)
	swapped[6] = notDrawn
	reordered := slices.Clone(confirmedDraw)
	reordered[10], reordered[11] = reordered[11], reordered[10]
	for _, c := range []struct {
		name string
		draw []int
		says string
	}{
		{"swapped.json", swapped, fmt.Sprintf("item 7 is %s, and the draw python3 random.Random(3231051).sample(sorted question cases by id, 50) gives %s there", ids[notDrawn], ids[confirmedDraw[6]])},
		{"reordered.json", reordered, fmt.Sprintf("item 11 is %s, and the draw python3 random.Random(3231051).sample(sorted question cases by id, 50) gives %s there", ids[confirmedDraw[11]], ids[confirmedDraw[10]])},
		{"three.json", newPyRandom(seed).sample(len(ids), 3), "reads 3 question cases, and R1's confirmation read draws 50"},
		{"none.json", nil, "reads 0 question cases, and R1's confirmation read draws 50"},
	} {
		if _, err := readConfirmed(write(c.name, c.draw), seed, questions); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want a refusal saying %q", c.name, err, c.says)
		}
	}
}

// fetch keeps the events created up to the end time, puts the whole body in place of a cut one,
// reads the document versions a case's record names, and a second run of the same flags writes
// the same snapshot from its work directory.
func TestDatasetFetchWritesTheSnapshotBuildReads(t *testing.T) {
	f := newDSFixture()
	f.issue("FETCH-1", "", "Fetch", "The first spec.")
	f.comment("cut-1", "FETCH-1", fixtureAgent, "The first part.", "")
	f.events[len(f.events)-1]["payload"].(map[string]any)["body_truncated"] = true
	f.owner("ask-1", "FETCH-1", "What is the part here?", "cut-1", wordrecords.WordMeaning)
	end := f.events[len(f.events)-1]["created_at"].(string)
	f.comment("late-1", "FETCH-1", fixtureAgent, "Written a minute after the end.", "")
	f.next += 30
	f.comment("later-1", "FETCH-1", fixtureAgent, "Written long after the end.", "")
	var mu sync.Mutex
	requests := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests[r.URL.Path]++
		mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/events/log":
			after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
			page := []map[string]any{}
			for _, ev := range f.events {
				if ev["id"].(int64) > after {
					page = append(page, ev)
				}
			}
			_ = json.NewEncoder(w).Encode(page)
		case r.URL.Path == "/api/v1/comments/cut-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"comment": map[string]any{"body": "The first part. And the whole rest of it."}})
		case r.URL.Path == "/api/v1/artifacts/art-FETCH-1/versions/1":
			_ = json.NewEncoder(w).Encode(map[string]any{"markdown": "The first spec."})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("stand-in"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISPATCH_URL", server.URL)
	t.Setenv("DISPATCH_TOKEN_FILE", token)
	out := filepath.Join(dir, "snapshot.jsonl.gz")
	fetch := func() string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), []string{"dataset", "fetch", "--end", end, "--out", out, "--owner", fixtureOwner}, &stdout, &stderr); code != 0 {
			t.Fatalf("fetch: exit %d: %s", code, stderr.String())
		}
		_, sum, _ := strings.Cut(stdout.String(), "snapshot sha256: ")
		return strings.TrimSpace(sum)
	}
	first := fetch()
	ix, err := loadIndex(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ix.events != 3 || ix.owner != fixtureOwner || ix.end == "" {
		t.Errorf("the snapshot holds %d events for owner %q to %q, want the 3 created up to the end for %s", ix.events, ix.owner, ix.end, fixtureOwner)
	}
	if c := ix.texts["comment:cut-1"]; len(c) != 1 || c[0].Cut || c[0].Text != "The first part. And the whole rest of it." {
		t.Errorf("the cut comment is %+v, want its whole body", c)
	}
	if !ix.versionsIn[dsVerKey{"art-FETCH-1", 1}] {
		t.Errorf("the snapshot lacks the version the case's record names: %v", ix.versionsIn)
	}
	if second := fetch(); second != first || first == "" {
		t.Errorf("a second fetch wrote %q, the first %q; want one snapshot", second, first)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests["/api/v1/comments/cut-1"] != 1 || requests["/api/v1/artifacts/art-FETCH-1/versions/1"] != 1 {
		t.Errorf("reads %v, want the body and the version read once across both runs", requests)
	}
}

// standInTQ answers `tq sql --format JSONEachRow <query>` from the rows in $TQ_ROWS, as the store
// would: the rows of the sessions the query names, at or before each one's cut, with no row of
// kind file, in session and seq order. It logs each query to $TQ_LOG.
const standInTQ = `#!/usr/bin/env bash
set -euo pipefail
[ "$1" = sql ] && [ "$2" = --format ] && [ "$3" = JSONEachRow ] || { echo "stand-in tq: unexpected arguments: $*" >&2; exit 2; }
printf '%s\n' "$4" >>"$TQ_LOG"
exec python3 - "$4" "$TQ_ROWS" <<'PY'
import json, re, sys
query, rows_path = sys.argv[1:]
def when(s):
    day, _, frac = s.replace("T", " ").rstrip("Z").partition(".")
    return day, (frac + "000000000")[:9]
asked = {sid: when(cut) for sid, cut in re.findall(r"session_id = '([0-9a-f-]+)' AND ts <= parseDateTime64BestEffort\('([^']+)', 9\)", query)}
rows = [json.loads(line) for line in open(rows_path) if line.strip()]
found = [r for r in rows if r["session_id"] in asked and r["kind"] != "file" and when(r["ts"]) <= asked[r["session_id"]]]
found.sort(key=lambda r: (r["session_id"], r["seq"], r["raw_file"], r["raw_line"]))
for r in found:
    print(json.dumps(r))
PY
`

// export-sessions.sh, run against a stand-in tq with the session list build writes, gives the
// parts and the manifest, a 0 for a session the store holds no row of, and never queries a
// session it leaves out; dataset sessions joins those rows to the cases at each write's place,
// and refuses a part whose SHA-256 differs from the manifest.
func TestExportSessionsScriptFeedsDatasetSessions(t *testing.T) {
	for _, tool := range []string{"bash", "python3", "gzip", "split"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("export-sessions.sh needs %s, which is not on PATH", tool)
		}
	}
	const (
		withRows = "01a1bbbb-0000-7000-8000-00000000000a"
		noRows   = "01a1bbbb-0000-7000-8000-00000000000b"
		leftOut  = "01a1bbbb-0000-7000-8000-00000000000c"
	)
	dir := t.TempDir()
	cases := []*dsCase{
		{ID: "question/a0", WriterSession: withRows, WriteTime: "2026-11-10T09:59:00Z"},
		{ID: "question/a1", WriterSession: withRows, WriteTime: "2026-11-10T10:00:30.5Z"},
		{ID: "plain/a2", WriterSession: withRows, WriteTime: "2026-11-10T10:02:00Z"},
		{ID: "answer/b1", WriterSession: noRows, WriteTime: "2026-11-10T11:00:00Z"},
		{ID: "phrase/c1", WriterSession: leftOut, WriteTime: "2026-11-10T12:00:00Z"},
		{ID: "plain/x1", WriterSession: "memory-job", WriteTime: "2026-11-10T12:00:00Z"},
		{ID: "plain/n1", WriteTime: "2026-11-10T12:00:00Z"},
	}
	casesPath := filepath.Join(dir, "cases.jsonl.gz")
	if _, err := dsWriteGzipJSONL(casesPath, cases); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(dir, "sessions.tsv")
	if _, listed, unlisted, err := writeSessionList(list, cases); err != nil || listed != 3 || unlisted != 1 {
		t.Fatalf("the session list: %d listed, %d left out, %v; want 3 and 1", listed, unlisted, err)
	}
	exclude := filepath.Join(dir, "exclude.txt")
	rows := filepath.Join(dir, "rows.jsonl")
	row := func(session string, seq int, ts, kind, text string) string {
		b, _ := json.Marshal(map[string]any{"session_id": session, "host": "pod-1", "seq": seq, "ts": ts, "role": "assistant",
			"kind": kind, "text": text, "raw_file": "session.jsonl", "raw_line": seq})
		return string(b) + "\n"
	}
	bin := filepath.Join(dir, "bin")
	for path, content := range map[string]string{
		exclude: leftOut + "\n",
		rows: row(withRows, 1, "2026-11-10 10:00:00.000000000", "text", "Rerun the flaky test.") +
			row(withRows, 2, "2026-11-10 10:01:00.000000000", "text", "I will rerun it now with password: correct-horse-battery-staple") +
			row(withRows, 3, "2026-11-10 10:01:10.000000000", "file", "The whole output of a tool.") +
			row(withRows, 4, "2026-11-10 10:01:30.000000000", "text", "The rerun passed.") +
			row(withRows, 5, "2026-11-10 10:05:00.000000000", "text", "Written after the last cut.") +
			row(leftOut, 1, "2026-11-10 11:00:00.000000000", "text", "A session that is left out."),
		filepath.Join(bin, "tq"): standInTQ,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	export := filepath.Join(dir, "export")
	tqLog := filepath.Join(dir, "tq.log")
	cmd := exec.Command("bash", "export-sessions.sh", "--sessions", list, "--exclude", exclude, "--out", export)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TQ_LOG="+tqLog, "TQ_ROWS="+rows)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("export-sessions.sh: %v\n%s", err, stderr.String())
	}
	if want := withRows + "\t3\n" + noRows + "\t0\n"; stdout.String() != want {
		t.Errorf("export-sessions.sh printed %q, want %q", stdout.String(), want)
	}
	queries, _ := os.ReadFile(tqLog)
	if strings.Contains(string(queries), leftOut) || !strings.Contains(string(queries), withRows) || !strings.Contains(string(queries), noRows) {
		t.Errorf("the queries were %s; want the two listed sessions and never the one left out", queries)
	}
	var m dsExportManifest
	data, err := os.ReadFile(filepath.Join(export, "manifest.json"))
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	if err != nil {
		t.Fatal(err)
	}
	if m.Schema != "infra-3231-session-export/2" || len(m.Parts) != 1 || m.Parts[0].Name != "part-000.jsonl.gz" ||
		m.RowsBySession[withRows] != 3 || m.RowsBySession[noRows] != 0 || len(m.RowsBySession) != 2 ||
		len(m.MasksBySession) != 1 || m.MasksBySession[withRows]["password"] != 1 ||
		strings.Join(m.SessionsMissing, ",") != noRows || m.SessionsLeftOut != 1 {
		t.Fatalf("the manifest is %s", data)
	}
	part, _ := os.ReadFile(filepath.Join(export, m.Parts[0].Name))
	if sha256Hex(part) != m.Parts[0].SHA256 || int64(len(part)) != m.Parts[0].Bytes {
		t.Fatalf("the part's SHA-256 and size are %s and %d, the manifest's %s and %d", sha256Hex(part), len(part), m.Parts[0].SHA256, m.Parts[0].Bytes)
	}
	var exported strings.Builder
	if err := eachGzipLine(filepath.Join(export, m.Parts[0].Name), func(line []byte) error {
		exported.Write(line)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(exported.String(), "correct-horse-battery-staple") || !strings.Contains(exported.String(), "[REDACTED:password]") {
		t.Errorf("the part holds the planted secret unmasked, or no mask: %s", exported.String())
	}

	ingest := func(out string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), []string{"dataset", "sessions", "--manifest", filepath.Join(export, "manifest.json"), "--cases", casesPath, "--out", out}, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	out := filepath.Join(dir, "ingest")
	if code, _, errText := ingest(out); code != 0 {
		t.Fatalf("dataset sessions: exit %d: %s", code, errText)
	}
	var sc dsSessionCounts
	if data, err := os.ReadFile(filepath.Join(out, dsSessionCountsFile)); err != nil || json.Unmarshal(data, &sc) != nil ||
		sc.Schema != m.Schema || sc.MasksBySession[withRows]["password"] != 1 || sc.CasesByStatus[dsPlaceFound] != 2 {
		t.Errorf("sessions-counts.json is %+v (%v), want the export's schema, its one mask and two cases found", sc, err)
	}
	var places []dsPlace
	if err := eachLine(filepath.Join(out, dsPlacesFile), func(line []byte, _ int) error {
		var p dsPlace
		err := json.Unmarshal(line, &p)
		places = append(places, p)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range places {
		got[p.CaseID] = fmt.Sprintf("%s %d %s", p.Status, p.Rows, string(p.PlaceSeq))
	}
	want := map[string]string{
		"question/a0": dsPlaceNoRowBefore + " 0 ", "question/a1": dsPlaceFound + " 1 1", "plain/a2": dsPlaceFound + " 3 4",
		"answer/b1": dsPlaceMissing + " 0 ", "phrase/c1": dsPlaceLeftOut + " 0 ", "plain/x1": dsPlaceNotAStoreID + " 0 ", "plain/n1": dsPlaceNoSession + " 0 ",
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("case %s: %q, want %q", id, got[id], w)
		}
	}
	var sessionRows []dsSessionRow
	if err := eachGzipLine(filepath.Join(out, dsSessionRowsFile), func(line []byte) error {
		var r dsSessionRow
		err := json.Unmarshal(line, &r)
		sessionRows = append(sessionRows, r)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(sessionRows) != 3 || sessionRows[2].SessionID != withRows || sessionRows[2].Index != 2 || !strings.Contains(string(sessionRows[2].Row), "The rerun passed.") {
		t.Errorf("the session rows are %+v, want the three rows of the session with rows, in order", sessionRows)
	}

	// A manifest that counts another number of rows for a session is refused after the rows are
	// read, and the refused export writes none of the three files: a reused --out keeps those of
	// the export before it.
	miscount := m
	miscount.RowsBySession = maps.Clone(m.RowsBySession)
	miscount.RowsBySession[withRows]++
	miscounted := filepath.Join(export, "miscounted.json")
	if data, err := json.Marshal(miscount); err != nil || os.WriteFile(miscounted, data, 0o644) != nil {
		t.Fatalf("the miscounted manifest: %v", err)
	}
	reused := filepath.Join(dir, "reused")
	if err := os.MkdirAll(reused, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{dsSessionRowsFile, dsPlacesFile, dsSessionCountsFile} {
		if err := os.WriteFile(filepath.Join(reused, name), []byte("the export before: "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := dirFiles(t, reused)
	var refusal bytes.Buffer
	if code := run(t.Context(), []string{"dataset", "sessions", "--manifest", miscounted, "--cases", casesPath, "--out", reused}, &bytes.Buffer{}, &refusal); code != 1 ||
		!strings.Contains(refusal.String(), "the parts hold 3 rows of session "+withRows+", and the manifest says 4") {
		t.Errorf("dataset sessions over a miscounted manifest: exit %d: %s", code, refusal.String())
	}
	if after := dirFiles(t, reused); !maps.Equal(after, before) {
		t.Errorf("the refused export changed the reused --out: %v", after)
	}

	// The negative control: a part whose bytes changed is refused, naming the part.
	part[len(part)/2] ^= 0xff
	if err := os.WriteFile(filepath.Join(export, m.Parts[0].Name), part, 0o644); err != nil {
		t.Fatal(err)
	}
	refused := filepath.Join(dir, "refused")
	if code, _, errText := ingest(refused); code == 0 || !strings.Contains(errText, "part-000.jsonl.gz") {
		t.Fatalf("dataset sessions over a changed part: exit %d: %s", code, errText)
	}
	if _, err := os.Stat(refused); !os.IsNotExist(err) {
		t.Errorf("the refused export made its --out (%v)", err)
	}
}

// export-sessions.sh refuses an --out inside a repository, given whole or relative to the
// directory it runs in, before it makes the directory or reads the store, as every step of the
// dataset refuses one.
func TestExportSessionsScriptRefusesAnOutInsideARepository(t *testing.T) {
	for _, tool := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("export-sessions.sh needs %s, which is not on PATH", tool)
		}
	}
	script, err := filepath.Abs("export-sessions.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	repo, bin, tqLog := filepath.Join(dir, "repo"), filepath.Join(dir, "bin"), filepath.Join(dir, "tq.log")
	list, exclude := filepath.Join(dir, "sessions.tsv"), filepath.Join(dir, "exclude.txt")
	for path, content := range map[string]string{filepath.Join(repo, ".jj", "repo"): "", list: "", exclude: "", filepath.Join(bin, "tq"): standInTQ} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, out := range []string{filepath.Join(repo, "data", "export"), "export"} {
		cmd := exec.Command("bash", script, "--sessions", list, "--exclude", exclude, "--out", out)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TQ_LOG="+tqLog, "TQ_ROWS="+exclude)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		var exit *exec.ExitError
		if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 2 ||
			!strings.Contains(stderr.String(), "export-sessions: --out "+out+" is inside the repository at ") || !strings.Contains(stderr.String(), "repo: its files hold Dispatch text") {
			t.Errorf("export-sessions.sh --out %s, inside a repository: %v\n%s", out, err, stderr.String())
		}
	}
	for _, made := range []string{filepath.Join(repo, "data"), filepath.Join(repo, "export"), tqLog} {
		if _, err := os.Stat(made); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the refused export made %s (%v)", made, err)
		}
	}
}

// A pack to the name of an earlier, larger pack removes that pack's manifest and parts, so build
// --from the directory reads the new bundle and not the earlier inputs.
func TestDatasetPackOverALargerBundleRemovesItsParts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snapshot, verdicts := standardFixture().write(t, dir)
	bundle := filepath.Join(dir, "bundle", "dataset.tar.gz")
	if err := os.MkdirAll(filepath.Dir(bundle), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, parts, err := packBundle(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed}, bundle, 4096); err != nil || len(parts) < 3 {
		t.Fatalf("the larger pack wrote %v (%v), want parts and a manifest", parts, err)
	}
	if _, parts, err := packBundle(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed + 1}, bundle, dsPartSize); err != nil || len(parts) != 0 {
		t.Fatalf("the smaller pack wrote %v (%v), want the bundle alone", parts, err)
	}
	if left := slices.Sorted(maps.Keys(dirFiles(t, filepath.Dir(bundle)))); !slices.Equal(left, []string{"dataset.tar.gz"}) {
		t.Errorf("the bundle's directory holds %v, want the new bundle alone", left)
	}
	out := filepath.Join(dir, "out")
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"dataset", "build", "--from", filepath.Dir(bundle), "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("build --from the directory: exit %d: %s", code, stderr.String())
	}
	var counts dsCounts
	if data, err := os.ReadFile(filepath.Join(out, "counts.json")); err != nil || json.Unmarshal(data, &counts) != nil || counts.Seed != fixtureSeed+1 {
		t.Errorf("build --from the directory read the seed %d (%v), want the new bundle's %d", counts.Seed, err, fixtureSeed+1)
	}
}

// Every dataset step refuses an --out inside a repository, as the harness does, and makes no
// directory there: each writes Dispatch or session text. fetch and named need --owner: they
// hold no default owner.
func TestDatasetStepsRefuseAnOutInsideARepository(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".jj"), 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(repo, "out")
	t.Setenv("DISPATCH_URL", "http://127.0.0.1:1")
	t.Setenv("DISPATCH_TOKEN_FILE", filepath.Join(repo, "token"))
	outside := t.TempDir()
	for _, args := range [][]string{
		{"fetch", "--end", fixtureEnd, "--owner", fixtureOwner, "--out", inside},
		{"fetch", "--end", fixtureEnd, "--owner", fixtureOwner, "--out", filepath.Join(outside, "snapshot.jsonl.gz"), "--work", inside},
		{"classify", "--snapshot", "snapshot.jsonl.gz", "--out", inside},
		{"build", "--snapshot", "snapshot.jsonl.gz", "--verdicts", "verdicts.jsonl", "--seed", "1", "--out", inside},
		{"build", "--from", "bundle.tar.gz", "--out", inside},
		{"pack", "--snapshot", "snapshot.jsonl.gz", "--verdicts", "verdicts.jsonl", "--seed", "1", "--out", inside},
		{"sessions", "--manifest", "manifest.json", "--cases", "build", "--out", inside},
		{"owner-texts", "--snapshot", "snapshot.jsonl.gz", "--out", inside},
		{"followups", "--from", "bundle.tar.gz", "--cases", "build", "--out", inside},
		{"named", "--doc", "NAMED-2/notes-md@2", "--owner", fixtureOwner, "--out", inside},
		{"lists", "--cases", "build", "--verdicts", "verdicts.jsonl", "--plain-labels", "labels.jsonl", "--out", inside},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), append([]string{"dataset"}, args...), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "is inside the repository at "+repo) {
			t.Errorf("dataset %v: exit %d: %s; want 2 and the refusal", args, code, stderr.String())
		}
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Errorf("a refused step made %s (%v)", inside, err)
	}
	for _, args := range [][]string{
		{"fetch", "--end", fixtureEnd, "--out", filepath.Join(outside, "snapshot.jsonl.gz")},
		{"named", "--doc", "NAMED-2/notes-md@2", "--out", filepath.Join(outside, "named")},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), append([]string{"dataset"}, args...), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--owner are required") {
			t.Errorf("dataset %v with no --owner: exit %d: %s; want 2 and the refusal", args, code, stderr.String())
		}
	}
}
