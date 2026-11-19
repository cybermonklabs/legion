package main

// The model half of the three defects of the first filing (the check
// c-defect-causes), read over HTTP from a real process. The file carries no build tag: the head's
// run needs none. The baseline quote runs the same test against the base release, whose binary
// DISPATCH_DEFECTS_SERVER_BINARY names, as
//
//	DISPATCH_DEFECTS_SERVER_BINARY=$TMPDIR/base-dispatch go test -tags basebuild ./cmd/dispatch/ -run '^TestTheThreeDefectsOfTheFirstFilingAreGoneFromTheModel$' -count=1 -v

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// defectsServerBinaryEnv names the envoy-dispatch binary of the base release, which the baseline
// quote boots instead of this head's build. Unset, the test boots this head (cmdtest.Build).
const defectsServerBinaryEnv = "DISPATCH_DEFECTS_SERVER_BINARY"

// defectsAgentSession is the session the shared agent bearer writes the agent's decision as.
const defectsAgentSession = "ses_defect_causes_agent"

// defectsSpec is the seeded issue's spec: three requirements, and under its decisions heading an
// ask, which bob answers. Every release this test boots accepts it.
const defectsSpec = "# The transport\n\n" +
	":::requirement{#r1}\nThe service answers its clients over one transport.\n:::\n\n" +
	":::requirement{#r2}\nEvery service of the quarter uses that transport.\n:::\n\n" +
	":::requirement{#r3}\nA client may stream its reads.\n:::\n\n" +
	"## Decisions\n\n" +
	":::ask{#a1 urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n"

// defectsAskOpener is how the spec's text opens the ask block a1, which the write that ties the
// ask to a requirement adds its shaped pointer to.
const defectsAskOpener = `:::ask{#a1 urgency="med" multiple="false"`

// defectsEntry is one entry of the Decisions read's entries member, as far as this test reads it,
// and the entry as the read answered it, for a failure to quote.
type defectsEntry struct {
	Node struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"node"`
	Shaped []model.DecisionLink `json:"shaped"`
	raw    string
}

// node is the entry's node, written kind:id.
func (entry defectsEntry) node() string {
	return entry.Node.Kind + ":" + entry.Node.ID
}

// shaped is each spec node the entry's shaped answers, written kind:id, with the tuple's event and
// whether the link is stale, for a message to print.
func (entry defectsEntry) shaped() string {
	if len(entry.Shaped) == 0 {
		return "no shaped link"
	}
	links := make([]string, len(entry.Shaped))
	for index, link := range entry.Shaped {
		event := "no event"
		if link.Event != nil {
			event = fmt.Sprintf("event %d", *link.Event)
		}
		links[index] = fmt.Sprintf("%s:%s (%s, stale %t)", link.To.Kind, link.To.ID, event, link.Stale)
	}
	return "the shaped links " + strings.Join(links, ", ")
}

// shapes reports whether the entry's shaped answers the spec node target, written kind:id.
func (entry defectsEntry) shapes(target string) bool {
	return slices.ContainsFunc(entry.Shaped, func(link model.DecisionLink) bool {
		return link.To.Kind+":"+link.To.ID == target
	})
}

// defectsRead is one answer of the Decisions read: its entries, whether the answer carries the
// entries member at all, and the answer's body as it came, for a failure to quote.
type defectsRead struct {
	entries []defectsEntry
	carried bool
	body    string
}

// entry is the entry of node, written kind:id, and whether the read answers one.
func (read defectsRead) entry(node string) (defectsEntry, bool) {
	for _, entry := range read.entries {
		if entry.node() == node {
			return entry, true
		}
	}
	return defectsEntry{}, false
}

// answered says what the read answers in its entries member: an empty list, saying so where the
// answer carries no such member, or the node of each entry.
func (read defectsRead) answered() string {
	switch {
	case !read.carried:
		return "an empty list (the answer carries no entries member)"
	case len(read.entries) == 0:
		return "an empty list"
	}
	nodes := make([]string, len(read.entries))
	for index, entry := range read.entries {
		nodes[index] = entry.node()
	}
	return fmt.Sprintf("%d entries, %s", len(read.entries), strings.Join(nodes, ", "))
}

// tieOn says what the read answers on the entry of node, for a failure to print: the entry's
// shaped links and the entry as the read answered it, or that the read answers no such entry and
// the whole answer.
func (read defectsRead) tieOn(node string) (answer, quote string) {
	if entry, found := read.entry(node); found {
		return entry.shaped(), "The entry answered: " + entry.raw
	}
	return "no entry", "The read answered: " + read.body
}

// defectsRun is one run of the test: the process it drives, the database that process serves,
// and the issue it seeded there.
type defectsRun struct {
	t        *testing.T
	process  *dispatchProcess
	database *pgx.Conn
	issue    string
	specID   string
}

// seed sends one write of the seeding as header's caller and requires status, answering the
// body. A failure says it is the seeding's, and names no defect.
func (run *defectsRun) seed(what, method, path, body string, header http.Header, status int) []byte {
	run.t.Helper()
	answer, err := run.process.trySend(method, path, body, header)
	if err != nil {
		run.t.Fatalf("seeding the issue failed at %s: %s %s did not answer: %v", what, method, path, err)
	}
	if answer.status != status {
		run.t.Fatalf("seeding the issue failed at %s: %s %s answered %s, want %d", what, method, path, answer, status)
	}
	return answer.body
}

// path is the Decisions read's route for the run's issue.
func (run *defectsRun) path() string {
	return "/api/v1/issues/" + run.issue + "/decisions"
}

// readDecisions is one Decisions read of the run's issue, as alice reads it. A read the route does
// not answer with 200 and a JSON object fails as the route's failure, and names no defect.
func (run *defectsRun) readDecisions(when string) defectsRead {
	run.t.Helper()
	answer, err := run.process.trySend(http.MethodGet, run.path(), "", asUser(dispatchTestLogin))
	if err != nil {
		run.t.Fatalf("reaching the Decisions read %s failed: GET %s did not answer: %v", when, run.path(), err)
	}
	if answer.status != http.StatusOK {
		run.t.Fatalf("reaching the Decisions read %s failed: GET %s answered %s, want 200", when, run.path(), answer)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(answer.body, &members); err != nil {
		run.t.Fatalf("reaching the Decisions read %s failed: GET %s answered a body that is no JSON object (%v): %s", when, run.path(), err, answer.body)
	}
	read := defectsRead{body: strings.TrimSpace(string(answer.body))}
	entries, carried := members["entries"]
	if carried && string(entries) != "null" {
		read.carried = true
		var raws []json.RawMessage
		if err := json.Unmarshal(entries, &raws); err != nil {
			run.t.Fatalf("reaching the Decisions read %s failed: GET %s answered an entries member that is no list (%v): %s", when, run.path(), err, entries)
		}
		read.entries = make([]defectsEntry, len(raws))
		for index, raw := range raws {
			if err := json.Unmarshal(raw, &read.entries[index]); err != nil {
				run.t.Fatalf("reaching the Decisions read %s failed: GET %s answered an entry that does not decode (%v): %s", when, run.path(), err, raw)
			}
			read.entries[index].raw = string(raw)
		}
	}
	return read
}

// awaitSpecSettled waits for the spec's settlement to finish, so no settlement the seeding
// queued writes a version of the spec while the test reads it.
func (run *defectsRun) awaitSpecSettled(when string) {
	run.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var owed bool
		if err := run.database.QueryRow(context.Background(),
			`select exists (select 1 from doc_settlements_pending where artifact_id = $1)`, run.specID,
		).Scan(&owed); err != nil {
			run.t.Fatalf("seeding the issue failed %s: read the spec's pending settlement: %v", when, err)
		}
		if !owed {
			return
		}
		if time.Now().After(deadline) {
			run.t.Fatalf("seeding the issue failed %s: the spec still owes its settlement after 30s:\n%s", when, run.process.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// latestSpecVersion is the number of the spec's newest version, as GET /api/v1/artifacts/{id}
// answers it.
func (run *defectsRun) latestSpecVersion(when string) int {
	run.t.Helper()
	answer, err := run.process.trySend(http.MethodGet, "/api/v1/artifacts/"+run.specID, "", asUser(dispatchTestLogin))
	if err != nil || answer.status != http.StatusOK {
		run.t.Fatalf("reaching the spec %s failed: GET /api/v1/artifacts/%s answered %s (%v), want 200", when, run.specID, answer, err)
	}
	var artifact model.Artifact
	if err := json.Unmarshal(answer.body, &artifact); err != nil || len(artifact.Versions) == 0 {
		run.t.Fatalf("reaching the spec %s failed: GET /api/v1/artifacts/%s answered no version (%v): %s", when, run.specID, err, answer.body)
	}
	return artifact.Versions[len(artifact.Versions)-1].Number
}

// writtenVersion is the number of the version a write's answer names under its version member.
func (run *defectsRun) writtenVersion(what string, body []byte) int {
	run.t.Helper()
	var written struct {
		Version *model.Version `json:"version"`
	}
	if err := json.Unmarshal(body, &written); err != nil || written.Version == nil {
		run.t.Fatalf("seeding the issue failed at %s: the write answered no version (%v): %s", what, err, body)
	}
	return written.Version.Number
}

// editSpec applies one replace to the spec as alice, as a named version, and answers its number.
func (run *defectsRun) editSpec(what, find, with string) int {
	run.t.Helper()
	body := censusBody(run.t, map[string]any{
		"ops":     []map[string]any{{"op": "replace", "find": find, "with": with}},
		"summary": what,
	})
	return run.writtenVersion(what, run.seed(what, http.MethodPost, "/api/v1/issues/"+run.issue+"/artifacts/spec/edits", body, asUser(dispatchTestLogin), http.StatusOK))
}

// requireDefectsServerBinary fails the test unless binary, the base release
// DISPATCH_DEFECTS_SERVER_BINARY names, is an executable file. It never skips: a baseline that did
// not run reads as one that held.
func requireDefectsServerBinary(t *testing.T, binary string) {
	t.Helper()
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("the base release %s that %s names cannot be read: %v", binary, defectsServerBinaryEnv, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		t.Fatalf("the base release %s that %s names is no executable file", binary, defectsServerBinaryEnv)
	}
}

// defectsDatabase creates an empty database on the test server for the base release alone, as
// censusDatabase does, and answers its URL: the base release runs its own migrations on it, which a
// clone of this build's migrated template would refuse.
func defectsDatabase(t *testing.T) string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(storetest.DatabaseURLEnv))
	if raw == "" {
		t.Fatalf("%s names the base release, which serves a database this test creates on the server %s names, and %s is unset",
			defectsServerBinaryEnv, storetest.DatabaseURLEnv, storetest.DatabaseURLEnv)
	}
	server, err := url.Parse(raw)
	if err != nil {
		// url.Parse's own error quotes the URL, which carries a credential.
		t.Fatalf("parse %s: %v", storetest.DatabaseURLEnv, errors.Unwrap(err))
	}
	admin := *server
	admin.Path = "/postgres"
	return testdb.DatabaseOn(t, admin.String(), "dispatch_defects")
}

// c-defect-causes. The model half of the three defects of the first filing: a Decisions view that
// showed nothing, a tie that did not appear, and a tie bound to one version of a document. Each is
// one assertion, read over HTTP from a real process (startDispatchProcessWith), and each failure
// opens with its own words, `defect 1: `, `defect 2: ` or `defect 3: `, and names its assertion. A
// failure while seeding the issue, setting up a read or reaching a route says so and names no
// defect.
//
// With no setting the process is this head's build (cmdtest.Build). It is given an issue whose
// spec holds three requirements and an ask, which bob answers, and an agent's decision in another
// document of the issue, and no decisions.md. Then:
//
//  1. the Decisions read answers every decision the issue holds in its entries member - the
//     answered ask's node and the decision's block node, and no other - rather than an empty list;
//  2. one write, alice's upload of the spec with a shaped pointer on the answered ask's block to
//     the first requirement, ties the ask to that requirement node, and the first Decisions read
//     after that write answers that node on the ask's entry, with no second write: the read before
//     that write answers no such tie, and the spec's newest version is still the one that write
//     wrote when the read has answered;
//  3. the ask's entry answers the same requirement node after each of two further versions of the
//     spec, one that rewords the third requirement and one that rewords the first, so the tie
//     stands at the three versions the decision covers.
//
// Each later assertion reads what the earlier one held - the ask's entry, then its tie - so a
// defect's failure ends the run.
//
// With DISPATCH_DEFECTS_SERVER_BINARY naming the base release's binary, the process is that
// binary, over an empty database the test creates on the test server, and the issue holds only
// what that release accepts: the spec with the ask bob answers. Only the first assertion runs, and
// on that release it fails, printing defect 1's words, for the base answers an empty list for an
// issue with no decisions.md.
func TestTheThreeDefectsOfTheFirstFilingAreGoneFromTheModel(t *testing.T) {
	ctx := context.Background()
	base := strings.TrimSpace(os.Getenv(defectsServerBinaryEnv))
	options := dispatchOptions{SettleDelay: 200 * time.Millisecond}
	var databaseURL string
	if base == "" {
		databaseURL = storetest.Open(t).Pool.Config().ConnString()
		t.Logf("the server is this head's build, over a clone of its migrated template")
	} else {
		requireDefectsServerBinary(t, base)
		databaseURL = defectsDatabase(t)
		options.Binary = base
		t.Logf("the server is the base release %s, over an empty database of this run's own", base)
	}
	process := startDispatchProcessWith(t, databaseURL, options)
	process.waitHealthy(t)
	database, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("seeding the issue failed: connect to the database the server serves: %v", err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	run := &defectsRun{t: t, process: process, database: database}

	// ───── the seeded issue ─────
	run.seed("the project", http.MethodPost, "/api/v1/projects", `{"key":"DEF","name":"The first filing"}`, asUser(dispatchTestLogin), http.StatusCreated)
	var created struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	issueBody := run.seed("the issue and its spec", http.MethodPost, "/api/v1/issues", censusBody(t, map[string]any{
		"project": "DEF", "title": "The transport", "spec": defectsSpec,
	}), asUser(dispatchTestLogin), http.StatusCreated)
	if err := json.Unmarshal(issueBody, &created); err != nil || created.Key == "" || created.PrimaryArtifactID == "" {
		t.Fatalf("seeding the issue failed at the issue and its spec: the answer names no issue and spec (%v): %s", err, issueBody)
	}
	run.issue, run.specID = created.Key, created.PrimaryArtifactID
	t.Logf("seeding: waiting for the settlement of %s's spec to open the ask of block a1", run.issue)
	askID := awaitBlockAsk(t, process, run.issue, "a1")
	ask := "ask:" + askID
	run.seed("bob's answer to the ask", http.MethodPost, "/api/v1/asks/"+askID+"/answer",
		`{"selected":["REST"],"text":"Plain HTTP for now.","expected_edited_at":null}`, asUser("bob"), http.StatusOK)
	held := map[string]string{ask: "the answered ask a1 of its spec"}
	if base == "" {
		run.seed("the agent's decision", http.MethodPost, "/api/v1/issues/"+run.issue+"/artifacts", censusBody(t, map[string]any{
			"name":    "choices.md",
			"content": ":::decision{#d1 shaped=\"dispatch://" + run.issue + "/spec#b-r2\"}\nREST for every service this quarter.\n:::\n",
			"actor":   map[string]any{"kind": "session", "id": defectsAgentSession},
		}), agentBearer(), http.StatusCreated)
		held[refs.BlockNodeKind+":"+run.issue+"/choices-md#d1"] = "the agent's decision d1 of its choices.md"
	}
	run.awaitSpecSettled("before the first Decisions read")

	// ───── defect 1: every decision the issue holds, rather than an empty list ─────
	read := run.readDecisions("after the seeding")
	var missing, extra []string
	for _, node := range slices.Sorted(maps.Keys(held)) {
		if _, found := read.entry(node); !found {
			missing = append(missing, node+" ("+held[node]+")")
		}
	}
	for _, entry := range read.entries {
		if _, holds := held[entry.node()]; !holds {
			extra = append(extra, entry.node())
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		stray := ""
		if len(extra) > 0 {
			stray = fmt.Sprintf(", and answers %s, which the issue does not hold", strings.Join(extra, ", "))
		}
		t.Fatalf("defect 1: the Decisions read does not answer every decision the issue holds in its entries member: GET %s answers %s "+
			"for %s, which holds %d decision(s) and no decisions.md, and answers no entry for %s%s. The read answered: %s",
			run.path(), read.answered(), run.issue, len(held), strings.Join(missing, ", "), stray, read.body)
	}
	t.Logf("the assertion of defect 1 holds: GET %s answers %s, one entry for each decision %s holds", run.path(), read.answered(), run.issue)
	if base != "" {
		return
	}

	// ───── defect 2: the tie appears on the first read after the write that ties it ─────
	// r1's node is keyed by the room the spec is edited in, its issue's tree: the issue is born
	// converted (refs.DocumentRoomSQL).
	var specRoom string
	if err := database.QueryRow(ctx, `select room.ref_key from artifacts room where room.id = `+refs.DocumentRoomSQL("$1::uuid"),
		run.specID).Scan(&specRoom); err != nil {
		t.Fatalf("read the room of %s's spec: %v", run.issue, err)
	}
	r1 := refs.BlockNodeKind + ":" + specRoom + "#r1"
	pointer := "dispatch://" + run.issue + "/spec#b-r1"
	if before, _ := read.entry(ask); before.shapes(r1) {
		t.Fatalf("setting up the tie failed: before any write ties the ask, GET %s answers %s on the entry of %s, so no read can show that write tying it",
			run.path(), before.shaped(), ask)
	}
	text := run.seed("reading the spec's text", http.MethodGet, "/api/v1/artifacts/"+run.specID+"/text", "", asUser(dispatchTestLogin), http.StatusOK)
	var spec struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(text, &spec); err != nil || strings.Count(spec.Markdown, defectsAskOpener) != 1 {
		t.Fatalf("seeding the issue failed at reading the spec's text: it holds the ask's opening %q %d time(s), want once (%v):\n%s",
			defectsAskOpener, strings.Count(spec.Markdown, defectsAskOpener), err, spec.Markdown)
	}
	tie := run.writtenVersion("the write that ties the ask to the first requirement", run.seed("the write that ties the ask to the first requirement",
		http.MethodPost, "/api/v1/issues/"+run.issue+"/artifacts", censusBody(t, map[string]any{
			"name":    "spec.md",
			"content": strings.Replace(spec.Markdown, defectsAskOpener, defectsAskOpener+` shaped="`+pointer+`"`, 1),
			"summary": "the answered ask shaped the first requirement",
		}), asUser(dispatchTestLogin), http.StatusCreated))
	read = run.readDecisions(fmt.Sprintf("first after version %d", tie))
	if latest := run.latestSpecVersion("after the first Decisions read"); latest != tie {
		t.Fatalf("setting up the tie failed: after the first Decisions read the spec's newest version is %d, not %d, which the write that ties the ask wrote, "+
			"so this run cannot hold that read to no second write", latest, tie)
	}
	entry, found := read.entry(ask)
	if !found || !entry.shapes(r1) {
		answer, quote := read.tieOn(ask)
		t.Fatalf("defect 2: the first Decisions read after the write that ties the answered ask to a requirement node does not answer that node on the ask's entry: "+
			"version %d of the spec wrote shaped=%q on the ask's block a1, and with no second write GET %s answers %s on the entry of %s, want %s. %s",
			tie, pointer, run.path(), answer, ask, r1, quote)
	}
	t.Logf("the assertion of defect 2 holds: the first Decisions read after version %d answers %s on the entry of %s", tie, entry.shaped(), ask)

	// ───── defect 3: the tie stands at every further version ─────
	for _, further := range []struct{ what, find, with string }{
		{"the first further version, which rewords the third requirement",
			"A client may stream its reads.", "A client may stream its reads and its writes."},
		{"the second further version, which rewords the first requirement, the one the ask shaped",
			"The service answers its clients over one transport.", "The service answers every client over one transport."},
	} {
		version := run.editSpec(further.what, further.find, further.with)
		read = run.readDecisions(fmt.Sprintf("after version %d", version))
		entry, found = read.entry(ask)
		if !found || !entry.shapes(r1) {
			answer, quote := read.tieOn(ask)
			t.Fatalf("defect 3: the entry does not answer the same requirement node after each of two further versions of the spec: "+
				"after version %d, %s, GET %s answers %s on the entry of %s, want %s, which version %d tied it to. %s",
				version, further.what, run.path(), answer, ask, r1, tie, quote)
		}
		t.Logf("the assertion of defect 3 holds at version %d, %s: the Decisions read answers %s on the entry of %s", version, further.what, entry.shaped(), ask)
	}
}
