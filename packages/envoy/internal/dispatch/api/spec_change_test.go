package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The approved spec of a repository is written by a person's approval of a change request, and by
// nothing else (R22, R26). An issue of a repository its project names as a spec source holds a
// change request: its own requirement, check and component blocks, each naming the approved version
// its writer read (base). These tests drive the approval routes and read the approved spec through
// GET /api/v1/projects/{project}/spec/{owner}/{repo}/nodes/{id}.

// specRepo and otherSpecRepo are the two repositories CORE names as its spec sources.
const (
	specRepo      = "acme/app"
	otherSpecRepo = "acme/lib"
)

// seedSpec is the change request of the issue that writes the first approved spec of specRepo: two
// statements about billing, and three components, one of them an interface.
const seedSpec = ":::requirement{#an-order-is-paid-once kind=\"behavior\" about=\"billing\"}\n" +
	"An order is paid once.\n:::\n\n" +
	":::requirement{#a-bill-names-its-order kind=\"behavior\" about=\"billing\"}\n" +
	"A bill names its order.\n:::\n\n" +
	":::component{#billing name=\"Billing\"}\nIt takes the payment of each order.\n:::\n\n" +
	":::component{#bank name=\"Bank\"}\nIt holds the money of the payments.\n:::\n\n" +
	":::component{#billing-to-bank name=\"Billing to bank\" kind=\"interface\" joins=\"bank billing\"}\n" +
	"Billing sends each payment to the bank.\n:::\n"

// paidOnceCheck is the check of an-order-is-paid-once at base, or a new one where base is "". A
// document keeps no soft line break, so each field of a check is a paragraph of its own.
func paidOnceCheck(base string) string {
	return paidOnceCheckClaiming("A second payment of one order is refused.", base)
}

// paidOnceCheckClaiming is paidOnceCheck with the claim claim.
func paidOnceCheckClaiming(claim, base string) string {
	return ":::check{#a-second-payment-is-refused statement=\"an-order-is-paid-once\" subject=\"code\" procedure=\"program\" " +
		"runner=\"go\" selector=\"example.com/billing TestASecondPaymentIsRefused\" job=\"go-test\"" + baseAttribute(base) + "}\n" +
		"Claim: " + claim + "\n\n" +
		"Procedure: The test pays one order two times.\n\n" +
		"Failing example: A build that takes the second payment.\n:::\n"
}

// refundCheck is the check fixture 2 adds for its new statement.
const refundCheck = ":::check{#a-refund-is-paid-back statement=\"a-refund-returns-the-payment\" subject=\"code\" procedure=\"program\" " +
	"runner=\"go\" selector=\"example.com/billing TestARefundIsPaidBack\" job=\"go-test\"}\n" +
	"Claim: A refund pays the payment of its order back.\n\n" +
	"Procedure: The test refunds one paid order.\n\n" +
	"Failing example: A build that keeps the payment.\n:::\n"

// fixtureTwoSpec is v-approval's fixture 2: it proposes one statement and changes another, with the
// components it reads as context at the versions the seed approved, and the memo first.
const fixtureTwoSpec = ":::callout{#memo}\nWe refund a paid order. Customers pay too often. A refund ends that.\n:::\n\n" +
	"Prose the person reads around the request.\n\n" +
	":::requirement{#an-order-is-paid-once kind=\"behavior\" about=\"billing\" base=\"1\"}\n" +
	"An order is paid once, and a second payment is refused.\n:::\n\n" +
	":::requirement{#a-refund-returns-the-payment kind=\"behavior\" about=\"billing\"}\n" +
	"A refund returns the payment of an order.\n:::\n\n" +
	":::component{#billing name=\"Billing\" base=\"1\"}\nIt takes the payment of each order.\n:::\n\n" +
	":::component{#bank name=\"Bank\" base=\"1\"}\nIt holds the money of the payments.\n:::\n\n" +
	":::component{#billing-to-bank name=\"Billing to bank\" kind=\"interface\" joins=\"bank billing\" base=\"1\"}\n" +
	"Billing sends each payment to the bank.\n:::\n"

// baseAttribute is ` base="<base>"`, or "" for a node the writer adds.
func baseAttribute(base string) string {
	if base == "" {
		return ""
	}
	return ` base="` + base + `"`
}

// specNodeRead is GET /api/v1/projects/{project}/spec/{owner}/{repo}/nodes/{id}'s answer.
type specNodeRead struct {
	Project  string `json:"project"`
	Repo     string `json:"repo"`
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Removed  bool   `json:"removed"`
	Versions []struct {
		Version    int            `json:"version"`
		At         time.Time      `json:"at"`
		Block      string         `json:"block"`
		Provenance specProvenance `json:"provenance"`
	} `json:"versions"`
}

// specProvenance is the provenance an approved version names. Notes names the ask and review of the
// person's own approval of the notes where an import read them from it, and BroughtBackWith the
// statement whose restored text brought a check back.
type specProvenance struct {
	Act             string       `json:"act"`
	Issue           string       `json:"issue"`
	Ask             *string      `json:"ask"`
	Review          *string      `json:"review"`
	Person          *model.Actor `json:"person"`
	BroughtBackWith string       `json:"brought_back_with"`
	Spec            *struct {
		ArtifactID string `json:"artifact_id"`
		Version    int    `json:"version"`
		Digest     string `json:"digest"`
	} `json:"spec"`
	Verification *struct {
		ArtifactID string `json:"artifact_id"`
		Version    int    `json:"version"`
		Digest     string `json:"digest"`
	} `json:"verification"`
	Notes *struct {
		ArtifactID string  `json:"artifact_id"`
		Version    int     `json:"version"`
		Digest     string  `json:"digest"`
		Ask        *string `json:"ask"`
		Review     *string `json:"review"`
	} `json:"notes"`
}

// specRig is a server with the project CORE, which names specRepo as a spec source.
type specRig struct {
	handler  http.Handler
	database *store.Store
}

func newSpecRig(t *testing.T) *specRig {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	rig := &specRig{handler: handler, database: database}
	rig.addSpecSource(t, specRepo)
	return rig
}

// addSpecSource names repo a spec source of CORE.
func (r *specRig) addSpecSource(t *testing.T, repo string) {
	t.Helper()
	response := dispatchRequest(t, r.handler, http.MethodPut, "/api/v1/projects/CORE/spec-sources/"+repo, map[string]any{"branch": "main"}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("name %s a spec source of CORE: status=%d body=%s", repo, response.Code, response.Body.String())
	}
}

// issue creates, as alice, an issue of CORE in repo with spec and, unless checks is "", a
// Verification document holding checks.
func (r *specRig) issue(t *testing.T, title, repo, spec, checks string) model.Issue {
	t.Helper()
	response := dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": title, "spec": spec, "repo": repo, "force": true,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue %q: status=%d body=%s", title, response.Code, response.Body.String())
	}
	issue := decodeBody[model.Issue](t, response)
	if checks != "" {
		uploadDocument(t, r.handler, "/api/v1/issues/"+issue.Key, "verification.md", checks)
	}
	return issue
}

// request opens the agent's approval request on the issue's spec, with its Verification document
// beside it where verification, and answers the ask's id.
func (r *specRig) request(t *testing.T, issue model.Issue, verification bool) string {
	t.Helper()
	response := sessionRequest(t, r.handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests",
		map[string]any{"actor": sessionActor(), "include_verification": verification})
	if response.Code != http.StatusCreated {
		t.Fatalf("request approval of %s: status=%d body=%s", issue.Key, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, response).Ask.ID
}

// answer answers the approval request as alice, with Approve or Request changes and the words, at
// the revision of the ask she reads first, as a person answers the request she sees.
func (r *specRig) answer(t *testing.T, ask, selected, words string) *httptest.ResponseRecorder {
	t.Helper()
	read := dispatchRequest(t, r.handler, http.MethodGet, "/api/v1/asks/"+ask, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the approval request %s: status=%d body=%s", ask, read.Code, read.Body.String())
	}
	revision := decodeBody[struct {
		Ask struct {
			EditedAt json.RawMessage `json:"edited_at"`
		} `json:"ask"`
	}](t, read).Ask.EditedAt
	body := map[string]any{"selected": []string{selected}, "expected_edited_at": revision}
	if words != "" {
		body["text"] = words
	}
	return dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/asks/"+ask+"/answer", body, "alice")
}

// retype is the edit that sets the attributes of the typed block id of type typ, in place.
func retype(id, typ string, attributes map[string]any) []map[string]any {
	return []map[string]any{{"op": "retype", "block": id, "type": typ, "attributes": attributes}}
}

// approve answers the approval request with Approve and fails unless the approval is recorded.
func (r *specRig) approve(t *testing.T, ask string) {
	t.Helper()
	if response := r.answer(t, ask, docs.ApprovalOptionApprove, ""); response.Code != http.StatusOK {
		t.Fatalf("approve the request %s: status=%d body=%s", ask, response.Code, response.Body.String())
	}
}

// refused answers the approval request with Approve and fails unless the answer is 409 code.
func (r *specRig) refused(t *testing.T, ask, code string) map[string]any {
	t.Helper()
	response := r.answer(t, ask, docs.ApprovalOptionApprove, "")
	body := map[string]any{}
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if response.Code != http.StatusConflict || body["code"] != code {
		t.Fatalf("approve the request %s: status=%d body=%s, want 409 %s", ask, response.Code, response.Body.String(), code)
	}
	return body
}

// seed writes the first approved spec of specRepo: seedSpec with paidOnceCheck, approved by alice.
func (r *specRig) seed(t *testing.T) model.Issue {
	t.Helper()
	issue := r.issue(t, "The first approved spec", specRepo, seedSpec, paidOnceCheck(""))
	r.approve(t, r.request(t, issue, true))
	return issue
}

// node reads the approved node id of repo, and whether the approved spec holds it.
func (r *specRig) node(t *testing.T, repo, id string) (specNodeRead, bool) {
	t.Helper()
	response := dispatchRequest(t, r.handler, http.MethodGet, "/api/v1/projects/CORE/spec/"+repo+"/nodes/"+id, nil, "alice")
	switch response.Code {
	case http.StatusOK:
		return decodeBody[specNodeRead](t, response), true
	case http.StatusNotFound:
		return specNodeRead{}, false
	}
	t.Fatalf("read the approved node %s of %s: status=%d body=%s", id, repo, response.Code, response.Body.String())
	return specNodeRead{}, false
}

// requireVersions fails unless the approved spec of repo holds id with exactly the versions whose
// blocks hold each of texts, oldest first, and answers the node.
func (r *specRig) requireVersions(t *testing.T, repo, id string, texts ...string) specNodeRead {
	t.Helper()
	node, held := r.node(t, repo, id)
	if !held {
		t.Fatalf("the approved spec of %s holds no %s, want %d versions", repo, id, len(texts))
	}
	if len(node.Versions) != len(texts) {
		t.Fatalf("the approved spec of %s holds %d versions of %s, want %d: %+v", repo, len(node.Versions), id, len(texts), node.Versions)
	}
	for index, text := range texts {
		version := node.Versions[index]
		if version.Version != index+1 || !strings.Contains(version.Block, text) {
			t.Errorf("version %d of %s of %s is %d %q, want version %d holding %q", index+1, id, repo, version.Version, version.Block, index+1, text)
		}
	}
	return node
}

// requireAbsent fails unless the approved spec of repo holds no node id.
func (r *specRig) requireAbsent(t *testing.T, repo, id string) {
	t.Helper()
	if node, held := r.node(t, repo, id); held {
		t.Fatalf("the approved spec of %s holds %s: %+v, want none", repo, id, node)
	}
}

// approvedTuples counts every fact the store holds of an approved node, so a test can tell an act
// wrote none.
func (r *specRig) approvedTuples(t *testing.T) int {
	t.Helper()
	var count int
	if err := r.database.Pool.QueryRow(context.Background(), `select count(*) from graph_tuples where from_kind = 'spec_node'`).Scan(&count); err != nil {
		t.Fatalf("count the facts of the approved spec: %v", err)
	}
	return count
}

// TestApprovalWritesTheChangeRequest: the approval of fixture 2 writes the whole change into the
// approved spec in one act - the statement it changes, the statement and the check it adds - and
// nothing that the request holds as context, each with the approval as its provenance (v-approval:
// "After an approval, a build whose approved spec does not hold the change, or holds a part of it,
// fails").
func TestApprovalWritesTheChangeRequest(t *testing.T) {
	rig := newSpecRig(t)
	seed := rig.seed(t)
	rig.requireVersions(t, specRepo, "an-order-is-paid-once", "An order is paid once.")
	rig.requireVersions(t, specRepo, "a-second-payment-is-refused", "Claim: A second payment of one order is refused.")
	rig.requireVersions(t, specRepo, "billing-to-bank", `kind="interface"`)

	fixture := rig.issue(t, "Fixture 2: refunds", specRepo, fixtureTwoSpec, paidOnceCheck("1")+"\n"+refundCheck)
	ask := rig.request(t, fixture, true)
	rig.approve(t, ask)

	changed := rig.requireVersions(t, specRepo, "an-order-is-paid-once", "An order is paid once.", "An order is paid once, and a second payment is refused.")
	added := rig.requireVersions(t, specRepo, "a-refund-returns-the-payment", "A refund returns the payment of an order.")
	check := rig.requireVersions(t, specRepo, "a-refund-is-paid-back", "Claim: A refund pays the payment of its order back.")
	for _, id := range []string{"a-second-payment-is-refused", "a-bill-names-its-order", "billing", "bank", "billing-to-bank"} {
		node, _ := rig.node(t, specRepo, id)
		if len(node.Versions) != 1 || node.Versions[0].Provenance.Issue != seed.Key {
			t.Errorf("the approved spec holds %s at %+v, want the one version the seed approved: the request holds it as context", id, node.Versions)
		}
	}
	for _, node := range []specNodeRead{changed, added, check} {
		written := node.Versions[len(node.Versions)-1]
		provenance := written.Provenance
		switch {
		case provenance.Act != "approval" || provenance.Issue != fixture.Key:
			t.Errorf("%s version %d names the act %q of %q, want the approval of %s", node.ID, written.Version, provenance.Act, provenance.Issue, fixture.Key)
		case provenance.Ask == nil || *provenance.Ask != ask:
			t.Errorf("%s version %d names the request %v, want %s", node.ID, written.Version, provenance.Ask, ask)
		case provenance.Person == nil || provenance.Person.ID != "alice":
			t.Errorf("%s version %d names the person %+v, want alice", node.ID, written.Version, provenance.Person)
		case provenance.Review == nil:
			t.Errorf("%s version %d names no review row", node.ID, written.Version)
		case provenance.Spec == nil || provenance.Spec.ArtifactID != fixture.PrimaryArtifactID || provenance.Spec.Digest == "":
			t.Errorf("%s version %d names the spec %+v, want %s with its digest", node.ID, written.Version, provenance.Spec, fixture.PrimaryArtifactID)
		case provenance.Verification == nil || provenance.Verification.Digest == "":
			t.Errorf("%s version %d names no Verification document", node.ID, written.Version)
		}
		if !written.At.Equal(changed.Versions[1].At) {
			t.Errorf("%s version %d is dated %s and %s version 2 %s: one act writes one time", node.ID, written.Version, written.At, changed.ID, changed.Versions[1].At)
		}
	}
	if changed.Repo != specRepo || changed.Project != "CORE" || changed.Kind != "statement" || check.Kind != "check" {
		t.Errorf("the node reads %s/%s %s and %s, want CORE/%s statement and check", changed.Project, changed.Repo, changed.Kind, check.Kind, specRepo)
	}
}

// TestRestoringAnEarlierTextBringsBackItsCheckBesideTheVerificationDocument: an approval that
// restores the first text of a statement brings back the check that text held (R25), whatever the
// issue's Verification document shows of the check: no such document, the document named by the
// approval request, or the document's latest version, which the approval reads where its request
// leaves the document out. Each document holds the check unchanged at its current version, which
// the request holds as context and does not write.
func TestRestoringAnEarlierTextBringsBackItsCheckBesideTheVerificationDocument(t *testing.T) {
	const (
		firstText  = "An order is paid once."
		secondText = "An order is paid once, and a second payment is refused."
		firstClaim = "Claim: A second payment of one order is refused."
		loggedText = "A second payment of one order is refused and logged."
	)
	paidOnce := func(text, base string) string {
		return ":::requirement{#an-order-is-paid-once kind=\"behavior\" about=\"billing\"" + baseAttribute(base) + "}\n" + text + "\n:::\n"
	}
	for _, arm := range []struct {
		name  string
		named bool
		// checks is the issue's Verification document, or "" for none.
		checks string
	}{
		{name: "no Verification document"},
		{name: "the Verification document named, holding the current check", named: true, checks: paidOnceCheckClaiming(loggedText, "2")},
		{name: "the Verification document left out, its latest version holding the current check", checks: paidOnceCheckClaiming(loggedText, "2")},
	} {
		t.Run(arm.name, func(t *testing.T) {
			rig := newSpecRig(t)
			rig.seed(t)
			second := rig.issue(t, "Refuse and log a second payment", specRepo, paidOnce(secondText, "1"), paidOnceCheckClaiming(loggedText, "1"))
			rig.approve(t, rig.request(t, second, true))
			rig.requireVersions(t, specRepo, "a-second-payment-is-refused", firstClaim, "Claim: "+loggedText)

			restore := rig.issue(t, "The first text again", specRepo, paidOnce(firstText, "2"), arm.checks)
			ask := rig.request(t, restore, arm.named)
			rig.approve(t, ask)
			rig.requireVersions(t, specRepo, "an-order-is-paid-once", firstText, secondText, firstText)
			check := rig.requireVersions(t, specRepo, "a-second-payment-is-refused", firstClaim, "Claim: "+loggedText, firstClaim)
			provenance := check.Versions[2].Provenance
			if provenance.Act != "brought_back" || provenance.BroughtBackWith != "an-order-is-paid-once" ||
				provenance.Issue != restore.Key || provenance.Ask == nil || *provenance.Ask != ask {
				t.Errorf("version 3 of a-second-payment-is-refused names %+v, want it brought back with an-order-is-paid-once by the approval of %s through %s",
					provenance, restore.Key, ask)
			}
		})
	}
}

// TestApprovalRefusals: Dispatch refuses the five approvals D4 names, each writing nothing; tells
// the owner of a refused stale request; and approves the request written again, a parent proposed
// with its child, and a request whose issue changed only in its prose.
func TestApprovalRefusals(t *testing.T) {
	rig := newSpecRig(t)
	rig.seed(t)

	t.Run("a statement under a parent neither the approved spec nor the request holds", func(t *testing.T) {
		before := rig.approvedTuples(t)
		orphan := rig.issue(t, "An orphan", specRepo,
			":::requirement{#a-receipt-follows-a-payment kind=\"behavior\" under=\"a-receipt-is-sent\"}\nA receipt follows each payment.\n:::\n", "")
		rig.refused(t, rig.request(t, orphan, false), "SPEC_PARENT_MISSING")
		rig.requireAbsent(t, specRepo, "a-receipt-follows-a-payment")
		if after := rig.approvedTuples(t); after != before {
			t.Errorf("the refused approval wrote %d facts of the approved spec", after-before)
		}
		both := rig.issue(t, "A parent and its child", specRepo,
			":::requirement{#a-receipt-is-sent kind=\"behavior\"}\nA receipt is sent for each order.\n:::\n\n"+
				":::requirement{#a-receipt-follows-a-payment kind=\"behavior\" under=\"a-receipt-is-sent\"}\nA receipt follows each payment.\n:::\n", "")
		rig.approve(t, rig.request(t, both, false))
		rig.requireVersions(t, specRepo, "a-receipt-is-sent", "A receipt is sent for each order.")
		rig.requireVersions(t, specRepo, "a-receipt-follows-a-payment", "A receipt follows each payment.")
	})

	t.Run("a loop across parent and supports", func(t *testing.T) {
		loop := rig.issue(t, "A loop", specRepo,
			":::requirement{#a-loop-starts-here kind=\"behavior\" under=\"a-loop-ends-here\"}\nThe first part holds.\n:::\n\n"+
				":::requirement{#a-loop-ends-here kind=\"behavior\" supports=\"a-loop-starts-here\"}\nThe second part holds.\n:::\n", "")
		rig.refused(t, rig.request(t, loop, false), "SPEC_LOOP")
		rig.requireAbsent(t, specRepo, "a-loop-starts-here")
		rig.requireAbsent(t, specRepo, "a-loop-ends-here")
	})

	t.Run("an approval that names no open approval request", func(t *testing.T) {
		unrequested := rig.issue(t, "No request", specRepo,
			":::requirement{#a-bill-is-sent-once kind=\"behavior\"}\nA bill is sent once.\n:::\n", "")
		response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+unrequested.PrimaryArtifactID+"/reviews",
			map[string]any{"state": "approved"}, "alice")
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "APPROVAL_REQUEST_REQUIRED") {
			t.Fatalf("approve with no request open: status=%d body=%s, want 409 APPROVAL_REQUEST_REQUIRED", response.Code, response.Body.String())
		}
		rig.requireAbsent(t, specRepo, "a-bill-is-sent-once")
	})

	t.Run("an approval of a request whose text changed after it was opened", func(t *testing.T) {
		moved := rig.issue(t, "A moved request", specRepo,
			":::requirement{#a-late-bill-is-flagged kind=\"behavior\"}\nA late bill is flagged.\n:::\n", "")
		ask := rig.request(t, moved, false)
		editSpec(t, rig.handler, moved.Key, []map[string]any{{"op": "replace", "find": "A late bill is flagged.", "with": "A late bill is flagged and sent again."}})
		rig.refused(t, ask, "APPROVAL_REQUEST_REQUIRED")
		rig.requireAbsent(t, specRepo, "a-late-bill-is-flagged")
	})

	t.Run("a request that proposes checks without its Verification document", func(t *testing.T) {
		unchecked := rig.issue(t, "Checks left out", specRepo,
			":::requirement{#a-paid-bill-is-closed kind=\"behavior\"}\nA paid bill is closed.\n:::\n",
			":::check{#a-paid-bill-reads-closed statement=\"a-paid-bill-is-closed\" subject=\"code\" procedure=\"program\" runner=\"go\" "+
				"selector=\"example.com/billing TestAPaidBillReadsClosed\" job=\"go-test\"}\n"+
				"Claim: A paid bill reads closed.\n\nProcedure: The test pays a bill.\n\nFailing example: A build that leaves it open.\n:::\n")
		rig.refused(t, rig.request(t, unchecked, false), "SPEC_CHECKS_NOT_INCLUDED")
		rig.requireAbsent(t, specRepo, "a-paid-bill-is-closed")
		rig.requireAbsent(t, specRepo, "a-paid-bill-reads-closed")
	})

	t.Run("two requests on one statement: the second is stale, its owner hears it, and its rewrite is approved", func(t *testing.T) {
		first := rig.issue(t, "First change of the bill", specRepo,
			":::requirement{#a-bill-names-its-order kind=\"behavior\" about=\"billing\" base=\"1\"}\nA bill names its order and its date.\n:::\n", "")
		second := rig.issue(t, "Second change of the bill", specRepo,
			":::requirement{#a-bill-names-its-order kind=\"behavior\" about=\"billing\" base=\"1\"}\nA bill names its order and its total.\n:::\n", "")
		firstAsk, secondAsk := rig.request(t, first, false), rig.request(t, second, false)
		rig.approve(t, firstAsk)
		body := rig.refused(t, secondAsk, "SPEC_REQUEST_STALE")
		if !strings.Contains(mustJSON(t, body), "a-bill-names-its-order") {
			t.Errorf("the refusal %v does not name the stale statement", body)
		}
		rig.requireVersions(t, specRepo, "a-bill-names-its-order", "A bill names its order.", "A bill names its order and its date.")
		stale := staleEvents(t, rig.database, second.Key)
		if len(stale) != 1 {
			t.Fatalf("%s holds %d spec.request_stale events, want one for its owner", second.Key, len(stale))
		}
		if got := stale[0]; !strings.Contains(got, `"a-bill-names-its-order"`) || !strings.Contains(got, `"base": 1`) || !strings.Contains(got, `"current": 2`) {
			t.Errorf("the spec.request_stale payload is %s, want the statement with base 1 and current 2", got)
		}
		if others := staleEvents(t, rig.database, first.Key); len(others) != 0 {
			t.Errorf("%s holds %d spec.request_stale events, want none", first.Key, len(others))
		}
		editSpec(t, rig.handler, second.Key, retype("a-bill-names-its-order", "requirement", map[string]any{"kind": "behavior", "about": "billing", "base": "2"}))
		editSpec(t, rig.handler, second.Key, []map[string]any{{"op": "replace", "find": "A bill names its order and its total.", "with": "A bill names its order, its date and its total."}})
		rig.approve(t, rig.request(t, second, false))
		rig.requireVersions(t, specRepo, "a-bill-names-its-order", "A bill names its order.", "A bill names its order and its date.", "A bill names its order, its date and its total.")
	})

	t.Run("an edit of the issue's prose alone leaves its request current", func(t *testing.T) {
		cosmetic := rig.issue(t, "A cosmetic edit", specRepo,
			"Why the bill changes.\n\n:::requirement{#a-bill-names-its-order kind=\"behavior\" about=\"billing\" base=\"3\"}\nA bill names its order, its date, its total and its tax.\n:::\n", "")
		rig.request(t, cosmetic, false)
		editSpec(t, rig.handler, cosmetic.Key, []map[string]any{{"op": "replace", "find": "Why the bill changes.", "with": "Why the bill changes now."}})
		rig.approve(t, rig.request(t, cosmetic, false))
		rig.requireVersions(t, specRepo, "a-bill-names-its-order", "A bill names its order.", "A bill names its order and its date.",
			"A bill names its order, its date and its total.", "A bill names its order, its date, its total and its tax.")
	})
}

// staleEvents is each spec.request_stale payload of the issue, oldest first, as indented JSON.
func staleEvents(t *testing.T, database *store.Store, issue string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select jsonb_pretty(payload) from events where issue_key = $1 and type = 'spec.request_stale' order by id`, issue)
	if err != nil {
		t.Fatalf("read the spec.request_stale events of %s: %v", issue, err)
	}
	defer rows.Close()
	var payloads []string
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("read a spec.request_stale event of %s: %v", issue, err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %v: %v", value, err)
	}
	return string(data)
}

// TestApprovalDigestCoversTheChangeRequest: after the approval of fixture 2, ten edits run one at
// a time; the spec digest moves on each but the ninth, a change of prose outside the memo
// (v-approval's ten edits; R22: the spec query also returns the component and interface nodes
// that the issue proposes).
func TestApprovalDigestCoversTheChangeRequest(t *testing.T) {
	rig := newSpecRig(t)
	rig.seed(t)
	fixture := rig.issue(t, "Fixture 2: refunds", specRepo, fixtureTwoSpec+
		"\n:::requirement{#a-refund-is-logged kind=\"behavior\" about=\"billing\"}\nA refund is logged.\n:::\n", paidOnceCheck("1")+"\n"+refundCheck)
	rig.approve(t, rig.request(t, fixture, true))
	digest := func() string {
		t.Helper()
		// The digest of the spec query's record (R22, the record an approval covers), read at the
		// head the edit left.
		record, err := docs.ReadQueryRecord(context.Background(), rig.database.Pool, fixture.Key, graphstore.QuerySpec, refs.TreeCut{})
		if err != nil {
			t.Fatalf("read the spec digest of %s: %v", fixture.Key, err)
		}
		return record.Digest
	}
	replace := func(find, with string) []map[string]any {
		return []map[string]any{{"op": "replace", "find": find, "with": with}}
	}
	for index, edit := range []struct {
		name  string
		ops   []map[string]any
		moves bool
	}{
		{"1: a word of the text of a statement", replace("A refund returns the payment of an order.", "A refund returns the whole payment of an order."), true},
		{"2: the parent of a statement", retype("a-refund-is-logged", "requirement",
			map[string]any{"kind": "behavior", "about": "billing", "under": "a-refund-returns-the-payment"}), true},
		{"3: the order of two statements under one parent", []map[string]any{{"op": "move", "block": "a-refund-returns-the-payment", "before": "block:an-order-is-paid-once"}}, true},
		{"4: the about relation of a statement", retype("a-refund-returns-the-payment", "requirement",
			map[string]any{"kind": "behavior", "about": "bank billing"}), true},
		{"5: the serves relation of a statement", retype("a-refund-returns-the-payment", "requirement",
			map[string]any{"kind": "behavior", "about": "bank billing", "supports": "an-order-is-paid-once"}), true},
		{"6: the purpose of a component", replace("It takes the payment of each order.", "It takes and refunds the payment of each order."), true},
		{"7: the joins relation of an interface", retype("billing-to-bank", "component",
			map[string]any{"name": "Billing to bank", "kind": "interface", "joins": "bank", "base": "1"}), true},
		{"8: the removal of a statement that the approval did not name", []map[string]any{{"op": "insert", "after": "end",
			"markdown": ":::requirement{#a-bill-names-its-order kind=\"behavior\" about=\"billing\" base=\"1\" removes=\"true\"}\nA bill names its order.\n:::\n"}}, true},
		{"9: only prose of the issue outside its memo", replace("Prose the person reads around the request.", "Prose the person reads beside the request."), false},
		{"10: one sentence of the memo", replace("A refund ends that.", "A refund ends that, once."), true},
	} {
		before := digest()
		editSpec(t, rig.handler, fixture.Key, edit.ops)
		after := digest()
		if moved := after != before; moved != edit.moves {
			t.Errorf("edit %d (%s) moves the spec digest from %s to %s: moved %t, want %t", index+1, edit.name, before, after, moved, edit.moves)
		}
	}
}

// TestTwoRepositoriesKeepTheirOwnIds: an id is unique in its repository (R1), so two spec sources
// of one project each hold their own node of one id, and an approval in one leaves the other as
// it was and stale in neither.
func TestTwoRepositoriesKeepTheirOwnIds(t *testing.T) {
	rig := newSpecRig(t)
	rig.addSpecSource(t, otherSpecRepo)
	statement := func(text, base string) string {
		return ":::requirement{#a-call-is-answered-once kind=\"behavior\"" + baseAttribute(base) + "}\n" + text + "\n:::\n"
	}
	app := rig.issue(t, "The app's statement", specRepo, statement("The app answers each call once.", ""), "")
	lib := rig.issue(t, "The library's statement", otherSpecRepo, statement("The library answers each call once.", ""), "")
	rig.approve(t, rig.request(t, app, false))
	rig.approve(t, rig.request(t, lib, false))
	again := rig.issue(t, "The app's statement again", specRepo, statement("The app answers each call once, in order.", "1"), "")
	rig.approve(t, rig.request(t, again, false))

	appNode := rig.requireVersions(t, specRepo, "a-call-is-answered-once", "The app answers each call once.", "The app answers each call once, in order.")
	libNode := rig.requireVersions(t, otherSpecRepo, "a-call-is-answered-once", "The library answers each call once.")
	if appNode.Repo != specRepo || libNode.Repo != otherSpecRepo {
		t.Errorf("the two nodes read repositories %s and %s, want %s and %s", appNode.Repo, libNode.Repo, specRepo, otherSpecRepo)
	}
	response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/projects/CORE/spec/acme/none/nodes/a-call-is-answered-once", nil, "alice")
	if response.Code != http.StatusNotFound {
		t.Errorf("a repository CORE names no spec source of answers %d %s, want 404", response.Code, response.Body.String())
	}
}

// TestAFalseCheckChangesNoStatement: a false result of an approved check leaves every approved
// version as it was (R26: "A false check changes no statement").
func TestAFalseCheckChangesNoStatement(t *testing.T) {
	rig := newSpecRig(t)
	seed := rig.seed(t)
	ids := []string{"an-order-is-paid-once", "a-bill-names-its-order", "a-second-payment-is-refused", "billing", "bank", "billing-to-bank"}
	read := func() map[string]specNodeRead {
		nodes := map[string]specNodeRead{}
		for _, id := range ids {
			node, held := rig.node(t, specRepo, id)
			if !held {
				t.Fatalf("the approved spec holds no %s", id)
			}
			nodes[id] = node
		}
		return nodes
	}
	before, facts := read(), rig.approvedTuples(t)
	recorded := recordResult(t, rig.handler, seed.Key, resultBody("a-second-payment-is-refused", "fail", headCommit, map[string]any{"failed": 1}))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a false result of the check: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	if after := read(); !reflect.DeepEqual(after, before) {
		t.Errorf("a false result moved the approved spec:\nbefore %+v\nafter  %+v", before, after)
	}
	if after := rig.approvedTuples(t); after != facts {
		t.Errorf("a false result wrote %d facts of the approved spec", after-facts)
	}
}

// TestRequestForChangesWritesNothing: a person's request for changes is an act that holds her
// words, and it writes nothing into the approved spec (v-approval: "The person requests changes in
// words. A build that does not store an act that holds those words ... fails").
func TestRequestForChangesWritesNothing(t *testing.T) {
	rig := newSpecRig(t)
	rig.seed(t)
	before := rig.approvedTuples(t)
	change := rig.issue(t, "A change the person sends back", specRepo,
		":::requirement{#an-order-is-paid-once kind=\"behavior\" about=\"billing\" base=\"1\"}\nAn order is paid at most once.\n:::\n", "")
	const words = "Say which payment is refused."
	if response := rig.answer(t, rig.request(t, change, false), docs.ApprovalOptionRequestChanges, words); response.Code != http.StatusOK {
		t.Fatalf("request changes: status=%d body=%s", response.Code, response.Body.String())
	}
	var state, reason string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select state, coalesce(reason, '') from artifact_reviews where artifact_id = $1 order by created_at desc limit 1
	`, change.PrimaryArtifactID).Scan(&state, &reason); err != nil {
		t.Fatalf("read the review: %v", err)
	}
	if state != "changes_requested" || reason != words {
		t.Errorf("the review is %s %q, want changes_requested %q", state, reason, words)
	}
	rig.requireVersions(t, specRepo, "an-order-is-paid-once", "An order is paid once.")
	if after := rig.approvedTuples(t); after != before {
		t.Errorf("a request for changes wrote %d facts of the approved spec", after-before)
	}
}
