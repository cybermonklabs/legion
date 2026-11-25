package api

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// gateSeeded is one issue the gate test seeds, with the row it puts in alice's Inbox and the gate
// that issue was seeded to: each line's step, its document ("" for none) and that document's
// approval state ("" for none).
type gateSeeded struct {
	issue model.Issue
	lines []gateSeededLine
}

type gateSeededLine struct {
	step, document, state string
}

// gateSeed is how one issue is seeded: its project and parent, whether the project gates
// [criteria, spec], and the state of its spec and of its Verification document (missing for an
// issue that holds none). Only a parent of the issue's own project makes it a child: under an
// issue of another project it is a root of its own project, and stands in that project's gate.
type gateSeed struct {
	project, parent    string
	gated              bool
	spec, verification string
}

// seedGateIssue writes one issue as seed says, through the API alone, and one row of it in alice's
// Inbox: the open approval request where its spec awaits approval, else a thread alice started
// that bob answered.
func seedGateIssue(t *testing.T, handler http.Handler, seed gateSeed, title string) gateSeeded {
	t.Helper()
	input := map[string]any{"project": seed.project, "title": title, "spec": "# Spec\n\n" + title + ".\n"}
	if seed.parent != "" {
		input["parent"] = seed.parent
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", input, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create %q: status=%d body=%s", title, created.Code, created.Body.String())
	}
	issue := decodeBody[model.Issue](t, created)

	review := func(id, state string) {
		t.Helper()
		body := map[string]any{"state": state}
		if state == "changes_requested" {
			body["reason"] = "Name the rollback path."
		}
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+id+"/reviews", body, "alice"); response.Code != http.StatusCreated && response.Code != http.StatusOK {
			t.Fatalf("review %s of %s as %s: status=%d body=%s", id, issue.Key, state, response.Code, response.Body.String())
		}
	}
	verification := ""
	if seed.verification != "missing" {
		verification = uploadIssueDocument(t, handler, issue.Key, "verification.md", "# Verification\n\nOne check.\n").ID
		if seed.verification != "draft" {
			review(verification, seed.verification)
		}
	}
	switch seed.spec {
	case "approved", "changes_requested":
		review(issue.PrimaryArtifactID, seed.spec)
	case "awaiting":
		if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{
			"actor": sessionActor(), "summary": "The spec of " + issue.Key + " is ready to read.",
		}); response.Code != http.StatusCreated && response.Code != http.StatusOK {
			t.Fatalf("request approval of %s's spec: status=%d body=%s", issue.Key, response.Code, response.Body.String())
		}
	}
	if seed.spec != "awaiting" {
		thread := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Alice asks about " + title + "."}, "alice")
		createThreadComment(t, handler, issue.Key, map[string]any{"body": "Bob answers.", "reply_to": thread.ID}, "bob")
	}

	primary := gateSeededLine{document: issue.PrimaryArtifactID, state: seed.spec}
	if !seed.gated || strings.HasPrefix(seed.parent, seed.project+"-") {
		return gateSeeded{issue: issue, lines: []gateSeededLine{primary}}
	}
	criteria := gateSeededLine{step: model.GatedStepCriteria, document: verification, state: seed.verification}
	if verification == "" {
		criteria.state = ""
	}
	primary.step = model.GatedStepSpec
	return gateSeeded{issue: issue, lines: []gateSeededLine{criteria, primary}}
}

// gateLinesOf is lines as the seed spells them, so a read and a seed compare as one shape.
func gateLinesOf(lines []approvalGateLine) []gateSeededLine {
	read := make([]gateSeededLine, len(lines))
	for index, line := range lines {
		read[index].step = line.Step
		if line.ArtifactID != nil {
			read[index].document = *line.ArtifactID
		}
		if line.Approval != nil {
			read[index].state = line.Approval.State
		}
	}
	return read
}

// readIssueApprovalGate is the approval gate the issue read answers for key, as alice reads it.
func readIssueApprovalGate(t *testing.T, handler http.Handler, key string) []approvalGateLine {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		ApprovalGate []approvalGateLine `json:"approval_gate"`
	}](t, response).ApprovalGate
}

// gateVersionProbe is the one shape in which the one read may name artifact_versions: a gate
// document's latest version as max(number) over that document's own rows (meInboxGateReads),
// which Postgres answers from the last entry of the document's range of the unique (artifact_id,
// number) index rather than by reading its version rows. Its three aliases are the one alias of
// the version rows it ranges over, which versionReads holds them to.
var gateVersionProbe = regexp.MustCompile(`\(select max\((\w+)\.number\) from artifact_versions (\w+) where (\w+)\.artifact_id = \w+\.id\)`)

// versionReads holds texts, the text of every query one read sent, to gateVersionProbe: how many
// probes they hold, and each place they name artifact_versions other than in a probe, with the
// text around it. Such a place is a read of a document's version rows, which can pick the latest
// in Go from one statement as well as the probe can, so a count of statements alone lets it
// through.
func versionReads(texts []string) (probes int, beside []string) {
	for _, text := range texts {
		rest := gateVersionProbe.ReplaceAllStringFunc(strings.Join(strings.Fields(text), " "), func(probe string) string {
			aliases := gateVersionProbe.FindStringSubmatch(probe)
			if aliases[1] != aliases[2] || aliases[3] != aliases[2] {
				return probe
			}
			probes++
			return "(the probe)"
		})
		for {
			at := strings.Index(rest, "artifact_versions")
			if at < 0 {
				break
			}
			beside = append(beside, rest[max(0, at-160):min(len(rest), at+160)])
			rest = rest[at+len("artifact_versions"):]
		}
	}
	return probes, beside
}

// newGateRig is newMeInboxRig's test server, its store traced by a meInboxStatements of its own
// that counts each statement of a batch on its own (eachOfABatch) for the one request marked
// countedRead, the read the test sends, and its horizon observing through the marked horizon pool
// (markedHorizonPool), so ownStatements leaves out the horizon's own. It is not newMeInboxRig
// because that rig's recorder may count by a window the test arms rather than by the marked
// request (legion/INFRA-1152's f43afdc7), which this test does not arm; a recorder built here
// counts the marked request on either.
func newGateRig(t *testing.T) *meInboxRig {
	t.Helper()
	database := storetest.Open(t)
	statements := &meInboxStatements{eachOfABatch: true}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, statements, nil)}
	handler, _, _ := newTestServer(t, testServerOptions{
		store:     traced,
		identity:  headerIdentity(database),
		horizonDB: markedHorizonPool(t, traced),
		// A document's settlement is database work of its own on the traced pool; a delay no
		// test reaches keeps it out of the read's statements.
		settle: time.Hour,
	})
	return &meInboxRig{database: database, handler: handler, statements: statements}
}

// The one read carries each entry's approval gate in its one statement (c-approval-one-statement):
// over 3 issues with a row each and over 12, alice's Inbox is one statement. The rig counts each
// statement of a batch on its own (newGateRig's eachOfABatch) and leaves out only the four that
// frame the read's statement in its read-only transaction and read nothing: its begin, its two
// set local settings and its commit (meInboxReadStatements). So no statement beside it, in its
// batch or not, reads a gate document's versions, its review or its request for each document;
// the text of every statement the read sent reads artifact_versions only through the probe of
// each gate document's latest version (gateVersionProbe), so it reads no document's version rows
// to pick the latest among them; each row carries the gate its issue was seeded to, on its ask
// and on its thread; and the issue read answers the same gate, field for field, so one
// computation answers both reads.
func TestTheOneReadCarriesTheGateInItsOneStatement(t *testing.T) {
	rig := newGateRig(t)
	handler := rig.handler
	createReferenceAPIProject(t, handler, "GATE")
	publishGatedSteps(t, handler, "GATE", []string{model.GatedStepCriteria, model.GatedStepSpec})
	createReferenceAPIProject(t, handler, "FREE")

	seeded := map[string]gateSeeded{}
	seed := func(seed gateSeed, title string) model.Issue {
		t.Helper()
		issue := seedGateIssue(t, handler, seed, title)
		seeded[issue.issue.Key] = issue
		return issue.issue
	}
	// The first three: an issue of a project that gates nothing, whose spec is approved; a root
	// of the gating project with no Verification document and its spec under request; and that
	// root's child, sent back.
	seed(gateSeed{project: "FREE", spec: "approved", verification: "missing"}, "An approved spec")
	root := seed(gateSeed{project: "GATE", gated: true, spec: "awaiting", verification: "missing"}, "A root under request")
	seed(gateSeed{project: "GATE", gated: true, parent: root.Key, spec: "changes_requested", verification: "missing"}, "A child sent back")

	// read reads alice's Inbox on a request whose statements the rig records, and holds each
	// row's gate to what its issue was seeded to. It answers the statements the rig counts and
	// every statement the read sent, each of its batch on its own.
	read := func(rows int) (meInboxResponse, []string, []string) {
		t.Helper()
		rig.statements.ownStatements()
		marked := context.WithValue(context.Background(), countedRead{}, true)
		answer := decodeMeInbox(t, meInboxRequestAs(t, handler, marked, "alice", url.Values{"state": {myRepliesStateInbox}}))
		sent := rig.statements.ownStatements()
		if len(answer.Entries) != rows {
			t.Fatalf("alice's Inbox holds %d rows, want the %d seeded: %v", len(answer.Entries), rows, meInboxNodes(answer))
		}
		asks, threads := 0, 0
		for _, entry := range answer.Entries {
			var key string
			switch {
			case entry.Ask != nil && entry.Ask.Issue != nil:
				key = entry.Ask.Issue.Key
				asks++
			case entry.Thread != nil && entry.Thread.Issue != nil:
				key = entry.Thread.Issue.Key
				threads++
			default:
				t.Fatalf("entry %s %s is no row of an issue", entry.Node.Kind, entry.Node.ID)
			}
			want, held := seeded[key]
			if !held {
				t.Fatalf("entry %s %s belongs to %s, which this test did not seed", entry.Node.Kind, entry.Node.ID, key)
			}
			if got := gateLinesOf(entry.ApprovalGate); !slices.Equal(got, want.lines) {
				t.Errorf("the row of %s carries the gate %+v, want the seeded %+v", key, got, want.lines)
			}
		}
		if asks == 0 || threads == 0 {
			t.Fatalf("alice's Inbox holds %d ask rows and %d thread rows, want both kinds", asks, threads)
		}
		return answer, meInboxReadStatements(sent), sent
	}

	_, three, threeSent := read(3)
	// Nine more, which make twelve: every state the seed writes, under both projects, as roots
	// and children, with and without a Verification document.
	seed(gateSeed{project: "FREE", spec: "draft", verification: "missing"}, "A spec nobody asked about")
	seed(gateSeed{project: "FREE", spec: "awaiting", verification: "draft"}, "An ungated spec under request")
	seed(gateSeed{project: "FREE", spec: "changes_requested", verification: "approved"}, "An ungated spec sent back")
	seed(gateSeed{project: "GATE", gated: true, spec: "draft", verification: "draft"}, "A root nobody asked about")
	seed(gateSeed{project: "GATE", gated: true, spec: "approved", verification: "approved"}, "A root approved twice")
	seed(gateSeed{project: "GATE", gated: true, spec: "awaiting", verification: "approved"}, "A root whose spec waits")
	seed(gateSeed{project: "GATE", gated: true, spec: "approved", verification: "changes_requested"}, "A root whose checks went back")
	seed(gateSeed{project: "GATE", gated: true, parent: root.Key, spec: "approved", verification: "missing"}, "An approved child")
	seed(gateSeed{project: "GATE", gated: true, parent: root.Key, spec: "draft", verification: "draft"}, "A child nobody asked about")
	twelve, twelveStatements, twelveSent := read(12)

	if len(three) != 1 || len(twelveStatements) != 1 {
		t.Fatalf("the 3-row read ran %d statements and the 12-row read %d, want 1 each, counting each statement of a batch "+
			"but the begin, the set local settings and the commit that frame the read's; every statement the 3-row read sent:\n%s\n"+
			"=== every statement the 12-row read sent:\n%s",
			len(three), len(twelveStatements), strings.Join(threeSent, "\n---\n"), strings.Join(twelveSent, "\n---\n"))
	}
	// One statement can pull every version row of each gate document and leave the latest to Go as
	// well as it can probe for it, so the text of what each read sent is held to the probe too.
	for _, counted := range []struct {
		rows int
		sent []string
	}{{3, threeSent}, {12, twelveSent}} {
		probes, beside := versionReads(counted.sent)
		if probes == 0 {
			heads := make([]string, 0, len(counted.sent))
			for _, text := range counted.sent {
				flat := strings.Join(strings.Fields(text), " ")
				heads = append(heads, flat[:min(len(flat), 100)])
			}
			t.Errorf("the %d-row read sent no max(number) probe of artifact_versions in its %d queries, which begin:\n%s",
				counted.rows, len(counted.sent), strings.Join(heads, "\n"))
		}
		if len(beside) > 0 {
			t.Errorf("the %d-row read names artifact_versions %d times beside the per-document max(number) probe, so it reads version rows:\n%s",
				counted.rows, len(beside), strings.Join(beside, "\n---\n"))
		}
	}

	// The issue read answers the gate each row carries, field for field.
	for _, entry := range twelve.Entries {
		var key string
		if entry.Ask != nil {
			key = entry.Ask.Issue.Key
		} else {
			key = entry.Thread.Issue.Key
		}
		issueGate := readIssueApprovalGate(t, handler, key)
		if len(issueGate) == 0 || !reflect.DeepEqual(issueGate, entry.ApprovalGate) {
			t.Errorf("%s's issue read answers the gate %+v, and its Inbox row carries %+v", key, issueGate, entry.ApprovalGate)
		}
	}
	if !t.Failed() {
		t.Logf("the 3-row read and the 12-row read each ran one statement, and neither names artifact_versions beside the max(number) probe")
	}
}

// An issue whose parent is an issue of another project is a root of its own project (gateRoot):
// a parent of another project passes nothing on, so the issue stands in its own project's gate
// and answers one line for each step that project gates, while an issue whose parent is of its
// own project is a child and answers the one line for its primary document. The issue read and
// the one read, whose statement spells the rule in SQL (meInboxGateReads), answer each issue the
// same gate. GATE gates [criteria, spec] and FREE nothing, so an issue of FREE under an issue of
// GATE is a root of FREE and answers the one line too: the steps are its own project's, never its
// parent's.
func TestAnIssueUnderAnIssueOfAnotherProjectIsARootOfItsOwnProjectsGate(t *testing.T) {
	handler := newMeInboxRig(t).handler
	createReferenceAPIProject(t, handler, "GATE")
	publishGatedSteps(t, handler, "GATE", []string{model.GatedStepCriteria, model.GatedStepSpec})
	createReferenceAPIProject(t, handler, "FREE")

	steps := []string{model.GatedStepCriteria, model.GatedStepSpec}
	elsewhere := seedGateIssue(t, handler, gateSeed{project: "FREE", spec: "approved", verification: "missing"}, "A parent in another project")
	own := seedGateIssue(t, handler, gateSeed{project: "GATE", gated: true, spec: "approved", verification: "approved"}, "A parent in its own project")
	cases := []struct {
		seeded gateSeeded
		steps  []string
		is     string
	}{
		{elsewhere, []string{""}, "an issue of FREE with no parent"},
		{own, steps, "an issue of GATE with no parent"},
		{seedGateIssue(t, handler, gateSeed{project: "GATE", gated: true, parent: elsewhere.issue.Key, spec: "awaiting", verification: "draft"}, "A root under another project's issue"),
			steps, "an issue of GATE under an issue of FREE"},
		{seedGateIssue(t, handler, gateSeed{project: "GATE", gated: true, parent: own.issue.Key, spec: "changes_requested", verification: "draft"}, "A child under its own project's issue"),
			[]string{""}, "an issue of GATE under an issue of GATE"},
		{seedGateIssue(t, handler, gateSeed{project: "FREE", parent: own.issue.Key, spec: "draft", verification: "missing"}, "An ungated issue under the gating project's issue"),
			[]string{""}, "an issue of FREE under an issue of GATE"},
	}

	rows := map[string][]approvalGateLine{}
	for _, entry := range readMeInboxAs(t, handler, "alice", url.Values{"state": {myRepliesStateInbox}}).Entries {
		switch {
		case entry.Ask != nil && entry.Ask.Issue != nil:
			rows[entry.Ask.Issue.Key] = entry.ApprovalGate
		case entry.Thread != nil && entry.Thread.Issue != nil:
			rows[entry.Thread.Issue.Key] = entry.ApprovalGate
		}
	}
	for _, want := range cases {
		key := want.seeded.issue.Key
		issueGate := readIssueApprovalGate(t, handler, key)
		rowGate, held := rows[key]
		if !held {
			t.Errorf("alice's Inbox holds no row of %s, %s", key, want.is)
			continue
		}
		for _, read := range []struct {
			name string
			gate []approvalGateLine
		}{{"the issue read", issueGate}, {"the one read", rowGate}} {
			named := make([]string, len(read.gate))
			for index, line := range read.gate {
				named[index] = line.Step
			}
			if !slices.Equal(named, want.steps) {
				t.Errorf("%s answers %s, %s, the gate of steps %q, want %q", read.name, key, want.is, named, want.steps)
			}
			if got := gateLinesOf(read.gate); !slices.Equal(got, want.seeded.lines) {
				t.Errorf("%s answers %s, %s, the gate %+v, want the seeded %+v", read.name, key, want.is, got, want.seeded.lines)
			}
		}
		if !reflect.DeepEqual(issueGate, rowGate) {
			t.Errorf("%s's issue read answers the gate %+v, and its Inbox row carries %+v", key, issueGate, rowGate)
		}
	}
}
