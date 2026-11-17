package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// rawCheckResults is a results read decoded with each result kept as its raw fields, so a test
// can tell a key the answer leaves out from one it carries: `currency` is left out wherever
// neither of its words is true, and a client before the results contract change prints
// "unknown" for a key it does not find.
type rawCheckResults struct {
	Issue  string `json:"issue"`
	Checks []struct {
		BlockID string                       `json:"block_id"`
		When    string                       `json:"when"`
		Text    string                       `json:"text"`
		Results []map[string]json.RawMessage `json:"results"`
	} `json:"checks"`
}

// readRawResults is readResults decoded as rawCheckResults, at the commit the reader names
// where at is not noCommitNamed.
func readRawResults(t *testing.T, handler http.Handler, issue, at string) rawCheckResults {
	t.Helper()
	target := "/api/v1/issues/" + issue + "/results"
	if at != noCommitNamed {
		target += "?at=" + at
	}
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return decodeBody[rawCheckResults](t, response)
}

// rawResult is the raw fields of one result of one check.
func rawResult(t *testing.T, read rawCheckResults, blockID, resultID string) map[string]json.RawMessage {
	t.Helper()
	for _, check := range read.Checks {
		if check.BlockID != blockID {
			continue
		}
		for _, result := range check.Results {
			var id string
			if err := json.Unmarshal(result["id"], &id); err == nil && id == resultID {
				return result
			}
		}
	}
	t.Fatalf("check %s holds no result %s", blockID, resultID)
	return nil
}

// currencyIs asserts the compatibility `currency` of one result, and first that the result is
// the case the caller names in the fields the results contract carries: where its commit
// stands, whether its check's words moved, and whether a retraction stands against it. Those
// three are asserted as the comparison read answers them with or without `currency` beside
// them, which is what holds them unchanged. want "" is the key left out.
func currencyIs(
	t *testing.T, read rawCheckResults, blockID, resultID, what, stands string, moved, retracted bool, want string,
) {
	t.Helper()
	result := rawResult(t, read, blockID, resultID)
	var commitStands model.ResultCommitStands
	if err := json.Unmarshal(result["commit_stands"], &commitStands); err != nil || commitStands.Answer != stands {
		t.Fatalf("%s: commit_stands=%s, want answer %q", what, result["commit_stands"], stands)
	}
	if got, want := string(result["check_words_moved"]), fmt.Sprint(moved); got != want {
		t.Fatalf("%s: check_words_moved=%s, want %s", what, got, want)
	}
	if _, held := result["retracted"]; held != retracted {
		t.Fatalf("%s: retracted=%s, want a retraction %v", what, result["retracted"], retracted)
	}
	raw, held := result["currency"]
	switch {
	case want == "" && held:
		t.Errorf("%s: currency=%s, want the key left out", what, raw)
	case want != "" && !held:
		t.Errorf("%s: currency left out, want %q", what, want)
	case want != "" && string(raw) != fmt.Sprintf("%q", want):
		t.Errorf("%s: currency=%s, want %q", what, raw, want)
	}
}

// Each result answers `currency` beside `commit_stands`, in the words a client before the
// results contract change (#3530) printed: `current` where the result's commit stands at a
// basis, under the check's words as they stand now, with no retraction against it, and `older`
// where its commit stands at another commit or the check's words moved after it. Where neither
// word is true the key is left out, and that client prints "unknown": a retracted result at
// its basis, a commit Dispatch could not place, and every result of a read pinned to a commit,
// which no such client makes and where `current` would mean "the commit the reader named"
// rather than the work as it stands.
func TestEachResultAnswersTheCurrencyOfTheClientBeforeTheResultsContract(t *testing.T) {
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })
	issue := issueInRepository(t, handler, "RESCW", "acme/widgets", "Currency")
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, handler, owner, "verification.md", siblingChecks)
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", issue.Key)

	first := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	currencyIs(t, readRawResults(t, handler, issue.Key, noCommitNamed), "k1", first.Result.ID,
		"a result at the open head", model.CommitAtBasis, false, false, "current")

	// A push moves the head: the first result is at another commit, a result at the new head
	// stands at it, and a fail is answered as a pass is.
	writeHead(t, database, "acme/widgets", 7, newHeadCommit, "open", issue.Key)
	moved := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", newHeadCommit, nil)))
	failed := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key,
		resultBody("k3", "fail", newHeadCommit, map[string]any{"failed": 1})))
	read := readRawResults(t, handler, issue.Key, noCommitNamed)
	currencyIs(t, read, "k1", first.Result.ID,
		"a result at the head a push replaced", model.CommitOtherCommit, false, false, "older")
	currencyIs(t, read, "k1", moved.Result.ID,
		"a result at the new head", model.CommitAtBasis, false, false, "current")
	currencyIs(t, read, "k3", failed.Result.ID,
		"a fail at the new head", model.CommitAtBasis, false, false, "current")

	// Rewording k1 moves its own words: its results are older, whether or not their commit
	// stands at the head, and the sibling's are not.
	uploadDocument(t, handler, owner, "verification.md", siblingChecksReworded)
	read = readRawResults(t, handler, issue.Key, noCommitNamed)
	currencyIs(t, read, "k1", moved.Result.ID,
		"a result at the head under words that moved", model.CommitAtBasis, true, false, "older")
	currencyIs(t, read, "k1", first.Result.ID,
		"a result at another commit under words that moved", model.CommitOtherCommit, true, false, "older")
	if response := retractAsPerson(t, handler, moved.Result.ID, "The run was against the wrong branch."); response.Code != http.StatusCreated {
		t.Fatalf("retract %s: status=%d body=%s", moved.Result.ID, response.Code, response.Body.String())
	}
	currencyIs(t, readRawResults(t, handler, issue.Key, noCommitNamed), "k1", moved.Result.ID,
		"a retracted result at the head under words that moved", model.CommitAtBasis, true, true, "older")
	currencyIs(t, read, "k3", failed.Result.ID,
		"the sibling check's result", model.CommitAtBasis, false, false, "current")
	uploadDocument(t, handler, owner, "verification.md", siblingChecks)

	// A retracted result at the head is not current, and it is at no earlier commit either: the
	// old words have none for it. A retracted result at another commit is still older.
	retractedAtHead := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key,
		resultBody("k3", "pass", newHeadCommit, nil)))
	for _, id := range []string{retractedAtHead.Result.ID, first.Result.ID} {
		if response := retractAsPerson(t, handler, id, "The run was against the wrong branch."); response.Code != http.StatusCreated {
			t.Fatalf("retract %s: status=%d body=%s", id, response.Code, response.Body.String())
		}
	}
	read = readRawResults(t, handler, issue.Key, noCommitNamed)
	currencyIs(t, read, "k3", retractedAtHead.Result.ID,
		"a retracted result at the head", model.CommitAtBasis, false, true, "")
	currencyIs(t, read, "k1", first.Result.ID,
		"a retracted result at another commit", model.CommitOtherCommit, false, true, "older")
	currencyIs(t, read, "k3", failed.Result.ID,
		"the fail at the head beside the retractions", model.CommitAtBasis, false, false, "current")

	// A read pinned to a commit answers no currency, even for a result at that commit.
	pinned := readRawResults(t, handler, issue.Key, newHeadCommit)
	currencyIs(t, pinned, "k1", moved.Result.ID,
		"a result at the commit the reader named", model.CommitAtBasis, false, true, "")
	currencyIs(t, pinned, "k1", first.Result.ID,
		"a result at another commit than the one the reader named", model.CommitOtherCommit, false, true, "")
}

// A commit Dispatch cannot place answers no currency, as the server before the results
// contract change answered unknown for it, whether or not the check's words moved since. An
// issue that resolves to no repository has nothing to compare with, and its results stand at
// another commit: older.
func TestAResultDispatchCannotPlaceAnswersNoCurrency(t *testing.T) {
	handler, _ := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })

	hidden := issueInRepository(t, handler, "RESCX", "acme/hidden", "Unreadable")
	owner := "/api/v1/issues/" + hidden.Key
	uploadDocument(t, handler, owner, "verification.md", siblingChecks)
	unplaced := decodeBody[recordedResult](t, recordResult(t, handler, hidden.Key, resultBody("k1", "pass", headCommit, nil)))
	currencyIs(t, readRawResults(t, handler, hidden.Key, noCommitNamed), "k1", unplaced.Result.ID,
		"a result of a repository the App cannot read", model.CommitUnknown, false, false, "")
	uploadDocument(t, handler, owner, "verification.md", siblingChecksReworded)
	currencyIs(t, readRawResults(t, handler, hidden.Key, noCommitNamed), "k1", unplaced.Result.ID,
		"a result of a repository the App cannot read, under words that moved", model.CommitUnknown, true, false, "")

	loose := createInteractionIssue(t, handler, "RESCZ", "No repository", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+loose.Key, "verification.md", siblingChecks)
	nowhere := decodeBody[recordedResult](t, recordResult(t, handler, loose.Key, resultBody("k1", "pass", headCommit, nil)))
	currencyIs(t, readRawResults(t, handler, loose.Key, noCommitNamed), "k1", nowhere.Result.ID,
		"a result of an issue with no repository", model.CommitOtherCommit, false, false, "older")
}

// The gate and the compatibility field share the same standing rule for all 60 combinations,
// including fields absent from a result's own POST answer. A fail has the same currency as a
// pass but cannot make the gate answer yes.
func TestCurrencyAndTheGateAgreeOnEveryResultState(t *testing.T) {
	unmoved, moved := false, true
	rows := []struct {
		commit            string
		wordsMoved        *bool
		currency          string
		retractedCurrency string
		passStands        string
	}{
		{"absent", nil, "", "", "no"},
		{"absent", &unmoved, "", "", "no"},
		{"absent", &moved, "", "", "no"},
		{"", nil, "", "", "no"},
		{"", &unmoved, "", "", "no"},
		{"", &moved, "", "", "no"},
		{"at_basis", nil, "", "", "no"},
		{"at_basis", &unmoved, "current", "", "yes"},
		{"at_basis", &moved, "older", "older", "no"},
		{"other_commit", nil, "", "", "no"},
		{"other_commit", &unmoved, "older", "older", "no"},
		{"other_commit", &moved, "older", "older", "no"},
		{"unknown", nil, "", "", "no"},
		{"unknown", &unmoved, "", "", "unknown"},
		{"unknown", &moved, "", "", "no"},
	}
	for _, row := range rows {
		for _, retracted := range []bool{false, true} {
			for _, verdict := range []string{"pass", "fail"} {
				words := "absent"
				if row.wordsMoved != nil {
					words = fmt.Sprint(*row.wordsMoved)
				}
				t.Run(fmt.Sprintf("commit=%s/moved=%s/retracted=%t/%s", row.commit, words, retracted, verdict), func(t *testing.T) {
					result := model.CheckResult{Verdict: verdict, Role: "tester", CheckWordsMoved: row.wordsMoved}
					if row.commit != "absent" {
						result.CommitStands = &model.ResultCommitStands{Answer: row.commit}
					}
					wantCurrency, wantPass := row.currency, row.passStands
					if retracted {
						result.Retracted = &model.CheckResultRetracted{}
						wantCurrency = row.retractedCurrency
					}
					if verdict != "pass" || retracted {
						wantPass = "no"
					}
					currency := compatibilityCurrency(result)
					if currency != wantCurrency {
						t.Errorf("currency=%q, want %q", currency, wantCurrency)
					}
					gate := passStandsOf(model.CheckBlock{Results: []model.CheckResult{result}}, comparison{})
					if gate.Answer != wantPass {
						t.Errorf("pass_stands=%q, want %q", gate.Answer, wantPass)
					}
					if (gate.Answer == model.PassStandsYes) != (verdict == "pass" && currency == model.CurrencyCurrent) {
						t.Errorf("pass_stands=%q disagrees with verdict=%q currency=%q", gate.Answer, verdict, currency)
					}
				})
			}
		}
	}
}

// oldClientResults is what `dispatch results` printed in every client before the results
// contract change (#3530), which read `currency` and nothing of the comparison: a copy, in Go,
// of that client's formatCheckResults over a read that has a Verification document, and of its
// one line per check,
//
//	`${head}${latest.verdict} at ${latest.commit_sha} by ${latest.role}, ${latest.currency ?? "unknown"}. ${check.text}`
//
// where head is `- ${check.block_id} (${check.when}): ` and latest is the check's first
// result, the newest. `??` prints "unknown" for a key the answer leaves out or sends as null.
func oldClientResults(t *testing.T, read rawCheckResults) string {
	t.Helper()
	field := func(result map[string]json.RawMessage, name string) string {
		var value string
		if err := json.Unmarshal(result[name], &value); err != nil {
			t.Fatalf("result field %s=%s: %v", name, result[name], err)
		}
		return value
	}
	lines := []string{"Checks of " + read.Issue + ":"}
	for _, check := range read.Checks {
		head := fmt.Sprintf("- %s (%s): ", check.BlockID, check.When)
		if len(check.Results) == 0 {
			lines = append(lines, head+"no result yet. "+check.Text)
			continue
		}
		latest := check.Results[0]
		currency := "unknown"
		if raw, held := latest["currency"]; held && string(raw) != "null" {
			currency = field(latest, "currency")
		}
		lines = append(lines, fmt.Sprintf("%s%s at %s by %s, %s. %s",
			head, field(latest, "verdict"), field(latest, "commit_sha"), field(latest, "role"), currency, check.Text))
	}
	return strings.Join(lines, "\n")
}

// A client before the results contract change prints `current` for a check whose newest result
// is at the pull request's head and `older` for one whose newest result is at the head a push
// replaced, and "unknown" for neither, as it did against the server it was released with.
func TestTheClientBeforeTheResultsContractPrintsCurrentAndOlder(t *testing.T) {
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })
	issue := issueInRepository(t, handler, "RESCV", "acme/widgets", "Printed")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecks)
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", issue.Key)
	recordResult(t, handler, issue.Key, resultBody("k3", "pass", headCommit, map[string]any{"role": "tester"}))
	writeHead(t, database, "acme/widgets", 7, newHeadCommit, "open", issue.Key)
	recordResult(t, handler, issue.Key, resultBody("k1", "pass", newHeadCommit, nil))

	read := readRawResults(t, handler, issue.Key, noCommitNamed)
	printed := oldClientResults(t, read)
	texts := map[string]string{}
	for _, check := range read.Checks {
		texts[check.BlockID] = check.Text
	}
	want := strings.Join([]string{
		"Checks of " + issue.Key + ":",
		fmt.Sprintf("- k1 (before_merge): pass at %s by implementer, current. %s", newHeadCommit, texts["k1"]),
		fmt.Sprintf("- k3 (before_merge): pass at %s by tester, older. %s", headCommit, texts["k3"]),
	}, "\n")
	if printed != want {
		t.Errorf("the client before the results contract prints\n%s\nwant\n%s", printed, want)
	}
}
