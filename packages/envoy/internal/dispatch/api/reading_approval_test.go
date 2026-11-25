package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// c-reading-approval: a reading never changes the decision it reads, so
// writing, editing or removing one never stales a person's approval of the document that holds
// the decision, and never clears it. The approval carry's reading case (docs.readingCase) holds
// that at the version the write stores: the document's current version reads approved through
// GET /api/v1/artifacts/{id}, the read the Legion daemon's design gate makes
// (packages/daemon/internal/dispatch HTTPClient.Approval and Approval.ApprovedLatest). A write
// that writes a decided ask's block from the agent's text, which holds none of the block's
// server-owned attributes, stores its version without them and the settlement after it stores
// one more that puts them back (R10's kept form); R10's attribute case composes with the reading
// case across the first and carries the approval alone across the second, so the current version
// is the second. The broken state this fails on is a write that carries the approval onto an
// older version alone and leaves the new current version unapproved, which a test reading the
// old approval would pass.

// The fixture spec of the reading tests, as c-reading-approval's fixture holds it: prose; the
// agent's decision d1, uploaded by its session with its level of certainty and its reason, that
// the reading r1 reads; the agent's decision d2, which nobody reads yet; and four asks the
// person is asked: a1, which offers options, and a2, whose answer is the person's own words, both
// of which the fixture answers, a3, which it leaves open, and a4, which the person declines with
// a note. Every version a write stores holds a3 and a4 beside the answered asks, so each carry is
// read agreeing with an open and a declined ask too.
const (
	readingProse       = "The service ships behind a flag."
	readingDecisionOne = "We chose REST."
	readingDecisionTwo = "We chose Postgres."
	// readingReason is the agent's reason for its level of certainty in d1, and readingCertainty
	// the two attributes d1's directive carries them in, as every rendering writes them.
	readingReason      = "A client that needs streaming would change it."
	readingCertainty   = ` uncertainty="medium" uncertainty_reason="` + readingReason + `"`
	readingOfOne       = "The agent reads it as REST for every service."
	readingQuestion    = "Which transport do we expose?"
	readingOpenWords   = "Which region goes first?"
	readingAskWithout  = ":::ask{#a2 urgency=\"med\" multiple=\"false\"}\n" + readingOpenWords + "\n:::\n"
	readingStillOpen   = "Which store holds the queue?"
	readingOpenAsk     = ":::ask{#a3 urgency=\"low\" multiple=\"false\"}\n" + readingStillOpen + "\n\n- Postgres: The one we run\n- Redis: The faster one\n:::\n"
	readingDeclined    = "Who owns the rollout?"
	readingDeclinedAsk = ":::ask{#a4 urgency=\"high\" multiple=\"false\"}\n" + readingDeclined + "\n\n- Ops: The ops team\n- Dev: The service team\n:::\n"
	readingDeclineNote = "Not mine to choose: ask the design lead."
	readingSpec        = "# Plan\n\n" + readingProse + "\n\n" +
		"::::decision{#d1" + readingCertainty + "}\n" + readingDecisionOne + "\n\n:::reading{#r1}\n" + readingOfOne + "\n:::\n::::\n\n" +
		":::decision{#d2}\n" + readingDecisionTwo + "\n:::\n\n" +
		":::ask{#a1 urgency=\"med\" multiple=\"false\"}\n" + readingQuestion + "\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n\n" +
		readingAskWithout + "\n" + readingOpenAsk + "\n" + readingDeclinedAsk
	// The readings the writes add: r2 under d2, and r3 inside an answered ask.
	readingOfTwo    = "The agent reads it as Postgres for every store."
	readingOfAnswer = "The agent reads the answer as the quiet region first."
)

// readingCarrySentence is the reason the reading case records, carrying the approval of version
// from (docs.carriedReasonPrefix, readingCase, carriedReasonEnd).
func readingCarrySentence(from int) string {
	return fmt.Sprintf("Carried forward from the approval of version %d: the readings of its decisions and of its answered asks, which change no decision, were the whole change since it.", from)
}

// readingAndAttributeCarrySentence is the reason the reading case and R10's attribute case record
// together, carrying the approval of version from to a version whose change since is both: the
// changes of the two cases in docs.carryCases' order, joined by ", and " (docs.carryVerdictOver).
func readingAndAttributeCarrySentence(from int) string {
	return fmt.Sprintf("Carried forward from the approval of version %d: the readings of its decisions and of its answered asks, which change no decision, and the server-owned attributes of its ask blocks, which hold no answer, were the whole change since it.", from)
}

// readingFixture is one issue whose spec, readingSpec, a person approved once its asks held their
// answers and its declined ask its decline: the issue, its spec, that approval, and the ask a3
// leaves open and the one a4 declines.
type readingFixture struct {
	issueKey, artifactID string
	approval             model.ArtifactReview
	open, declined       string
}

// approvedReadingSpec has the agent's session create an issue of project TEST holding readingSpec,
// so d1 and d2 are the agent's decisions, waits for settlement to open the asks of a1 to a4, has
// alice answer a1 and a2 and decline a4 with a note - each of which settles into a version of its
// own, the ask block's kept state (R3) - and approve the spec's latest version once it owes no
// settlement. It fails unless the spec is the check's fixture: d1 decided by the session with its
// level of certainty and its reason, a3 open, and a4 declined with the note.
func approvedReadingSpec(t *testing.T, handler http.Handler, database *store.Store, title string) readingFixture {
	t.Helper()
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "TEST", "title": title, "spec": readingSpec, "force": true, "actor": sessionActor(),
	})
	requireAgentWrite(t, "the agent's issue "+title, created, http.StatusCreated)
	issue := decodeBody[model.Issue](t, created)
	asks := readingAsksOpened(t, handler, issue.Key, map[string]string{
		"a1": readingQuestion, "a2": readingOpenWords, "a3": readingStillOpen, "a4": readingDeclined,
	})
	answerDecisionBlock(t, handler, asks["a1"], "alice", []string{"REST"}, "", nil)
	answerDecisionBlock(t, handler, asks["a2"], "alice", []string{}, "The quiet one, East.", nil)
	if declined := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+asks["a4"]+"/decline",
		map[string]any{"note": readingDeclineNote}, "alice"); declined.Code != http.StatusOK {
		t.Fatalf("alice declines a4: status=%d body=%s", declined.Code, declined.Body.String())
	}
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	requireDecidedByNode(t, database, "the agent's decision d1", spec.address("d1"), graphstore.KindSession, decidingSession)
	requireValue(t, database, spec.address("d1"), refs.ValueUncertainty, "medium")
	requireValue(t, database, spec.address("d1"), refs.ValueUncertaintyReason, readingReason)
	// The answers and the decline each settle into a version of their own, the ask block's kept
	// state (R3): the person approves once the spec owes no settlement, so the version approved is
	// the latest and the versions each case's write stores follow it.
	awaitSettled(t, database, issue.PrimaryArtifactID)
	approval := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	return readingFixture{issueKey: issue.Key, artifactID: issue.PrimaryArtifactID, approval: approval, open: asks["a3"], declined: asks["a4"]}
}

// readingAsksOpened waits for settlement to open an ask for each block of questions (block id to
// question) on the issue, and answers each ask's id by block id. It reads the issue's own asks: the
// open a3 every earlier fixture leaves stands in alice's Inbox under the same block id.
func readingAsksOpened(t *testing.T, handler http.Handler, issueKey string, questions map[string]string) map[string]string {
	t.Helper()
	ids := make(map[string]string, len(questions))
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/asks?state=all", nil, "alice")
		if listed.Code != http.StatusOK {
			t.Fatalf("list the asks of %s: status=%d body=%s", issueKey, listed.Code, listed.Body.String())
		}
		for _, ask := range decodeBody[[]model.Ask](t, listed) {
			if ask.BlockID == nil {
				continue
			}
			if question, asked := questions[*ask.BlockID]; asked && ask.Question == question && ask.State == "open" {
				ids[*ask.BlockID] = ask.ID
			}
		}
		if len(ids) == len(questions) {
			return ids
		}
		if time.Now().After(deadline) {
			t.Fatalf("settlement opened the asks %v on %s, want one for each block of %v", ids, issueKey, questions)
		}
	}
}

// requireOpenAndDeclinedAsks fails unless version number of fixture's spec, stored as markdown,
// holds the open ask a3 and the declined ask a4 as the agent wrote them, but for the server-owned
// attributes a stored version keeps on a decided ask's block (R10's kept form: a4's
// state="resolved"), which both sides are compared without (askServerAttribute), and the two asks
// still read open, and declined by alice with her note.
func requireOpenAndDeclinedAsks(t *testing.T, handler http.Handler, fixture readingFixture, number int, markdown string) {
	t.Helper()
	stored := askServerAttribute.ReplaceAllString(markdown, "")
	for _, block := range []string{readingOpenAsk, readingDeclinedAsk} {
		if written := askServerAttribute.ReplaceAllString(block, ""); !strings.Contains(stored, written) {
			t.Fatalf("version %d reads\n%s\nwant it holding the ask block\n%s\nbut for its server-owned attributes", number, markdown, written)
		}
	}
	if open := readBlockAsk(t, handler, fixture.open); open.State != "open" {
		t.Fatalf("a3 reads %q at version %d, want it still open", open.State, number)
	}
	declined := readBlockAsk(t, handler, fixture.declined)
	if declined.State != "resolved" || declined.Resolution == nil || declined.Resolution.Kind != model.AskResolutionDeclined ||
		declined.Resolution.Reason != readingDeclineNote {
		t.Fatalf("a4 reads %q with the resolution %+v at version %d, want it declined with the note %q", declined.State, declined.Resolution, number, readingDeclineNote)
	}
}

// replacedOnce is markdown with old replaced by with, failing unless markdown holds old once.
func replacedOnce(t *testing.T, markdown, old, with string) string {
	t.Helper()
	if count := strings.Count(markdown, old); count != 1 {
		t.Fatalf("the spec holds %q %d times, want once:\n%s", old, count, markdown)
	}
	return strings.Replace(markdown, old, with, 1)
}

// readingWrite is one write a session makes to the approved spec: the upload of the whole
// document, made from the markdown the spec reads back, and the edit of it, as operations of the
// edits route (nil where the edits route writes no such change); whether that edit writes a
// decided ask's whole block from the agent's text, as every upload writes each decided ask's
// block; and a passage of the spec the write leaves, which the read-back must hold, and one it
// takes away, which it must not ("" for none).
type readingWrite struct {
	name        string
	upload      func(t *testing.T, markdown string) string
	edit        []map[string]string
	rewritesAsk bool
	holds       string
	drops       string
}

// carryReasons is the reason of the carry at each version write stores past the approved version
// from, oldest first, by an upload of the whole document when upload is set and by its edit
// otherwise. An edit that leaves each decided ask's block as the live document holds it stores one
// version, which keeps every block's server-owned attributes and which the reading case carries
// alone. A write that writes a decided ask's block from the agent's text - every upload, since the
// text an agent reads and uploads back carries none of those attributes, and an edit that
// rewrites such a block whole - stores its version without that block's kept attributes, which
// the reading case and R10's attribute case carry together, and the settlement after it stores
// one more that puts them back (R10's kept form), which the attribute case carries alone, still
// naming the version the person approved (docs.approvalOrigin).
func (write readingWrite) carryReasons(from int, upload bool) []string {
	if !upload && !write.rewritesAsk {
		return []string{readingCarrySentence(from)}
	}
	return []string{readingAndAttributeCarrySentence(from), attributeRemovalSentence(from)}
}

// The writes that touch readings alone: one added under a decision, one added inside each
// answered ask, a reading's text edited, and a reading removed. An edit's find holds one
// textblock, and a with that names blocks rewrites the typed block it lands in only where it
// rewrites all of it, so the edits route writes a reading after an ask's question and never
// after the options that follow the question: the ask that offers options takes its reading
// through the upload alone, and the edit that writes one into the answered a2 rewrites a2's whole
// block from the agent's text (rewritesAsk).
var readingWrites = []readingWrite{
	{
		name: "a reading added under a decision",
		upload: func(t *testing.T, markdown string) string {
			return replacedOnce(t, markdown, ":::decision{#d2 replaced=\"false\"}\n"+readingDecisionTwo+"\n:::\n",
				"::::decision{#d2 replaced=\"false\"}\n"+readingDecisionTwo+"\n\n:::reading{#r2}\n"+readingOfTwo+"\n:::\n::::\n")
		},
		edit: []map[string]string{replaceOp(readingDecisionTwo,
			"::::decision{#d2}\n"+readingDecisionTwo+"\n\n:::reading{#r2}\n"+readingOfTwo+"\n:::\n::::")},
		holds: ":::reading{#r2}\n" + readingOfTwo + "\n:::\n",
	},
	{
		name: "a reading added inside an answered ask",
		upload: func(t *testing.T, markdown string) string {
			opened := replacedOnce(t, markdown, ":::ask{#a2 ", "::::ask{#a2 ")
			return replacedOnce(t, opened, readingOpenWords+"\n:::\n",
				readingOpenWords+"\n\n:::reading{#r3}\n"+readingOfAnswer+"\n:::\n::::\n")
		},
		edit: []map[string]string{replaceOp(readingOpenWords,
			"::::ask{#a2 urgency=\"med\" multiple=\"false\"}\n"+readingOpenWords+"\n\n:::reading{#r3}\n"+readingOfAnswer+"\n:::\n::::")},
		rewritesAsk: true,
		holds:       ":::reading{#r3}\n" + readingOfAnswer + "\n:::\n",
	},
	{
		name: "a reading added inside the answered ask that offers options",
		upload: func(t *testing.T, markdown string) string {
			opened := replacedOnce(t, markdown, ":::ask{#a1 ", "::::ask{#a1 ")
			return replacedOnce(t, opened, "Streaming\n:::\n",
				"Streaming\n\n:::reading{#r4}\n"+readingOfAnswer+"\n:::\n::::\n")
		},
		holds: ":::reading{#r4}\n" + readingOfAnswer + "\n:::\n",
	},
	{
		name: "a reading's text edited",
		upload: func(t *testing.T, markdown string) string {
			return replacedOnce(t, markdown, "for every service.", "for every public service.")
		},
		edit:  []map[string]string{replaceOp("for every service.", "for every public service.")},
		holds: "The agent reads it as REST for every public service.",
		drops: readingOfOne,
	},
	{
		name: "a reading removed",
		upload: func(t *testing.T, markdown string) string {
			return replacedOnce(t, markdown, "::::decision{#d1"+readingCertainty+" replaced=\"false\"}\n"+readingDecisionOne+"\n\n:::reading{#r1}\n"+readingOfOne+"\n:::\n::::\n",
				":::decision{#d1"+readingCertainty+" replaced=\"false\"}\n"+readingDecisionOne+"\n:::\n")
		},
		edit:  []map[string]string{{"op": "delete", "block": "r1"}},
		holds: ":::decision{#d1" + readingCertainty + " replaced=\"false\"}\n" + readingDecisionOne + "\n:::\n",
		drops: readingOfOne,
	},
}

// The four writes that must still supersede the approval: the decision's own words, prose
// outside any reading, a write that changes both at once beside a reading, and a reading retyped
// into a decision.
var supersedingWrites = []readingWrite{
	{name: "the decision's own words changed", edit: []map[string]string{replaceOp(readingDecisionOne, "We chose gRPC.")}},
	{name: "prose outside any reading changed", edit: []map[string]string{replaceOp(readingProse, "The service ships behind a flag and a canary.")}},
	{name: "the decision's words and the prose changed at once, beside a reading", edit: []map[string]string{
		replaceOp(readingDecisionOne, "We chose gRPC."),
		replaceOp(readingProse, "The service ships behind a flag and a canary."),
		replaceOp("for every service.", "for every public service."),
	}},
	{name: "a reading retyped into a decision", edit: []map[string]string{{"op": "retype", "block": "r1", "type": "decision"}}},
}

// writeReadingSpec makes write to fixture's spec as the session, through the upload of the whole
// document when upload is set and through the edits route otherwise, waits until the spec owes no
// settlement and fails unless its latest version is versions past the approved one, the versions
// the write stores (readingWrite.carryReasons), and answers the advice the write came back with.
// It reads each of those versions back and fails unless it holds what the write leaves, and the
// open and the declined ask beside it (requireOpenAndDeclinedAsks).
func writeReadingSpec(t *testing.T, handler http.Handler, database *store.Store, fixture readingFixture, write readingWrite, upload bool, versions int) *testWriteAdvice {
	t.Helper()
	first, latest := fixture.approval.Version+1, fixture.approval.Version+versions
	var advice *testWriteAdvice
	if upload {
		uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/artifacts", map[string]any{
			"actor": sessionActor(), "name": "spec.md", "content": write.upload(t, documentMarkdown(t, handler, fixture.artifactID)),
			"precondition": uploadBase(t, handler, fixture.artifactID),
		})
		advice = adviceFromResponse(t, uploaded.Code, uploaded.Body.String())
		waitForArtifactVersion(t, handler, fixture.artifactID, first)
	} else {
		advice = agentEdit(t, handler, fixture.artifactID, first, write.edit...)
	}
	awaitSettled(t, database, fixture.artifactID)
	if stored := documentVersions(t, handler, fixture.artifactID); stored[len(stored)-1].Number != latest {
		t.Fatalf("%s: once the spec owes no settlement its latest version is %d, want %d: the write stores %d versions past the approved version %d",
			write.name, stored[len(stored)-1].Number, latest, versions, fixture.approval.Version)
	}
	for version := first; version <= latest; version++ {
		stored := versionMarkdown(t, handler, fixture.artifactID, version)
		if !strings.Contains(stored, write.holds) || write.drops != "" && strings.Contains(stored, write.drops) {
			t.Fatalf("%s: version %d reads back as\n%s\nwant it holding %q and not %q", write.name, version, stored, write.holds, write.drops)
		}
		requireOpenAndDeclinedAsks(t, handler, fixture, version, stored)
	}
	return advice
}

// requireApprovedAtTheCurrentVersion is the four reads after a write that touched readings alone,
// whose carry recorded reasons, one for each version the write stored past the approved one,
// oldest first (readingWrite.carryReasons): GET /api/v1/artifacts/{id} answers the approval
// approved at a version equal to the latest version, which is the last version the write stored;
// the carried reviews, the document's newest, approve each of those versions in the approver's
// name with its reason, which records the version the person approved; the write's advice names
// no superseded approval; and the person's own approval still stands at its own version.
func requireApprovedAtTheCurrentVersion(t *testing.T, handler http.Handler, fixture readingFixture, advice *testWriteAdvice, reasons []string, what string) {
	t.Helper()
	approved, written := fixture.approval.Version, fixture.approval.Version+len(reasons)
	reviews := documentReviews(t, handler, fixture.artifactID)
	reached := 0
	for _, review := range reviews {
		if review.State == "approved" {
			reached = review.Version
			break
		}
	}
	got := readApproval(t, handler, fixture.artifactID)
	if got.Approval == nil || got.Approval.State != "approved" || got.Approval.Version == nil || *got.Approval.Version != got.Approval.LatestVersion {
		state, at, latest := "none", 0, 0
		if got.Approval != nil {
			state, latest = got.Approval.State, got.Approval.LatestVersion
			if got.Approval.Version != nil {
				at = *got.Approval.Version
			}
		}
		t.Fatalf("%s: GET /api/v1/artifacts/%s answers the approval %s at version %d with the latest version %d, so the current version is not approved: the carry reached version %d, and the person must approve version %d again",
			what, fixture.artifactID, state, at, latest, reached, latest)
	}
	if got.Approval.LatestVersion != written {
		t.Fatalf("%s: the latest version is %d, want %d, the last of the %d versions the write stored", what, got.Approval.LatestVersion, written, len(reasons))
	}
	var person *servedReview
	for index := range reviews {
		if reviews[index].ID == fixture.approval.ID {
			person = &reviews[index]
		}
	}
	if person == nil || person.Version != approved || person.State != "approved" || len(reviews) != len(reasons)+1 {
		t.Fatalf("%s: the reviews are %+v, want the person's approval %s still standing at version %d and one carry for each of the %d versions the write stored",
			what, reviews, fixture.approval.ID, approved, len(reasons))
	}
	// The reviews are listed newest first: the carry to the last version the write stored leads,
	// and the person's own approval closes the list.
	for offset, reason := range reasons {
		version := approved + 1 + offset
		carried := reviews[len(reasons)-1-offset]
		if carried.ID == fixture.approval.ID || carried.Version != version || carried.State != "approved" || string(carried.Actor) != string(person.Actor) ||
			carried.Reason == nil || *carried.Reason != reason {
			recorded := "<none>"
			if carried.Reason != nil {
				recorded = *carried.Reason
			}
			t.Fatalf("%s: the review of version %d is %+v (reason %q), want the carry approving it as %s with the reason %q",
				what, version, carried, recorded, person.Actor, reason)
		}
	}
	if advice.SupersededApproval != nil {
		t.Fatalf("%s: the write's advice reports the superseded approval %+v, want none: its version carries the approval", what, advice.SupersededApproval)
	}
}

// requireSupersededByTheWrite is the reads after a write that must supersede the approval: GET
// /api/v1/artifacts/{id} answers no approval of the latest version, and the write's advice
// reports the person's approval as the one it superseded.
func requireSupersededByTheWrite(t *testing.T, handler http.Handler, fixture readingFixture, advice *testWriteAdvice, what string) {
	t.Helper()
	approved := fixture.approval.Version
	got := readApproval(t, handler, fixture.artifactID)
	if got.Approval == nil {
		t.Fatalf("%s: GET /api/v1/artifacts/%s answers no approval state at all", what, fixture.artifactID)
	}
	if got.Approval.State == "approved" && got.Approval.Version != nil && *got.Approval.Version == got.Approval.LatestVersion {
		t.Fatalf("%s: GET /api/v1/artifacts/%s answers the latest version %d approved, want no approval of it: the write changed more than a reading",
			what, fixture.artifactID, got.Approval.LatestVersion)
	}
	if superseded := advice.SupersededApproval; superseded == nil || superseded.ApprovedVersion != approved || superseded.Version != approved+1 || superseded.ApprovedBy != "alice" {
		t.Fatalf("%s: the write's advice reports the superseded approval %+v, want alice's approval of version %d superseded by version %d",
			what, advice.SupersededApproval, approved, approved+1)
	}
}

// A person approves the fixture spec at a version - the agent's decision with its level of
// certainty, its reason and a reading under it, beside an answered ask, an open one and a declined
// one with a note (approvedReadingSpec) - and a session writes a reading under one of its
// decisions and reads the document back once it owes no settlement; the four reads after the
// write find the current version approved (requireApprovedAtTheCurrentVersion), the open and the
// declined ask standing in each version that carries (requireOpenAndDeclinedAsks). The same holds
// for a reading added inside the person's answered ask, an edit of a reading's text and the
// removal of one, each written as an upload of the whole document and as an edit of it (the
// reading inside the ask that offers options as an upload alone: readingWrites). A write that
// writes a decided ask's block from the agent's text stores two versions, each carried
// (readingWrite.carryReasons). Four writes still supersede the approval: the decision's own
// words, prose outside any reading, both at once beside a reading, and a reading retyped into a
// decision (requireSupersededByTheWrite).
func TestAReadingKeepsAPersonsApprovalOfTheCurrentSpec(t *testing.T) {
	t.Parallel()
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "TEST project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	issues := 0
	fixture := func(t *testing.T) readingFixture {
		t.Helper()
		issues++
		return approvedReadingSpec(t, handler, database, fmt.Sprintf("Reading fixture %d", issues))
	}
	for _, write := range readingWrites {
		for _, upload := range []bool{true, false} {
			if !upload && write.edit == nil {
				continue
			}
			how := "an edit of the document"
			if upload {
				how = "an upload of the whole document"
			}
			t.Run(write.name+", by "+how, func(t *testing.T) {
				spec := fixture(t)
				reasons := write.carryReasons(spec.approval.Version, upload)
				advice := writeReadingSpec(t, handler, database, spec, write, upload, len(reasons))
				requireApprovedAtTheCurrentVersion(t, handler, spec, advice, reasons, write.name+", by "+how)
				t.Logf("%s, by %s: version %d reads approved, carried from the person's approval of version %d",
					write.name, how, spec.approval.Version+len(reasons), spec.approval.Version)
			})
		}
	}
	for _, write := range supersedingWrites {
		t.Run(write.name, func(t *testing.T) {
			spec := fixture(t)
			advice := agentEdit(t, handler, spec.artifactID, spec.approval.Version+1, write.edit...)
			requireSupersededByTheWrite(t, handler, spec, advice, write.name)
			t.Logf("%s: version %d supersedes the person's approval of version %d", write.name, spec.approval.Version+1, spec.approval.Version)
		})
	}
}
