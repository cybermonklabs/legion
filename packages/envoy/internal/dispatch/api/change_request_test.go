package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The change request of an issue and the end of it (D6, D10 of the work-units plan; R23, R26,
// R27). GET /api/v1/issues/{key}/change-request reads what an issue's spec proposes against the
// approved spec of its repository, and judges the conclusion its spec holds against the reviewer's
// final list; POST /api/v1/issues/{key}/outcomes records the three lists of outcomes of a pull
// request's head; GET /api/v1/issues/{key}/change-request/ending answers whether the change may
// land at a head. These tests drive those routes on the spec source the G1 tests seed (specRig).

// The heads of a pull request these tests record lists at.
var (
	headOne = strings.Repeat("a", 40)
	headTwo = strings.Repeat("b", 40)
)

// The sessions that write the lists: the pull request's author and its reviewer.
const (
	authorSession   = "session-author-0123456789"
	reviewerSession = "session-reviewer-0123456789"
)

// changeRequestNode is one entry of proposals or changed_checks.
type changeRequestNode struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	What     string `json:"what"`
	Base     int    `json:"base"`
	Current  int    `json:"current"`
	Block    string `json:"block"`
	Approved bool   `json:"approved"`
}

// outcomeJSON is one outcome in its JSON form.
type outcomeJSON struct {
	Text      string `json:"text"`
	Statement string `json:"statement,omitempty"`
	Restored  bool   `json:"restored,omitempty"`
}

// judgmentReason is one reason of a judgment.
type judgmentReason struct {
	Code    string `json:"code"`
	Detail  string `json:"detail"`
	Outcome string `json:"outcome"`
}

// finalListRef is the final list a judgment read.
type finalListRef struct {
	ID          int64     `json:"id"`
	PullRequest string    `json:"pull_request"`
	Head        string    `json:"head"`
	RecordedAt  time.Time `json:"recorded_at"`
}

// changeRequestRead is GET /api/v1/issues/{key}/change-request's answer, as the contract writes it.
type changeRequestRead struct {
	Issue                   string              `json:"issue"`
	Version                 int                 `json:"version"`
	SpecSource              bool                `json:"spec_source"`
	Repo                    string              `json:"repo"`
	VerificationVersion     int                 `json:"verification_version"`
	ApprovedSpecMark        int64               `json:"approved_spec_mark"`
	Proposes                bool                `json:"proposes"`
	ChangesChecks           bool                `json:"changes_checks"`
	ProposesUnapproved      bool                `json:"proposes_unapproved"`
	ChangesChecksUnapproved bool                `json:"changes_checks_unapproved"`
	Proposals               []changeRequestNode `json:"proposals"`
	ChangedChecks           []changeRequestNode `json:"changed_checks"`
	Problems                []struct {
		Block  string `json:"block"`
		Rule   string `json:"rule"`
		Detail string `json:"detail"`
	} `json:"problems"`
	Conclusion *struct {
		Block       string        `json:"block"`
		Statement   string        `json:"statement"`
		Difference  string        `json:"difference"`
		NoStatement bool          `json:"no_statement"`
		Evidence    []string      `json:"evidence"`
		Outcomes    []outcomeJSON `json:"outcomes"`
		Answers     []struct {
			Comment   string      `json:"comment"`
			Author    model.Actor `json:"author"`
			Body      string      `json:"body"`
			CreatedAt time.Time   `json:"created_at"`
		} `json:"answers"`
	} `json:"conclusion"`
	Judgment        string           `json:"judgment"`
	JudgmentReasons []judgmentReason `json:"judgment_reasons"`
	FinalList       *finalListRef    `json:"final_list"`
}

// changeRequestEnding is GET /api/v1/issues/{key}/change-request/ending's answer.
type changeRequestEnding struct {
	Issue   string `json:"issue"`
	Head    string `json:"head"`
	Version int    `json:"version"`
	State   string `json:"state"`
	Reasons []struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"reasons"`
	Judgment        string           `json:"judgment"`
	JudgmentReasons []judgmentReason `json:"judgment_reasons"`
	Approved        bool             `json:"approved"`
	FinalList       *finalListRef    `json:"final_list"`
}

// outcomeRecord is one record POST /api/v1/issues/{key}/outcomes answers and the list reads.
type outcomeRecord struct {
	ID          int64         `json:"id"`
	Issue       string        `json:"issue"`
	PullRequest string        `json:"pull_request"`
	Head        string        `json:"head"`
	List        string        `json:"list"`
	Outcomes    []outcomeJSON `json:"outcomes"`
	Actor       model.Actor   `json:"actor"`
	RecordedAt  time.Time     `json:"recorded_at"`
}

// conclusionBlock is a conclusion block of id with attributes (each ` name="value"`) and the
// paragraphs of its body, one Evidence: or Outcome: line each.
func conclusionBlock(id, attributes string, paragraphs ...string) string {
	return ":::conclusion{#" + id + attributes + "}\n" + strings.Join(paragraphs, "\n\n") + "\n:::\n"
}

// readChangeRequest answers GET /api/v1/issues/{key}/change-request with query as alice reads it.
func readChangeRequest(t *testing.T, rig *specRig, key, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/issues/" + key + "/change-request"
	if query != "" {
		target += "?" + query
	}
	return dispatchRequest(t, rig.handler, http.MethodGet, target, nil, "alice")
}

// changeRequestOf is the change request of key, failing unless it is answered 200.
func changeRequestOf(t *testing.T, rig *specRig, key, query string) (changeRequestRead, string) {
	t.Helper()
	response := readChangeRequest(t, rig, key, query)
	if response.Code != http.StatusOK {
		t.Fatalf("read the change request of %s (%s): status=%d body=%s", key, query, response.Code, response.Body.String())
	}
	return decodeBody[changeRequestRead](t, response), response.Body.String()
}

// endingOf is the ending of key at head, failing unless it is answered 200.
func endingOf(t *testing.T, rig *specRig, key, head string) changeRequestEnding {
	t.Helper()
	response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+key+"/change-request/ending?head="+head, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the ending of %s at %s: status=%d body=%s", key, head, response.Code, response.Body.String())
	}
	return decodeBody[changeRequestEnding](t, response)
}

// endingCodes is the codes of the ending's reasons, in order.
func endingCodes(ending changeRequestEnding) []string {
	codes := []string{}
	for _, reason := range ending.Reasons {
		codes = append(codes, reason.Code)
	}
	return codes
}

// reasonCodes is the codes of a judgment's reasons, each with the outcome it names after a space.
func reasonCodes(reasons []judgmentReason) []string {
	codes := []string{}
	for _, reason := range reasons {
		code := reason.Code
		if reason.Outcome != "" {
			code += " " + reason.Outcome
		}
		codes = append(codes, code)
	}
	return codes
}

// postOutcomes records list at head of the pull request as the session, with outcomes, each one
// outcome in its JSON form.
func postOutcomes(t *testing.T, rig *specRig, key, session, pullRequest, head, list string, outcomes ...outcomeJSON) *httptest.ResponseRecorder {
	t.Helper()
	if outcomes == nil {
		outcomes = []outcomeJSON{}
	}
	return sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+key+"/outcomes", map[string]any{
		"actor": map[string]any{"kind": "session", "id": session}, "pull_request": pullRequest, "head": head, "list": list, "outcomes": outcomes,
	})
}

// recordOutcomes is postOutcomes, failing unless the record is written, and answers it.
func recordOutcomes(t *testing.T, rig *specRig, key, session, pullRequest, head, list string, outcomes ...outcomeJSON) outcomeRecord {
	t.Helper()
	response := postOutcomes(t, rig, key, session, pullRequest, head, list, outcomes...)
	if response.Code != http.StatusCreated {
		t.Fatalf("record the %s list of %s at %s as %s: status=%d body=%s", list, pullRequest, head, session, response.Code, response.Body.String())
	}
	return decodeBody[outcomeRecord](t, response)
}

// recordThreeLists records the author's list, the reviewer's own list and the reviewer's final
// list of the pull request at head, the final list holding final.
func recordThreeLists(t *testing.T, rig *specRig, key, pullRequest, head string, final ...outcomeJSON) outcomeRecord {
	t.Helper()
	recordOutcomes(t, rig, key, authorSession, pullRequest, head, "author", final...)
	recordOutcomes(t, rig, key, reviewerSession, pullRequest, head, "reviewer", final...)
	return recordOutcomes(t, rig, key, reviewerSession, pullRequest, head, "final", final...)
}

// refusedWith fails unless response is status with the code.
func refusedWith(t *testing.T, name string, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	body := map[string]any{}
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if response.Code != status || body["code"] != code {
		t.Errorf("%s: status=%d body=%s, want %d %s", name, response.Code, response.Body.String(), status, code)
	}
}

// TestChangeRequestReadsWhatAnIssueProposes: an issue of a spec source that proposes nothing, one
// that proposes a statement, one that changes an approved check, and an issue of a repository that
// is no spec source, each read at its latest version with its conclusion judged.
func TestChangeRequestReadsWhatAnIssueProposes(t *testing.T) {
	rig := newSpecRig(t)
	rig.seed(t)

	t.Run("an issue that proposes nothing", func(t *testing.T) {
		// A statement written again at the version it was approved at is context, not a proposal.
		spec := "Why the deploy changes.\n\n" +
			":::requirement{#an-order-is-paid-once kind=\"behavior\" about=\"billing\" base=\"1\"}\nAn order is paid once.\n:::\n\n" +
			conclusionBlock("c-deploy", ` no_statement="true"`, "Evidence: The deploy job named the wrong host.")
		issue := rig.issue(t, "Fix the deploy host", specRepo, spec, "")
		read, body := changeRequestOf(t, rig, issue.Key, "")
		if read.Issue != issue.Key || read.Version != 1 || !read.SpecSource || read.Repo != specRepo || read.VerificationVersion != 0 {
			t.Errorf("the request reads issue %s version %d spec_source %t repo %s verification %d, want %s 1 true %s 0",
				read.Issue, read.Version, read.SpecSource, read.Repo, read.VerificationVersion, issue.Key, specRepo)
		}
		if read.Proposes || read.ChangesChecks {
			t.Errorf("the request proposes %t and changes checks %t, want neither: %s", read.Proposes, read.ChangesChecks, body)
		}
		for _, empty := range []string{`"proposals":[]`, `"changed_checks":[]`, `"problems":[]`} {
			if !strings.Contains(body, empty) {
				t.Errorf("the answer %s does not hold %s", body, empty)
			}
		}
		if read.Conclusion == nil || read.Conclusion.Block != "c-deploy" || !read.Conclusion.NoStatement ||
			!reflect.DeepEqual(read.Conclusion.Evidence, []string{"The deploy job named the wrong host."}) {
			t.Errorf("the conclusion reads %+v, want c-deploy with no_statement and its evidence", read.Conclusion)
		}
		if read.Judgment != "complete" || len(read.JudgmentReasons) != 0 || read.FinalList != nil {
			t.Errorf("the judgment is %s %v with the final list %+v, want complete, no reason and no final list", read.Judgment, read.JudgmentReasons, read.FinalList)
		}
	})

	t.Run("an issue that proposes a statement", func(t *testing.T) {
		issue := rig.issue(t, "Refunds", specRepo,
			":::requirement{#a-refund-returns-the-payment kind=\"behavior\" about=\"billing\"}\nA refund returns the payment of an order.\n:::\n", "")
		read, _ := changeRequestOf(t, rig, issue.Key, "")
		want := []changeRequestNode{{ID: "a-refund-returns-the-payment", Kind: "statement", What: "added", Base: 0, Current: 0,
			Block: specAddress(t, rig, issue) + "#a-refund-returns-the-payment"}}
		if !read.Proposes || read.ChangesChecks || !reflect.DeepEqual(read.Proposals, want) || len(read.ChangedChecks) != 0 {
			t.Errorf("the request reads proposes %t changes_checks %t proposals %+v changed %+v, want it to propose %+v alone",
				read.Proposes, read.ChangesChecks, read.Proposals, read.ChangedChecks, want)
		}
		if read.Conclusion != nil || read.Judgment != "none" || len(read.JudgmentReasons) != 0 {
			t.Errorf("an issue with no conclusion judges %s %v with %+v, want none", read.Judgment, read.JudgmentReasons, read.Conclusion)
		}
	})

	t.Run("an issue that changes an approved check", func(t *testing.T) {
		changed := strings.Replace(paidOnceCheck("1"), "Claim: A second payment of one order is refused.", "Claim: A second payment of one order is refused at once.", 1)
		issue := rig.issue(t, "A sharper check", specRepo, "The check of the payment changes.\n", changed)
		read, _ := changeRequestOf(t, rig, issue.Key, "")
		want := []changeRequestNode{{ID: "a-second-payment-is-refused", Kind: "check", What: "changed", Base: 1, Current: 1,
			Block: verificationAddress(t, rig, issue) + "#a-second-payment-is-refused"}}
		if read.Proposes || !read.ChangesChecks || len(read.Proposals) != 0 || !reflect.DeepEqual(read.ChangedChecks, want) || read.VerificationVersion != 1 {
			t.Errorf("the request reads proposes %t changes_checks %t proposals %+v changed %+v verification %d, want only %+v at verification 1",
				read.Proposes, read.ChangesChecks, read.Proposals, read.ChangedChecks, read.VerificationVersion, want)
		}
	})

	t.Run("an issue of a repository that is no spec source", func(t *testing.T) {
		issue := rig.issue(t, "Elsewhere", "acme/elsewhere",
			":::requirement{#a-page-loads kind=\"behavior\"}\nA page loads.\n:::\n\n"+
				conclusionBlock("c-elsewhere", ` statement="an-order-is-paid-once" difference="code_breaks"`, "Evidence: The page test fails."), "")
		read, body := changeRequestOf(t, rig, issue.Key, "")
		if read.SpecSource || read.Proposes || read.ChangesChecks || read.Repo != "acme/elsewhere" {
			t.Errorf("the request reads spec_source %t proposes %t changes_checks %t repo %s, want none of a spec source: %s",
				read.SpecSource, read.Proposes, read.ChangesChecks, read.Repo, body)
		}
		for _, empty := range []string{`"proposals":[]`, `"changed_checks":[]`, `"problems":[]`} {
			if !strings.Contains(body, empty) {
				t.Errorf("the answer %s does not hold %s", body, empty)
			}
		}
		// Nothing is approved outside a spec source, so the statement the conclusion names is none.
		if read.Judgment != "incomplete" || !reflect.DeepEqual(reasonCodes(read.JudgmentReasons), []string{"statement_not_approved"}) {
			t.Errorf("the conclusion outside a spec source judges %s %v, want incomplete for statement_not_approved", read.Judgment, reasonCodes(read.JudgmentReasons))
		}
	})
}

// specAddress and verificationAddress are the addresses of the issue's spec and Verification
// document as a proposal's block names them.
func specAddress(t *testing.T, rig *specRig, issue model.Issue) string {
	t.Helper()
	artifact, err := artifactMetadata(context.Background(), rig.database.Pool, issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("read the spec of %s: %v", issue.Key, err)
	}
	return artifact.RefKey
}

func verificationAddress(t *testing.T, rig *specRig, issue model.Issue) string {
	t.Helper()
	var refKey string
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select ref_key from artifacts where issue_key = $1 and slug = 'verification-md'`, issue.Key).Scan(&refKey); err != nil {
		t.Fatalf("read the Verification document of %s: %v", issue.Key, err)
	}
	return refKey
}

// TestChangeRequestRefusesWhatItCannotRead: a version or a head that is no version or head, a
// version the spec does not hold, an issue Dispatch does not hold and a version whose text the
// parser cannot read are each an error, never an empty answer.
func TestChangeRequestRefusesWhatItCannotRead(t *testing.T) {
	rig := newSpecRig(t)
	issue := rig.issue(t, "An unreadable request", specRepo, "Prose.\n", "")
	for _, test := range []struct {
		name, key, query string
		status           int
		code             string
	}{
		{"a version that is no number", issue.Key, "version=latest", http.StatusBadRequest, "INVALID_VERSION"},
		{"a version below 1", issue.Key, "version=0", http.StatusBadRequest, "INVALID_VERSION"},
		{"a version the spec does not hold", issue.Key, "version=9", http.StatusNotFound, "VERSION_NOT_FOUND"},
		{"a head that is no commit", issue.Key, "head=HEAD", http.StatusBadRequest, "INVALID_HEAD"},
		{"an issue Dispatch does not hold", fmt.Sprintf("%s-%d", "CORE", 9999), "", http.StatusNotFound, "ISSUE_NOT_FOUND"},
	} {
		refusedWith(t, test.name, readChangeRequest(t, rig, test.key, test.query), test.status, test.code)
	}
	for _, query := range []string{"", "head=", "head=" + strings.ToUpper(headOne)} {
		response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/change-request/ending?"+query, nil, "alice")
		refusedWith(t, "the ending with "+query, response, http.StatusBadRequest, "INVALID_HEAD")
	}
	storeUnreadableVersion(t, rig.database, issue.PrimaryArtifactID, 1)
	refusedWith(t, "a version the parser cannot read", readChangeRequest(t, rig, issue.Key, ""), http.StatusConflict, "CHANGE_REQUEST_UNREADABLE")
	refusedWith(t, "the ending of a version the parser cannot read",
		dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/change-request/ending?head="+headOne, nil, "alice"),
		http.StatusConflict, "CHANGE_REQUEST_UNREADABLE")
}

// TestConclusionRules: the change request judges its conclusion by D6's rules, against the
// reviewer's final list where one is recorded: a conclusion with no evidence, a kind with no
// statement and a statement with no kind are incomplete; no_statement with evidence is complete; a
// final list's outcome the conclusion leaves out is incomplete; an outcome no approved statement
// covers is uncovered; an outcome an approved statement requires is covered.
func TestConclusionRules(t *testing.T) {
	rig := newSpecRig(t)
	rig.seed(t)
	const pullRequest = "acme/app#12"
	for _, test := range []struct {
		name       string
		conclusion string
		final      []outcomeJSON
		judgment   string
		reasons    []string
	}{
		{"no evidence", conclusionBlock("c-rule", ` no_statement="true"`, "Prose with no evidence."), nil, "incomplete", []string{"no_evidence"}},
		{"a kind with no statement", conclusionBlock("c-rule", ` difference="code_breaks"`, "Evidence: The test fails."), nil,
			"incomplete", []string{"no_statement_or_kind"}},
		{"a statement with no kind", conclusionBlock("c-rule", ` statement="an-order-is-paid-once"`, "Evidence: The test fails."), nil,
			"incomplete", []string{"no_statement_or_kind"}},
		{"no_statement with evidence", conclusionBlock("c-rule", ` no_statement="true"`, "Evidence: The deploy job named the wrong host."), []outcomeJSON{},
			"complete", []string{}},
		{"an outcome of the final list the conclusion leaves out", conclusionBlock("c-rule", ` statement="an-order-is-paid-once" difference="code_breaks"`,
			"Evidence: The test fails before the fix.", "Outcome: (restored) A second payment is refused again."),
			[]outcomeJSON{{Text: "A second payment is refused again.", Restored: true}, {Text: "The refusal names the order.", Statement: "a-bill-names-its-order"}},
			"incomplete", []string{"omits_outcome The refusal names the order."}},
		{"an outcome no approved statement covers", conclusionBlock("c-rule", ` statement="an-order-is-paid-once" difference="code_breaks"`,
			"Evidence: The test fails before the fix.", "Outcome: A refund is paid back."),
			[]outcomeJSON{{Text: "A refund is paid back."}}, "uncovered", []string{"uncovered_outcome A refund is paid back."}},
		{"an approved statement covers the outcome", conclusionBlock("c-rule", ` statement="an-order-is-paid-once" difference="code_breaks"`,
			"Evidence: The test fails before the fix.", "Outcome: (restored) A second payment is refused again.",
			"Outcome: (requires a-bill-names-its-order) The refusal names the order."),
			[]outcomeJSON{{Text: "A second payment is refused again.", Restored: true}, {Text: "The refusal names the order.", Statement: "a-bill-names-its-order"}},
			"complete", []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := rig.issue(t, test.name, specRepo, "Prose.\n\n"+test.conclusion, "")
			query := ""
			if test.final != nil {
				recordThreeLists(t, rig, issue.Key, pullRequest, headOne, test.final...)
				query = "head=" + headOne
			}
			read, body := changeRequestOf(t, rig, issue.Key, query)
			if read.Judgment != test.judgment || !reflect.DeepEqual(reasonCodes(read.JudgmentReasons), test.reasons) {
				t.Errorf("the conclusion judges %s %v, want %s %v: %s", read.Judgment, reasonCodes(read.JudgmentReasons), test.judgment, test.reasons, body)
			}
			if test.final != nil && (read.FinalList == nil || read.FinalList.Head != headOne || read.FinalList.PullRequest != pullRequest) {
				t.Errorf("the judgment read the final list %+v, want the one at %s of %s", read.FinalList, headOne, pullRequest)
			}
		})
	}
}

// TestOutcomeListRefusals: the author's own reviewer and final lists are refused, as is the
// reviewer's author list; a final list before any author list of the pull request, and one before
// the reviewer's own list at its head, are refused; a malformed record is refused; and each list
// recorded is a record of its own, read back oldest first, with spec.outcomes_recorded appended on
// the issue for each.
func TestOutcomeListRefusals(t *testing.T) {
	rig := newSpecRig(t)
	issue := rig.issue(t, "Lists of outcomes", specRepo, "Prose.\n", "")
	const pullRequest = "acme/app#7"
	outcome := outcomeJSON{Text: "A second payment is refused.", Statement: "an-order-is-paid-once"}

	refusedWith(t, "a final list before any author list", postOutcomes(t, rig, issue.Key, reviewerSession, pullRequest, headOne, "final", outcome),
		http.StatusConflict, "OUTCOME_AUTHOR_LIST_FIRST")
	author := recordOutcomes(t, rig, issue.Key, authorSession, pullRequest, headOne, "author", outcome)
	if author.Issue != issue.Key || author.PullRequest != pullRequest || author.Head != headOne || author.List != "author" ||
		author.Actor.Kind != "session" || author.Actor.ID != authorSession || !reflect.DeepEqual(author.Outcomes, []outcomeJSON{outcome}) || author.ID == 0 {
		t.Errorf("the author's record reads %+v, want the list as sent, by %s", author, authorSession)
	}
	refusedWith(t, "the author's reviewer list", postOutcomes(t, rig, issue.Key, authorSession, pullRequest, headOne, "reviewer", outcome),
		http.StatusForbidden, "OUTCOME_LIST_BY_AUTHOR")
	refusedWith(t, "the author's final list", postOutcomes(t, rig, issue.Key, authorSession, pullRequest, headOne, "final", outcome),
		http.StatusForbidden, "OUTCOME_LIST_BY_AUTHOR")
	refusedWith(t, "a final list before the reviewer's own list", postOutcomes(t, rig, issue.Key, reviewerSession, pullRequest, headOne, "final", outcome),
		http.StatusConflict, "OUTCOME_REVIEWER_LIST_FIRST")
	recordOutcomes(t, rig, issue.Key, reviewerSession, pullRequest, headTwo, "reviewer", outcome)
	refusedWith(t, "a final list after the reviewer's own list at another head",
		postOutcomes(t, rig, issue.Key, reviewerSession, pullRequest, headOne, "final", outcome), http.StatusConflict, "OUTCOME_REVIEWER_LIST_FIRST")
	recordOutcomes(t, rig, issue.Key, reviewerSession, pullRequest, headOne, "reviewer", outcome)
	final := recordOutcomes(t, rig, issue.Key, reviewerSession, pullRequest, headOne, "final", outcome, outcomeJSON{Text: "The refusal names the order."})
	refusedWith(t, "the reviewer's author list", postOutcomes(t, rig, issue.Key, reviewerSession, pullRequest, headOne, "author", outcome),
		http.StatusForbidden, "OUTCOME_LIST_BY_REVIEWER")
	empty := recordOutcomes(t, rig, issue.Key, reviewerSession, pullRequest, headOne, "final")
	if len(empty.Outcomes) != 0 || empty.ID <= final.ID {
		t.Errorf("the empty final list reads %+v, want a later record holding no outcome", empty)
	}

	for _, test := range []struct {
		name, pullRequest, head, list string
		outcomes                      []outcomeJSON
		code                          string
	}{
		{"a pull request that is no reference", "acme/app", headOne, "author", []outcomeJSON{outcome}, "INVALID_PULL_REQUEST"},
		{"a head that is no commit", pullRequest, "abc", "author", []outcomeJSON{outcome}, "INVALID_HEAD"},
		{"a list that is none of the three", pullRequest, headOne, "draft", []outcomeJSON{outcome}, "INVALID_LIST"},
		{"a blank outcome", pullRequest, headOne, "author", []outcomeJSON{{Text: " "}}, "INVALID_OUTCOME"},
		{"an outcome both required and restored", pullRequest, headOne, "author", []outcomeJSON{{Text: "Both.", Statement: "an-order-is-paid-once", Restored: true}}, "INVALID_OUTCOME"},
		{"an outcome whose statement is no id", pullRequest, headOne, "author", []outcomeJSON{{Text: "Bad id.", Statement: "Not An Id"}}, "INVALID_OUTCOME"},
		{"two outcomes of one key", pullRequest, headOne, "author", []outcomeJSON{{Text: "One outcome."}, {Text: "one   OUTCOME"}}, "INVALID_OUTCOME"},
	} {
		refusedWith(t, test.name, postOutcomes(t, rig, issue.Key, authorSession, test.pullRequest, test.head, test.list, test.outcomes...), http.StatusBadRequest, test.code)
	}

	records := decodeBody[[]outcomeRecord](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/outcomes", nil, "alice"))
	var lists []string
	for _, record := range records {
		lists = append(lists, record.List+" "+record.Head[:1])
	}
	if want := []string{"author a", "reviewer b", "reviewer a", "final a", "final a"}; !reflect.DeepEqual(lists, want) {
		t.Errorf("the records read %v, want %v, oldest first", lists, want)
	}
	finals := decodeBody[[]outcomeRecord](t, dispatchRequest(t, rig.handler, http.MethodGet,
		"/api/v1/issues/"+issue.Key+"/outcomes?list=final&head="+headOne+"&pull_request="+url.QueryEscape(pullRequest), nil, "alice"))
	if len(finals) != 2 || finals[0].ID != final.ID || finals[1].ID != empty.ID {
		t.Errorf("the final lists at %s read %+v, want the two recorded", headOne, finals)
	}

	var payloads []string
	rows, err := rig.database.Pool.Query(context.Background(), `select payload::text from events where issue_key = $1 and type = 'spec.outcomes_recorded' order by id`, issue.Key)
	if err != nil {
		t.Fatalf("read the spec.outcomes_recorded events of %s: %v", issue.Key, err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("read a spec.outcomes_recorded event: %v", err)
		}
		payloads = append(payloads, payload)
	}
	if len(payloads) != len(records) {
		t.Fatalf("%s holds %d spec.outcomes_recorded events, want one per record (%d)", issue.Key, len(payloads), len(records))
	}
	var last map[string]any
	if err := json.Unmarshal([]byte(payloads[len(payloads)-2]), &last); err != nil {
		t.Fatalf("decode the payload %s: %v", payloads[len(payloads)-2], err)
	}
	want := map[string]any{"issue": issue.Key, "record": float64(final.ID), "pull_request": pullRequest, "head": headOne, "list": "final", "outcomes": float64(2)}
	if !reflect.DeepEqual(last, want) {
		t.Errorf("the final list's event payload is %v, want %v", last, want)
	}
}

// TestChangeRequestEnding: the ending of a change is bound to the head that would land. It waits
// while no final list is recorded, while the only final list is of another head, while no
// conclusion exists, while the conclusion is incomplete, and while an outcome is uncovered and the
// person has not approved the latest spec; it is ready once the head's final list and a complete
// conclusion exist, and for an uncovered outcome once the person approves the latest version. A
// final list recorded on a descendant issue counts for its root.
func TestChangeRequestEnding(t *testing.T) {
	rig := newSpecRig(t)
	rig.seed(t)
	const pullRequest = "acme/app#21"
	root := rig.issue(t, "A change that proposes nothing", specRepo, "Why the deploy changes.\n", "")
	child := childIssue(t, rig, root.Key, "The work under it")
	expect := func(t *testing.T, key, head, state string, codes []string) changeRequestEnding {
		t.Helper()
		ending := endingOf(t, rig, key, head)
		if ending.Issue != key || ending.Head != head || ending.State != state || !reflect.DeepEqual(endingCodes(ending), codes) {
			t.Errorf("the ending at %s reads %s %s %s %v, want %s %s %v", head, ending.Issue, ending.Head, ending.State, endingCodes(ending), key, state, codes)
		}
		for _, reason := range ending.Reasons {
			if reason.Detail == "" {
				t.Errorf("the reason %s carries no detail", reason.Code)
			}
		}
		return ending
	}

	expect(t, root.Key, headOne, "waiting", []string{"no_final_list_for_head", "no_conclusion"})
	recordThreeLists(t, rig, child.Key, pullRequest, headOne)
	expect(t, root.Key, headTwo, "waiting", []string{"final_list_of_another_head", "no_conclusion"})
	atOne := expect(t, root.Key, headOne, "waiting", []string{"no_conclusion"})
	if atOne.FinalList == nil || atOne.FinalList.Head != headOne || atOne.Judgment != "none" {
		t.Errorf("the ending at %s read the final list %+v and judged %s, want the child's final list and none", headOne, atOne.FinalList, atOne.Judgment)
	}

	editSpec(t, rig.handler, root.Key, []map[string]any{{"op": "insert", "after": "end",
		"markdown": conclusionBlock("c-ending", ` no_statement="true"`, "The host is wrong.")}})
	incomplete := expect(t, root.Key, headOne, "waiting", []string{"incomplete"})
	if incomplete.Judgment != "incomplete" || !reflect.DeepEqual(reasonCodes(incomplete.JudgmentReasons), []string{"no_evidence"}) || incomplete.Version != 2 {
		t.Errorf("the ending judged %s %v at version %d, want incomplete for no_evidence at version 2", incomplete.Judgment, reasonCodes(incomplete.JudgmentReasons), incomplete.Version)
	}
	editSpec(t, rig.handler, root.Key, []map[string]any{{"op": "replace", "find": "The host is wrong.", "with": "Evidence: The deploy job named the wrong host."}})
	ready := expect(t, root.Key, headOne, "ready", []string{})
	if ready.Judgment != "complete" || ready.Approved {
		t.Errorf("the ready ending judged %s with approved %t, want complete and no approval", ready.Judgment, ready.Approved)
	}
	expect(t, root.Key, headTwo, "waiting", []string{"final_list_of_another_head"})

	// Another change whose reviewer finds behaviour no approved statement covers waits for the
	// person's approval of its whole spec, and the exemption that let it start does not stand for it.
	uncovered := outcomeJSON{Text: "A deploy names its host in the log."}
	behaviour := rig.issue(t, "A change of behaviour", specRepo, "Why the log changes.\n\n"+
		conclusionBlock("c-log", ` no_statement="true"`, "Evidence: The log names no host.", "Outcome: "+uncovered.Text), "")
	recordThreeLists(t, rig, behaviour.Key, "acme/app#22", headOne, uncovered)
	waiting := expect(t, behaviour.Key, headOne, "waiting", []string{"uncovered_without_approval"})
	if waiting.Judgment != "uncovered" || waiting.Approved {
		t.Errorf("the ending judged %s with approved %t, want uncovered and no approval", waiting.Judgment, waiting.Approved)
	}
	rig.approve(t, rig.request(t, behaviour, false))
	approved := expect(t, behaviour.Key, headOne, "ready", []string{})
	if approved.Judgment != "uncovered" || !approved.Approved {
		t.Errorf("the approved ending judged %s with approved %t, want uncovered and approved", approved.Judgment, approved.Approved)
	}
}

// childIssue creates, as alice, an issue under parent whose spec holds prose alone.
func childIssue(t *testing.T, rig *specRig, parent, title string) model.Issue {
	t.Helper()
	response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": title, "spec": "The work under the change.\n", "parent": parent, "force": true,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create the child %q of %s: status=%d body=%s", title, parent, response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// TestConclusionAnswersAreAnchoredComments: the person's answer to a conclusion is a comment
// anchored to the conclusion's block, and the change request shows each such comment, oldest
// first, and no other comment of the issue.
func TestConclusionAnswersAreAnchoredComments(t *testing.T) {
	rig := newSpecRig(t)
	issue := rig.issue(t, "An answered conclusion", specRepo, "Prose about the change.\n\n"+
		conclusionBlock("c-answered", ` no_statement="true"`, "Evidence: The deploy job named the wrong host."), "")
	comment := func(body string, anchor map[string]any) string {
		t.Helper()
		input := map[string]any{"body": body}
		if anchor != nil {
			input["anchor"] = anchor
		}
		response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", input, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("post the comment %q: status=%d body=%s", body, response.Code, response.Body.String())
		}
		return decodeBody[struct {
			ID string `json:"id"`
		}](t, response).ID
	}
	answer := comment("The host was wrong; this lands as it is.", map[string]any{
		"artifact": "spec", "quote": "The deploy job named the wrong host.", "block_id": "c-answered"})
	comment("A remark on the prose.", map[string]any{"artifact": "spec", "quote": "Prose about the change."})
	comment("A remark on the issue.", nil)
	read, body := changeRequestOf(t, rig, issue.Key, "")
	if read.Conclusion == nil || len(read.Conclusion.Answers) != 1 {
		var anchors []string
		rows, err := rig.database.Pool.Query(context.Background(), `select coalesce(anchor::text, 'none') from comments where issue_key = $1 order by created_at`, issue.Key)
		if err == nil {
			for rows.Next() {
				var anchor string
				if rows.Scan(&anchor) == nil {
					anchors = append(anchors, anchor)
				}
			}
			rows.Close()
		}
		t.Fatalf("the conclusion reads %s, want the one anchored answer; the comments hold the anchors %v", body, anchors)
	}
	got := read.Conclusion.Answers[0]
	if got.Comment != answer || got.Author.ID != "alice" || got.Body != "The host was wrong; this lands as it is." || got.CreatedAt.IsZero() {
		t.Errorf("the answer reads %+v, want comment %s by alice with its body", got, answer)
	}
}

// acceptanceCheckDocument is a Verification document holding the issue's own acceptance check: a
// check block whose statement attribute names no statement, so it is no check of The Spec.
const acceptanceCheckDocument = ":::check{#the-deploy-names-its-host subject=\"code\" procedure=\"agent\"}\n" +
	"Claim: The deploy log names the host it deploys to.\n\n" +
	"Procedure: The agent deploys and reads the log.\n\n" +
	"Failing example: A deploy whose log names no host.\n:::\n"

// TestAnAcceptanceCheckIsNoPartOfTheChangeRequest: a check block is a check of The Spec only when
// its statement names a statement the request proposes or the approved spec holds (R22, R25). The
// issue's own acceptance check holds no gate - the change request of an issue that proposes
// nothing beside it reads proposes and changes_checks false - and the approval of a spec-source
// issue that holds one writes nothing for it and is not refused, with or without its
// Verification document.
func TestAnAcceptanceCheckIsNoPartOfTheChangeRequest(t *testing.T) {
	rig := newSpecRig(t)
	rig.seed(t)
	quiet := rig.issue(t, "A deploy fix with its own acceptance check", specRepo, "Why the deploy changes.\n", acceptanceCheckDocument)
	read, body := changeRequestOf(t, rig, quiet.Key, "")
	if read.Proposes || read.ChangesChecks || len(read.ChangedChecks) != 0 || len(read.Problems) != 0 || read.VerificationVersion != 1 {
		t.Errorf("the change request reads %s, want it to propose nothing and change no check beside the acceptance check", body)
	}
	for _, verification := range []bool{false, true} {
		issue := rig.issue(t, fmt.Sprintf("A refund with its own acceptance check (Verification document named: %t)", verification), specRepo,
			fmt.Sprintf(":::requirement{#a-refund-is-paid-back-%t kind=\"behavior\" about=\"billing\"}\nA refund is paid back.\n:::\n", verification),
			acceptanceCheckDocument)
		rig.approve(t, rig.request(t, issue, verification))
		rig.requireVersions(t, specRepo, fmt.Sprintf("a-refund-is-paid-back-%t", verification), "A refund is paid back.")
		rig.requireAbsent(t, specRepo, "the-deploy-names-its-host")
	}
}

// twoSentenceCheck is a check of a-bill-names-its-order whose claim is two sentences: a problem of
// form that keeps the node (specgraph.KeepsEachNode), so an approval writes the check as it stands.
func twoSentenceCheck(base string) string {
	return ":::check{#a-bill-shows-its-number statement=\"a-bill-names-its-order\" subject=\"code\" procedure=\"program\" " +
		"runner=\"go\" selector=\"example.com/billing TestABillShowsItsNumber\" job=\"go-test\"" + baseAttribute(base) + "}\n" +
		"Claim: A bill names its order. It shows the order's number.\n\n" +
		"Procedure: The test prints the bill of one order.\n\n" +
		"Failing example: A build whose bill names no order.\n:::\n"
}

// TestAProblemOfFormAloneProposesNothing: a problem that keeps each node (a claim of two sentences)
// is the form's, as the approval write reads it (specstore's nodeLosses), so it neither proposes
// nor changes a check, and an approved version that holds one is still read as its node. An issue
// whose Verification document holds such an approved check as context reads proposes and
// changes_checks false and still lists the problem, and its approval writes no new version.
func TestAProblemOfFormAloneProposesNothing(t *testing.T) {
	rig := newSpecRig(t)
	rig.seed(t)
	writer := rig.issue(t, "A bill shows its number", specRepo, "Why the bill changes.\n", twoSentenceCheck(""))
	rig.approve(t, rig.request(t, writer, true))
	rig.requireVersions(t, specRepo, "a-bill-shows-its-number", "A bill names its order. It shows the order's number.")
	quiet := rig.issue(t, "A printer fix beside the approved bill check", specRepo, "Why the printer changes.\n", twoSentenceCheck("1"))
	read, body := changeRequestOf(t, rig, quiet.Key, "")
	if read.Proposes || read.ChangesChecks || len(read.ChangedChecks) != 0 || read.VerificationVersion != 1 {
		t.Errorf("the change request of an issue holding an approved check as context reads %s; "+
			"want no proposal and no changed check, since a claim of two sentences keeps the node", body)
	}
	if len(read.Problems) != 1 || read.Problems[0].Rule != "bad-field" {
		t.Errorf("the change request lists the problems %+v, want the one bad-field problem of the two-sentence claim", read.Problems)
	}
	rig.approve(t, rig.request(t, quiet, true))
	rig.requireVersions(t, specRepo, "a-bill-shows-its-number", "A bill names its order. It shows the order's number.")
}

// TestTheEndingWaitsForWhatTheChangeRequestChangesUntilAPersonApprovesIt: the ending of a change
// is judged with its change request as the latest spec version and the latest settled version of
// the Verification document hold it (R23, R25, D10). An edit of the Verification document alone
// that changes an approved check makes the ending wait (changes_checks_without_approval) until a
// person approves the spec with that document; the same edit of an acceptance check that names no
// statement is no part of the change request and changes nothing. A version of the spec that
// proposes a statement makes it wait too (proposes_without_approval), until the person approves
// that version.
func TestTheEndingWaitsForWhatTheChangeRequestChangesUntilAPersonApprovesIt(t *testing.T) {
	rig := newSpecRig(t)
	rig.seed(t)
	conclusion := conclusionBlock("c-deploy-host", ` no_statement="true"`, "Evidence: The deploy job named the wrong host.")
	checked := rig.issue(t, "A deploy fix beside the approved payment check", specRepo, "Why the deploy changes.\n\n"+conclusion, paidOnceCheck("1"))
	accepted := rig.issue(t, "A deploy fix that holds its own acceptance check", specRepo, "Why the deploy changes.\n\n"+conclusion, acceptanceCheckDocument)
	proposing := rig.issue(t, "A deploy fix that gains a statement", specRepo, "Why the deploy changes.\n\n"+conclusion, "")
	for place, issue := range []model.Issue{checked, accepted, proposing} {
		recordThreeLists(t, rig, issue.Key, fmt.Sprintf("acme/app#%d", 41+place), headOne)
		if ending := endingOf(t, rig, issue.Key, headOne); ending.State != "ready" {
			t.Fatalf("the ending of %s before any edit reads %s %v, want ready: it proposes nothing and changes no approved check",
				issue.Key, ending.State, endingCodes(ending))
		}
	}

	// The Verification document alone changes the approved check; the spec stands as it was.
	editIssueDocument(t, rig.handler, checked.Key, "verification-md", []map[string]any{{"op": "replace",
		"find": "Claim: A second payment of one order is refused.", "with": "Claim: A second payment of one order is refused at once."}})
	if read, body := changeRequestOf(t, rig, checked.Key, ""); read.Version != 1 || read.VerificationVersion != 2 || read.Proposes || !read.ChangesChecks {
		t.Fatalf("the change request after the edit of the check reads %s; want spec version 1, Verification version 2 and a changed check alone", body)
	}
	waits := endingOf(t, rig, checked.Key, headOne)
	if waits.State != "waiting" || !reflect.DeepEqual(endingCodes(waits), []string{"changes_checks_without_approval"}) {
		t.Errorf("the ending after an edit of the approved check alone reads %s %v, want waiting for changes_checks_without_approval",
			waits.State, endingCodes(waits))
	}
	for _, reason := range waits.Reasons {
		if reason.Detail == "" {
			t.Errorf("the reason %s carries no detail", reason.Code)
		}
	}

	// The same edit of the acceptance check, which names no statement, changes nothing.
	editIssueDocument(t, rig.handler, accepted.Key, "verification-md", []map[string]any{{"op": "replace",
		"find": "Claim: The deploy log names the host it deploys to.", "with": "Claim: The deploy log names the host it deploys to, first."}})
	if read, body := changeRequestOf(t, rig, accepted.Key, ""); read.VerificationVersion != 2 || read.Proposes || read.ChangesChecks {
		t.Fatalf("the change request after the edit of the acceptance check reads %s; want Verification version 2 and no change", body)
	}
	if ending := endingOf(t, rig, accepted.Key, headOne); ending.State != "ready" {
		t.Errorf("the ending after an edit of the acceptance check reads %s %v, want ready", ending.State, endingCodes(ending))
	}

	// A version of the spec that proposes a statement waits for the person too.
	editSpec(t, rig.handler, proposing.Key, []map[string]any{{"op": "insert", "after": "end",
		"markdown": ":::requirement{#a-deploy-names-its-host kind=\"behavior\" about=\"billing\"}\nA deploy names its host.\n:::\n"}})
	proposes := endingOf(t, rig, proposing.Key, headOne)
	if proposes.State != "waiting" || !reflect.DeepEqual(endingCodes(proposes), []string{"proposes_without_approval"}) {
		t.Errorf("the ending after a version that proposes a statement reads %s %v, want waiting for proposes_without_approval",
			proposes.State, endingCodes(proposes))
	}

	// The person's approval of the spec, with the Verification document where it changed, releases each.
	rig.approve(t, rig.request(t, checked, true))
	if ending := endingOf(t, rig, checked.Key, headOne); ending.State != "ready" {
		t.Errorf("the ending after the person approved the spec with its Verification document reads %s %v, want ready",
			ending.State, endingCodes(ending))
	}
	rig.approve(t, rig.request(t, proposing, false))
	if ending := endingOf(t, rig, proposing.Key, headOne); ending.State != "ready" || !ending.Approved {
		t.Errorf("the ending after the person approved the version that proposes a statement reads %s %v approved %t, want ready and approved",
			ending.State, endingCodes(ending), ending.Approved)
	}
}

// The sessions of the tests below beside authorSession and reviewerSession: a second writer of an
// author's list of the same pull request.
const secondAuthorSession = "session-second-author-0123456789"

// TestTheListsOfAPullRequestAreOneHistoryWhicheverIssueOfTheTreeRecordsThem (R27): the ending of
// a tree reads the final lists recorded for every issue of the tree, so the author and reviewer
// separation is the pull request's, whichever issue a list is recorded for. The session that
// recorded the author's list of a pull request through one issue cannot record its reviewer's or
// final list through another issue of the tree, a session that recorded a reviewer's list cannot
// record the author's list through another issue, and the ending takes no final list written that
// way. Another session records the reviewer's and final lists through any issue of the tree, and
// the ending takes them.
func TestTheListsOfAPullRequestAreOneHistoryWhicheverIssueOfTheTreeRecordsThem(t *testing.T) {
	rig := newSpecRig(t)
	rig.seed(t)
	const pullRequest = "acme/app#31"
	root := rig.issue(t, "A change whose pull request two issues record", specRepo, "Why the deploy changes.\n\n"+
		conclusionBlock("c-two-issues", ` no_statement="true"`, "Evidence: The deploy job named the wrong host."), "")
	child := childIssue(t, rig, root.Key, "The work that holds the pull request")

	recordOutcomes(t, rig, child.Key, authorSession, pullRequest, headOne, "author")
	// A second author's list of the same pull request, through the root.
	recordOutcomes(t, rig, root.Key, secondAuthorSession, pullRequest, headOne, "author")
	refusedWith(t, "the author's reviewer list through another issue of the tree",
		postOutcomes(t, rig, root.Key, authorSession, pullRequest, headOne, "reviewer"), http.StatusForbidden, "OUTCOME_LIST_BY_AUTHOR")
	refusedWith(t, "the author's final list through another issue of the tree",
		postOutcomes(t, rig, root.Key, authorSession, pullRequest, headOne, "final"), http.StatusForbidden, "OUTCOME_LIST_BY_AUTHOR")
	ending := endingOf(t, rig, root.Key, headOne)
	if ending.State != "waiting" || ending.FinalList != nil || !reflect.DeepEqual(endingCodes(ending), []string{"no_final_list_for_head"}) {
		t.Errorf("the ending after the author tried its own final list through the root reads %s %v with the final list %+v, "+
			"want waiting for no_final_list_for_head and no final list", ending.State, endingCodes(ending), ending.FinalList)
	}

	// The reviewer's own list through the child; then it cannot write the author's list through the root.
	recordOutcomes(t, rig, child.Key, reviewerSession, pullRequest, headOne, "reviewer")
	refusedWith(t, "the reviewer's author list through another issue of the tree",
		postOutcomes(t, rig, root.Key, reviewerSession, pullRequest, headOne, "author"), http.StatusForbidden, "OUTCOME_LIST_BY_REVIEWER")

	// The comparison: the reviewer, which wrote no author's list, records its final list through the
	// root, and the ending takes it.
	final := recordOutcomes(t, rig, root.Key, reviewerSession, pullRequest, headOne, "final")
	ready := endingOf(t, rig, root.Key, headOne)
	if ready.State != "ready" || ready.FinalList == nil || ready.FinalList.ID != final.ID {
		t.Errorf("the ending after the reviewer's final list reads %s %v with the final list %+v, want ready on record %d",
			ready.State, endingCodes(ready), ready.FinalList, final.ID)
	}
}

// TestAFinalListNeedsTheAuthorsListAtItsHead (R27): the final list of a head judges what the
// change makes different at that head, so it waits for the author's list at the same head (409
// OUTCOME_AUTHOR_LIST_FIRST); the reviewer's own list at that head does not, since the reviewer
// writes it before it reads the author's.
func TestAFinalListNeedsTheAuthorsListAtItsHead(t *testing.T) {
	rig := newSpecRig(t)
	issue := rig.issue(t, "Lists of outcomes at two heads", specRepo, "Prose.\n", "")
	const pullRequest = "acme/app#32"
	recordOutcomes(t, rig, issue.Key, authorSession, pullRequest, headOne, "author")
	recordOutcomes(t, rig, issue.Key, reviewerSession, pullRequest, headTwo, "reviewer")
	refusedWith(t, "a final list at a head the author's list is not of",
		postOutcomes(t, rig, issue.Key, reviewerSession, pullRequest, headTwo, "final"), http.StatusConflict, "OUTCOME_AUTHOR_LIST_FIRST")
	recordOutcomes(t, rig, issue.Key, authorSession, pullRequest, headTwo, "author")
	if final := recordOutcomes(t, rig, issue.Key, reviewerSession, pullRequest, headTwo, "final"); final.Head != headTwo {
		t.Errorf("the final list reads the head %s, want %s", final.Head, headTwo)
	}
}

// TestAChangedCheckWaitsUntilTheApprovedSpecHoldsItAsWritten (R25): a person's approval of a change
// request covers each check it writes into the approved spec, and no other. A check of The Spec
// that the Verification document changes waits for the person (changes_checks_unapproved, and the
// ending's changes_checks_without_approval) until the approved spec holds it as the document
// writes it. After the approval, the check reads stale and approved: an acceptance check added
// beside it changes nothing. A second change of the approved check waits again, and an approval of
// the Verification document alone, which writes nothing into the approved spec, does not release
// it.
func TestAChangedCheckWaitsUntilTheApprovedSpecHoldsItAsWritten(t *testing.T) {
	rig := newSpecRig(t)
	rig.seed(t)
	conclusion := conclusionBlock("c-deploy-host", ` no_statement="true"`, "Evidence: The deploy job named the wrong host.")
	issue := rig.issue(t, "A deploy fix that changes the payment check twice", specRepo, "Why the deploy changes.\n\n"+conclusion, paidOnceCheck("1"))
	recordThreeLists(t, rig, issue.Key, "acme/app#51", headOne)
	check := verificationAddress(t, rig, issue) + "#a-second-payment-is-refused"
	waitsOnTheCheck := func(when string) {
		t.Helper()
		read, body := changeRequestOf(t, rig, issue.Key, "")
		if !read.ChangesChecks || !read.ChangesChecksUnapproved || len(read.ChangedChecks) != 1 || read.ChangedChecks[0].Approved {
			t.Errorf("%s: the change request reads %s; want the changed check unapproved", when, body)
		}
		if ending := endingOf(t, rig, issue.Key, headOne); ending.State != "waiting" ||
			!reflect.DeepEqual(endingCodes(ending), []string{"changes_checks_without_approval"}) {
			t.Errorf("%s: the ending reads %s %v, want waiting for changes_checks_without_approval", when, ending.State, endingCodes(ending))
		}
	}
	approvedAsWritten := func(when string, verification int) {
		t.Helper()
		read, body := changeRequestOf(t, rig, issue.Key, "")
		want := []changeRequestNode{{ID: "a-second-payment-is-refused", Kind: "check", What: "stale", Base: 1, Current: 2, Block: check, Approved: true}}
		if read.VerificationVersion != verification || read.ChangesChecksUnapproved || read.ProposesUnapproved || !reflect.DeepEqual(read.ChangedChecks, want) {
			t.Errorf("%s: the change request reads %s; want at Verification version %d the check %+v and nothing unapproved", when, body, verification, want)
		}
		if ending := endingOf(t, rig, issue.Key, headOne); ending.State != "ready" {
			t.Errorf("%s: the ending reads %s %v, want ready", when, ending.State, endingCodes(ending))
		}
	}

	editIssueDocument(t, rig.handler, issue.Key, "verification-md", []map[string]any{{"op": "replace",
		"find": "Claim: A second payment of one order is refused.", "with": "Claim: A second payment of one order is refused at once."}})
	waitsOnTheCheck("an edit of the approved check")

	rig.approve(t, rig.request(t, issue, true))
	approvedAsWritten("the person's approval of the spec with its Verification document", 2)

	editIssueDocument(t, rig.handler, issue.Key, "verification-md", []map[string]any{{"op": "insert", "after": "end", "markdown": acceptanceCheckDocument}})
	approvedAsWritten("an acceptance check added beside the approved check", 3)

	editIssueDocument(t, rig.handler, issue.Key, "verification-md", []map[string]any{{"op": "delete", "block": "a-second-payment-is-refused"},
		{"op": "insert", "after": "end", "markdown": paidOnceCheckClaiming("A second payment of one order is refused at once, and logged.", "2")}})
	waitsOnTheCheck("a second change of the approved check")

	var verificationID string
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select id from artifacts where issue_key = $1 and slug = 'verification-md'`, issue.Key).Scan(&verificationID); err != nil {
		t.Fatalf("read the Verification document of %s: %v", issue.Key, err)
	}
	reviewDocument(t, rig.handler, verificationID, "alice", "approved", "")
	waitsOnTheCheck("an approval of the Verification document alone")
}
