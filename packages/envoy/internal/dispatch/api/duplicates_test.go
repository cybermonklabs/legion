package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The near-copy gate's tests. Every fixture scores at an extreme - an exact copy of an open
// issue's title and spec holds every picked term, a text sharing nothing holds none - so each
// case holds for any pair of bars with 0 < duplicateNudgeScore < duplicateRefuseScore < 1, and
// the replay that sets those two numbers cannot break them.
//
// A case reads the gate's verdict, so its server runs the gate under a bound the case owns
// (newDuplicateTestHandler), never production's duplicateGateTimeout: a read that outruns that
// bound goes in unchecked, as it must in production, and a case run under it would measure the
// machine. TestCreateIssueGoesInUncheckedWhenTheGateRunsPastItsBound proves the bound itself.

func createIssueRequest(t *testing.T, handler http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
}

func createDuplicateTestProject(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": key, "name": key + " project",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create project %q: status=%d body=%s", key, response.Code, response.Body.String())
	}
}

func requireCreatedIssue(t *testing.T, response *httptest.ResponseRecorder) model.Issue {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// duplicateRefusal is the 409 body the gate answers, read as a client reads it.
type duplicateRefusal struct {
	Error      string                     `json:"error"`
	Code       string                     `json:"code"`
	Candidates []model.DuplicateCandidate `json:"candidates"`
}

func requireDuplicateRefusal(t *testing.T, response *httptest.ResponseRecorder) duplicateRefusal {
	t.Helper()
	if response.Code != http.StatusConflict {
		t.Fatalf("near copy: status=%d body=%s", response.Code, response.Body.String())
	}
	body := decodeBody[duplicateRefusal](t, response)
	if body.Code != "POSSIBLE_DUPLICATE" {
		t.Fatalf("refusal code = %q, want POSSIBLE_DUPLICATE", body.Code)
	}
	return body
}

// nearCopyVerdictBound is the near-copy gate's bound on a server whose case reads the gate's
// verdict: the wait Envoy's tests give a step they cannot hurry (testwait.Bounded: a minute, or
// what the test binary's deadline leaves less a second), not production's duplicateGateTimeout.
// Under production's bound a read on a loaded machine ran out of it and the create went
// in unchecked, so a case wanting a refusal failed on the machine's speed, and a case wanting a
// create passed whatever the gate would have said.
func nearCopyVerdictBound(t *testing.T) time.Duration {
	return testwait.Bounded(t, time.Minute, time.Second)
}

// newDuplicateTestHandler is the server a near-copy case drives: its gate runs under
// nearCopyVerdictBound.
func newDuplicateTestHandler(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{duplicateBound: nearCopyVerdictBound(t)})
	return handler, database
}

// requireAnsweredCreate is requireCreatedIssue for a create whose 201 is the gate's verdict: the
// gate answered, and refused nothing. A create whose gate did not answer goes in all the same
// and says so in near_copy_check, so a 201 alone would pass a case the gate never judged.
func requireAnsweredCreate(t *testing.T, response *httptest.ResponseRecorder) model.Issue {
	t.Helper()
	issue := requireCreatedIssue(t, response)
	if check := createdSuggestions(t, response).NearCopyCheck; check != "" {
		t.Fatalf("create %s went in unchecked: near_copy_check=%q", issue.Key, check)
	}
	return issue
}

// createdSuggestions is a create's advice.suggestions, as a client reads them.
func createdSuggestions(t *testing.T, response *httptest.ResponseRecorder) model.Suggestions {
	t.Helper()
	return decodeBody[struct {
		Advice struct {
			Suggestions model.Suggestions `json:"suggestions"`
		} `json:"advice"`
	}](t, response).Advice.Suggestions
}

// The texts below are built from words that appear in one fixture and not the other, so a pair
// either shares its whole vocabulary or none of it.
const (
	duplicateSpecAstrolabe = "The astrolabe's calibration drifts whenever the gnomon is reseated, " +
		"so every altitude reading taken after a reseat is wrong by a constant offset. " +
		"Record the offset at each reseat and subtract it from the altitude the dial reports."
	duplicateSpecHarvest = "Orchard pickers cannot log a crate until the weighbridge answers, " +
		"and the weighbridge answers only while the loading dock has power. " +
		"Buffer each crate locally and reconcile the weights when the dock comes back."
)

func TestCreateIssueDuplicateRefusesANearCopyOfAnOpenIssue(t *testing.T) {
	handler, _ := newDuplicateTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	first := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	second := createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	})
	body := requireDuplicateRefusal(t, second)
	if len(body.Candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1: %#v", len(body.Candidates), body.Candidates)
	}
	candidate := body.Candidates[0]
	if candidate.Key != first.Key || candidate.Title != "Astrolabe calibration drifts after a reseat" ||
		candidate.Status != "triage" || candidate.SharedTerms == 0 ||
		candidate.Href != "/issues/"+first.Key {
		t.Fatalf("candidate = %#v", candidate)
	}
	if !strings.Contains(candidate.Snippet, "<mark>astrolabe</mark>") &&
		!strings.Contains(candidate.Snippet, "<mark>Astrolabe</mark>") {
		t.Fatalf("candidate snippet marks no shared word: %q", candidate.Snippet)
	}
	if body.Error != "possible duplicate of "+first.Key+": "+first.Title {
		t.Fatalf("refusal error = %q", body.Error)
	}

	// Nothing was written, so the next create takes the next number.
	next := requireAnsweredCreate(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Orchard pickers cannot log a crate", "spec": duplicateSpecHarvest,
	}))
	if next.Key != "TEST-2" {
		t.Fatalf("issue created after the refusal = %q, want TEST-2", next.Key)
	}
}

// A candidate's snippet marks the words of its title the new text shares and nothing else: a
// sentinel the candidate's author wrote in the title is shown as U+FFFD, where markSnippet would
// have drawn it as a <mark> around a word nothing matched (duplicateScoreQuery).
func TestCreateIssueDuplicateSnippetShowsTheSentinelsATitleHoldsAsText(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")
	title := "Astrolabe " + markStart + "calibration" + markEnd + " drifts after a reseat"
	first := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": title, "spec": duplicateSpecAstrolabe,
	}))
	body := requireDuplicateRefusal(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": title, "spec": duplicateSpecAstrolabe,
	}))
	if len(body.Candidates) != 1 || body.Candidates[0].Key != first.Key {
		t.Fatalf("candidates = %#v, want %s alone", body.Candidates, first.Key)
	}
	snippet := body.Candidates[0].Snippet
	if !strings.Contains(snippet, "<mark>Astrolabe</mark>") {
		t.Errorf("candidate snippet %q does not mark the shared word Astrolabe", snippet)
	}
	shown := html.UnescapeString(strings.NewReplacer("<mark>", "", "</mark>", "").Replace(snippet))
	if want := strings.NewReplacer(markStart, "\uFFFD", markEnd, "\uFFFD").Replace(first.Title); shown != want {
		t.Errorf("candidate snippet %q reads %q, want the title %q", snippet, shown, want)
	}
}

func TestCreateIssueDuplicateCreatesACopyOfADoneIssue(t *testing.T) {
	handler, _ := newDuplicateTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	first := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+first.Key, map[string]string{
		"status": "done",
	}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
	second := requireAnsweredCreate(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	if second.Key != "TEST-2" {
		t.Fatalf("copy of a done issue = %q, want TEST-2", second.Key)
	}
}

func TestCreateIssueDuplicateCreatesACopyOfAnAncestor(t *testing.T) {
	handler, _ := newDuplicateTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	// The grandparent is the only near copy in the project, so a gate that leaves out the parent
	// alone refuses the grandchild.
	grandparent := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	parent := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "parent": grandparent.Key,
		"title": "Orchard pickers cannot log a crate", "spec": duplicateSpecHarvest,
	}))
	grandchild := requireAnsweredCreate(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "parent": parent.Key,
		"title": "Astrolabe calibration drifts after a reseat", "spec": duplicateSpecAstrolabe,
	}))
	if grandchild.Key != "TEST-3" {
		t.Fatalf("copy of a grandparent = %q, want TEST-3", grandchild.Key)
	}

	// The parent itself is the other half of the clause.
	child := requireAnsweredCreate(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "parent": parent.Key,
		"title": "Orchard pickers cannot log a crate", "spec": duplicateSpecHarvest,
	}))
	if child.Key != "TEST-4" {
		t.Fatalf("copy of a parent = %q, want TEST-4", child.Key)
	}
}

func TestCreateIssueDuplicateCreatesATextSharingOnlyCommonWords(t *testing.T) {
	handler, _ := newDuplicateTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	// Every word these two texts share is an English stop word, which the tokenizer removes, so
	// no term of the new text is one the open issue holds.
	second := requireAnsweredCreate(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Orchard pickers cannot log a crate after the dock is down",
		"spec": duplicateSpecHarvest,
	}))
	if second.Key != "TEST-2" {
		t.Fatalf("text sharing only common words = %q, want TEST-2", second.Key)
	}
}

func TestCreateIssueDuplicateForcePassesTheGate(t *testing.T) {
	handler, _ := newDuplicateTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	forced := requireAnsweredCreate(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe, "force": true,
	}))
	if forced.Key != "TEST-2" {
		t.Fatalf("forced issue key = %q, want TEST-2", forced.Key)
	}
}

func TestCreateIssueDuplicateIgnoresACitedIssueKey(t *testing.T) {
	handler, _ := newDuplicateTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	cited := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	// The new text shares nothing with the cited issue but its key, which `skill://dispatch-first`
	// tells a filer to write down. The key is the rarest lexeme in the project, so a gate that
	// scored it would refuse this create.
	second := requireAnsweredCreate(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Orchard pickers cannot log a crate",
		"spec": duplicateSpecHarvest + "\n\nNearest issue ruled out: dispatch://" + cited.Key + ".",
	}))
	if second.Key != "TEST-2" {
		t.Fatalf("create citing %s = %q, want TEST-2", cited.Key, second.Key)
	}
}

func TestCreateIssueDuplicateCreatesATitleThatNamesAnotherIssue(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	// Issues filed from one template, one for each issue they concern, share every scored word:
	// the key each title names is the only difference, and the gate scores no key. Without the
	// title rule the second create scores 1.0 against the first and is refused.
	reviewed12 := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Review TEST-12 against its verification.md",
		"spec": duplicateSpecAstrolabe,
	}))
	reviewed15 := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Review TEST-15 against its verification.md",
		"spec": duplicateSpecAstrolabe,
	}))
	if reviewed15.Key != "TEST-2" {
		t.Fatalf("create naming TEST-15 = %q, want TEST-2", reviewed15.Key)
	}

	// The same issue reviewed twice is still a near copy, and the refusal names only the issue
	// whose title names the same key.
	body := requireDuplicateRefusal(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Review TEST-12 against its verification.md",
		"spec": duplicateSpecAstrolabe,
	}))
	if len(body.Candidates) != 1 || body.Candidates[0].Key != reviewed12.Key {
		t.Fatalf("candidates = %#v, want only %s", body.Candidates, reviewed12.Key)
	}

	// A title that names no issue is scored as before: a copy of the text with the key left out
	// of the title is still a near copy of both.
	body = requireDuplicateRefusal(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Review against its verification.md",
		"spec": duplicateSpecAstrolabe,
	}))
	if len(body.Candidates) != 2 {
		t.Fatalf("candidates = %#v, want %s and %s", body.Candidates, reviewed12.Key, reviewed15.Key)
	}
}

func TestCreateIssueDuplicateRefusesACopyWhoseTitleNamesTheCandidate(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	// The candidate's title cites another issue, and the copy's title names the candidate itself,
	// as a copy whose title ends "(carries ACME-28)" names ACME-28 when ACME-28's own title cites
	// ACME-23. The two titles share no key, but the copy is about the candidate, so it is refused.
	original := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat under TEST-40",
		"spec": duplicateSpecAstrolabe,
	}))
	body := requireDuplicateRefusal(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat (carries " + original.Key + ")",
		"spec": duplicateSpecAstrolabe,
	}))
	if len(body.Candidates) != 1 || body.Candidates[0].Key != original.Key {
		t.Fatalf("candidates = %#v, want only %s", body.Candidates, original.Key)
	}
}

func TestCreateIssueDuplicateRefusesAKeyedCopyOfATitleThatNamesNoIssue(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	// The rule leaves a candidate out only when both titles name a key. A copy that adds a key to
	// the title of an original whose title names none is still about the original, so it is
	// still refused.
	original := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	body := requireDuplicateRefusal(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat (see TEST-40)",
		"spec": duplicateSpecAstrolabe,
	}))
	if len(body.Candidates) != 1 || body.Candidates[0].Key != original.Key {
		t.Fatalf("candidates = %#v, want only %s", body.Candidates, original.Key)
	}
}

func TestCreateIssueDuplicateKeepsACopyThatTemplateIssuesWouldCrowdOut(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	// More issues from one template than the gate answers, each newer than the one the copy
	// copies and each scoring as high against it: a rule applied after the statement's limit
	// would read only the newer five, leave them all out, and let the copy through.
	original := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Review TEST-12 against its verification.md",
		"spec": duplicateSpecAstrolabe,
	}))
	for reviewed := 13; reviewed < 13+duplicateCandidateLimit; reviewed++ {
		requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
			"project": "TEST", "title": fmt.Sprintf("Review TEST-%d against its verification.md", reviewed),
			"spec": duplicateSpecAstrolabe,
		}))
	}
	body := requireDuplicateRefusal(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Review TEST-12 against its verification.md",
		"spec": duplicateSpecAstrolabe,
	}))
	if len(body.Candidates) != 1 || body.Candidates[0].Key != original.Key {
		t.Fatalf("candidates = %#v, want only %s", body.Candidates, original.Key)
	}
}

func TestCreateIssueDuplicateScopesTheGateToTheProjectAndSkipsExternal(t *testing.T) {
	handler, _ := newDuplicateTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")
	createDuplicateTestProject(t, handler, "OTHER")

	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	other := requireAnsweredCreate(t, createIssueRequest(t, handler, map[string]any{
		"project": "OTHER", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	if other.Key != "OTHER-1" {
		t.Fatalf("cross-project issue key = %q, want OTHER-1", other.Key)
	}
	mapping := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "TEST",
	}, "alice")
	if mapping.Code != http.StatusOK {
		t.Fatalf("map external repository: status=%d body=%s", mapping.Code, mapping.Body.String())
	}
	external := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"external": "owner/repo#41", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	if external.Key != "TEST-2" {
		t.Fatalf("external issue key = %q, want TEST-2", external.Key)
	}
}

func TestCreateIssueDuplicateOrdersCandidatesByScore(t *testing.T) {
	handler, _ := newDuplicateTestHandler(t)
	createDuplicateTestProject(t, handler, "ORDER")

	// Three open issues holding progressively more of the new text's words. Each is forced in,
	// because each is a near copy of the one before it.
	whole := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe, "force": true,
	}))
	half := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Astrolabe calibration drifts after a reseat",
		"spec":  "The astrolabe's calibration drifts whenever the gnomon is reseated.",
		"force": true,
	}))
	sliver := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Astrolabe calibration", "force": true,
	}))
	duplicate := createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	})
	body := requireDuplicateRefusal(t, duplicate)
	keys := make([]string, 0, len(body.Candidates))
	for _, candidate := range body.Candidates {
		keys = append(keys, candidate.Key)
	}
	// Each named candidate is at or above the refusal bar; the order is by score, best first.
	if len(keys) == 0 || keys[0] != whole.Key {
		t.Fatalf("candidates = %v, want %q first", keys, whole.Key)
	}
	for index := 1; index < len(keys); index++ {
		if keys[index] == whole.Key {
			t.Fatalf("candidates = %v, want %q once and first", keys, whole.Key)
		}
	}
	rank := map[string]int{whole.Key: 0, half.Key: 1, sliver.Key: 2}
	for index := 1; index < len(keys); index++ {
		if rank[keys[index-1]] > rank[keys[index]] {
			t.Fatalf("candidates = %v, want them ordered %q, %q, %q", keys, whole.Key, half.Key, sliver.Key)
		}
	}
}

// pickRelatedTerms drops a lexeme more than a tenth of the project holds, and never fewer than
// relatedCommonFloor of its items (`commonAbove`). So in a project whose items have made the
// words of a title common, a title yields no picked term at all and there is no share to score.
//
// dispatch://INFRA-1160/artifact/verification-md's check k4 names INFRA-432 against INFRA-431 —
// two open issues with one title and no spec at all — as a pair the rule must refuse. Below
// duplicateMinTerms the gate scores nothing and asks the other question instead: does an open
// issue hold the same lexemes, title and spec alike (duplicateEqualLexemesQuery). A copy does,
// which is this case and that pair, and a title differing in one meaning word does not, which is
// TestCreateIssueDuplicateCreatesTwoTitlesThatDifferInTheirOneMeaningWord.
func TestCreateIssueDuplicateRefusesACopiedTitleWhoseWordsAreCommon(t *testing.T) {
	handler, database := newDuplicateTestHandler(t)
	createDuplicateTestProject(t, handler, "COMMON")
	// Twelve issues holding the three words of the title below, so each word's document
	// frequency passes relatedCommonFloor and the picker drops it as a common word.
	for index := range 12 {
		requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
			"project": "COMMON",
			"title":   fmt.Sprintf("Astrolabe reseat note %s", string(rune('a'+index))),
			"spec":    "The astrolabe is reseated and its gimbal recalibrated.",
			"force":   true,
		}))
	}
	original := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "COMMON", "title": "Astrolabe gimbal reseated", "force": true,
	}))
	// The statistics as a create meets them in production: current, not ten creates behind.
	if _, err := refreshSearchTermStats(context.Background(), database.Pool, "COMMON"); err != nil {
		t.Fatalf("refresh the term statistics: %v", err)
	}

	copied := createIssueRequest(t, handler, map[string]any{
		"project": "COMMON", "title": "Astrolabe gimbal reseated",
	})

	body := requireDuplicateRefusal(t, copied)
	keys := make([]string, 0, len(body.Candidates))
	for _, candidate := range body.Candidates {
		keys = append(keys, candidate.Key)
	}
	if len(keys) == 0 || keys[0] != original.Key {
		t.Fatalf("candidates = %v, want %q first", keys, original.Key)
	}
}

// Two short titles that differ in the one word carrying their meaning are different work, and
// R2 says a text sharing only common words is created. pickRelatedTerms now fills its list back
// up to duplicateMinTerms from the words it dropped as common, so in a project of two issues the
// filled list is almost all shared words and the share the candidate holds reaches the refusal
// bar: the second issue is refused as a near copy of the first.
//
// These two titles are the seed of packages/dispatch/e2e/agent-name.e2e.ts, whose ten browser
// tests fail at this head with `POST /api/v1/issues failed: 409` on the second create.
func TestCreateIssueDuplicateCreatesTwoTitlesThatDifferInTheirOneMeaningWord(t *testing.T) {
	handler, _ := newDuplicateTestHandler(t)
	createDuplicateTestProject(t, handler, "AGT")

	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "AGT", "title": "Held by a running agent",
		"spec": "The running agent comments here. The stopped agent comments there.",
	}))
	requireAnsweredCreate(t, createIssueRequest(t, handler, map[string]any{
		"project": "AGT", "title": "Held by a stopped agent",
	}))
}

// A spec is stored as Dispatch renders it, not as the request wrote it: the seed parses the
// markdown and stores its rendering (docs' SeedText), which renumbers an ordered list, and
// Postgres keeps a list's numbers as lexemes. Below duplicateMinTerms the gate compares the new
// spec's lexemes with the stored row's, so it renders the new spec the same way first
// (storedMarkdown); built from the request's markdown instead, the copy of a spec written
// `1.` `1.` holds no `2` where the row it copies holds one, and the exact copy R2 refuses was
// created.
func TestCreateIssueDuplicateRefusesACopiedSpecWhoseListIsRenumbered(t *testing.T) {
	handler, _ := newDuplicateTestHandler(t)
	createDuplicateTestProject(t, handler, "LIST")

	first := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "LIST", "title": "Restart the scanner", "spec": "1. Restart\n1. Check\n",
	}))
	// The copy as the first create wrote it, the numbering the renderer changes.
	copied := requireDuplicateRefusal(t, createIssueRequest(t, handler, map[string]any{
		"project": "LIST", "title": "Restart the scanner", "spec": "1. Restart\n1. Check\n",
	}))
	if len(copied.Candidates) != 1 || copied.Candidates[0].Key != first.Key {
		t.Fatalf("candidates for the copy = %#v, want %q alone", copied.Candidates, first.Key)
	}
	// The same copy written with the numbers the renderer gives it: the other ordering of the
	// one spec, and the same near copy.
	renumbered := requireDuplicateRefusal(t, createIssueRequest(t, handler, map[string]any{
		"project": "LIST", "title": "Restart the scanner", "spec": "1. Restart\n2. Check\n",
	}))
	if len(renumbered.Candidates) != 1 || renumbered.Candidates[0].Key != first.Key {
		t.Fatalf("candidates for the renumbered copy = %#v, want %q alone", renumbered.Candidates, first.Key)
	}
}

// gateBoundStandIn is a near-copy read that never answers on its own (server.beforeDuplicateRead):
// it blocks until the context the gate gives it ends, and records the deadline that context
// carried, when the read began and why it ended, so a case can tell what ended it. wait is the
// case's own bound on the block, so a gate whose bound is gone fails the case by name rather
// than holding it to the test binary's deadline.
type gateBoundStandIn struct {
	wait time.Duration

	mu       sync.Mutex
	began    time.Time
	deadline time.Time
	bounded  bool
	ended    error
}

func (s *gateBoundStandIn) read(ctx context.Context) error {
	deadline, bounded := ctx.Deadline()
	began := time.Now()
	ended := errors.New("the near-copy read's context carries no deadline")
	if bounded {
		select {
		case <-ctx.Done():
			ended = ctx.Err()
		case <-time.After(s.wait):
			ended = fmt.Errorf("the near-copy read's context did not end within %s", s.wait)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.began, s.deadline, s.bounded, s.ended = began, deadline, bounded, ended
	return ended
}

// The gate's bound, production's duplicateGateTimeout, is what ends a read that does not answer.
// A read held until its context ends (gateBoundStandIn) is ended by that bound: the create of an
// exact copy of an open issue goes in rather than being refused, is told that no near-copy check
// was made in that time, and the server logs the bound it ran out of. This case runs under
// production's bound, which every other case of the gate replaces with one of its own
// (newDuplicateTestHandler), and nothing in it waits on the machine's speed: the read never
// answers, however fast the machine is.
func TestCreateIssueGoesInUncheckedWhenTheGateRunsPastItsBound(t *testing.T) {
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	createDuplicateTestProject(t, handler, "BOUND")
	const title = "Astrolabe calibration drifts after a reseat"
	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "BOUND", "title": title, "spec": duplicateSpecAstrolabe,
	}))
	standIn := &gateBoundStandIn{wait: nearCopyVerdictBound(t)}
	served.beforeDuplicateRead = standIn.read
	logs := captureLogs(t)

	asked := time.Now()
	response := createIssueRequest(t, handler, map[string]any{
		"project": "BOUND", "title": title, "spec": duplicateSpecAstrolabe,
	})
	created := requireCreatedIssue(t, response)
	standIn.mu.Lock()
	began, deadline, bounded, ended := standIn.began, standIn.deadline, standIn.bounded, standIn.ended
	standIn.mu.Unlock()
	if !bounded {
		t.Fatalf("the gate's read ran under no bound: %v", ended)
	}
	// The bound starts inside the request, before the read: its deadline is no sooner than
	// duplicateGateTimeout after the request was sent, and no later than that after the read began.
	if deadline.Before(asked.Add(duplicateGateTimeout)) || deadline.After(began.Add(duplicateGateTimeout)) {
		t.Fatalf("the gate's read had until %s after the request was sent and %s after it began, want %s from a moment between",
			deadline.Sub(asked), deadline.Sub(began), duplicateGateTimeout)
	}
	if !errors.Is(ended, context.DeadlineExceeded) {
		t.Fatalf("the gate's read ended with %v, want its bound's %v", ended, context.DeadlineExceeded)
	}
	if check, want := createdSuggestions(t, response).NearCopyCheck, nearCopyCheckNotMade(duplicateGateTimeout); check != want {
		t.Fatalf("near_copy_check = %q, want %q", check, want)
	}
	warned := false
	for _, line := range strings.Split(logs.String(), "\n") {
		warned = warned || strings.Contains(line, "level=WARN") &&
			strings.Contains(line, `msg="dispatch: the near-copy check did not answer within its bound"`) &&
			strings.Contains(line, "project=BOUND") && strings.Contains(line, "bound="+duplicateGateTimeout.String())
	}
	if !warned {
		t.Fatalf("no WARN line names the near-copy check's bound %s:\n%s", duplicateGateTimeout, logs.String())
	}
	if _, ok := rawCreatedPayload(t, handler, created.Key)["forced"]; ok {
		t.Fatalf("an unforced create the gate could not check carries a forced record")
	}
}
