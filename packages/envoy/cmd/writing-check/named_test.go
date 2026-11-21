package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/cmd/writing-check/harness"
)

// The texts of the named fixture, invented for these tests. Version 3 of the notes holds all of
// version 2 and one paragraph more, so its whole text and its new blocks differ.
const (
	namedSpec1  = "# Spec\n\nThe work builds the zork."
	namedSpec2  = "# Spec\n\nThe work builds the zork and its gantry."
	namedNotes1 = "# Notes\n\nState: nothing is open."
	namedNotes2 = "# Notes\n\nItem one waits for the gantry.\n\nState: open."
	namedNotes3 = namedNotes2 + "\n\nItem two waits for the rails.\n\nState: done."
	namedNotes4 = namedNotes3 + "\n\nA line the owner added himself."
)

// namedFixture is the issue NAMED-2 under NAMED-1 with three documents: its spec, notes.md with
// four versions, and gone.md, whose version the stand-in does not serve. The issue's title
// changes between versions 2 and 3 of the notes, and the spec's second version comes between
// them too. It answers the events, the Markdown of each version by "<artifact>/versions/<n>",
// and the ids of the events that wrote versions 2 and 3 of the notes.
func namedFixture() (f *dsFixture, versions map[string]string, second, third int64) {
	f = newDSFixture()
	f.issue("NAMED-2", "NAMED-1", "First title", namedSpec1)
	document := func(id, name string, actor map[string]any) {
		f.event("artifact.created", actor, "NAMED-2", map[string]any{"artifact": map[string]any{
			"id": id, "kind": "doc", "name": name, "issue_key": "NAMED-2", "versions": []map[string]any{{"number": 1}}}})
	}
	version := func(id, name string, number int, actor map[string]any) int64 {
		return f.event("artifact.version", actor, "NAMED-2", map[string]any{
			"artifact_id": id, "name": name, "version": map[string]any{"number": number}})
	}
	document("art-gone", "gone.md", fixtureAgent)
	document("art-notes", "notes.md", fixtureAgent)
	second = version("art-notes", "notes.md", 2, fixtureAgent)
	f.event("issue.updated", fixtureAgent, "NAMED-2", map[string]any{"key": "NAMED-2", "project": "NAMED", "title": "Second title"})
	version("art-NAMED-2", "spec.md", 2, fixtureAgent)
	third = version("art-notes", "notes.md", 3, fixtureOther)
	version("art-notes", "notes.md", 4, fixturePerson)
	versions = map[string]string{
		"art-NAMED-2/versions/1": namedSpec1, "art-NAMED-2/versions/2": namedSpec2,
		"art-notes/versions/1": namedNotes1, "art-notes/versions/2": namedNotes2,
		"art-notes/versions/3": namedNotes3, "art-notes/versions/4": namedNotes4,
	}
	return f, versions, second, third
}

// namedAskIssues are the asks the stand-in Dispatch serves, each with the issue it gives the ask.
// No event of the fixture opens ask-unopened.
var namedAskIssues = map[string]string{"ask-named": "NAMED-2", "ask-unopened": "NAMED-2", "ask-elsewhere": "OTHER-1"}

// standInDispatch serves the routes `dataset named` reads, to a caller with the bearer token
// "stand-in": a document of NAMED-2 by its slug or its file name, each ask of namedAskIssues, the
// issue's event log two events a page by sequence, the parent issue, and the versions. It sets
// the environment the command reads its Dispatch from.
func standInDispatch(t *testing.T, f *dsFixture, versions map[string]string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer stand-in" {
			http.Error(w, "no bearer token", http.StatusUnauthorized)
			return
		}
		path := r.URL.Path
		switch {
		case path == "/api/v1/issues/NAMED-2/artifacts/notes-md", path == "/api/v1/issues/NAMED-2/artifacts/notes.md":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "art-notes", "issue_key": "NAMED-2", "slug": "notes-md", "name": "notes.md", "kind": "doc"})
		case path == "/api/v1/issues/NAMED-2/artifacts/picture-png":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "art-picture", "issue_key": "NAMED-2", "slug": "picture-png", "name": "picture.png", "kind": "image"})
		case path == "/api/v1/issues/NAMED-2/events":
			after, _ := strconv.Atoi(r.URL.Query().Get("after"))
			page := []map[string]any{}
			for i, ev := range f.events {
				if seq := i + 1; seq > after && len(page) < 2 {
					row := maps.Clone(ev)
					row["seq"] = seq
					page = append(page, row)
				}
			}
			_ = json.NewEncoder(w).Encode(page)
		case path == "/api/v1/issues/NAMED-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"key": "NAMED-1", "parent": nil})
		case strings.HasPrefix(path, "/api/v1/artifacts/") && versions[strings.TrimPrefix(path, "/api/v1/artifacts/")] != "":
			_ = json.NewEncoder(w).Encode(map[string]any{"markdown": versions[strings.TrimPrefix(path, "/api/v1/artifacts/")]})
		case strings.HasPrefix(path, "/api/v1/asks/") && namedAskIssues[strings.TrimPrefix(path, "/api/v1/asks/")] != "":
			id := strings.TrimPrefix(path, "/api/v1/asks/")
			_ = json.NewEncoder(w).Encode(map[string]any{"ask": map[string]any{"id": id, "issue_key": namedAskIssues[id], "kind": "question"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("stand-in\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISPATCH_URL", server.URL)
	t.Setenv("DISPATCH_TOKEN_FILE", token)
}

// dataset named writes a build directory the harness's own case reader loads: each named version
// is a plain case of the development part, with an id of its issue, document and version, the
// version's whole Markdown as its text, the writer Dispatch records, and the record build gives a
// document version: the issue's title, its root and each document as they stood before the write.
func TestDatasetNamedWritesCasesTheHarnessLoads(t *testing.T) {
	f, versions, second, third := namedFixture()
	standInDispatch(t, f, versions)
	out := filepath.Join(t.TempDir(), "named")
	var stdout, stderr bytes.Buffer
	// The document goes by its file name here and by its slug in the case's id.
	if code := run(t.Context(), []string{"dataset", "named", "--doc", "NAMED-2/notes.md@3,2", "--out", out, "--owner", fixtureOwner}, &stdout, &stderr); code != 0 {
		t.Fatalf("named: exit %d: %s", code, stderr.String())
	}
	ds, err := harness.LoadDataset(out, harness.PartDevelopment, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.Cases) != 2 {
		t.Fatalf("the development part holds %d cases, want the 2 versions named", len(ds.Cases))
	}
	agent, other := fixtureAgent["id"].(string), fixtureOther["id"].(string)
	for i, want := range []struct {
		id, text, writer, title string
		event                   int64
		documents               []harness.CaseDocument
		record                  string
	}{
		{"plain/named:NAMED-2/notes-md@2", namedNotes2, agent, "First title", second,
			[]harness.CaseDocument{{ArtifactID: "art-NAMED-2", Name: "spec.md", Version: 1}, {ArtifactID: "art-gone", Name: "gone.md", Version: 1, Missing: true}, {ArtifactID: "art-notes", Name: "notes.md", Version: 1}},
			"<document name=\"notes.md\" version=1 role=\"the document the text is a version of, as it stood before the text\">\n" + namedNotes1},
		{"plain/named:NAMED-2/notes-md@3", namedNotes3, other, "Second title", third,
			[]harness.CaseDocument{{ArtifactID: "art-NAMED-2", Name: "spec.md", Version: 2}, {ArtifactID: "art-gone", Name: "gone.md", Version: 1, Missing: true}, {ArtifactID: "art-notes", Name: "notes.md", Version: 2}},
			"<document name=\"notes.md\" version=2 role=\"the document the text is a version of, as it stood before the text\">\n" + namedNotes2},
	} {
		c := ds.Cases[i]
		if c.ID != want.id || c.Group != harness.GroupPlain || c.Part != harness.PartDevelopment {
			t.Errorf("case %d is %s of group %s in part %s, want %s of plain in development", i, c.ID, c.Group, c.Part, want.id)
		}
		if c.Agent.Text != want.text {
			t.Errorf("%s: the text is %q, want the version's whole Markdown %q", c.ID, c.Agent.Text, want.text)
		}
		if c.Kind != "document" || c.Conversation || c.Agent.Key != "version:art-notes:"+want.id[len(want.id)-1:] || c.Agent.EventID != want.event || c.Agent.EventType != "artifact.version" {
			t.Errorf("%s: kind %s, conversation %t, key %s, event %d (%s); want a document outside a conversation, written by event %d", c.ID, c.Kind, c.Conversation, c.Agent.Key, c.Agent.EventID, c.Agent.EventType, want.event)
		}
		if c.WriterSession != want.writer || c.Agent.ActorKind != "session" || c.Agent.Actor != want.writer || c.WriteTime != c.Agent.Time || c.WriteTime == "" {
			t.Errorf("%s: writer session %q, actor %s %q, written %q; want session %s, the event's actor", c.ID, c.WriterSession, c.Agent.ActorKind, c.Agent.Actor, c.WriteTime, want.writer)
		}
		if c.Tree != "NAMED-1" || c.SplitGroup != "NAMED-1" {
			t.Errorf("%s: tree %s, split group %s; want the root of its issue, NAMED-1, for both", c.ID, c.Tree, c.SplitGroup)
		}
		is := c.Record.Issue
		if is == nil || is.Key != "NAMED-2" || is.Title != want.title || is.Parent != "NAMED-1" || is.Root != "NAMED-1" || c.Record.Project != "NAMED" {
			t.Errorf("%s: the record's issue is %+v in project %q, want NAMED-2 %q under NAMED-1 in NAMED", c.ID, is, c.Record.Project, want.title)
		}
		if !slices.Equal(c.Record.Documents, want.documents) {
			t.Errorf("%s: the record's documents are %+v, want %+v", c.ID, c.Record.Documents, want.documents)
		}
		if part := ds.RecordPart(c); !strings.Contains(part, want.record) || !strings.Contains(part, "<other_documents>\n- gone.md\n") {
			t.Errorf("%s: the record a grading call reads lacks the version before the text, or the name of the document Dispatch did not serve:\n%s", c.ID, part)
		}
		if c.OwnerReply != nil || c.Question != nil || c.Phrase != nil || len(c.Record.Thread) != 0 {
			t.Errorf("%s holds an owner reply, a question, a phrase or a thread; a named version has none", c.ID)
		}
	}
	if held, err := harness.LoadDataset(out, harness.PartHeldOut, 0); err != nil || len(held.Cases) != 0 {
		t.Errorf("the held-out part holds %d cases (%v), want none", len(held.Cases), err)
	}

	// What `harness load` and export-sessions.sh read beside the cases.
	var counts dsNamedCounts
	data, err := os.ReadFile(filepath.Join(out, "counts.json"))
	if err != nil || json.Unmarshal(data, &counts) != nil {
		t.Fatalf("counts.json: %v: %s", err, data)
	}
	if counts.Owner != fixtureOwner || counts.Cases != 2 || strings.Join(counts.Named, " ") != "NAMED-2/notes-md@2 NAMED-2/notes-md@3" {
		t.Errorf("counts.json is %+v, want the owner %s and the 2 versions named", counts, fixtureOwner)
	}
	sessions, err := os.ReadFile(filepath.Join(out, "sessions.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	if want := agent + "\t" + ds.Cases[0].WriteTime + "\n" + other + "\t" + ds.Cases[1].WriteTime + "\n"; string(sessions) != want {
		t.Errorf("sessions.tsv is %q, want %q", sessions, want)
	}

	// The same versions, named in another order and twice, write the same bytes.
	again := filepath.Join(t.TempDir(), "again")
	if code := run(t.Context(), []string{"dataset", "named", "--doc", "NAMED-2/notes-md@2", "--doc", "NAMED-2/notes-md@3,2", "--out", again, "--owner", fixtureOwner}, &stdout, &stderr); code != 0 {
		t.Fatalf("named again: exit %d: %s", code, stderr.String())
	}
	for _, name := range []string{"cases.jsonl.gz", "documents.jsonl.gz", "sessions.tsv", "counts.json"} {
		first, err1 := os.ReadFile(filepath.Join(out, name))
		second, err2 := os.ReadFile(filepath.Join(again, name))
		if err1 != nil || err2 != nil || !bytes.Equal(first, second) {
			t.Errorf("%s differs between two runs that name the same versions (%v, %v)", name, err1, err2)
		}
	}
}

// A version a person wrote himself is a case with the person as its actor and no writer session,
// and the command says so.
func TestDatasetNamedRecordsAPersonAsTheAuthor(t *testing.T) {
	f, versions, _, _ := namedFixture()
	standInDispatch(t, f, versions)
	out := filepath.Join(t.TempDir(), "named")
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"dataset", "named", "--doc", "NAMED-2/notes-md@4", "--out", out, "--owner", fixtureOwner}, &stdout, &stderr); code != 0 {
		t.Fatalf("named: exit %d: %s", code, stderr.String())
	}
	ds, err := harness.LoadDataset(out, harness.PartDevelopment, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.Cases) != 1 {
		t.Fatalf("the development part holds %d cases, want the one version named", len(ds.Cases))
	}
	c := ds.Cases[0]
	if c.ID != "plain/named:NAMED-2/notes-md@4" || c.Agent.Text != namedNotes4 || c.WriterSession != "" || c.Agent.ActorKind != "user" || c.Agent.Actor != fixtureOwner {
		t.Errorf("the case is %s by %s %q with writer session %q, want version 4 by the person with no session", c.ID, c.Agent.ActorKind, c.Agent.Actor, c.WriterSession)
	}
	if !strings.Contains(stderr.String(), "NAMED-2/notes-md@4 was written by user "+fixtureOwner) {
		t.Errorf("stderr does not say a person wrote the version: %s", stderr.String())
	}
}

// An ask is a case of its own: the question and the options its ask.opened wrote, not a later
// edit, by the writer Dispatch records, with the record build gives an ask: the issue's title and
// each document as they stood before the ask was opened. The ask heads its thread, so the record
// holds no thread.
func TestDatasetNamedMakesACaseOfAnAsk(t *testing.T) {
	f, versions, _, _ := namedFixture()
	opened := f.event("ask.opened", fixtureAgent, "NAMED-2", map[string]any{"id": "ask-named", "issue_key": "NAMED-2", "kind": "question",
		"question": "Which gantry goes first?", "options": []map[string]any{{"label": "East", "description": "the east gantry"}, {"label": "West"}}})
	f.event("ask.edited", fixtureAgent, "NAMED-2", map[string]any{"id": "ask-named", "issue_key": "NAMED-2", "kind": "question",
		"question": "Which gantry do we build first?", "options": []map[string]any{{"label": "East"}, {"label": "West"}}})
	f.event("artifact.version", fixtureAgent, "NAMED-2", map[string]any{"artifact_id": "art-notes", "name": "notes.md", "version": map[string]any{"number": 5}})
	versions["art-notes/versions/5"] = namedNotes4 + "\n\nA line written after the ask."
	standInDispatch(t, f, versions)
	out := filepath.Join(t.TempDir(), "named")
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"dataset", "named", "--ask", "NAMED-2/ask-named", "--doc", "NAMED-2/notes-md@2", "--out", out, "--owner", fixtureOwner}, &stdout, &stderr); code != 0 {
		t.Fatalf("named: exit %d: %s", code, stderr.String())
	}
	ds, err := harness.LoadDataset(out, harness.PartDevelopment, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.Cases) != 2 || ds.Cases[0].ID != "plain/named:NAMED-2/ask:ask-named" || ds.Cases[1].ID != "plain/named:NAMED-2/notes-md@2" {
		t.Fatalf("the development part holds %d cases, want the ask's and the version's", len(ds.Cases))
	}
	c, agent := ds.Cases[0], fixtureAgent["id"].(string)
	if c.Group != harness.GroupPlain || c.Part != harness.PartDevelopment || c.Kind != "ask" || c.Conversation {
		t.Errorf("%s is a %s of group %s in part %s (conversation %t), want a plain ask in development outside a conversation", c.ID, c.Kind, c.Group, c.Part, c.Conversation)
	}
	if want := "Which gantry goes first?\n- East: the east gantry\n- West"; c.Agent.Text != want {
		t.Errorf("the text is %q, want the question and the options as the ask was opened, %q", c.Agent.Text, want)
	}
	if c.Agent.Key != "ask:ask-named" || c.Agent.EventID != opened || c.Agent.EventType != "ask.opened" {
		t.Errorf("the text is %s of event %d (%s), want ask:ask-named of event %d (ask.opened)", c.Agent.Key, c.Agent.EventID, c.Agent.EventType, opened)
	}
	if c.WriterSession != agent || c.Agent.ActorKind != "session" || c.Agent.Actor != agent || c.WriteTime != c.Agent.Time {
		t.Errorf("writer session %q, actor %s %q, written %q; want session %s, the event's actor", c.WriterSession, c.Agent.ActorKind, c.Agent.Actor, c.WriteTime, agent)
	}
	if c.Tree != "NAMED-1" || c.SplitGroup != "NAMED-1" {
		t.Errorf("tree %s, split group %s; want the root of its issue, NAMED-1, for both", c.Tree, c.SplitGroup)
	}
	if is := c.Record.Issue; is == nil || is.Key != "NAMED-2" || is.Title != "Second title" || is.Root != "NAMED-1" {
		t.Errorf("the record's issue is %+v, want NAMED-2 \"Second title\" under NAMED-1", is)
	}
	documents := []harness.CaseDocument{{ArtifactID: "art-NAMED-2", Name: "spec.md", Version: 2}, {ArtifactID: "art-gone", Name: "gone.md", Version: 1, Missing: true}, {ArtifactID: "art-notes", Name: "notes.md", Version: 4}}
	if !slices.Equal(c.Record.Documents, documents) {
		t.Errorf("the record's documents are %+v, want each as it stood before the ask, %+v", c.Record.Documents, documents)
	}
	if part := ds.RecordPart(c); !strings.Contains(part, namedNotes4+"\n</document>") {
		t.Errorf("the record a grading call reads lacks version 4 of the notes:\n%s", part)
	}
	if c.OwnerReply != nil || c.Question != nil || c.Phrase != nil || len(c.Record.Thread) != 0 {
		t.Errorf("%s holds an owner reply, a question, a phrase or a thread; an ask named here has none", c.ID)
	}
	var counts dsNamedCounts
	data, err := os.ReadFile(filepath.Join(out, "counts.json"))
	if err != nil || json.Unmarshal(data, &counts) != nil {
		t.Fatalf("counts.json: %v: %s", err, data)
	}
	if counts.Cases != 2 || strings.Join(counts.Named, " ") != "NAMED-2/ask:ask-named NAMED-2/notes-md@2" {
		t.Errorf("counts.json is %+v, want the ask and the version named", counts)
	}
}

// The command refuses what it cannot build a case from, and writes no file then.
func TestDatasetNamedRefusals(t *testing.T) {
	f, versions, _, _ := namedFixture()
	standInDispatch(t, f, versions)
	for _, tc := range []struct {
		name  string
		flag  string
		value string
		code  int
		want  string
	}{
		{"a version no event wrote", "--doc", "NAMED-2/notes-md@9", 1, "NAMED-2/notes-md@9: no event of its issue wrote this version"},
		{"a document the issue does not hold", "--doc", "NAMED-2/absent-md@1", 1, "document absent-md of NAMED-2: not found"},
		{"an image", "--doc", "NAMED-2/picture-png@1", 1, "picture-png of NAMED-2 is not a document: Dispatch gives its kind as image"},
		{"no issue", "--doc", "notes-md@1", 2, "is not <issue>/<document>@<version>[,<version>...]"},
		{"no version", "--doc", "NAMED-2/notes-md", 2, "is not <issue>/<document>@<version>[,<version>...]"},
		{"a version that is no number", "--doc", "NAMED-2/notes-md@2,latest", 2, `version "latest" is not a number above 0`},
		{"an ask no event opened", "--ask", "NAMED-2/ask-unopened", 1, "NAMED-2/ask:ask-unopened: no event of its issue opened this ask with words"},
		{"an ask of another issue", "--ask", "NAMED-2/ask-elsewhere", 1, `ask ask-elsewhere is not an ask of NAMED-2: Dispatch gives its issue as "OTHER-1"`},
		{"an ask Dispatch does not hold", "--ask", "NAMED-2/ask-absent", 1, "ask ask-absent of NAMED-2: not found"},
		{"an ask with no issue", "--ask", "ask-named", 2, "is not <issue>/<ask id>"},
	} {
		out := filepath.Join(t.TempDir(), "named")
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), []string{"dataset", "named", tc.flag, tc.value, "--out", out, "--owner", fixtureOwner}, &stdout, &stderr)
		if code != tc.code || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%s: exit %d, stderr %q; want exit %d and %q", tc.name, code, stderr.String(), tc.code, tc.want)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Errorf("%s: the refused run wrote %s (%v)", tc.name, out, err)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"dataset", "named", "--out", filepath.Join(t.TempDir(), "named"), "--owner", fixtureOwner}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--doc or --ask, --out and --owner are required") {
		t.Errorf("with neither --doc nor --ask: exit %d, stderr %q; want exit 2", code, stderr.String())
	}
	stderr.Reset()
	t.Setenv("DISPATCH_TOKEN_FILE", "")
	if code := run(t.Context(), []string{"dataset", "named", "--doc", "NAMED-2/notes-md@2", "--out", filepath.Join(t.TempDir(), "named"), "--owner", fixtureOwner}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "DISPATCH_URL and DISPATCH_TOKEN_FILE must be set") {
		t.Errorf("with no token file named: exit %d, stderr %q; want exit 2", code, stderr.String())
	}
}
