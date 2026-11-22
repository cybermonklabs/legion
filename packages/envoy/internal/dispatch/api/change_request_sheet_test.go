package api

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// What an approval covers, as the page restates it before the person approves (R22: "Before the
// person approves on a phone, Dispatch shows a sheet that restates what the approval covers"). The
// change request read answers, beside each node it proposes or each check it changes, the block's
// text as the version holds it (text) and the approved spec's current version of the node
// (approved_text), and the digest a person's approval of the spec version and of the Verification
// version pins (digest, verification_digest). The digest is read from the function the review
// trigger pins (artifact_version_text_sha256), so these tests read it back from the reviews the
// approval then writes: the sheet can never show a digest the approval does not pin.

// sheetRead is the part of GET /api/v1/issues/{key}/change-request's answer the sheet reads.
type sheetRead struct {
	Version             int         `json:"version"`
	VerificationVersion int         `json:"verification_version"`
	SpecSource          bool        `json:"spec_source"`
	Digest              string      `json:"digest"`
	VerificationDigest  string      `json:"verification_digest"`
	Proposals           []sheetNode `json:"proposals"`
	ChangedChecks       []sheetNode `json:"changed_checks"`
}

// sheetNode is one node the sheet lists.
type sheetNode struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	What         string `json:"what"`
	Text         string `json:"text"`
	ApprovedText string `json:"approved_text"`
}

// sheetDigest is a digest as artifact_version_text_sha256 writes one: 64 lower-case hex digits.
var sheetDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// sheetSpec changes an-order-is-paid-once, adds a-refund-returns-the-payment and removes
// a-bill-names-its-order, each against version 1 of the seed. The removal's own words are not the
// approved text, so a read that answered the issue's block for the text the approval removes
// would show another sentence.
const sheetSpec = "Why billing changes.\n\n" +
	":::requirement{#an-order-is-paid-once kind=\"behavior\" about=\"billing\" base=\"1\"}\n" +
	"An order is paid once, and a second payment is refused.\n:::\n\n" +
	":::requirement{#a-refund-returns-the-payment kind=\"behavior\" about=\"billing\"}\n" +
	"A refund returns the payment of an order.\n:::\n\n" +
	":::requirement{#a-bill-names-its-order kind=\"behavior\" about=\"billing\" base=\"1\" removes=\"true\"}\n" +
	"This statement goes.\n:::\n"

// storedVersion is the markdown Dispatch stores for version of the document artifactID.
func storedVersion(t *testing.T, rig *specRig, artifactID string, version int) string {
	t.Helper()
	var markdown string
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select markdown from artifact_versions where artifact_id = $1::uuid and number = $2`, artifactID, version).Scan(&markdown); err != nil {
		t.Fatalf("read version %d of %s: %v", version, artifactID, err)
	}
	return markdown
}

// verificationArtifact is the id of the issue's Verification document.
func verificationArtifact(t *testing.T, rig *specRig, key string) string {
	t.Helper()
	var id string
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select id::text from artifacts where issue_key = $1 and slug = 'verification-md'`, key).Scan(&id); err != nil {
		t.Fatalf("read the Verification document of %s: %v", key, err)
	}
	return id
}

// approvalDigest is the digest the person's approval answering ask pinned on artifactID, read from
// that document's reviews.
func approvalDigest(t *testing.T, rig *specRig, artifactID, ask string) string {
	t.Helper()
	response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/reviews", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the reviews of %s: status=%d body=%s", artifactID, response.Code, response.Body.String())
	}
	for _, review := range decodeBody[[]struct {
		State         string  `json:"state"`
		AskID         *string `json:"ask_id"`
		ContentSHA256 *string `json:"content_sha256"`
	}](t, response) {
		if review.State == "approved" && review.AskID != nil && *review.AskID == ask && review.ContentSHA256 != nil {
			return *review.ContentSHA256
		}
	}
	t.Fatalf("%s holds no approval answering %s with a digest: %s", artifactID, ask, response.Body.String())
	return ""
}

// requireBlockOf fails unless text is one whole block of type typ and id, a slice of markdown,
// holding words.
func requireBlockOf(t *testing.T, name, text, markdown, typ, id, words string) {
	t.Helper()
	opening := ":::" + typ + "{#" + id
	if !strings.HasPrefix(text, opening) || !strings.HasSuffix(strings.TrimRight(text, "\n"), ":::") ||
		strings.Count(text, ":::"+typ+"{") != 1 || !strings.Contains(text, words) || !strings.Contains(markdown, text) {
		t.Errorf("%s: text %q is not the one block %s of the version holding %q (the version: %q)", name, text, opening, words, markdown)
	}
}

// TestTheChangeRequestRestatesEachNodeAndTheDigestTheApprovalPins: an issue that changes, adds and
// removes a statement and changes a check reads each node with the block its version holds and the
// approved text it replaces or removes, and the digests of the spec and Verification versions,
// which equal the digests the person's approval then pins on each review. An issue of no spec
// source reads the digest its approval pins too, and no Verification digest while it has no
// Verification document.
func TestTheChangeRequestRestatesEachNodeAndTheDigestTheApprovalPins(t *testing.T) {
	rig := newSpecRig(t)
	rig.seed(t)
	changedCheck := strings.Replace(paidOnceCheck("1"), "Claim: A second payment of one order is refused.",
		"Claim: A second payment of one order is refused at once.", 1)
	issue := rig.issue(t, "Refunds, and bills that go", specRepo, sheetSpec, changedCheck)
	ask := rig.request(t, issue, true)

	read := decodeBody[sheetRead](t, readChangeRequest(t, rig, issue.Key, "version=1"))
	if !read.SpecSource || read.Version != 1 || read.VerificationVersion != 1 {
		t.Fatalf("the request reads spec_source %t version %d verification %d, want true 1 1", read.SpecSource, read.Version, read.VerificationVersion)
	}
	spec := storedVersion(t, rig, issue.PrimaryArtifactID, 1)
	verificationID := verificationArtifact(t, rig, issue.Key)
	checks := storedVersion(t, rig, verificationID, 1)
	approvedText := func(id string) string {
		node, held := rig.node(t, specRepo, id)
		if !held || len(node.Versions) == 0 {
			t.Fatalf("the approved spec holds no %s", id)
		}
		return node.Versions[len(node.Versions)-1].Block
	}

	nodes := map[string]sheetNode{}
	order := []string{}
	for _, node := range read.Proposals {
		nodes[node.ID] = node
		order = append(order, node.What+" "+node.ID)
	}
	if want := []string{"changed an-order-is-paid-once", "added a-refund-returns-the-payment", "removed a-bill-names-its-order"}; strings.Join(order, ", ") != strings.Join(want, ", ") {
		t.Fatalf("the request proposes %v, want %v", order, want)
	}
	changed := nodes["an-order-is-paid-once"]
	requireBlockOf(t, "the changed statement", changed.Text, spec, "requirement", "an-order-is-paid-once", "An order is paid once, and a second payment is refused.")
	if changed.ApprovedText != approvedText("an-order-is-paid-once") || !strings.Contains(changed.ApprovedText, "An order is paid once.\n") {
		t.Errorf("the changed statement replaces %q, want the approved version 1 %q", changed.ApprovedText, approvedText("an-order-is-paid-once"))
	}
	added := nodes["a-refund-returns-the-payment"]
	requireBlockOf(t, "the added statement", added.Text, spec, "requirement", "a-refund-returns-the-payment", "A refund returns the payment of an order.")
	if added.ApprovedText != "" {
		t.Errorf("the added statement replaces %q, want no approved text", added.ApprovedText)
	}
	removed := nodes["a-bill-names-its-order"]
	requireBlockOf(t, "the removed statement", removed.Text, spec, "requirement", "a-bill-names-its-order", "This statement goes.")
	if removed.ApprovedText != approvedText("a-bill-names-its-order") || !strings.Contains(removed.ApprovedText, "A bill names its order.\n") {
		t.Errorf("the removed statement removes %q, want the approved version 1 %q", removed.ApprovedText, approvedText("a-bill-names-its-order"))
	}
	if len(read.ChangedChecks) != 1 {
		t.Fatalf("the request changes the checks %+v, want a-second-payment-is-refused alone", read.ChangedChecks)
	}
	check := read.ChangedChecks[0]
	requireBlockOf(t, "the changed check", check.Text, checks, "check", "a-second-payment-is-refused", "Claim: A second payment of one order is refused at once.")
	if check.ApprovedText != approvedText("a-second-payment-is-refused") || !strings.Contains(check.ApprovedText, "Claim: A second payment of one order is refused.\n") {
		t.Errorf("the changed check replaces %q, want the approved version 1 %q", check.ApprovedText, approvedText("a-second-payment-is-refused"))
	}

	if !sheetDigest.MatchString(read.Digest) || !sheetDigest.MatchString(read.VerificationDigest) || read.Digest == read.VerificationDigest {
		t.Fatalf("the request reads the digests %q and %q, want two digests of two texts", read.Digest, read.VerificationDigest)
	}
	rig.approve(t, ask)
	if pinned := approvalDigest(t, rig, issue.PrimaryArtifactID, ask); read.Digest != pinned {
		t.Errorf("the request read the spec's digest %s before the approval, and the approval pinned %s", read.Digest, pinned)
	}
	if pinned := approvalDigest(t, rig, verificationID, ask); read.VerificationDigest != pinned {
		t.Errorf("the request read the Verification document's digest %s before the approval, and the approval pinned %s", read.VerificationDigest, pinned)
	}

	t.Run("an issue of no spec source", func(t *testing.T) {
		elsewhere := rig.issue(t, "Elsewhere", "acme/elsewhere", "A page loads.\n", "")
		ask := rig.request(t, elsewhere, false)
		read := decodeBody[sheetRead](t, readChangeRequest(t, rig, elsewhere.Key, ""))
		if read.SpecSource || len(read.Proposals) != 0 || read.VerificationVersion != 0 || read.VerificationDigest != "" || !sheetDigest.MatchString(read.Digest) {
			t.Fatalf("the request of no spec source reads spec_source %t proposals %+v verification %d %q digest %q, want no source, no proposal, no Verification digest and a digest",
				read.SpecSource, read.Proposals, read.VerificationVersion, read.VerificationDigest, read.Digest)
		}
		rig.approve(t, ask)
		if pinned := approvalDigest(t, rig, elsewhere.PrimaryArtifactID, ask); read.Digest != pinned {
			t.Errorf("the request read the digest %s, and the approval pinned %s", read.Digest, pinned)
		}
	})
}
